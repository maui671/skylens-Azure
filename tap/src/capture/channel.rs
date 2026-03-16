use anyhow::Result;
use std::collections::HashSet;
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use tokio::time::Duration;
use tracing::{debug, info, warn};

use super::nl80211::{
    build_hop_sequence, fallback_steps, set_channel_iw, set_freq_iw,
    ChanWidth, HopStep, Nl80211Channel,
};

/// Channels that get extra dwell time for ASTM F3411 RemoteID.
/// Channel 6 = mandatory NAN discovery channel (2.4 GHz) — 7x dwell
/// Channel 36 = UNII-1 (DJI Mini series, some Autel) — 2x dwell
/// Channel 44 = NAN social channel (5 GHz UNII-1) — 3x dwell
/// Channel 149 = preferred WiFi Beacon RemoteID channel (5 GHz) — 5x dwell
const PRIORITY_CHANNEL_6: i32 = 6;
const PRIORITY_CHANNEL_36: i32 = 36;
const PRIORITY_CHANNEL_44: i32 = 44;
const PRIORITY_CHANNEL_149: i32 = 149;

/// Channel hopper that cycles through configured WiFi channels.
/// Uses nl80211 netlink for fast channel switching with iw subprocess fallback.
/// Supports widest-first hopping: 80 MHz on 5 GHz, auto-fallback to 40/20 MHz.
pub struct ChannelHopper {
    interface: String,
    /// Shared channel list (can be updated at runtime)
    channels: Arc<RwLock<Vec<i32>>>,
    /// Shared hop interval (can be updated at runtime)
    hop_interval: Arc<RwLock<Duration>>,
    current_channel: Arc<AtomicI32>,
    running: Arc<AtomicBool>,
    /// Count of successful nl80211 hops (for stats logging)
    pub nl80211_hops: Arc<AtomicU64>,
    /// Count of iw fallback hops
    pub iw_fallback_hops: Arc<AtomicU64>,
    /// Count of wide-channel hops (40+80 MHz)
    pub width_hops: Arc<AtomicU64>,
    /// Epoch millis of last successful hop (for health monitoring)
    pub last_hop_time: Arc<AtomicU64>,
    /// Force iw subprocess (skip nl80211) — for drivers with broken nl80211
    force_iw: bool,
    /// Enable widest-first channel width hopping on 5 GHz
    width_hopping: bool,
}

impl ChannelHopper {
    pub fn new(
        interface: &str,
        channels: Vec<i32>,
        hop_interval_ms: u64,
        force_iw: bool,
        width_hopping: bool,
    ) -> Self {
        info!(
            interface, channels = ?channels, hop_interval_ms, force_iw, width_hopping,
            "Channel hopper initialized"
        );
        Self {
            interface: interface.to_string(),
            channels: Arc::new(RwLock::new(channels)),
            hop_interval: Arc::new(RwLock::new(Duration::from_millis(hop_interval_ms))),
            current_channel: Arc::new(AtomicI32::new(0)),
            running: Arc::new(AtomicBool::new(false)),
            nl80211_hops: Arc::new(AtomicU64::new(0)),
            iw_fallback_hops: Arc::new(AtomicU64::new(0)),
            width_hops: Arc::new(AtomicU64::new(0)),
            last_hop_time: Arc::new(AtomicU64::new(0)),
            force_iw,
            width_hopping,
        }
    }

    pub fn current_channel(&self) -> Arc<AtomicI32> {
        self.current_channel.clone()
    }

    pub fn stop_flag(&self) -> Arc<AtomicBool> {
        self.running.clone()
    }

    /// Get reference to channels for hot-reload updates
    pub fn channels_ref(&self) -> Arc<RwLock<Vec<i32>>> {
        self.channels.clone()
    }

    /// Get reference to hop interval for hot-reload updates
    pub fn interval_ref(&self) -> Arc<RwLock<Duration>> {
        self.hop_interval.clone()
    }

    /// Get last hop timestamp for health monitoring
    pub fn last_hop_time(&self) -> Arc<AtomicU64> {
        self.last_hop_time.clone()
    }

    /// Run the channel hopper loop with widest-first hopping + auto-fallback.
    pub async fn run(&self) -> Result<()> {
        self.running.store(true, Ordering::SeqCst);

        // Try to initialize nl80211 netlink channel switcher (skip if force_iw)
        let nl80211 = if self.force_iw {
            info!(interface = %self.interface, "force_iw=true, using iw subprocess for all channel switches");
            None
        } else {
            match Nl80211Channel::new(&self.interface) {
                Ok(nl) => {
                    info!(interface = %self.interface, "nl80211 netlink channel switcher ready");
                    Some(nl)
                }
                Err(e) => {
                    warn!(error = %e, "nl80211 init failed, using iw subprocess only");
                    None
                }
            }
        };

        let base_interval = self.hop_interval.read().map(|i| *i).unwrap_or(Duration::from_millis(200));
        let mut last_base_interval = base_interval;
        let mut use_iw_only = false; // Fall back to iw-only if nl80211 fails repeatedly

        // Build initial hop sequence
        let channels = self.channels.read().map(|ch| ch.clone()).unwrap_or_else(|_| vec![1, 6, 11]);
        let mut hop_sequence = build_hop_sequence(&channels, self.width_hopping);
        let mut last_channels = channels;

        // Track failed (channel, width) combos to avoid retrying every cycle
        let mut failed_widths: HashSet<(i32, u32)> = HashSet::new();

        log_hop_sequence(&hop_sequence);

        let mut idx: usize = 0;

        while self.running.load(Ordering::SeqCst) {
            // Check for interval changes
            if let Ok(current_interval) = self.hop_interval.read() {
                if *current_interval != last_base_interval {
                    info!(
                        old_ms = last_base_interval.as_millis(),
                        new_ms = current_interval.as_millis(),
                        "Hop interval changed"
                    );
                    last_base_interval = *current_interval;
                }
            }

            // Check for channel list changes → rebuild hop sequence
            if let Ok(current_channels) = self.channels.read() {
                if *current_channels != last_channels {
                    info!(old = last_channels.len(), new = current_channels.len(), "Channel list changed, rebuilding hop sequence");
                    last_channels = current_channels.clone();
                    hop_sequence = build_hop_sequence(&last_channels, self.width_hopping);
                    failed_widths.clear();
                    idx = 0;
                    log_hop_sequence(&hop_sequence);
                }
            }

            if hop_sequence.is_empty() {
                warn!("Hop sequence is empty, skipping");
                tokio::time::sleep(last_base_interval).await;
                continue;
            }

            // Wrap index
            if idx >= hop_sequence.len() {
                idx = 0;
            }

            // Check if this width was previously failed — skip to narrower
            if hop_sequence[idx].width != ChanWidth::Width20NoHt
                && failed_widths.contains(&(hop_sequence[idx].channel, hop_sequence[idx].width as u32))
            {
                // Already failed — replace with fallback steps inline
                let step_clone = hop_sequence[idx].clone();
                let replacements = fallback_steps(&step_clone);
                if !replacements.is_empty() {
                    info!(
                        channel = step_clone.channel,
                        width = ?step_clone.width,
                        fallback_count = replacements.len(),
                        "Replacing failed width with narrower fallback"
                    );
                    hop_sequence.splice(idx..idx + 1, replacements);
                    continue; // Retry at same idx with new step
                }
            }

            let step = &hop_sequence[idx];

            // Determine dwell time based on which channels this step covers
            let dwell = if step.covers.contains(&PRIORITY_CHANNEL_6) {
                // Ch 6 — NAN discovery + DJI DroneID — 7x dwell (works for both 20MHz and HT40+)
                // DJI NAN cycle is ~600ms; 1400ms guarantees catching 2+ full cycles per visit
                last_base_interval * 7
            } else if step.channel == 1 && step.width == ChanWidth::Width40 {
                // Ch 1 HT40+ — DJI DroneID burst coverage (2402-2442 MHz) — 3x dwell
                // DroneID bursts every 600ms, need 600ms dwell to guarantee catching one
                last_base_interval * 3
            } else if step.covers.contains(&PRIORITY_CHANNEL_44) {
                // Ch 44 — third NAN social channel (5 GHz UNII-1) — 3x dwell
                last_base_interval * 3
            } else if step.covers.contains(&PRIORITY_CHANNEL_36) {
                // Ch 36 — UNII-1 drones (DJI Mini, some Autel) — 2x dwell
                last_base_interval * 2
            } else if step.covers.contains(&PRIORITY_CHANNEL_149) {
                // ch149 block gets 5x dwell for NAN 5GHz + OcuSync coverage
                last_base_interval * 5
            } else {
                last_base_interval
            };

            // Execute the hop
            let success = self.execute_hop(step, &nl80211, use_iw_only).await;

            match success {
                HopResult::Ok => {
                    self.current_channel.store(step.channel, Ordering::Relaxed);
                    let now_ms = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_millis() as u64;
                    self.last_hop_time.store(now_ms, Ordering::Relaxed);

                    // Count wide hops
                    if step.width == ChanWidth::Width80 || step.width == ChanWidth::Width40 {
                        self.width_hops.fetch_add(1, Ordering::Relaxed);
                    }

                    if step.covers.contains(&PRIORITY_CHANNEL_6) {
                        debug!(channel = step.channel, width = ?step.width, dwell_ms = dwell.as_millis(), "NAN discovery channel (7x dwell)");
                    } else if step.width != ChanWidth::Width20NoHt {
                        debug!(
                            channel = step.channel,
                            width = ?step.width,
                            covers = ?step.covers,
                            dwell_ms = dwell.as_millis(),
                            "Wide-channel hop"
                        );
                    } else {
                        debug!(channel = step.channel, "Hopped to channel");
                    }

                    idx = (idx + 1) % hop_sequence.len();
                    tokio::time::sleep(dwell).await;
                }
                HopResult::Failed => {
                    // Width failed — cache failure and splice in fallback
                    if step.width != ChanWidth::Width20NoHt {
                        warn!(
                            channel = step.channel,
                            width = ?step.width,
                            "Wide hop failed, falling back to narrower width"
                        );
                        failed_widths.insert((step.channel, step.width as u32));
                        let replacements = fallback_steps(step);
                        if !replacements.is_empty() {
                            hop_sequence.splice(idx..idx + 1, replacements);
                            log_hop_sequence(&hop_sequence);
                            continue; // Retry at same idx with narrower step
                        }
                    }

                    // 20 MHz also failed — check for nl80211→iw escalation
                    // Only escalate if nl80211 was available but never succeeded
                    if !use_iw_only && nl80211.is_some() {
                        let nl_total = self.nl80211_hops.load(Ordering::Relaxed);
                        let iw_total = self.iw_fallback_hops.load(Ordering::Relaxed);
                        if nl_total == 0 && iw_total >= 10 {
                            warn!("nl80211 never succeeded after 10 iw fallbacks, switching to iw-only mode");
                            use_iw_only = true;
                        }
                    }

                    idx = (idx + 1) % hop_sequence.len();
                    tokio::time::sleep(Duration::from_millis(10)).await;
                }
            }
        }

        Ok(())
    }

    /// Execute a single hop step, trying nl80211 first then iw fallback.
    async fn execute_hop(&self, step: &HopStep, nl80211: &Option<Nl80211Channel>, use_iw_only: bool) -> HopResult {
        if let Some(ref nl) = nl80211 {
            if !use_iw_only {
                // Try nl80211 first
                match nl.set_freq(step.freq, step.width, step.center_freq1) {
                    Ok(()) => {
                        self.nl80211_hops.fetch_add(1, Ordering::Relaxed);
                        return HopResult::Ok;
                    }
                    Err(e) => {
                        debug!(channel = step.channel, width = ?step.width, error = %e, "nl80211 failed, trying iw");
                    }
                }
            }
        }

        // iw fallback
        let iw_result = if step.width == ChanWidth::Width20NoHt || step.width == ChanWidth::Width20 {
            set_channel_iw(&self.interface, step.channel).await
        } else {
            set_freq_iw(&self.interface, step.freq, step.width, step.center_freq1).await
        };

        match iw_result {
            Ok(()) => {
                self.iw_fallback_hops.fetch_add(1, Ordering::Relaxed);
                HopResult::Ok
            }
            Err(e) => {
                warn!(channel = step.channel, width = ?step.width, error = %e, "Channel hop failed");
                HopResult::Failed
            }
        }
    }
}

enum HopResult {
    Ok,
    Failed,
}

fn log_hop_sequence(seq: &[HopStep]) {
    let summary: Vec<String> = seq.iter().map(|s| {
        if s.width == ChanWidth::Width20NoHt {
            format!("ch{}@20", s.channel)
        } else {
            format!("ch{}@{}({})", s.channel, s.width.mhz(), s.covers.iter().map(|c| c.to_string()).collect::<Vec<_>>().join(","))
        }
    }).collect();
    info!(
        steps = seq.len(),
        sequence = %summary.join(" → "),
        "Hop sequence built"
    );
}

/// Enable monitor mode on the interface
pub async fn setup_monitor_mode(interface: &str) -> Result<()> {
    // First bring the interface down
    let down = tokio::process::Command::new("ip")
        .args(["link", "set", interface, "down"])
        .output()
        .await?;

    if !down.status.success() {
        warn!(
            "Failed to bring {} down: {}",
            interface,
            String::from_utf8_lossy(&down.stderr)
        );
    }

    // Set monitor mode
    let monitor = tokio::process::Command::new("iw")
        .args(["dev", interface, "set", "type", "monitor"])
        .output()
        .await?;

    if !monitor.status.success() {
        let stderr = String::from_utf8_lossy(&monitor.stderr);
        if !stderr.contains("already") {
            anyhow::bail!("Failed to set monitor mode: {}", stderr.trim());
        }
    }

    // Bring interface back up
    let up = tokio::process::Command::new("ip")
        .args(["link", "set", interface, "up"])
        .output()
        .await?;

    if !up.status.success() {
        anyhow::bail!(
            "Failed to bring {} up: {}",
            interface,
            String::from_utf8_lossy(&up.stderr)
        );
    }

    // Disable power management (iw replaces deprecated iwconfig)
    let ps_result = tokio::process::Command::new("iw")
        .args(["dev", interface, "set", "power_save", "off"])
        .output()
        .await;
    if let Err(e) = ps_result {
        warn!(error = %e, "Failed to disable power_save via iw");
    }

    Ok(())
}
