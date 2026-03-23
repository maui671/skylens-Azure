use anyhow::{Context, Result};
use async_nats::Client;
use futures::StreamExt;
use prost::Message;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use tokio::sync::mpsc;
use tracing::{debug, error, info, warn};

use super::buffer::{BufferConfig, BufferedPublisher, BufferMetrics};

/// NATS publisher that sends protobuf-encoded messages with reliable delivery
pub struct NatsPublisher {
    client: Client,
    tap_id: String,
    /// Pre-computed topic strings (avoid format!() allocation per publish)
    detection_topic: String,
    heartbeat_topic: String,
    ack_topic: String,
    command_topic: String,
    /// Buffered publisher for reliable message delivery
    buffered: BufferedPublisher,
    /// Mirror clients for fire-and-forget replication to additional NATS servers
    mirror_clients: Vec<Client>,
    /// Legacy counters (now proxied from buffer metrics)
    pub messages_sent: Arc<AtomicU64>,
    pub errors: Arc<AtomicU64>,
    /// Heartbeat publish counter — tracks successful heartbeat sends
    pub heartbeats_sent: Arc<AtomicU64>,
}

impl NatsPublisher {
    /// Connect to NATS server with reliable buffered publishing
    pub async fn connect(url: &str, tap_id: &str, buffer_config: BufferConfig, mirror_urls: &[String]) -> Result<Self> {
        info!(url, "Connecting to NATS");

        let client = async_nats::ConnectOptions::new()
            .retry_on_initial_connect()
            // Detect dead connections faster (default ~2min is too slow over Tailscale)
            .ping_interval(std::time::Duration::from_secs(10))
            .request_timeout(Some(std::time::Duration::from_secs(5)))
            // Reconnect aggressively — TAPs should never give up
            .max_reconnects(None)
            .event_callback({
                let url = url.to_string();
                move |event| {
                    let url = url.clone();
                    async move {
                        match event {
                            async_nats::Event::Disconnected => {
                                warn!(url, "NATS disconnected");
                            }
                            async_nats::Event::Connected => {
                                info!(url, "NATS reconnected");
                            }
                            async_nats::Event::SlowConsumer(count) => {
                                warn!(count, "NATS slow consumer");
                            }
                            _ => {}
                        }
                    }
                }
            })
            .connect(url)
            .await
            .with_context(|| format!("Failed to connect to NATS at {}", url))?;

        info!(url, "NATS connected");

        // Connect mirror clients (fire-and-forget, non-blocking)
        let mut mirror_clients = Vec::new();
        for mirror_url in mirror_urls {
            info!(url = %mirror_url, "Connecting to mirror NATS");
            match async_nats::ConnectOptions::new()
                .retry_on_initial_connect()
                .ping_interval(std::time::Duration::from_secs(5))
                .max_reconnects(None)
                .event_callback({
                    let url = mirror_url.clone();
                    move |event| {
                        let url = url.clone();
                        async move {
                            match event {
                                async_nats::Event::Disconnected => {
                                    warn!(url, "Mirror NATS disconnected");
                                }
                                async_nats::Event::Connected => {
                                    info!(url, "Mirror NATS connected");
                                }
                                _ => {}
                            }
                        }
                    }
                })
                .connect(mirror_url.as_str())
                .await
            {
                Ok(c) => {
                    info!(url = %mirror_url, "Mirror NATS connected");
                    mirror_clients.push(c);
                }
                Err(e) => {
                    warn!(url = %mirror_url, error = %e, "Failed to connect mirror NATS (will retry in background)");
                }
            }
        }

        // Create the buffered publisher
        let buffered = BufferedPublisher::new(
            client.clone(),
            tap_id.to_string(),
            buffer_config,
        );

        Ok(Self {
            client,
            tap_id: tap_id.to_string(),
            detection_topic: format!("skylens.detections.{}", tap_id),
            heartbeat_topic: format!("skylens.heartbeats.{}", tap_id),
            ack_topic: format!("skylens.acks.{}", tap_id),
            command_topic: format!("skylens.commands.{}", tap_id),
            buffered,
            mirror_clients,
            messages_sent: Arc::new(AtomicU64::new(0)),
            errors: Arc::new(AtomicU64::new(0)),
            heartbeats_sent: Arc::new(AtomicU64::new(0)),
        })
    }

    /// Publish a detection message with reliable delivery
    ///
    /// If NATS is disconnected, the detection is buffered and will be
    /// automatically retried when the connection is restored.
    /// Also publishes fire-and-forget to any mirror servers.
    pub async fn publish_detection(&self, detection: &crate::proto::Detection) -> Result<()> {
        self.buffered.publish_detection(detection, self.detection_topic.clone()).await?;
        self.messages_sent.fetch_add(1, Ordering::Relaxed);

        // Fire-and-forget to mirrors — spawned as detached tasks so they NEVER
        // block the detection hot path. Flush after publish to ensure data
        // actually hits the wire (async_nats buffers internally).
        if !self.mirror_clients.is_empty() {
            let data: bytes::Bytes = detection.encode_to_vec().into();
            let topic = self.detection_topic.clone();
            for mirror in self.mirror_clients.clone() {
                let data = data.clone();
                let topic = topic.clone();
                tokio::spawn(async move {
                    if let Err(e) = mirror.publish(topic, data).await {
                        debug!(error = %e, "Mirror detection publish failed");
                        return;
                    }
                    // Flush with timeout — don't let a slow mirror hang the task forever
                    let _ = tokio::time::timeout(
                        std::time::Duration::from_secs(2),
                        mirror.flush()
                    ).await;
                });
            }
        }

        Ok(())
    }

    /// Publish a heartbeat message
    ///
    /// Only the latest heartbeat is kept when offline.
    /// Old heartbeats are discarded since only the current state matters.
    /// Also publishes fire-and-forget to any mirror servers.
    pub async fn publish_heartbeat(&self, heartbeat: &crate::proto::TapHeartbeat) -> Result<()> {
        self.buffered.publish_heartbeat(heartbeat, self.heartbeat_topic.clone()).await?;
        self.heartbeats_sent.fetch_add(1, Ordering::Relaxed);

        // Fire-and-forget to mirrors — spawned with flush
        if !self.mirror_clients.is_empty() {
            let data: bytes::Bytes = heartbeat.encode_to_vec().into();
            let topic = self.heartbeat_topic.clone();
            for mirror in self.mirror_clients.clone() {
                let data = data.clone();
                let topic = topic.clone();
                tokio::spawn(async move {
                    if let Err(e) = mirror.publish(topic, data).await {
                        debug!(error = %e, "Mirror heartbeat publish failed");
                        return;
                    }
                    let _ = tokio::time::timeout(
                        std::time::Duration::from_secs(2),
                        mirror.flush()
                    ).await;
                });
            }
        }

        Ok(())
    }

    /// Publish a command acknowledgment (high priority, not buffered)
    pub async fn publish_ack(&self, ack: &crate::proto::TapCommandAck) -> Result<()> {
        let data = ack.encode_to_vec();
        self.buffered.publish_ack(data, self.ack_topic.clone()).await?;
        Ok(())
    }

    /// Subscribe to commands for this tap
    pub async fn subscribe_commands(
        &self,
    ) -> Result<mpsc::Receiver<crate::proto::TapCommand>> {
        let (tx, rx) = mpsc::channel(64);

        // Subscribe to tap-specific commands
        let specific = self
            .client
            .subscribe(self.command_topic.clone())
            .await?;

        // Subscribe to broadcast commands
        let broadcast = self.client.subscribe("skylens.commands.broadcast").await?;

        let tap_id = self.tap_id.clone();
        info!(tap_id, "Subscribed to command channels");

        // Spawn handler for tap-specific commands
        tokio::spawn(async move {
            let mut specific = specific;
            let mut broadcast = broadcast;

            loop {
                tokio::select! {
                    Some(msg) = specific.next() => {
                        match crate::proto::TapCommand::decode(msg.payload.as_ref()) {
                            Ok(cmd) => {
                                if tx.send(cmd).await.is_err() {
                                    break;
                                }
                            }
                            Err(e) => warn!(error = %e, "Failed to decode command"),
                        }
                    }
                    Some(msg) = broadcast.next() => {
                        match crate::proto::TapCommand::decode(msg.payload.as_ref()) {
                            Ok(cmd) => {
                                if tx.send(cmd).await.is_err() {
                                    break;
                                }
                            }
                            Err(e) => warn!(error = %e, "Failed to decode broadcast command"),
                        }
                    }
                    else => break,
                }
            }
        });

        Ok(rx)
    }

    /// Flush the NATS client write buffer to ensure all messages are sent
    pub async fn flush(&self) -> Result<()> {
        self.client
            .flush()
            .await
            .with_context(|| "Failed to flush NATS client")?;
        Ok(())
    }

    /// Check if connected
    pub fn is_connected(&self) -> bool {
        self.client.connection_state()
            == async_nats::connection::State::Connected
    }

    /// Get buffer metrics
    pub fn buffer_metrics(&self) -> &Arc<BufferMetrics> {
        &self.buffered.metrics
    }

    /// Get current buffer size
    pub fn buffer_size(&self) -> u64 {
        self.buffered.buffer_size()
    }

    /// Request graceful shutdown of the buffer worker
    pub async fn shutdown(&self) -> Result<()> {
        self.buffered.shutdown().await
    }
}
