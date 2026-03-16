use anyhow::{Context, Result};
use pcap::{Capture, Active};
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Instant;
use tracing::{info, warn, error};

/// Result of a pcap read attempt
pub enum PacketResult {
    /// Successfully captured a packet
    Packet(Vec<u8>),
    /// Timeout expired (normal, no packet available)
    Timeout,
    /// Hard error (adapter disconnected, interface gone, etc.)
    Error(String),
}

/// Wrapper around libpcap capture with self-healing reopen capability.
/// On some drivers (MT7921U), channel hopping can disconnect the AF_PACKET
/// socket, causing pcap to stop receiving packets. The `reopen()` method
/// allows the capture loop to detect this and recover.
pub struct PcapCapture {
    cap: Capture<Active>,
    interface: String,
    bpf_filter: Option<String>,
    buffer_mb: u32,
    pub packets_captured: Arc<AtomicU64>,
    pub bytes_captured: Arc<AtomicU64>,
    pub pcap_received: Arc<AtomicU32>,
    pub pcap_dropped: Arc<AtomicU32>,
    running: Arc<AtomicBool>,
    stats_counter: u64,
    last_stats_time: Instant,
}

impl PcapCapture {
    /// Open a capture on the given interface with an optional BPF filter
    pub fn new(interface: &str, bpf_filter: Option<&str>, buffer_mb: u32) -> Result<Self> {
        info!(interface, buffer_mb, "Opening pcap capture");

        let cap = Self::open_capture(interface, bpf_filter, buffer_mb)?;

        Ok(Self {
            cap,
            interface: interface.to_string(),
            bpf_filter: bpf_filter.map(|s| s.to_string()),
            buffer_mb,
            packets_captured: Arc::new(AtomicU64::new(0)),
            bytes_captured: Arc::new(AtomicU64::new(0)),
            pcap_received: Arc::new(AtomicU32::new(0)),
            pcap_dropped: Arc::new(AtomicU32::new(0)),
            running: Arc::new(AtomicBool::new(true)),
            stats_counter: 0,
            last_stats_time: Instant::now(),
        })
    }

    /// Open a pcap capture handle (shared between new() and reopen())
    fn open_capture(interface: &str, bpf_filter: Option<&str>, buffer_mb: u32) -> Result<Capture<Active>> {
        // NOTE: Do NOT use immediate_mode(true) here — it enables TPACKET_V3 which
        // can block indefinitely on some drivers (MT7921U) when the AF_PACKET socket
        // gets disconnected by channel hopping. Without immediate_mode, TPACKET_V2
        // properly respects the timeout, allowing stuck detection to work.
        let buffer_bytes = (buffer_mb as i32) * 1024 * 1024;
        let cap = Capture::from_device(interface)
            .with_context(|| format!("Failed to open device: {}", interface))?
            .promisc(true)
            .snaplen(2304) // 802.11 max MSDU — NAN MessagePacks with 6 ODID msgs can exceed 1024
            .buffer_size(buffer_bytes)
            .timeout(50) // 50ms read timeout — halves worst-case frame delivery latency
            .rfmon(false) // Monitor mode set externally via iw
            .open()
            .with_context(|| format!("Failed to activate capture on {}", interface))?;

        let mut cap = cap;

        // Apply BPF filter if provided
        if let Some(filter) = bpf_filter {
            info!(filter, "Setting BPF filter");
            cap.filter(filter, true)
                .with_context(|| format!("Failed to set BPF filter: {}", filter))?;
        }

        Ok(cap)
    }

    pub fn stop_flag(&self) -> Arc<AtomicBool> {
        self.running.clone()
    }

    /// Read the next packet. Returns Packet on success, Timeout on no data, Error on failure.
    ///
    /// Transient errors (kernel socket cleared, filter change race) are demoted to
    /// Timeout so they don't count toward the consecutive-error shutdown threshold.
    /// Only genuinely unrecoverable errors (interface disappeared, device gone) are
    /// returned as Error.
    pub fn next_packet(&mut self) -> PacketResult {
        if !self.running.load(Ordering::Relaxed) {
            return PacketResult::Timeout;
        }

        match self.cap.next_packet() {
            Ok(packet) => {
                let data = packet.data.to_vec();
                self.packets_captured.fetch_add(1, Ordering::Relaxed);
                self.bytes_captured
                    .fetch_add(data.len() as u64, Ordering::Relaxed);

                // Update kernel stats every 500 packets OR every 5 seconds (whichever first).
                // At low packet rates (10 pps), 500-packet refresh takes 50s — far too stale
                // for the 5s heartbeat cycle.
                self.stats_counter += 1;
                if self.stats_counter % 500 == 0
                    || self.last_stats_time.elapsed().as_secs() >= 5
                {
                    self.refresh_stats();
                    self.last_stats_time = Instant::now();
                }

                PacketResult::Packet(data)
            }
            Err(pcap::Error::TimeoutExpired) => PacketResult::Timeout,
            Err(e) => {
                let msg = format!("{}", e);

                // Classify error: transient kernel conditions are demoted to Timeout.
                // libpcap 1.10.x on Linux with TPACKET_V2 internally retries EINTR
                // in poll()/recvfrom(), so true EINTR rarely reaches us. But the
                // kernel can raise SO_ERROR on the AF_PACKET socket during channel
                // hops (nl80211 SET_WIPHY), producing:
                //   "Error condition on packet socket: Reported error was 0"
                //   "recv failed when changing filter"
                // These are transient and self-clearing.
                if Self::is_transient_error(&msg) {
                    warn!(error = %msg, "pcap transient error (treated as timeout)");
                    PacketResult::Timeout
                } else {
                    error!(error = %msg, "pcap read error");
                    PacketResult::Error(msg)
                }
            }
        }
    }

    /// Returns true if the pcap error string indicates a transient condition
    /// that will self-resolve (not a USB disconnect or interface removal).
    fn is_transient_error(msg: &str) -> bool {
        // "Error condition on packet socket: Reported error was 0" — kernel cleared
        // the error before libpcap could read it; no actual failure occurred.
        if msg.contains("Reported error was 0") {
            return true;
        }
        // "recv failed when changing filter" — race during BPF hot-swap, next read works.
        if msg.contains("recv failed when changing filter") {
            return true;
        }
        // IoError(Interrupted) from the pcap crate — EINTR leaked through.
        if msg.contains("Interrupted") {
            return true;
        }
        // IoError(WouldBlock) — EAGAIN on non-blocking (shouldn't happen, but safe to retry).
        if msg.contains("WouldBlock") {
            return true;
        }
        false
    }

    /// Close and reopen the pcap capture handle.
    /// Called when the capture loop detects zero packets for an extended period
    /// (stuck AF_PACKET socket, common on MT7921U after channel hopping).
    ///
    /// Verifies monitor mode is still active after reopen by checking
    /// /sys/class/net/<iface>/type == 803 (ARPHRD_IEEE80211_RADIOTAP).
    pub fn reopen(&mut self) -> Result<()> {
        warn!(
            interface = %self.interface,
            "Reopening pcap capture handle (stuck socket recovery)"
        );

        // Verify monitor mode before reopening — if it's gone, reopen will
        // succeed but capture radically wrong data (or nothing at all).
        self.verify_monitor_mode()
            .with_context(|| format!("Monitor mode lost on {} during reopen", self.interface))?;

        // Drop the old capture by replacing it
        let new_cap = Self::open_capture(&self.interface, self.bpf_filter.as_deref(), self.buffer_mb)?;
        self.cap = new_cap;
        self.stats_counter = 0;
        self.last_stats_time = Instant::now();

        info!(interface = %self.interface, "pcap capture handle reopened");
        Ok(())
    }

    /// Check that the interface is still in monitor mode.
    /// Returns Ok(()) if /sys/class/net/<iface>/type == 803 (radiotap).
    /// Returns Err if the file is missing (interface gone) or type != 803.
    fn verify_monitor_mode(&self) -> Result<()> {
        let path = format!("/sys/class/net/{}/type", self.interface);
        let content = std::fs::read_to_string(&path)
            .with_context(|| format!("Cannot read {} — interface may be gone", path))?;
        let iftype: u32 = content.trim().parse()
            .with_context(|| format!("Invalid interface type value: {:?}", content.trim()))?;
        if iftype != 803 {
            anyhow::bail!(
                "Interface {} type is {} (expected 803 for radiotap monitor mode)",
                self.interface, iftype
            );
        }
        Ok(())
    }

    /// Refresh pcap kernel stats into atomics
    fn refresh_stats(&mut self) {
        if let Ok(stats) = self.cap.stats() {
            self.pcap_received.store(stats.received, Ordering::Relaxed);
            self.pcap_dropped.store(stats.dropped, Ordering::Relaxed);
        }
    }

    /// Get pcap stats (packets received/dropped by kernel)
    pub fn stats(&mut self) -> Option<(u32, u32)> {
        match self.cap.stats() {
            Ok(stats) => Some((stats.received, stats.dropped)),
            Err(_) => None,
        }
    }
}
