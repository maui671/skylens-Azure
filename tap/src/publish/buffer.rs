//! Reliable message delivery buffer for NATS publishing.
//!
//! Provides offline buffering and retry logic to ensure detections are never lost
//! during network disruptions or Node restarts.

use crate::proto::{Detection, TapHeartbeat};
use prost::Message;
use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::mpsc;
use tracing::{debug, error, info, warn};

/// Configuration for the detection buffer
#[derive(Debug, Clone)]
pub struct BufferConfig {
    /// Maximum number of detections to buffer (default: 10000)
    pub max_size: usize,
    /// Maximum retries before dropping a detection (default: 100)
    pub max_retries: u64,
    /// Initial retry delay in milliseconds (default: 1000)
    pub initial_retry_delay_ms: u64,
    /// Maximum retry delay in milliseconds (default: 30000)
    pub max_retry_delay_ms: u64,
    /// Buffer fullness warning threshold (default: 0.8 = 80%)
    pub warning_threshold: f32,
}

impl Default for BufferConfig {
    fn default() -> Self {
        Self {
            max_size: 10_000,
            max_retries: 10_000, // Effectively unlimited — buffer overflow handles eviction
            initial_retry_delay_ms: 1_000,
            max_retry_delay_ms: 30_000,
            warning_threshold: 0.8,
        }
    }
}

/// A buffered detection with retry metadata
#[derive(Debug, Clone)]
struct BufferedDetection {
    /// The encoded protobuf detection data
    data: Vec<u8>,
    /// Topic to publish to
    topic: String,
    /// Number of retry attempts
    retry_count: u64,
    /// When this detection was first buffered
    buffered_at: Instant,
    /// When the next retry should be attempted
    next_retry_at: Instant,
}

/// Metrics for the detection buffer
#[derive(Debug, Default)]
pub struct BufferMetrics {
    /// Current number of buffered detections
    pub buffer_size: AtomicU64,
    /// Total detections dropped due to overflow
    pub buffer_dropped: AtomicU64,
    /// Total publish retry attempts
    pub publish_retries: AtomicU64,
    /// Total successful publishes
    pub publishes_success: AtomicU64,
    /// Total failed publishes (after max retries)
    pub publishes_failed: AtomicU64,
    /// Number of NATS disconnection events
    pub nats_disconnects: AtomicU64,
    /// Number of NATS reconnection events
    pub nats_reconnects: AtomicU64,
}

impl BufferMetrics {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }
}

/// Message types for the buffer worker
pub enum BufferMessage {
    /// A new detection to publish
    Detection {
        detection: Detection,
        topic: String,
    },
    /// A heartbeat to publish (only latest is kept)
    Heartbeat {
        heartbeat: TapHeartbeat,
        topic: String,
    },
    /// Acknowledgment message (high priority, no buffering)
    Ack {
        data: Vec<u8>,
        topic: String,
    },
    /// NATS connection state changed
    ConnectionState(bool),
    /// Shutdown signal
    Shutdown,
}

/// Reliable detection buffer with offline persistence
pub struct DetectionBuffer {
    /// Pending detections queue (oldest first)
    pending: VecDeque<BufferedDetection>,
    /// Latest heartbeat (only keep most recent)
    latest_heartbeat: Option<(Vec<u8>, String)>,
    /// Configuration
    config: BufferConfig,
    /// Metrics
    metrics: Arc<BufferMetrics>,
    /// Whether NATS is currently connected
    is_connected: bool,
    /// Last time we logged a buffer warning
    last_warning_log: Option<Instant>,
}

impl DetectionBuffer {
    /// Create a new detection buffer
    pub fn new(config: BufferConfig, metrics: Arc<BufferMetrics>) -> Self {
        Self {
            pending: VecDeque::with_capacity(config.max_size),
            latest_heartbeat: None,
            config,
            metrics,
            is_connected: false,
            last_warning_log: None,
        }
    }

    /// Add a detection to the buffer
    pub fn push_detection(&mut self, data: Vec<u8>, topic: String) {
        let now = Instant::now();

        // Check if buffer is full
        if self.pending.len() >= self.config.max_size {
            // Drop oldest detection to make room
            if self.pending.pop_front().is_some() {
                self.metrics.buffer_dropped.fetch_add(1, Ordering::Relaxed);
                warn!(
                    buffer_size = self.pending.len(),
                    max_size = self.config.max_size,
                    "Buffer full, dropped oldest detection"
                );
            }
        }

        // Check buffer fullness warning
        let fullness = self.pending.len() as f32 / self.config.max_size as f32;
        if fullness >= self.config.warning_threshold {
            let should_log = self.last_warning_log
                .map(|t| now.duration_since(t) > Duration::from_secs(10))
                .unwrap_or(true);

            if should_log {
                warn!(
                    buffer_size = self.pending.len(),
                    max_size = self.config.max_size,
                    fullness_pct = fullness * 100.0,
                    "Detection buffer is getting full"
                );
                self.last_warning_log = Some(now);
            }
        }

        let detection = BufferedDetection {
            data,
            topic,
            retry_count: 0,
            buffered_at: now,
            next_retry_at: now, // Try immediately
        };

        self.pending.push_back(detection);
        self.metrics.buffer_size.store(self.pending.len() as u64, Ordering::Relaxed);
    }

    /// Update the latest heartbeat (replaces any previous)
    pub fn set_heartbeat(&mut self, data: Vec<u8>, topic: String) {
        self.latest_heartbeat = Some((data, topic));
    }

    /// Get the next detection ready for retry, if any.
    /// Returns (data, topic, retry_count) so the caller can pass the correct
    /// retry_count back to requeue() on failure.
    pub fn pop_ready(&mut self) -> Option<(Vec<u8>, String, u64)> {
        let now = Instant::now();

        // Find the first detection that is ready for retry
        if let Some(idx) = self.pending.iter().position(|d| d.next_retry_at <= now) {
            if let Some(detection) = self.pending.remove(idx) {
                // Update metrics
                self.metrics.buffer_size.store(self.pending.len() as u64, Ordering::Relaxed);

                // Return the detection with its current retry count
                let data = detection.data;
                let topic = detection.topic;
                let retry_count = detection.retry_count;

                return Some((data, topic, retry_count));
            }
        }

        None
    }

    /// Re-queue a detection that failed to publish
    pub fn requeue(&mut self, data: Vec<u8>, topic: String, retry_count: u64) {
        let now = Instant::now();

        // Check if we've exceeded max retries
        if retry_count >= self.config.max_retries {
            self.metrics.publishes_failed.fetch_add(1, Ordering::Relaxed);
            warn!(
                retry_count,
                max_retries = self.config.max_retries,
                "Detection dropped after max retries"
            );
            return;
        }

        // Calculate exponential backoff delay
        let delay_ms = self.config.initial_retry_delay_ms
            .saturating_mul(2u64.saturating_pow(retry_count as u32))
            .min(self.config.max_retry_delay_ms);

        let detection = BufferedDetection {
            data,
            topic,
            retry_count,
            buffered_at: now,
            next_retry_at: now + Duration::from_millis(delay_ms),
        };

        // Insert at front for FIFO order after accounting for retry delay
        self.pending.push_front(detection);
        self.metrics.buffer_size.store(self.pending.len() as u64, Ordering::Relaxed);
        self.metrics.publish_retries.fetch_add(1, Ordering::Relaxed);

        debug!(
            retry_count,
            delay_ms,
            buffer_size = self.pending.len(),
            "Detection requeued for retry"
        );
    }

    /// Get and clear the latest heartbeat
    pub fn take_heartbeat(&mut self) -> Option<(Vec<u8>, String)> {
        self.latest_heartbeat.take()
    }

    /// Set connection state
    pub fn set_connected(&mut self, connected: bool) {
        let was_connected = self.is_connected;
        self.is_connected = connected;

        if connected && !was_connected {
            self.metrics.nats_reconnects.fetch_add(1, Ordering::Relaxed);
            info!(
                buffer_size = self.pending.len(),
                "NATS reconnected, will flush {} buffered detections",
                self.pending.len()
            );
        } else if !connected && was_connected {
            self.metrics.nats_disconnects.fetch_add(1, Ordering::Relaxed);
            warn!("NATS disconnected, buffering detections");
        }
    }

    /// Check if connected
    pub fn is_connected(&self) -> bool {
        self.is_connected
    }

    /// Get current buffer size
    pub fn len(&self) -> usize {
        self.pending.len()
    }

    /// Check if buffer is empty
    pub fn is_empty(&self) -> bool {
        self.pending.is_empty()
    }

    /// Record a successful publish
    pub fn record_success(&self) {
        self.metrics.publishes_success.fetch_add(1, Ordering::Relaxed);
    }

    /// Drain ALL pending detections, regardless of backoff state.
    /// Used during shutdown to ensure no detections are silently abandoned.
    pub fn drain_all(&mut self) -> Vec<(Vec<u8>, String)> {
        let items: Vec<_> = self.pending.drain(..)
            .map(|d| (d.data, d.topic))
            .collect();
        self.metrics.buffer_size.store(0, Ordering::Relaxed);
        items
    }
}

/// Buffered publisher that wraps NatsPublisher with reliability guarantees
pub struct BufferedPublisher {
    /// Channel to send messages to the buffer worker
    tx: mpsc::Sender<BufferMessage>,
    /// Metrics (shared with buffer worker)
    pub metrics: Arc<BufferMetrics>,
    /// Shutdown flag
    shutdown: Arc<AtomicBool>,
}

impl BufferedPublisher {
    /// Create a new buffered publisher
    ///
    /// Spawns a background worker that manages the buffer and handles retries.
    pub fn new(
        client: async_nats::Client,
        tap_id: String,
        config: BufferConfig,
    ) -> Self {
        let metrics = BufferMetrics::new();
        let (tx, rx) = mpsc::channel(2048); // Large channel to avoid backpressure
        let shutdown = Arc::new(AtomicBool::new(false));

        // Spawn the buffer worker
        let worker_metrics = metrics.clone();
        let worker_shutdown = shutdown.clone();
        tokio::spawn(async move {
            buffer_worker(client, tap_id, config, worker_metrics, rx, worker_shutdown).await;
        });

        Self {
            tx,
            metrics,
            shutdown,
        }
    }

    /// Queue a detection for publishing
    pub async fn publish_detection(&self, detection: &Detection, topic: String) -> anyhow::Result<()> {
        let msg = BufferMessage::Detection {
            detection: detection.clone(),
            topic,
        };
        self.tx.send(msg).await
            .map_err(|e| anyhow::anyhow!("Buffer channel closed: {}", e))
    }

    /// Queue a heartbeat for publishing (only latest is kept)
    pub async fn publish_heartbeat(&self, heartbeat: &TapHeartbeat, topic: String) -> anyhow::Result<()> {
        let msg = BufferMessage::Heartbeat {
            heartbeat: heartbeat.clone(),
            topic,
        };
        self.tx.send(msg).await
            .map_err(|e| anyhow::anyhow!("Buffer channel closed: {}", e))
    }

    /// Publish an acknowledgment (high priority, not buffered)
    pub async fn publish_ack(&self, data: Vec<u8>, topic: String) -> anyhow::Result<()> {
        let msg = BufferMessage::Ack { data, topic };
        self.tx.send(msg).await
            .map_err(|e| anyhow::anyhow!("Buffer channel closed: {}", e))
    }

    /// Notify of connection state change
    pub async fn set_connection_state(&self, connected: bool) -> anyhow::Result<()> {
        self.tx.send(BufferMessage::ConnectionState(connected)).await
            .map_err(|e| anyhow::anyhow!("Buffer channel closed: {}", e))
    }

    /// Request shutdown
    pub async fn shutdown(&self) -> anyhow::Result<()> {
        self.shutdown.store(true, Ordering::SeqCst);
        self.tx.send(BufferMessage::Shutdown).await
            .map_err(|e| anyhow::anyhow!("Buffer channel closed: {}", e))
    }

    /// Get current buffer size
    pub fn buffer_size(&self) -> u64 {
        self.metrics.buffer_size.load(Ordering::Relaxed)
    }
}

/// Background worker that manages the detection buffer
async fn buffer_worker(
    client: async_nats::Client,
    tap_id: String,
    config: BufferConfig,
    metrics: Arc<BufferMetrics>,
    mut rx: mpsc::Receiver<BufferMessage>,
    shutdown: Arc<AtomicBool>,
) {
    let mut buffer = DetectionBuffer::new(config.clone(), metrics.clone());

    // Track connection state
    let mut last_state_check = Instant::now();
    let state_check_interval = Duration::from_millis(100);

    // Batch flush interval — lower = faster detection delivery to node
    let flush_interval = Duration::from_millis(10);
    let mut flush_ticker = tokio::time::interval(flush_interval);
    flush_ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

    // Batch flush: track whether we have un-flushed publishes
    let mut needs_flush = false;

    info!(
        tap_id = %tap_id,
        max_size = config.max_size,
        max_retries = config.max_retries,
        "Detection buffer worker started"
    );

    loop {
        // Note: We do NOT check the shutdown AtomicBool here. Shutdown is signaled
        // exclusively via the BufferMessage::Shutdown variant through the channel.
        // Checking the flag here caused a race: the flag was set before the Shutdown
        // message arrived, causing the worker to break before draining messages
        // still in the rx channel — permanently losing them.

        // Periodically check NATS connection state
        let now = Instant::now();
        if now.duration_since(last_state_check) >= state_check_interval {
            let connected = client.connection_state() == async_nats::connection::State::Connected;
            if connected != buffer.is_connected() {
                buffer.set_connected(connected);
            }
            last_state_check = now;
        }

        tokio::select! {
            // Process incoming messages
            Some(msg) = rx.recv() => {
                match msg {
                    BufferMessage::Detection { detection, topic } => {
                        let data = detection.encode_to_vec();

                        // If connected and buffer empty, publish without per-detection flush.
                        // Flush is batched on the next tick for ~10-20x fewer network round-trips.
                        if buffer.is_connected() && buffer.is_empty() {
                            match client.publish(topic.clone(), data.clone().into()).await {
                                Ok(()) => {
                                    buffer.record_success();
                                    needs_flush = true;
                                }
                                Err(e) => {
                                    debug!(error = %e, "Direct publish failed, buffering");
                                    buffer.push_detection(data, topic);
                                }
                            }
                        } else {
                            // Buffer the detection
                            buffer.push_detection(data, topic);
                        }
                    }

                    BufferMessage::Heartbeat { heartbeat, topic } => {
                        let data = heartbeat.encode_to_vec();

                        // Heartbeats: try to send immediately, otherwise just keep latest
                        if buffer.is_connected() {
                            match client.publish(topic.clone(), data.clone().into()).await {
                                Ok(()) => {
                                    needs_flush = true;
                                    debug!(topic, "Heartbeat published");
                                }
                                Err(e) => {
                                    debug!(error = %e, "Heartbeat publish failed, keeping latest");
                                    buffer.set_heartbeat(data, topic);
                                }
                            }
                        } else {
                            buffer.set_heartbeat(data, topic);
                        }
                    }

                    BufferMessage::Ack { data, topic } => {
                        // Acks are high priority, try immediately
                        if buffer.is_connected() {
                            if let Err(e) = client.publish(topic.clone(), data.into()).await {
                                warn!(error = %e, topic, "Failed to publish ack");
                            } else {
                                needs_flush = true;
                            }
                        } else {
                            // Drop acks when disconnected (they're time-sensitive)
                            warn!("Dropping ack, NATS disconnected");
                        }
                    }

                    BufferMessage::ConnectionState(connected) => {
                        buffer.set_connected(connected);
                    }

                    BufferMessage::Shutdown => {
                        info!("Buffer worker received shutdown signal");
                        break;
                    }
                }
            }

            // Flush buffer periodically when connected
            _ = flush_ticker.tick() => {
                if !buffer.is_connected() {
                    continue;
                }

                // Try to publish heartbeat first (if any)
                if let Some((data, topic)) = buffer.take_heartbeat() {
                    if let Err(e) = client.publish(topic.clone(), data.clone().into()).await {
                        debug!(error = %e, "Heartbeat flush failed");
                        buffer.set_heartbeat(data, topic);
                    } else {
                        needs_flush = true;
                    }
                }

                // Flush pending detections (batch up to 100 per tick)
                let mut flushed = 0;
                while flushed < 100 {
                    if let Some((data, topic, retry_count)) = buffer.pop_ready() {
                        match client.publish(topic.clone(), data.clone().into()).await {
                            Ok(()) => {
                                buffer.record_success();
                                needs_flush = true;
                                flushed += 1;
                            }
                            Err(e) => {
                                debug!(error = %e, retry_count, "Buffer flush publish failed, requeueing");
                                buffer.requeue(data, topic, retry_count + 1);
                                break; // Stop flushing if we hit an error
                            }
                        }
                    } else {
                        break; // No more ready detections
                    }
                }

                if flushed > 0 {
                    debug!(flushed, remaining = buffer.len(), "Buffer flush completed");
                }

                // Single batched flush for all publishes since last tick
                if needs_flush {
                    match tokio::time::timeout(Duration::from_secs(2), client.flush()).await {
                        Ok(Ok(())) => {
                            needs_flush = false;
                        }
                        Ok(Err(e)) => {
                            warn!(error = %e, "Periodic flush failed");
                        }
                        Err(_) => {
                            warn!("Periodic flush timeout (2s)");
                        }
                    }
                }
            }
        }
    }

    // Final drain attempt on shutdown — drain ALL detections including those in backoff.
    // Previously used pop_ready() which skipped detections with future next_retry_at,
    // silently abandoning them.
    if buffer.is_connected() {
        // Also drain any remaining messages from the rx channel that arrived
        // between the shutdown flag being set and the worker exiting the loop.
        while let Ok(msg) = rx.try_recv() {
            if let BufferMessage::Detection { detection, topic } = msg {
                let data = detection.encode_to_vec();
                buffer.push_detection(data, topic);
            }
        }

        let all_pending = buffer.drain_all();
        let total = all_pending.len();
        info!(total, "Attempting final buffer drain on shutdown (including backoff items)");
        let mut drained = 0;
        for (data, topic) in all_pending {
            if client.publish(topic, data.into()).await.is_ok() {
                drained += 1;
            } else {
                break;
            }
        }
        info!(drained, total, "Final drain completed");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_buffer_overflow() {
        let config = BufferConfig {
            max_size: 3,
            ..Default::default()
        };
        let metrics = BufferMetrics::new();
        let mut buffer = DetectionBuffer::new(config, metrics.clone());

        // Add 5 detections to a buffer of size 3
        for i in 0..5 {
            buffer.push_detection(vec![i as u8], format!("topic.{}", i));
        }

        // Should have dropped 2 oldest
        assert_eq!(buffer.len(), 3);
        assert_eq!(metrics.buffer_dropped.load(Ordering::Relaxed), 2);
    }

    #[test]
    fn test_heartbeat_replacement() {
        let config = BufferConfig::default();
        let metrics = BufferMetrics::new();
        let mut buffer = DetectionBuffer::new(config, metrics);

        buffer.set_heartbeat(vec![1], "hb1".to_string());
        buffer.set_heartbeat(vec![2], "hb2".to_string());

        let (data, topic) = buffer.take_heartbeat().unwrap();
        assert_eq!(data, vec![2]);
        assert_eq!(topic, "hb2");
        assert!(buffer.take_heartbeat().is_none());
    }

    #[test]
    fn test_exponential_backoff() {
        let config = BufferConfig {
            initial_retry_delay_ms: 1000,
            max_retry_delay_ms: 30000,
            ..Default::default()
        };
        let metrics = BufferMetrics::new();
        let mut buffer = DetectionBuffer::new(config, metrics);

        // Calculate expected delays
        // retry 0: 1000ms
        // retry 1: 2000ms
        // retry 2: 4000ms
        // retry 3: 8000ms
        // retry 4: 16000ms
        // retry 5: 30000ms (capped)

        buffer.requeue(vec![1], "topic".to_string(), 0);
        assert_eq!(buffer.len(), 1);

        buffer.requeue(vec![2], "topic".to_string(), 5);
        assert_eq!(buffer.len(), 2);
    }
}
