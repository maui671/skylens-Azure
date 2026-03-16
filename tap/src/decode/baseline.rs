use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tracing::{info, warn};

/// Entry for a single BSSID in the baseline
#[allow(dead_code)]
struct BssidEntry {
    first_seen: Instant,
    last_seen: Instant,
    ssid: String,
    channel: i32,
    frame_count: u64,
    rssi_sum: i64,
    rssi_min: i32,
    rssi_max: i32,
}

/// Shared metrics exposed to heartbeat/stats via atomics
pub struct BaselineMetrics {
    pub total_bssids: AtomicU64,
    pub new_bssids_interval: AtomicU64,
}

/// Maximum BSSID entries before triggering eviction.
/// At ~200 bytes per entry, 50K entries ≈ 10 MB — well within Pi 5 budget.
const MAX_BSSID_ENTRIES: usize = 50_000;

/// Eviction threshold: remove entries not seen in the last 30 minutes.
const EVICTION_AGE: Duration = Duration::from_secs(30 * 60);

/// Tracks all BSSIDs seen by the TAP for baseline/anomaly detection.
/// Runs in the capture thread (single-threaded, no locking needed for the map).
pub struct BssidBaseline {
    entries: HashMap<[u8; 6], BssidEntry>,
    metrics: Arc<BaselineMetrics>,
    last_eviction: Instant,
}

impl BssidBaseline {
    pub fn new() -> (Self, Arc<BaselineMetrics>) {
        let metrics = Arc::new(BaselineMetrics {
            total_bssids: AtomicU64::new(0),
            new_bssids_interval: AtomicU64::new(0),
        });
        (
            Self {
                entries: HashMap::new(),
                metrics: metrics.clone(),
                last_eviction: Instant::now(),
            },
            metrics,
        )
    }

    /// Record a BSSID observation. Called from capture loop for every management frame.
    /// Returns true if this is a brand new BSSID (never seen before).
    pub fn observe(&mut self, bssid: &[u8; 6], ssid: &str, channel: i32, rssi: i32) -> bool {
        let now = Instant::now();

        if let Some(entry) = self.entries.get_mut(bssid) {
            // Update existing entry
            entry.last_seen = now;
            entry.frame_count += 1;
            entry.rssi_sum += rssi as i64;
            if rssi < entry.rssi_min {
                entry.rssi_min = rssi;
            }
            if rssi > entry.rssi_max {
                entry.rssi_max = rssi;
            }
            // Update SSID if non-empty (some frames have empty SSID)
            if !ssid.is_empty() && entry.ssid != ssid {
                entry.ssid = ssid.to_string();
            }
            // Update channel (BSSID may be seen on multiple channels)
            entry.channel = channel;
            false
        } else {
            // New BSSID
            self.entries.insert(*bssid, BssidEntry {
                first_seen: now,
                last_seen: now,
                ssid: ssid.to_string(),
                channel,
                frame_count: 1,
                rssi_sum: rssi as i64,
                rssi_min: rssi,
                rssi_max: rssi,
            });

            let total = self.entries.len() as u64;
            self.metrics.total_bssids.store(total, Ordering::Relaxed);
            self.metrics.new_bssids_interval.fetch_add(1, Ordering::Relaxed);

            info!(
                bssid = %format_mac(bssid),
                ssid,
                channel,
                rssi,
                total_bssids = total,
                "New BSSID discovered"
            );

            // Evict stale entries when map grows too large
            if self.entries.len() > MAX_BSSID_ENTRIES
                && self.last_eviction.elapsed() > Duration::from_secs(60)
            {
                let before = self.entries.len();
                self.entries.retain(|_, e| e.last_seen.elapsed() < EVICTION_AGE);
                let after = self.entries.len();
                self.metrics.total_bssids.store(after as u64, Ordering::Relaxed);
                self.last_eviction = now;
                warn!(
                    before,
                    after,
                    evicted = before - after,
                    "BSSID baseline eviction (exceeded {} entries)",
                    MAX_BSSID_ENTRIES
                );
            }

            true
        }
    }

    /// Reset the interval counter (called from heartbeat to get new-BSSIDs-per-interval)
    pub fn reset_interval_counter(metrics: &BaselineMetrics) -> u64 {
        metrics.new_bssids_interval.swap(0, Ordering::Relaxed)
    }

    /// Get total unique BSSIDs
    pub fn total_count(&self) -> u64 {
        self.entries.len() as u64
    }
}

fn format_mac(mac: &[u8; 6]) -> String {
    format!(
        "{:02X}:{:02X}:{:02X}:{:02X}:{:02X}:{:02X}",
        mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]
    )
}
