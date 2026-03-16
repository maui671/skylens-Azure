mod capture;
mod config;
mod decode;
mod publish;

pub mod proto {
    include!(concat!(env!("OUT_DIR"), "/skylens.rs"));
}

use anyhow::Result;
use capture::channel::{setup_monitor_mode, ChannelHopper};
use capture::pcap::{PcapCapture, PacketResult};
use capture::ble::BleStats;
use config::Config;
use decode::baseline::{BssidBaseline, BaselineMetrics};
use decode::frame::{self, format_mac, is_dji_droneid_ie, is_remoteid_ie, is_parrot_ie, is_autel_ie, get_drone_manufacturer_from_oui, FrameType};
use decode::oui::IntelDatabase;
use publish::buffer::BufferConfig as PublishBufferConfig;
use publish::nats::NatsPublisher;

use std::collections::{HashMap, HashSet};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};
use tokio::sync::{mpsc, watch};
use tracing::{debug, error, info, warn};
use uuid::Uuid;

/// Shared runtime configuration that can be hot-reloaded without restart
#[derive(Debug, Clone)]
struct RuntimeConfig {
    /// Channel list for hopping
    channels: Vec<i32>,
    /// Dwell time per channel in milliseconds
    hop_interval_ms: u64,
    /// Current BPF filter (None = no filter / all packets)
    bpf_filter: Option<String>,
}

/// Message type for signaling runtime config updates to the capture thread
#[derive(Debug)]
enum ConfigUpdate {
    Channels { channels: Vec<i32>, hop_interval_ms: u64 },
    BpfFilter { filter: Option<String> },
}

static DETECTIONS_SENT: AtomicU64 = AtomicU64::new(0);
/// WiFi detections dropped because det_tx channel was full
static WIFI_CHANNEL_DROPS: AtomicU64 = AtomicU64::new(0);

/// Result of behavioral drone detection analysis
/// Returns (is_suspect, confidence_score, primary_reason)
type BehaviorResult = (bool, f32, &'static str);

/// Threshold for behavioral detection (score >= this = suspect)
/// Raised from 0.25 to 0.45 to require multiple signals — a single weak signal
/// (like 5.8GHz channel alone or LAA MAC alone) should NOT trigger a detection.
const BEHAVIOR_THRESHOLD: f32 = 0.45;

/// Dedup interval for DJI RC controllers (5 minutes).
/// Controllers broadcast constantly but rarely carry new data. Standard dedup (1s)
/// floods the node with duplicate detections. 5 minutes still provides fresh RSSI
/// for trilateration while cutting ~1800 daily duplicates to ~288.
const CONTROLLER_DEDUP_SECS: u64 = 300;

/// Fast inline check for drone-like SSIDs in probe requests
/// Optimized for hot path - uses byte-level comparisons where beneficial
#[inline]
fn is_drone_ssid_fast(ssid: &[u8]) -> (bool, f32, &'static str) {
    // Empty SSID = not a drone SSID
    if ssid.is_empty() {
        return (false, 0.0, "");
    }

    // Convert to uppercase using stack buffer — SSIDs are max 32 bytes, zero heap alloc
    let mut upper_buf = [0u8; 32];
    let len = ssid.len().min(32);
    for i in 0..len {
        upper_buf[i] = ssid[i].to_ascii_uppercase();
    }
    let upper_str = match std::str::from_utf8(&upper_buf[..len]) {
        Ok(s) => s,
        Err(_) => return (false, 0.0, ""),
    };

    // Toy drone manufacturers — must START with the brand name to avoid
    // false positives like "Hyundai_Syma" (car WiFi containing "SYMA")
    const TOY_PREFIXES: &[&str] = &[
        "SYMA", "JJRC", "MJX", "EACHINE", "HUBSAN", "SNAPTAIN", "CHEERWING", "UDIRC",
    ];
    for prefix in TOY_PREFIXES {
        if upper_str.starts_with(prefix) {
            return (true, 0.30, "toy_drone_ssid");
        }
    }

    // Strong drone indicators - +0.50
    if upper_str.contains("DRONE") {
        return (true, 0.50, "ssid_contains_drone");
    }
    // "UAV" requires word-boundary-like check to avoid "GUAVA" → "UAV" false positive
    if upper_str.starts_with("UAV") || upper_str.contains("-UAV") || upper_str.contains("_UAV")
        || upper_str.contains(" UAV") || upper_str.ends_with("UAV") && upper_str.len() <= 5 {
        return (true, 0.45, "ssid_contains_uav");
    }

    // Medium drone indicators - +0.35
    // "QUAD" requires word boundary — "Quadcopter" yes, "GeekSquad" no
    if upper_str.contains("QUADCOPTER") || upper_str.contains("QUAD-") || upper_str.starts_with("QUAD")
        || upper_str.contains("COPTER") {
        return (true, 0.35, "ssid_quad_copter");
    }

    // FPV indicator - +0.40 (very specific to drones/RC)
    if upper_str.contains("FPV") {
        return (true, 0.40, "ssid_contains_fpv");
    }

    // RC prefix - +0.30
    if upper_str.starts_with("RC-") || upper_str.contains("-RC-") || upper_str.contains("_RC_") {
        return (true, 0.30, "ssid_rc_prefix");
    }

    // Aerial/flight keywords - +0.25
    if upper_str.contains("AERIAL") || upper_str.contains("FLIGHT") {
        return (true, 0.30, "ssid_aerial_flight");
    }

    // RemoteID SSID patterns — "RID-<serial>" is the WiFi Beacon Remote ID format
    // per ASTM F3411 / FAA rule. This is a DEFINITIVE drone indicator, not ambiguous.
    if upper_str.starts_with("RID-") {
        return (true, 0.70, "ssid_remoteid_rid");
    }
    if upper_str.starts_with("UAS-") {
        return (true, 0.45, "ssid_remoteid_pattern");
    }

    // DJI generic — DJI drones broadcast SSIDs like "DJI-0B7EL3L9D02E21"
    if upper_str.starts_with("DJI-") || upper_str.starts_with("DJI_") || upper_str.starts_with("DJI ") {
        return (true, 0.45, "ssid_dji_prefix");
    }

    // DJI RC controller hotspot — "PROJ" prefix + 6 hex chars from MAC
    // DJI RC Pro / RC-N1 / RC controllers broadcast this for phone app connection
    if upper_str.starts_with("PROJ") && ssid.len() >= 10 && ssid.len() <= 14 {
        return (true, 0.60, "ssid_dji_controller");
    }

    // DJI Enterprise RC — "RM " prefix (e.g. "RM E70536 1210091")
    // Used by Matrice 300/350/4T controllers (RC Plus, RC Enterprise)
    if upper_str.starts_with("RM ") && ssid.len() >= 8 {
        return (true, 0.55, "ssid_dji_enterprise_rc");
    }

    // Known drone product names — must START with the name to avoid false
    // positives from car WiFi (Chevrolet SPARK, Mitsubishi EVO), hotel networks, etc.
    // Removed: SPARK, EVO, HOVER (too generic as substrings)
    const PRODUCT_PREFIX: &[&str] = &[
        "MAVIC", "PHANTOM", "TELLO", "ANAFI", "BEBOP", "AUTEL", "SKYDIO",
        "FIMI", "XAG",
    ];
    for name in PRODUCT_PREFIX {
        if upper_str.starts_with(name) {
            return (true, 0.35, "ssid_known_product");
        }
    }

    // HOVERAir is a specific drone product — requires exact prefix, not "HOVER" substring
    if upper_str.starts_with("HOVERAIR") || upper_str.starts_with("HOVER AIR") {
        return (true, 0.35, "ssid_known_product");
    }

    // DJI Mini variants — require DJI prefix to avoid BMW "MINI Cooper", Apple hotspots, etc.
    if upper_str.starts_with("DJI") && upper_str.contains("MINI") {
        return (true, 0.30, "ssid_mini_variant");
    }

    (false, 0.0, "")
}

/// Check if an SSID looks like a drone — used to gate OUI-only detections.
/// Manufacturers like Parrot make non-drone products (car kits, speakers) that
/// share OUIs with their drones. Requiring a drone-like SSID prevents FPs.
fn has_drone_ssid(ssid: &Option<String>) -> bool {
    let ssid = match ssid {
        Some(s) if !s.is_empty() => s,
        _ => return false,
    };
    // Check against the fast SSID classifier
    let (is_match, _, _) = is_drone_ssid_fast(ssid.as_bytes());
    if is_match {
        return true;
    }
    // Also accept SSIDs containing the manufacturer name (e.g. "Anafi-XXXXXX", "EVO2-XXXXX")
    let upper = ssid.to_ascii_uppercase();
    // Parrot drone SSIDs: Anafi-*, BebopDrone-*, Disco-*, SkyController-*, Mambo-*, Swing-*
    if upper.starts_with("ANAFI") || upper.starts_with("BEBOP") || upper.starts_with("DISCO-")
        || upper.starts_with("SKYCONTROLLER") || upper.starts_with("MAMBO") || upper.starts_with("SWING-")
        || upper.starts_with("PARROT") {
        return true;
    }
    // Autel drone SSIDs: Evo*, Autel-*, Dragonfish-*, LitePlus-*
    if upper.starts_with("EVO") || upper.starts_with("DRAGONFISH") || upper.starts_with("LITEPLUS")
        || upper.starts_with("AUTEL") {
        return true;
    }
    // Skydio SSIDs: Skydio-*, S2-*, X2-*, X10-*
    if upper.starts_with("SKYDIO") || upper.starts_with("S2-") || upper.starts_with("X2-")
        || upper.starts_with("X10-") {
        return true;
    }
    // Yuneec SSIDs: Typhoon*, Breeze*, Mantis*, Yuneec*
    if upper.starts_with("TYPHOON") || upper.starts_with("BREEZE") || upper.starts_with("MANTIS")
        || upper.starts_with("YUNEEC") {
        return true;
    }
    false
}

/// Enhanced behavioral heuristics to detect drone-like devices even without OUI match.
/// Uses a scoring system to accumulate evidence from multiple signals.
///
/// Returns: (is_suspect, confidence_score, primary_reason)
/// - is_suspect: true if score >= BEHAVIOR_THRESHOLD (0.45)
/// - confidence_score: accumulated score from all signals (0.0 - 1.0+)
/// - primary_reason: the strongest signal that triggered detection
fn is_drone_like_behavior(parsed: &frame::ParsedFrame, channel: i32) -> BehaviorResult {
    // ══════════════════════════════════════════════════════════════════════════
    // EARLY-RETURN EXCLUSIONS — devices that are DEFINITELY not drones.
    // These bypass scoring entirely because a false positive is worse than a miss.
    // ══════════════════════════════════════════════════════════════════════════

    // Exclusion 1: WFA NAN vendor IE (Wi-Fi Aware phones/tablets)
    // OUI 50:6F:9A type 0x13 = Wi-Fi Alliance NAN (Neighbor Awareness Networking)
    for vie in &parsed.vendor_ies {
        if vie.oui == [0x50, 0x6F, 0x9A] && vie.oui_type == 0x13 {
            return (false, 0.0, "excluded_wfa_nan");
        }
    }

    // Exclusion 2: Known non-drone SSID prefixes (cars, phones, ISPs, etc.)
    if let Some(ssid) = &parsed.ssid {
        // Stack-based uppercase — avoids heap alloc on every SSID-bearing frame
        let mut upper_buf = [0u8; 32];
        let slen = ssid.len().min(32);
        for (i, b) in ssid.as_bytes()[..slen].iter().enumerate() {
            upper_buf[i] = b.to_ascii_uppercase();
        }
        let upper = std::str::from_utf8(&upper_buf[..slen]).unwrap_or("");
        const NON_DRONE_PREFIXES: &[&str] = &[
            "KIA_", "HYUNDAI_", "UCONNECT-", "SMARTPHONE_CONNECT_",
            "CHEVROLET", "TMOBILE", "XFINITY",
            "FORD_", "BMW_", "HONDA_", "TOYOTA_", "NISSAN_",
            "COMCAST", "ATT-WIFI", "SPECTRUM",
            "BESTBUY", "GEEKSQUAD",
        ];
        for prefix in NON_DRONE_PREFIXES {
            if upper.starts_with(prefix) {
                return (false, 0.0, "excluded_known_non_drone_ssid");
            }
        }
    }

    let mut score: f32 = 0.0;
    let mut primary_reason: &'static str = "";
    let mut primary_score: f32 = 0.0;

    // Helper to update primary reason (tracks the strongest signal)
    macro_rules! add_score {
        ($delta:expr, $reason:expr) => {
            score += $delta;
            if $delta > primary_score {
                primary_score = $delta;
                primary_reason = $reason;
            }
        };
    }

    // ==========================================================================
    // SIGNAL 1: 5.8 GHz channels (149-165) - heavily used by consumer drones
    // ==========================================================================
    let is_58ghz = channel >= 149 && channel <= 165;
    if is_58ghz {
        add_score!(0.15, "5.8ghz_channel");

        // Beacon on 5.8GHz is more suspicious (routers rarely beacon here)
        if parsed.frame_type == FrameType::Beacon {
            add_score!(0.10, "beacon_on_5.8ghz");
        }
    }

    // ==========================================================================
    // SIGNAL 2: 2.4 GHz unusual channels (not 1, 6, 11)
    // ==========================================================================
    if channel >= 1 && channel <= 14 && channel != 1 && channel != 6 && channel != 11 {
        add_score!(0.05, "unusual_2.4ghz_channel");
    }

    // ==========================================================================
    // SIGNAL 3: SSID-based detection
    // ==========================================================================
    if let Some(ssid) = &parsed.ssid {
        let (is_drone_ssid, ssid_score, ssid_reason) = is_drone_ssid_fast(ssid.as_bytes());
        if is_drone_ssid {
            add_score!(ssid_score, ssid_reason);
        }
    }

    // ==========================================================================
    // SIGNAL 4: Beacon interval analysis
    // ==========================================================================
    if let Some(bi) = parsed.beacon_interval {
        // Toy drone range: 40-60 TU (aggressive for battery/latency)
        if bi >= 40 && bi <= 60 {
            add_score!(0.20, "beacon_interval_toy_drone");
        }
        // DJI-like: 102-104 TU (slightly above standard 100)
        // Score low — Cisco/enterprise APs also use 102 TU
        else if bi >= 102 && bi <= 104 {
            add_score!(0.05, "beacon_interval_dji_like");
        }
        // Very aggressive: <40 TU (unusual, could be custom/racing drone)
        // Score low — car WiFi hotspots also use short beacon intervals
        else if bi > 0 && bi < 40 {
            add_score!(0.10, "beacon_interval_aggressive");
        }
        // Power-save mode: 250-500 TU (some toy drones in idle)
        // Scored low (0.05) because enterprise APs also use 300 TU beacon intervals
        else if bi >= 250 && bi <= 500 {
            add_score!(0.05, "beacon_interval_power_save");
        }
    }

    // ==========================================================================
    // SIGNAL 5: MAC address analysis
    // ==========================================================================
    // Locally administered address (LAA) - bit 1 of first byte set
    // Common in DIY drones, spoofed MACs, or devices without proper OUI
    let is_laa = (parsed.src_mac[0] & 0x02) != 0;
    if is_laa {
        add_score!(0.10, "locally_administered_mac");
    }

    // Multicast source MAC (bit 0 of first byte set) - very unusual for legitimate devices
    let is_multicast = (parsed.src_mac[0] & 0x01) != 0;
    if is_multicast {
        add_score!(0.10, "multicast_source_mac");
    }

    // ==========================================================================
    // SIGNAL 6: Frame type analysis (Probe Request = potential controller)
    // ==========================================================================
    if parsed.frame_type == FrameType::ProbeRequest {
        // Probe request for a drone SSID = likely a controller looking for its drone
        // Only add bonus if SSID wasn't already scored in Signal 3 (avoid double-counting)
        if let Some(ssid) = &parsed.ssid {
            let (is_drone_ssid, _, _) = is_drone_ssid_fast(ssid.as_bytes());
            if is_drone_ssid && score == 0.0 {
                // Controller detection — only score if no SSID score already added
                add_score!(0.45, "probe_request_drone_ssid");
            } else if is_drone_ssid {
                // SSID already scored in Signal 3; add small probe bonus (not full double)
                add_score!(0.10, "probe_request_bonus");
            }
        }
    }

    // ==========================================================================
    // FINAL DECISION
    // ==========================================================================
    let is_suspect = score >= BEHAVIOR_THRESHOLD;

    (is_suspect, score, primary_reason)
}

#[tokio::main]
async fn main() -> Result<()> {
    // Parse args
    let config_path = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "config.toml".to_string());

    let config = Config::load(&PathBuf::from(&config_path))?;

    // Init logging
    let env_filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(&config.logging.level));

    tracing_subscriber::fmt()
        .with_env_filter(env_filter)
        .with_target(true)
        .compact()
        .init();

    info!(
        tap_id = config.tap.id,
        tap_name = config.tap.name,
        interface = config.capture.interface,
        nats_url = config.nats.url,
        "Skylens Tap starting"
    );

    // Load intel database
    let intel_path = PathBuf::from(
        std::env::args()
            .nth(2)
            .unwrap_or_else(|| "intel/drone_models.json".to_string()),
    );
    let intel = Arc::new(IntelDatabase::load(&intel_path)?);

    // Set up monitor mode (skip in passive mode — another program owns the interface)
    if config.capture.passive {
        info!(interface = config.capture.interface, "Passive mode — skipping monitor mode setup and channel hopping");
    } else {
        info!(interface = config.capture.interface, "Setting up monitor mode");
        setup_monitor_mode(&config.capture.interface).await?;
        info!("Monitor mode enabled");
    }

    // Connect to NATS with reliable buffered publishing
    let buffer_config = PublishBufferConfig {
        max_size: config.nats.buffer.max_size,
        max_retries: config.nats.buffer.max_retries,
        initial_retry_delay_ms: config.nats.buffer.initial_retry_delay_ms,
        max_retry_delay_ms: config.nats.buffer.max_retry_delay_ms,
        warning_threshold: config.nats.buffer.warning_threshold,
    };
    let nats = Arc::new(NatsPublisher::connect(&config.nats.url, &config.tap.id, buffer_config, &config.nats.mirror_urls).await?);

    // Subscribe to commands from node
    let mut cmd_rx = nats.subscribe_commands().await?;

    // Build BPF filter
    let bpf_filter = intel.build_bpf_filter();
    match &bpf_filter {
        Some(f) => info!(filter = f, "BPF filter"),
        None => info!("No BPF filter — all filtering in userspace"),
    }

    // Open pcap capture
    let mut pcap = PcapCapture::new(&config.capture.interface, bpf_filter.as_deref(), config.capture.pcap_buffer_mb)?;
    let packets_captured = pcap.packets_captured.clone();
    let bytes_captured = pcap.bytes_captured.clone();
    let pcap_received = pcap.pcap_received.clone();
    let pcap_dropped = pcap.pcap_dropped.clone();
    let pcap_stop = pcap.stop_flag();

    // Shared runtime config for hot-reload
    let runtime_config = Arc::new(RwLock::new(RuntimeConfig {
        channels: config.capture.channels.clone(),
        hop_interval_ms: config.capture.hop_interval_ms,
        bpf_filter: bpf_filter.clone(),
    }));

    // Channel for sending config updates to hopper/capture
    let (config_tx, mut config_rx) = mpsc::channel::<ConfigUpdate>(16);

    // Start channel hopper (skip in passive mode)
    let hopper = Arc::new(ChannelHopper::new(
        &config.capture.interface,
        config.capture.channels.clone(),
        config.capture.hop_interval_ms,
        config.capture.force_iw,
        config.capture.channel_width_hopping,
    ));
    let current_channel = hopper.current_channel();
    let hopper_running = hopper.stop_flag();
    let hopper_channels = hopper.channels_ref();
    let hopper_interval = hopper.interval_ref();
    let last_hop_time = hopper.last_hop_time();

    if !config.capture.passive {
        let h = hopper.clone();
        let running = hopper_running.clone();
        tokio::spawn(async move {
            loop {
                let h2 = h.clone();
                // Inner spawn catches panics — if hopper panics, JoinHandle returns Err
                let handle = tokio::spawn(async move {
                    h2.run().await
                });

                match handle.await {
                    Ok(Ok(())) => {
                        // Clean shutdown (running flag set to false)
                        info!("Channel hopper stopped cleanly");
                        break;
                    }
                    Ok(Err(e)) => {
                        error!(error = %e, "Channel hopper CRASHED, restarting in 2s");
                    }
                    Err(e) => {
                        error!(error = ?e, "Channel hopper PANICKED, restarting in 2s");
                    }
                }

                if !running.load(Ordering::SeqCst) {
                    break;
                }

                tokio::time::sleep(Duration::from_secs(2)).await;
                warn!("Restarting channel hopper...");
            }
        });
    }

    // Detection channel: pcap thread → async publisher
    let (det_tx, mut det_rx) = mpsc::channel::<proto::Detection>(65535);

    // BSSID baseline tracker
    let (baseline, baseline_metrics) = BssidBaseline::new();

    // Build MAC denylist as a HashSet of [u8; 6] for O(1) lookup
    let mac_denylist: HashSet<[u8; 6]> = config
        .capture
        .mac_denylist
        .iter()
        .filter_map(|mac_str| {
            let parts: Vec<&str> = mac_str.split(':').collect();
            if parts.len() != 6 {
                warn!(mac = %mac_str, "Invalid MAC in denylist, skipping");
                return None;
            }
            let mut bytes = [0u8; 6];
            for (i, part) in parts.iter().enumerate() {
                match u8::from_str_radix(part, 16) {
                    Ok(b) => bytes[i] = b,
                    Err(_) => {
                        warn!(mac = %mac_str, "Invalid hex in MAC denylist, skipping");
                        return None;
                    }
                }
            }
            Some(bytes)
        })
        .collect();
    if !mac_denylist.is_empty() {
        info!(count = mac_denylist.len(), "MAC denylist loaded");
    }

    // BLE MAC denylist — string format (XX:XX:XX:XX:XX:XX uppercase) for BLE scanner
    let ble_mac_denylist: Arc<HashSet<String>> = Arc::new(
        config.capture.mac_denylist.iter()
            .map(|s| s.to_uppercase())
            .collect()
    );

    // Channel diversity tracking — written by capture_loop, read by stats logger + heartbeat
    let distinct_channels = Arc::new(AtomicU32::new(0));
    // Debug counters for Action frame investigation
    let action_frame_count = Arc::new(AtomicU64::new(0));
    let parse_error_count = Arc::new(AtomicU64::new(0));

    // tshark co-process removed — MT7921U receives all frames natively (including NAN Category 4).
    // Keep config field for backward compatibility but warn if still enabled.
    if config.capture.tshark {
        warn!("capture.tshark is deprecated — MT7921U receives all frames natively. Ignoring.");
    }

    // BLE scanner: check availability and clone det_tx before pcap thread takes it
    let ble_enabled = config.ble.enabled && capture::ble::is_ble_available(&config.ble.adapter);
    let det_tx_for_ble = if ble_enabled {
        Some(det_tx.clone())
    } else {
        if config.ble.enabled {
            warn!("BLE enabled in config but adapter not found — running without BLE");
        }
        None
    };
    let ble_stats = Arc::new(BleStats::default());

    // Spawn the capture thread (blocking pcap reads in a dedicated thread)
    let cap_intel = intel.clone();
    let tap_id = config.tap.id.clone();
    let cap_current_channel = current_channel.clone();
    let dedup_interval = Duration::from_millis(config.capture.dedup_interval_ms);
    let cap_stop = pcap_stop.clone();
    let cap_is_passive = config.capture.passive;
    let cap_interface = config.capture.interface.clone();
    let cap_distinct_ch = distinct_channels.clone();
    let cap_action_count = action_frame_count.clone();
    let cap_parse_errors = parse_error_count.clone();
    let cap_tap_lat = config.tap.latitude;
    let cap_tap_lon = config.tap.longitude;
    let capture_handle = std::thread::Builder::new()
        .name("pcap-capture".to_string())
        .spawn(move || {
            capture_loop(
                &mut pcap,
                &cap_intel,
                &tap_id,
                &cap_current_channel,
                &det_tx,
                dedup_interval,
                &cap_stop,
                baseline,
                &mac_denylist,
                cap_is_passive,
                &cap_interface,
                &cap_distinct_ch,
                &cap_action_count,
                &cap_parse_errors,
                cap_tap_lat,
                cap_tap_lon,
            );
        })?;

    // Spawn BLE scanner task (if enabled and adapter available)
    if let Some(ble_tx) = det_tx_for_ble {
        let ble_stop = pcap_stop.clone();
        let ble_adapter = config.ble.adapter.clone();
        let ble_tap_id = config.tap.id.clone();
        let bs = ble_stats.clone();
        let ble_denylist = ble_mac_denylist.clone();
        tokio::spawn(async move {
            capture::ble::ble_scan_loop(ble_adapter, ble_stop, ble_tx, ble_tap_id, bs, ble_denylist).await;
        });
        info!(adapter = config.ble.adapter, "BLE scanner task spawned");
    }

    // Spawn heartbeat task with buffer metrics
    let hb_nats = nats.clone();
    let hb_config = config.clone();
    let hb_packets = packets_captured.clone();
    let hb_bytes = bytes_captured.clone();
    let hb_channel = current_channel.clone();
    let hb_pcap_received = pcap_received.clone();
    let hb_pcap_dropped = pcap_dropped.clone();
    let hb_buffer_metrics = nats.buffer_metrics().clone();
    let hb_baseline_metrics = baseline_metrics.clone();
    let hb_distinct_ch = distinct_channels.clone();
    let hb_ble_stats = ble_stats.clone();
    let hb_ble_enabled = ble_enabled;
    let hb_ble_adapter = config.ble.adapter.clone();
    let start_time = Instant::now();

    tokio::spawn(async move {
        heartbeat_loop(
            &hb_nats,
            &hb_config,
            &hb_packets,
            &hb_bytes,
            &hb_channel,
            &hb_pcap_received,
            &hb_pcap_dropped,
            &hb_buffer_metrics,
            &hb_baseline_metrics,
            &hb_distinct_ch,
            &hb_ble_stats,
            hb_ble_enabled,
            &hb_ble_adapter,
            start_time,
        )
        .await;
    });

    // Spawn stats logger with buffer metrics + pcap watchdog
    let stats_is_passive = config.capture.passive;
    let stats_packets = packets_captured.clone();
    let stats_nats_msgs = nats.messages_sent.clone();
    let stats_nats_errs = nats.errors.clone();
    let stats_pcap_received = pcap_received.clone();
    let stats_pcap_dropped = pcap_dropped.clone();
    let stats_buffer_metrics = nats.buffer_metrics().clone();
    let stats_baseline_metrics = baseline_metrics.clone();
    let stats_distinct_ch = distinct_channels.clone();
    let stats_action_frames = action_frame_count.clone();
    let stats_parse_errors = parse_error_count.clone();
    let stats_nl80211_hops = hopper.nl80211_hops.clone();
    let stats_iw_hops = hopper.iw_fallback_hops.clone();
    let stats_width_hops = hopper.width_hops.clone();
    let stats_last_hop = last_hop_time.clone();
    let stats_ble = ble_stats.clone();
    let stats_hb_sent = nats.heartbeats_sent.clone();

    tokio::spawn(async move {
        let mut interval = tokio::time::interval(tokio::time::Duration::from_secs(10));
        let mut last_packets = 0u64;
        let mut last_detections = 0u64;

        // Pcap watchdog: detect stuck pcap from outside the capture thread.
        // If pcap.next_packet() blocks forever (AF_PACKET socket corruption),
        // the in-loop self-healing never runs. This watchdog exits the process
        // so systemd can restart cleanly.
        let mut watchdog_last_packets = 0u64;
        let mut watchdog_stale_ticks = 0u32;
        const WATCHDOG_STALE_LIMIT: u32 = 6; // 6 ticks × 10s = 60s of zero packets → exit

        loop {
            interval.tick().await;
            let pkts = stats_packets.load(Ordering::Relaxed);
            let dets = DETECTIONS_SENT.load(Ordering::Relaxed);
            let msgs = stats_nats_msgs.load(Ordering::Relaxed);
            let errs = stats_nats_errs.load(Ordering::Relaxed);
            let kern_recv = stats_pcap_received.load(Ordering::Relaxed);
            let kern_drop = stats_pcap_dropped.load(Ordering::Relaxed);

            // Buffer metrics
            let buffer_size = stats_buffer_metrics.buffer_size.load(Ordering::Relaxed);
            let buffer_dropped = stats_buffer_metrics.buffer_dropped.load(Ordering::Relaxed);
            let publish_retries = stats_buffer_metrics.publish_retries.load(Ordering::Relaxed);
            let nats_disconnects = stats_buffer_metrics.nats_disconnects.load(Ordering::Relaxed);

            // Baseline metrics
            let total_bssids = stats_baseline_metrics.total_bssids.load(Ordering::Relaxed);

            // Hopper metrics
            let nl_hops = stats_nl80211_hops.load(Ordering::Relaxed);
            let iw_hops = stats_iw_hops.load(Ordering::Relaxed);
            let hop_ms = stats_last_hop.load(Ordering::Relaxed);
            let hop_age_s = if hop_ms > 0 {
                let now_ms = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_millis() as u64;
                (now_ms.saturating_sub(hop_ms)) / 1000
            } else {
                0
            };

            let pps = (pkts - last_packets) / 10;
            let dps = (dets - last_detections) / 10;

            let distinct_ch = stats_distinct_ch.load(Ordering::Relaxed);

            info!(
                packets = pkts,
                pps,
                detections = dets,
                dps,
                nats_sent = msgs,
                nats_errors = errs,
                kern_recv,
                kern_drop,
                buffer_size,
                buffer_dropped,
                publish_retries,
                nats_disconnects,
                total_bssids,
                nl_hops,
                iw_hops,
                width_hops = stats_width_hops.load(Ordering::Relaxed),
                hop_age_s,
                distinct_ch,
                action_frames = stats_action_frames.load(Ordering::Relaxed),
                parse_errors = stats_parse_errors.load(Ordering::Relaxed),
                ble_devs = stats_ble.devices_seen.load(Ordering::Relaxed),
                ble_adverts = stats_ble.advertisements.load(Ordering::Relaxed),
                ble_dets = stats_ble.detections.load(Ordering::Relaxed),
                ble_svc_events = stats_ble.svc_data_events.load(Ordering::Relaxed),
                ble_mfr_events = stats_ble.mfr_data_events.load(Ordering::Relaxed),
                ble_ch_drops = stats_ble.channel_drops.load(Ordering::Relaxed),
                wifi_ch_drops = WIFI_CHANNEL_DROPS.load(Ordering::Relaxed),
                hb_sent = stats_hb_sent.load(Ordering::Relaxed),
                "Stats"
            );

            // Pcap watchdog: check if packet counter is advancing
            if !stats_is_passive {
                if pkts == watchdog_last_packets && pkts > 0 {
                    // Packets haven't changed since last tick
                    watchdog_stale_ticks += 1;
                    if watchdog_stale_ticks >= WATCHDOG_STALE_LIMIT {
                        error!(
                            packets = pkts,
                            stale_seconds = watchdog_stale_ticks * 10,
                            "WATCHDOG: pcap stuck — no new packets for {}s, exiting for systemd restart",
                            watchdog_stale_ticks * 10
                        );
                        std::process::exit(1);
                    } else if watchdog_stale_ticks >= 3 {
                        warn!(
                            packets = pkts,
                            stale_seconds = watchdog_stale_ticks * 10,
                            "WATCHDOG: pcap may be stuck — no new packets for {}s",
                            watchdog_stale_ticks * 10
                        );
                    }
                } else {
                    watchdog_stale_ticks = 0;
                }
                watchdog_last_packets = pkts;
            }

            last_packets = pkts;
            last_detections = dets;
        }
    });

    // Graceful shutdown signal — watch channel for responsive select! in main loop
    let shutdown = Arc::new(AtomicBool::new(false));
    let shutdown_flag = shutdown.clone();
    let (shutdown_tx, shutdown_rx) = watch::channel(false);

    // Spawn command handler with hot-reload support
    let cmd_nats = nats.clone();
    let cmd_tap_id = config.tap.id.clone();
    let cmd_shutdown = shutdown.clone();
    let cmd_runtime_config = runtime_config.clone();
    let cmd_hopper_channels = hopper_channels.clone();
    let cmd_hopper_interval = hopper_interval.clone();
    // Clone shutdown_tx so handle_command can wake the main loop on graceful restart
    let cmd_shutdown_tx = shutdown_tx.clone();
    tokio::spawn(async move {
        while let Some(cmd) = cmd_rx.recv().await {
            handle_command(
                &cmd_nats,
                &cmd_tap_id,
                cmd,
                &cmd_shutdown,
                &cmd_shutdown_tx,
                &cmd_runtime_config,
                &cmd_hopper_channels,
                &cmd_hopper_interval,
            )
            .await;
        }
    });

    // Notify systemd we are ready (keep NOTIFY_SOCKET for watchdog pings)
    let _ = sd_notify::notify(false, &[sd_notify::NotifyState::Ready]);

    // Main async loop: publish detections and handle shutdown
    info!("Capture running, publishing detections to NATS");

    tokio::spawn(async move {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut sigterm) => {
                let sigint = tokio::signal::ctrl_c();
                tokio::select! {
                    _ = sigterm.recv() => info!("Received SIGTERM, shutting down"),
                    _ = sigint => info!("Received SIGINT, shutting down"),
                }
            }
            Err(e) => {
                warn!(error = %e, "Failed to install SIGTERM handler, using SIGINT only");
                let _ = tokio::signal::ctrl_c().await;
                info!("Received SIGINT, shutting down");
            }
        }
        shutdown_flag.store(true, Ordering::SeqCst);
        let _ = shutdown_tx.send(true); // Wake up main loop immediately
    });

    // Dedicated watchdog pinger — runs independently so NATS blockage can never
    // prevent the systemd watchdog from being fed. This fixes the root cause of
    // TAP-003 watchdog timeouts when DERP relay drops cause NATS IO errors.
    let wd_shutdown = shutdown.clone();
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(tokio::time::Duration::from_secs(10));
        loop {
            interval.tick().await;
            let _ = sd_notify::notify(false, &[sd_notify::NotifyState::Watchdog]);
            if wd_shutdown.load(Ordering::SeqCst) {
                break;
            }
        }
    });

    // Detection publishing loop
    let is_passive = config.capture.passive;
    let wd_last_hop = last_hop_time.clone();
    let mut health_interval = tokio::time::interval(tokio::time::Duration::from_secs(10));

    // Detection merge window: collect detections from pcap + BLE for the same
    // MAC within 50ms, merge all fields into one rich detection, then publish once.
    // NAN ODID bursts (BasicID → Location → System) arrive within ~10ms of each
    // other, so 50ms is plenty. Lower window = lower latency per detection.
    let mut pending_detections: HashMap<String, (Instant, proto::Detection)> = HashMap::new();
    let merge_window = Duration::from_millis(50);
    let mut flush_interval = tokio::time::interval(tokio::time::Duration::from_millis(25));

    // Drone state cache: accumulates serial/GPS/operator across multiple beacon frames
    // from the same MAC. RemoteID drones cycle through message types (Basic ID, Location,
    // System) in different beacons — this cache merges them so every published detection
    // includes the best-known data even if the current beacon only has partial info.
    let mut drone_state_cache: HashMap<String, (Instant, proto::Detection)> = HashMap::new();
    let cache_ttl = Duration::from_secs(120); // Expire cached state after 2 minutes of silence
    let mut last_cache_cleanup = Instant::now();
    let mut shutdown_watch = shutdown_rx;

    loop {
        tokio::select! {
            // Immediate shutdown response — no polling delay
            _ = shutdown_watch.changed() => {
                // Flush all detections still in the merge window before shutting down.
                // Without this, detections waiting for the merge window expire
                // and are permanently lost on shutdown.
                let pending_count = pending_detections.len();
                for (_, (_, mut det)) in pending_detections.drain() {
                    // Sanitize first, then merge (same order as normal flush)
                    sanitize_operator_location(&mut det, config.tap.latitude, config.tap.longitude);
                    if let Some((_, cached)) = drone_state_cache.get(&det.mac_address) {
                        merge_detection(&mut det, cached);
                    }
                    if let Err(e) = nats.publish_detection(&det).await {
                        error!(error = %e, "Failed to flush merge-window detection at shutdown");
                    } else {
                        DETECTIONS_SENT.fetch_add(1, Ordering::Relaxed);
                    }
                }
                if pending_count > 0 {
                    info!(count = pending_count, "Flushed merge-window detections at shutdown");
                }
                info!("Shutdown signal received via watch channel, stopping capture");
                break;
            }
            detection = det_rx.recv() => {
                match detection {
                    Some(det) => {
                        let now = Instant::now();

                        // Immediately merge into drone_state_cache so enrichment data
                        // (serial, operator GPS) accumulates even between publish cycles.
                        // This ensures dedup-bypassed frames contribute their data.
                        // Only store validated coordinates — bogus coords (e.g. DJI default
                        // 30.21/82.57) must not pollute the cache.
                        if !det.is_controller {
                            if let Some((ts, cached)) = drone_state_cache.get_mut(&det.mac_address) {
                                merge_detection(cached, &det);
                                if det.latitude != 0.0 && is_valid_location(det.latitude, det.longitude, config.tap.latitude, config.tap.longitude) {
                                    cached.latitude = det.latitude;
                                    cached.longitude = det.longitude;
                                }
                                if det.operator_latitude != 0.0 && is_valid_location(det.operator_latitude, det.operator_longitude, config.tap.latitude, config.tap.longitude) {
                                    cached.operator_latitude = det.operator_latitude;
                                    cached.operator_longitude = det.operator_longitude;
                                }
                                *ts = now;
                            }
                            // Don't insert new cache entries here — wait for publish
                        }

                        // Fast path: if detection already has a serial number AND
                        // confidence >= 0.80, publish immediately — no merge window needed.
                        // DJI beacons and full NAN cycles arrive with complete data in one frame.
                        // Skipping the 50ms merge window saves ~25-75ms per detection.
                        let has_serial = !det.serial_number.is_empty();
                        let high_confidence = det.confidence >= 0.80;
                        if has_serial && high_confidence && !pending_detections.contains_key(&det.mac_address) {
                            let mac = det.mac_address.clone();
                            let mut fast_det = det;
                            sanitize_operator_location(&mut fast_det, config.tap.latitude, config.tap.longitude);
                            if let Some((_, cached)) = drone_state_cache.get(&mac) {
                                merge_detection(&mut fast_det, cached);
                            }
                            // Update cache
                            if !fast_det.is_controller {
                                if let Some((ts, cached)) = drone_state_cache.get_mut(&mac) {
                                    if fast_det.latitude != 0.0 {
                                        cached.latitude = fast_det.latitude;
                                        cached.longitude = fast_det.longitude;
                                    }
                                    if fast_det.operator_latitude != 0.0 {
                                        cached.operator_latitude = fast_det.operator_latitude;
                                        cached.operator_longitude = fast_det.operator_longitude;
                                    }
                                    merge_detection(cached, &fast_det);
                                    *ts = now;
                                } else {
                                    let mut cache_det = fast_det.clone();
                                    cache_det.raw_frame.clear();
                                    cache_det.remoteid_payload.clear();
                                    drone_state_cache.insert(mac.clone(), (now, cache_det));
                                }
                            }
                            let source_name = match fast_det.source {
                                1 => "BEACON", 2 => "NAN", 3 => "PROBE_RESP",
                                6 => "DJI_OCUSYNC", _ => "UNKNOWN",
                            };
                            info!(
                                mac = %fast_det.mac_address,
                                id = %fast_det.serial_number,
                                conf = fast_det.confidence,
                                source = source_name,
                                "FAST-PUBLISH detection (skip merge window)"
                            );
                            if let Err(e) = nats.publish_detection(&fast_det).await {
                                error!(error = %e, "Failed to publish fast-path detection");
                            } else {
                                DETECTIONS_SENT.fetch_add(1, Ordering::Relaxed);
                            }
                        } else if let Some((_first_seen, existing)) = pending_detections.get_mut(&det.mac_address) {
                            // Merge new detection into existing — fill in missing fields
                            merge_detection(existing, &det);
                        } else {
                            pending_detections.insert(det.mac_address.clone(), (now, det));
                        }
                    }
                    None => {
                        error!("Capture thread died (channel closed). Triggering shutdown.");
                        break;
                    }
                }
            }
            // Flush merged detections whose merge window has expired
            _ = flush_interval.tick() => {
                let now = Instant::now();
                let ready: Vec<String> = pending_detections
                    .iter()
                    .filter(|(_, (first_seen, _))| now.duration_since(*first_seen) >= merge_window)
                    .map(|(mac, _)| mac.clone())
                    .collect();

                for mac in ready {
                    if let Some((_, mut det)) = pending_detections.remove(&mac) {
                        // Sanitize bogus coords FIRST — before merge and cache update.
                        // ODID frames can have partial bogus data (e.g. lat=0, lon=117.8).
                        // If we merge first, cache fills the empty lat with real data,
                        // creating a chimera (real lat + bogus lon) that pollutes the cache.
                        // By sanitizing first, bogus coords are zeroed, then merge fills
                        // BOTH lat and lon from cache with clean data.
                        sanitize_operator_location(&mut det, config.tap.latitude, config.tap.longitude);

                        // Merge from drone state cache — fill in missing serial/GPS/operator
                        // from previous beacon frames of this same MAC
                        if let Some((_, cached)) = drone_state_cache.get(&mac) {
                            merge_detection(&mut det, cached);
                        }

                        // Update drone state cache with sanitized+merged detection data
                        // (only for non-controller detections to avoid polluting drone cache)
                        if !det.is_controller {
                            if let Some((ts, cached)) = drone_state_cache.get_mut(&mac) {
                                // Update position fields only if new data is better (non-zero)
                                // For location, always prefer the NEWEST non-zero value
                                if det.latitude != 0.0 {
                                    cached.latitude = det.latitude;
                                    cached.longitude = det.longitude;
                                }
                                if det.operator_latitude != 0.0 {
                                    cached.operator_latitude = det.operator_latitude;
                                    cached.operator_longitude = det.operator_longitude;
                                }
                                // Merge everything else (serial, manufacturer, etc.)
                                merge_detection(cached, &det);
                                *ts = now;
                            } else {
                                // Strip heavy fields before caching — cache is only for
                                // merge enrichment (serial, GPS, operator), not raw data
                                let mut cache_det = det.clone();
                                cache_det.raw_frame.clear();
                                cache_det.remoteid_payload.clear();
                                drone_state_cache.insert(mac.clone(), (now, cache_det));
                            }
                        }

                        // Periodic cache cleanup — time-based (every 30s)
                        if last_cache_cleanup.elapsed() >= Duration::from_secs(30) {
                            drone_state_cache.retain(|_, (ts, _)| now.duration_since(*ts) < cache_ttl);
                            last_cache_cleanup = now;
                        }
                        // Log the FINAL detection after sanitize+merge — reflects what NATS gets
                        let source_name = match det.source {
                            1 => "BEACON", 2 => "NAN", 3 => "PROBE_RESP",
                            6 => "DJI_OCUSYNC", 8 => "TSHARK_RID", 9 => "TSHARK_SSID",
                            _ => "UNKNOWN",
                        };
                        info!(
                            mac = %det.mac_address,
                            id = %det.identifier,
                            manufacturer = %det.manufacturer,
                            model = %det.designation,
                            rssi = det.rssi,
                            channel = det.channel,
                            freq_mhz = det.frequency_mhz,
                            source = %source_name,
                            confidence = det.confidence,
                            is_controller = det.is_controller,
                            ssid = %det.ssid,
                            op_lat = format!("{:.6}", det.operator_latitude),
                            op_lon = format!("{:.6}", det.operator_longitude),
                            drone_lat = format!("{:.6}", det.latitude),
                            drone_lon = format!("{:.6}", det.longitude),
                            "DETECTION"
                        );

                        match nats.publish_detection(&det).await {
                            Ok(()) => {
                                DETECTIONS_SENT.fetch_add(1, Ordering::Relaxed);
                            }
                            Err(e) => {
                                // This only fires if the buffer worker task has died
                                // (channel closed). Log as critical — detections are being lost.
                                error!(error = %e, "CRITICAL: Failed to publish detection — buffer worker dead?");
                            }
                        }
                    }
                }
            }
            _ = health_interval.tick() => {
                // Check channel hopper health (skip in passive mode)
                if !is_passive {
                    let hop_ms = wd_last_hop.load(Ordering::Relaxed);
                    if hop_ms > 0 {
                        let now_ms = std::time::SystemTime::now()
                            .duration_since(std::time::UNIX_EPOCH)
                            .unwrap_or_default()
                            .as_millis() as u64;
                        let stale_s = (now_ms.saturating_sub(hop_ms)) / 1000;
                        if stale_s > 60 {
                            error!(
                                stale_secs = stale_s,
                                "CRITICAL: Channel hopper STALLED — no hop in {}s! Restart loop should recover.",
                                stale_s
                            );
                        }
                    }
                }

                // Shutdown is now handled by the watch channel select! branch above
            }
        }
    }

    // Cleanup: stop capture thread and channel hopper
    pcap_stop.store(false, Ordering::SeqCst);
    hopper_running.store(false, Ordering::SeqCst);

    // Give the capture thread up to 2 seconds to finish (pcap timeout is 50ms)
    let join_result = tokio::task::spawn_blocking(move || {
        capture_handle.join()
    });
    match tokio::time::timeout(Duration::from_secs(2), join_result).await {
        Ok(Ok(Ok(()))) => {}
        Ok(Ok(Err(_))) => warn!("Capture thread panicked during shutdown"),
        Ok(Err(e)) => warn!(error = %e, "Capture thread join task failed"),
        Err(_) => warn!("Capture thread did not stop within 2s, proceeding with shutdown"),
    }

    // Gracefully shutdown NATS buffer worker with timeout
    info!(
        buffer_size = nats.buffer_size(),
        "Flushing detection buffer before shutdown"
    );
    match tokio::time::timeout(Duration::from_secs(3), nats.shutdown()).await {
        Ok(Ok(())) => {}
        Ok(Err(e)) => warn!(error = %e, "Failed to shutdown NATS buffer gracefully"),
        Err(_) => warn!("NATS buffer shutdown timed out after 3s"),
    }

    // Final flush with timeout
    match tokio::time::timeout(Duration::from_secs(2), nats.flush()).await {
        Ok(Ok(())) => {}
        Ok(Err(e)) => warn!(error = %e, "Failed to flush NATS connection"),
        Err(_) => warn!("NATS flush timed out after 2s"),
    }

    info!("Skylens Tap shut down");
    Ok(())
}

/// Merge a new detection into an existing one, filling in missing fields.
/// Takes the best data from each source: SSID from beacon, GPS from OpenDroneID,
/// home point from DJI vendor IE, etc. Higher confidence wins for the overall score.
fn merge_detection(existing: &mut proto::Detection, new: &proto::Detection) {
    // Always upgrade confidence and source to the better detection.
    // When upgrading, also adopt the higher-confidence detection's identity
    // (identifier, manufacturer, model). This ensures a low-confidence RemoteID
    // detection (identifier=MAC, manufacturer="RemoteID") gets upgraded to the
    // DJI DroneID's serial and model when cache-enriched.
    if new.confidence > existing.confidence {
        existing.confidence = new.confidence;
        existing.source = new.source;
        if !new.identifier.is_empty() {
            existing.identifier = new.identifier.clone();
        }
        if !new.manufacturer.is_empty() {
            existing.manufacturer = new.manufacturer.clone();
        }
        if !new.designation.is_empty() {
            existing.designation = new.designation.clone();
        }
    }

    // Take earliest timestamp
    if new.timestamp_ns < existing.timestamp_ns && new.timestamp_ns > 0 {
        existing.timestamp_ns = new.timestamp_ns;
    }

    // Fill string fields if empty
    macro_rules! merge_str {
        ($field:ident) => {
            if existing.$field.is_empty() && !new.$field.is_empty() {
                existing.$field = new.$field.clone();
            }
        };
    }
    merge_str!(identifier);
    merge_str!(serial_number);
    merge_str!(registration);
    merge_str!(session_id);
    merge_str!(utm_id);
    merge_str!(ssid);
    merge_str!(uav_type);
    merge_str!(designation);
    merge_str!(manufacturer);
    merge_str!(operator_id);

    // Fill float fields if zero
    macro_rules! merge_f {
        ($field:ident) => {
            if existing.$field == 0.0 && new.$field != 0.0 {
                existing.$field = new.$field;
            }
        };
    }
    merge_f!(latitude);
    merge_f!(longitude);
    merge_f!(altitude_geodetic);
    merge_f!(altitude_pressure);
    merge_f!(height_agl);
    merge_f!(speed);
    merge_f!(vertical_speed);
    merge_f!(heading);
    merge_f!(track_direction);
    merge_f!(operator_latitude);
    merge_f!(operator_longitude);
    merge_f!(operator_altitude);

    // Take stronger RSSI (closer to 0 = stronger signal)
    // Handle unset (0) as missing — any real RSSI reading is better than 0
    if existing.rssi == 0 && new.rssi != 0 {
        existing.rssi = new.rssi;
    } else if new.rssi < 0 && (existing.rssi == 0 || new.rssi > existing.rssi) {
        existing.rssi = new.rssi;
    }

    // Fill enum fields if unknown/undeclared (value 0 or 1)
    if existing.operational_status <= 1 && new.operational_status > 1 {
        existing.operational_status = new.operational_status;
    }
    if existing.uav_category <= 1 && new.uav_category > 1 {
        existing.uav_category = new.uav_category;
    }
    if existing.operator_location_type == 0 && new.operator_location_type != 0 {
        existing.operator_location_type = new.operator_location_type;
    }
    if existing.height_reference == 0 && new.height_reference != 0 {
        existing.height_reference = new.height_reference;
    }
}

/// Validate and sanitize operator location before publishing.
/// Drones can broadcast bogus operator coordinates (e.g. default DC coords 38.87/-77.05).
/// Reject operator location if it's too far from the TAP or altitude is absurd.
fn sanitize_operator_location(det: &mut proto::Detection, tap_lat: f64, tap_lon: f64) {
    const MAX_OPERATOR_DISTANCE_KM: f64 = 100.0;
    const MAX_OPERATOR_ALT_M: f32 = 10_000.0;
    const MIN_OPERATOR_ALT_M: f32 = -500.0;

    // Validate operator lat/lon
    if det.operator_latitude != 0.0 || det.operator_longitude != 0.0 {
        let dist_km = haversine_km(tap_lat, tap_lon, det.operator_latitude, det.operator_longitude);
        if dist_km > MAX_OPERATOR_DISTANCE_KM {
            warn!(
                mac = %det.mac_address,
                op_lat = det.operator_latitude,
                op_lon = det.operator_longitude,
                distance_km = format!("{:.1}", dist_km),
                "Operator location rejected: {:.1}km from TAP (max {}km)",
                dist_km, MAX_OPERATOR_DISTANCE_KM
            );
            det.operator_latitude = 0.0;
            det.operator_longitude = 0.0;
            det.operator_location_type = proto::OperatorLocationType::OpLocUnknown as i32;
        }
    }

    // Validate operator altitude
    if det.operator_altitude != 0.0
        && (det.operator_altitude > MAX_OPERATOR_ALT_M || det.operator_altitude < MIN_OPERATOR_ALT_M)
    {
        warn!(
            mac = %det.mac_address,
            op_alt = det.operator_altitude,
            "Operator altitude rejected: {:.1}m outside range [{}, {}]",
            det.operator_altitude, MIN_OPERATOR_ALT_M, MAX_OPERATOR_ALT_M
        );
        det.operator_altitude = 0.0;
    }

    // Validate drone location too — same sanity check
    if det.latitude != 0.0 || det.longitude != 0.0 {
        let dist_km = haversine_km(tap_lat, tap_lon, det.latitude, det.longitude);
        if dist_km > MAX_OPERATOR_DISTANCE_KM {
            warn!(
                mac = %det.mac_address,
                lat = det.latitude,
                lon = det.longitude,
                distance_km = format!("{:.1}", dist_km),
                "Drone location rejected: {:.1}km from TAP (max {}km)",
                dist_km, MAX_OPERATOR_DISTANCE_KM
            );
            det.latitude = 0.0;
            det.longitude = 0.0;
        }
    }

    // Validate drone altitude
    if det.altitude_geodetic != 0.0
        && (det.altitude_geodetic > MAX_OPERATOR_ALT_M || det.altitude_geodetic < MIN_OPERATOR_ALT_M)
    {
        det.altitude_geodetic = 0.0;
    }
    if det.altitude_pressure != 0.0
        && (det.altitude_pressure > MAX_OPERATOR_ALT_M || det.altitude_pressure < MIN_OPERATOR_ALT_M)
    {
        det.altitude_pressure = 0.0;
    }
}

/// Quick check if a location is within plausible range of the TAP.
/// Used as a guard before storing coordinates in the drone state cache
/// to prevent bogus coords from polluting the cache.
fn is_valid_location(lat: f64, lon: f64, tap_lat: f64, tap_lon: f64) -> bool {
    const MAX_DISTANCE_KM: f64 = 100.0;
    haversine_km(tap_lat, tap_lon, lat, lon) <= MAX_DISTANCE_KM
}

/// Haversine distance between two lat/lon points in kilometers
fn haversine_km(lat1: f64, lon1: f64, lat2: f64, lon2: f64) -> f64 {
    let r = 6371.0; // Earth radius in km
    let dlat = (lat2 - lat1).to_radians();
    let dlon = (lon2 - lon1).to_radians();
    let a = (dlat / 2.0).sin().powi(2)
        + lat1.to_radians().cos() * lat2.to_radians().cos() * (dlon / 2.0).sin().powi(2);
    let c = 2.0 * a.sqrt().asin();
    r * c
}

/// Handle an incoming command from the node
/// Supports hot-reload for channels and BPF filter without requiring restart
async fn handle_command(
    nats: &NatsPublisher,
    tap_id: &str,
    cmd: proto::TapCommand,
    shutdown: &Arc<AtomicBool>,
    shutdown_tx: &watch::Sender<bool>,
    runtime_config: &Arc<RwLock<RuntimeConfig>>,
    hopper_channels: &Arc<RwLock<Vec<i32>>>,
    hopper_interval: &Arc<RwLock<Duration>>,
) {
    let command_id = cmd.command_id.clone();
    info!(command_id, "Received command");

    let (success, error_message, latency_ns) = match cmd.command {
        Some(proto::tap_command::Command::Ping(ping)) => {
            let now = chrono::Utc::now().timestamp_nanos_opt().unwrap_or(0);
            let latency = now - ping.sent_at_ns;
            info!(latency_ms = latency / 1_000_000, "Ping received");
            (true, String::new(), latency)
        }
        Some(proto::tap_command::Command::Restart(restart)) => {
            if restart.graceful {
                info!("Graceful restart command — draining in-flight detections");
                let ack = proto::TapCommandAck {
                    tap_id: tap_id.to_string(),
                    command_id: command_id.clone(),
                    success: true,
                    error_message: String::new(),
                    latency_ns: 0,
                };
                let _ = nats.publish_ack(&ack).await;
                let _ = nats.flush().await;
                // Signal graceful shutdown — main loop will drain and exit
                shutdown.store(true, Ordering::SeqCst);
                let _ = shutdown_tx.send(true); // Wake main loop via watch channel
                return;
            } else {
                info!("Restart command received — exiting for systemd restart");
                let ack = proto::TapCommandAck {
                    tap_id: tap_id.to_string(),
                    command_id: command_id.clone(),
                    success: true,
                    error_message: String::new(),
                    latency_ns: 0,
                };
                let _ = nats.publish_ack(&ack).await;
                let _ = nats.flush().await;
                std::process::exit(0);
            }
        }
        Some(proto::tap_command::Command::SetChannels(set_ch)) => {
            // HOT-RELOAD: Update channel hopper without restart
            let channels: Vec<i32> = set_ch.channels.clone();
            let hop_ms = if set_ch.hop_interval_ms > 0 {
                set_ch.hop_interval_ms as u64
            } else {
                200 // Default 200ms if not specified
            };

            info!(
                channels = ?channels,
                hop_ms,
                "SetChannels command — applying hot-reload"
            );

            // Validate channels
            if channels.is_empty() {
                (false, "Channel list cannot be empty".to_string(), 0)
            } else {
                // Update runtime config
                if let Ok(mut cfg) = runtime_config.write() {
                    cfg.channels = channels.clone();
                    cfg.hop_interval_ms = hop_ms;
                }

                // Update channel hopper atomically
                if let Ok(mut ch) = hopper_channels.write() {
                    *ch = channels.clone();
                }
                if let Ok(mut interval) = hopper_interval.write() {
                    *interval = Duration::from_millis(hop_ms);
                }

                info!(
                    channels = ?channels,
                    hop_ms,
                    "Channel hopper updated successfully"
                );
                (true, String::new(), 0)
            }
        }
        Some(proto::tap_command::Command::SetFilter(set_filter)) => {
            // HOT-RELOAD: Update BPF filter
            // NOTE: BPF filter changes take effect on NEXT capture session.
            // For truly live BPF updates, we would need to close/reopen pcap handle,
            // which briefly interrupts capture. For now, we update the runtime config
            // and log a warning that a brief interruption may occur.
            let filter = if set_filter.bpf_filter.is_empty() {
                None
            } else {
                Some(set_filter.bpf_filter.clone())
            };

            info!(
                filter = ?filter,
                "SetFilter command — updating BPF filter (hot-reload)"
            );

            // Validate BPF filter syntax by attempting a compile
            // This uses pcap's filter validation without applying it
            match validate_bpf_filter(filter.as_deref()) {
                Ok(()) => {
                    if let Ok(mut cfg) = runtime_config.write() {
                        cfg.bpf_filter = filter.clone();
                    }
                    info!("BPF filter updated successfully");
                    // Note: The actual pcap filter will be applied on next packet
                    // For immediate effect, a brief capture restart would be needed
                    (true, String::new(), 0)
                }
                Err(e) => {
                    error!(error = %e, "Invalid BPF filter syntax");
                    (false, format!("Invalid BPF filter: {}", e), 0)
                }
            }
        }
        Some(proto::tap_command::Command::UpdateConfig(_update)) => {
            info!("UpdateConfig command");
            (true, "Config update requires restart".to_string(), 0)
        }
        None => {
            warn!("Received empty command");
            (false, "Empty command".to_string(), 0)
        }
    };

    let ack = proto::TapCommandAck {
        tap_id: tap_id.to_string(),
        command_id,
        success,
        error_message,
        latency_ns,
    };

    if let Err(e) = nats.publish_ack(&ack).await {
        error!(error = %e, "Failed to publish command ack");
    }
}

/// Validate BPF filter syntax without applying it
fn validate_bpf_filter(filter: Option<&str>) -> Result<(), String> {
    let filter = match filter {
        Some(f) if !f.is_empty() => f,
        _ => return Ok(()), // Empty/None filter is always valid
    };

    // Use pcap's filter compilation to validate syntax
    // We create a dummy capture just for validation
    match pcap::Capture::dead(pcap::Linktype::IEEE802_11_RADIOTAP) {
        Ok(mut cap) => match cap.filter(filter, true) {
            Ok(_) => Ok(()),
            Err(e) => Err(format!("{}", e)),
        },
        Err(e) => Err(format!("Failed to create validation capture: {}", e)),
    }
}

/// Main capture loop — runs in a dedicated thread (blocking pcap reads)
/// Tracks what data has been seen for a MAC within the dedup window.
/// Allows new data types (serial, drone GPS, operator GPS) to bypass dedup
/// so NAN message bursts contribute all their data to the detection.
struct PcapDedupState {
    last_sent: Instant,
    session_id: String,
    has_serial: bool,
    has_drone_gps: bool,
    has_operator_gps: bool,
    is_controller: bool,
}

fn capture_loop(
    pcap: &mut PcapCapture,
    intel: &IntelDatabase,
    tap_id: &str,
    current_channel: &Arc<AtomicI32>,
    det_tx: &mpsc::Sender<proto::Detection>,
    dedup_interval: Duration,
    stop_flag: &Arc<AtomicBool>,
    mut baseline: BssidBaseline,
    mac_denylist: &HashSet<[u8; 6]>,
    is_passive: bool,
    interface: &str,
    distinct_channels: &Arc<AtomicU32>,
    action_frame_counter: &Arc<AtomicU64>,
    parse_error_counter: &Arc<AtomicU64>,
    tap_lat: f64,
    tap_lon: f64,
) {
    // Deduplication + session tracking: MAC → PcapDedupState
    // Allows frames with NEW data types to bypass dedup within the window
    let mut last_seen: HashMap<String, PcapDedupState> = HashMap::new();
    let mut cleanup_counter: u64 = 0;
    let session_timeout = Duration::from_secs(60);
    let mut bad_fcs_count: u64 = 0;
    let mut panic_count: u64 = 0;
    let mut consecutive_errors: u64 = 0;
    let mut first_error_time: Option<Instant> = None;
    const MAX_CONSECUTIVE_ERRORS: u64 = 100;
    // Require errors sustained over a real time window, not just count.
    // pcap_next_ex returns immediately on error (no 100ms timeout wait),
    // so 100 errors could fire in <100ms without a time gate.
    const MIN_ERROR_WINDOW: Duration = Duration::from_secs(3);

    // Channel diversity tracking — detect stuck adapter
    let mut observed_freqs: HashSet<u16> = HashSet::new();
    let mut freq_check_time = Instant::now();
    let mut freq_check_packets: u32 = 0;
    let mut stuck_count: u32 = 0;

    // Pcap self-healing: detect stuck AF_PACKET socket (common on MT7921U
    // when channel hopping causes kernel to disconnect the socket).
    // If zero packets for 30s, reopen the pcap handle. If reopen fails
    // or doesn't help after 2 attempts, break for systemd restart.
    let mut last_packet_time = Instant::now();
    let mut reopen_attempts: u32 = 0;
    const PCAP_STUCK_TIMEOUT: Duration = Duration::from_secs(30);
    const MAX_REOPEN_ATTEMPTS: u32 = 2;

    loop {
        let packet_data = match pcap.next_packet() {
            PacketResult::Packet(data) => {
                consecutive_errors = 0; // Reset on any successful packet
                first_error_time = None;
                last_packet_time = Instant::now();
                reopen_attempts = 0; // Reset reopen counter on successful packet
                data
            }
            PacketResult::Timeout => {
                // Normal timeout, no packet available
                if !stop_flag.load(Ordering::Relaxed) {
                    break;
                }

                // Check for stuck pcap handle: zero packets for PCAP_STUCK_TIMEOUT
                if last_packet_time.elapsed() > PCAP_STUCK_TIMEOUT {
                    if reopen_attempts >= MAX_REOPEN_ATTEMPTS {
                        error!(
                            attempts = reopen_attempts,
                            elapsed_s = last_packet_time.elapsed().as_secs(),
                            "pcap stuck after {} reopen attempts — breaking for restart",
                            reopen_attempts
                        );
                        break;
                    }

                    warn!(
                        elapsed_s = last_packet_time.elapsed().as_secs(),
                        attempt = reopen_attempts + 1,
                        "pcap stuck (zero packets for {}s) — reopening capture handle",
                        last_packet_time.elapsed().as_secs()
                    );

                    match pcap.reopen() {
                        Ok(()) => {
                            reopen_attempts += 1;
                            last_packet_time = Instant::now();
                            info!(attempt = reopen_attempts, "pcap handle reopened, resuming capture");
                        }
                        Err(e) => {
                            error!(error = %e, "Failed to reopen pcap — breaking for restart");
                            break;
                        }
                    }
                }

                continue;
            }
            PacketResult::Error(ref msg) => {
                // Check if we've been told to stop
                if !stop_flag.load(Ordering::Relaxed) {
                    break;
                }
                consecutive_errors += 1;
                // Track when the error burst started
                let error_start = *first_error_time.get_or_insert_with(Instant::now);

                // Require BOTH count threshold AND minimum time window.
                // pcap_next_ex returns immediately on error (no 100ms timeout),
                // so 100 errors could fire in <100ms from a transient glitch.
                // A real USB disconnect produces sustained errors over seconds.
                let elapsed = error_start.elapsed();
                if consecutive_errors >= MAX_CONSECUTIVE_ERRORS && elapsed >= MIN_ERROR_WINDOW {
                    error!(
                        consecutive_errors,
                        elapsed_ms = elapsed.as_millis() as u64,
                        last_error = %msg,
                        "CRITICAL: {} consecutive pcap errors over {}s — USB adapter likely disconnected. Triggering shutdown for systemd restart.",
                        consecutive_errors, elapsed.as_secs()
                    );
                    // Signal stop so systemd can restart us
                    stop_flag.store(false, Ordering::SeqCst);
                    break;
                }
                if consecutive_errors % 10 == 1 {
                    warn!(
                        consecutive_errors,
                        error = %msg,
                        elapsed_ms = elapsed.as_millis() as u64,
                        "pcap read error (will shutdown after {} consecutive + {}s window)",
                        MAX_CONSECUTIVE_ERRORS, MIN_ERROR_WINDOW.as_secs()
                    );
                }
                continue;
            }
        };

        // Parse the 802.11 frame
        let parsed = match frame::parse_packet(&packet_data) {
            Ok(f) => f,
            Err(e) => {
                let count = parse_error_counter.fetch_add(1, Ordering::Relaxed) + 1;
                if count <= 10 || count % 1000 == 0 {
                    warn!(error = %e, count, pkt_len = packet_data.len(), "Parse error");
                }
                continue;
            }
        };

        // ── Channel diversity tracking ──
        if parsed.radiotap.frequency > 0 {
            observed_freqs.insert(parsed.radiotap.frequency);
            freq_check_packets += 1;
        }

        if freq_check_time.elapsed() >= Duration::from_secs(10) {
            let distinct = observed_freqs.len() as u32;
            distinct_channels.store(distinct, Ordering::Relaxed);

            if !is_passive && freq_check_packets > 20 && distinct <= 2 {
                stuck_count += 1;
                warn!(
                    distinct,
                    freq_check_packets,
                    stuck_count,
                    "Channel diversity LOW — adapter may be stuck"
                );
                if stuck_count >= 3 {
                    error!(
                        "CRITICAL: Adapter stuck on same channel for 30s — breaking for systemd restart (interface {})",
                        interface
                    );
                    // Do NOT run `ip link down/up` here — it destroys monitor mode
                    // while the pcap handle is still open, and the break already
                    // triggers a systemd restart which re-initializes everything cleanly.
                    break; // Exit capture loop → systemd restarts
                }
            } else {
                stuck_count = 0;
            }

            tracing::debug!(distinct, freq_check_packets, "Channel diversity check");
            observed_freqs.clear();
            freq_check_packets = 0;
            freq_check_time = Instant::now();
        }

        // Skip frames with bad FCS (corrupted) — saves CPU on detection checks
        if parsed.radiotap.bad_fcs {
            bad_fcs_count += 1;
            if bad_fcs_count % 1000 == 1 {
                warn!(bad_fcs_total = bad_fcs_count, "Skipping frame with bad FCS");
            }
            continue;
        }

        // Filter frame types: management frames get full detection pipeline,
        // data frames get lightweight OUI-only check (for OcuSync traffic detection)
        match parsed.frame_type {
            FrameType::Beacon | FrameType::ProbeResponse | FrameType::ProbeRequest => {}
            FrameType::Action => {
                action_frame_counter.fetch_add(1, Ordering::Relaxed);
                // Fall through to detection pipeline — Action frames carry
                // vendor IEs (category 127) and NAN RemoteID (category 4)
            }
            FrameType::Other(2, _) => {
                // Data frame — lightweight OUI check only (no IE parsing needed)
                // Catches drone MACs in OcuSync/data traffic that Realtek CAN deliver
                if let Some(mfr) = get_drone_manufacturer_from_oui(&parsed.src_mac) {
                    let hopper_ch = current_channel.load(Ordering::Relaxed);
                    let ch = if hopper_ch != 0 { hopper_ch } else { parsed.radiotap.channel as i32 };
                    info!(
                        mac = %frame::format_mac(&parsed.src_mac),
                        manufacturer = mfr,
                        rssi = parsed.radiotap.rssi,
                        channel = ch,
                        "Drone OUI in DATA frame"
                    );
                    // Fall through to full detection pipeline
                } else {
                    continue; // Not a drone OUI, skip
                }
            }
            _ => continue,
        }

        // Skip denylisted MACs (known false positive sources)
        if !mac_denylist.is_empty() && mac_denylist.contains(&parsed.src_mac) {
            continue;
        }

        // Track BSSID in baseline (every management frame, before detection checks)
        // In passive mode (hopper not running), use radiotap channel from the frame
        let hopper_ch = current_channel.load(Ordering::Relaxed);
        let frame_channel = if hopper_ch != 0 { hopper_ch } else { parsed.radiotap.channel as i32 };
        let bl_ssid = parsed.ssid.as_deref().unwrap_or("");
        baseline.observe(&parsed.bssid, bl_ssid, frame_channel, parsed.radiotap.rssi);

        // Wrap detection logic in catch_unwind to prevent panics from crashing the TAP.
        // Malformed frames can trigger panics in decode logic (e.g. slice out of bounds).
        let detection_result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            process_frame(
                &parsed, &packet_data, intel, tap_id, frame_channel,
                &mut last_seen, &mut cleanup_counter, dedup_interval,
                session_timeout, tap_lat, tap_lon,
            )
        }));

        match detection_result {
            Ok(Some(detection)) => {
                if det_tx.try_send(detection).is_err() {
                    let drops = WIFI_CHANNEL_DROPS.fetch_add(1, Ordering::Relaxed) + 1;
                    if drops % 100 == 1 {
                        warn!(total_drops = drops, "Detection channel full, dropping frame");
                    }
                }
            }
            Ok(None) => {} // No detection (filtered out)
            Err(_) => {
                panic_count += 1;
                error!(
                    panic_total = panic_count,
                    mac = %format_mac(&parsed.src_mac),
                    channel = frame_channel,
                    frame_type = ?parsed.frame_type,
                    "PANIC in frame processing — caught and continuing"
                );
            }
        }
    }
}

/// Process a single parsed frame through detection checks.
/// Returns Some(Detection) if a drone was detected, None otherwise.
/// Extracted from capture_loop so it can be wrapped in catch_unwind.
fn process_frame(
    parsed: &frame::ParsedFrame,
    packet_data: &[u8],
    intel: &IntelDatabase,
    tap_id: &str,
    frame_channel: i32,
    last_seen: &mut HashMap<String, PcapDedupState>,
    cleanup_counter: &mut u64,
    dedup_interval: Duration,
    session_timeout: Duration,
    tap_lat: f64,
    tap_lon: f64,
) -> Option<proto::Detection> {
    // Check 1: OUI match (src MAC or BSSID)
    // Skip locally-administered (randomized) MACs — they collide with vendor IE OUIs
    let src_is_laa = parsed.src_mac[0] & 0x02 != 0;
    let bssid_is_laa = parsed.bssid[0] & 0x02 != 0;
    let oui_match = if src_is_laa { None } else { intel.match_oui(&parsed.src_mac) }
        .or_else(|| if bssid_is_laa { None } else { intel.match_oui(&parsed.bssid) });

    // Check 2: SSID match
    let ssid_match = parsed
        .ssid
        .as_ref()
        .and_then(|ssid| intel.match_ssid(ssid));

    // Check 3: RemoteID vendor IE
    let remoteid_ie = parsed.vendor_ies.iter().find(|vie| is_remoteid_ie(vie));

    // Check 4: DJI DroneID vendor IE
    let dji_ie = parsed.vendor_ies.iter().find(|vie| is_dji_droneid_ie(vie));

    // Check 5: Parrot vendor IE
    let parrot_ie = parsed.vendor_ies.iter().find(|vie| is_parrot_ie(vie));

    // Check 6: Autel vendor IE
    let autel_ie = parsed.vendor_ies.iter().find(|vie| is_autel_ie(vie));

    // Check 7: OUI-based manufacturer detection (catches drones without vendor IEs)
    let oui_manufacturer = get_drone_manufacturer_from_oui(&parsed.src_mac);

    // Check 8: Behavioral heuristics for UNKNOWN drones
    let (is_suspect_drone, behavior_score, behavior_reason) = is_drone_like_behavior(
        parsed,
        frame_channel,
    );

    // Diagnostic: log any vendor IE from known drone OUIs (helps debug zero-detection issues)
    // WFA OUI (50:6F:9A) only logged for NAN type 0x13 to avoid Wi-Fi P2P/WFD noise
    for vie in &parsed.vendor_ies {
        let is_drone_oui = matches!(vie.oui,
            [0x26, 0x37, 0x12] | [0x26, 0x6F, 0x48] | [0x60, 0x60, 0x1F] |
            [0xFA, 0x0B, 0xBC] | [0x90, 0x03, 0xB7] | [0xA0, 0x14, 0x3D] |
            [0x70, 0x88, 0x6B] | [0xEC, 0x5B, 0xCD] | [0x18, 0xD7, 0x93]
        );
        let is_wfa_nan = vie.oui == [0x50, 0x6F, 0x9A] && vie.oui_type == 0x13;
        if is_drone_oui || is_wfa_nan {
            debug!(
                mac = %format_mac(&parsed.src_mac),
                oui = format!("{:02X}:{:02X}:{:02X}", vie.oui[0], vie.oui[1], vie.oui[2]),
                oui_type = format!("0x{:02X}", vie.oui_type),
                data_len = vie.data.len(),
                frame_type = ?parsed.frame_type,
                "Vendor IE from KNOWN drone OUI"
            );
        }
    }

    // If nothing matched AND not behaviorally suspicious, skip
    if oui_match.is_none() && ssid_match.is_none() && remoteid_ie.is_none() && dji_ie.is_none()
        && parrot_ie.is_none() && autel_ie.is_none() && oui_manufacturer.is_none()
        && !is_suspect_drone
    {
        return None;
    }

    // Deduplication: rate-limit per MAC address with data-completeness bypass.
    // Within the dedup window, allow frames through if they carry NEW data types
    // (serial, drone GPS, operator GPS) that the previous emission lacked.
    let mac_str = format_mac(&parsed.src_mac);
    let now = Instant::now();

    // Quick pre-check: does this frame carry serial/GPS data?
    // RemoteID IEs and DJI IEs can carry serial and location data.
    let frame_has_remoteid = remoteid_ie.is_some();
    let frame_has_dji = dji_ie.is_some();
    let frame_has_id_source = frame_has_remoteid || frame_has_dji;

    let session_id = if let Some(state) = last_seen.get(&mac_str) {
        // Controllers use longer dedup (5 min) — they broadcast constantly but
        // rarely carry new data. Still allows re-report with fresh RSSI for trilateration.
        let effective_dedup = if state.is_controller {
            Duration::from_secs(CONTROLLER_DEDUP_SECS)
        } else if state.has_serial && state.has_drone_gps && state.has_operator_gps && !frame_has_dji {
            // State has complete data (likely from DJI DroneID). This frame is non-DJI.
            // DJI drones broadcast both proprietary DroneID (with full data) and ASTM F3411
            // ODID beacons (often empty/bogus). Once we have serial+GPS+operator from DJI,
            // the ODID beacons add nothing — DJI beacons every ~10-15s already provide
            // fresh RSSI. Extend dedup to 30s to suppress noise. The dedup_bypass still
            // allows frames through if they somehow carry new data types.
            Duration::from_secs(30)
        } else {
            dedup_interval
        };
        if now.duration_since(state.last_sent) < effective_dedup {
            // Within dedup window — check if this frame might have new data
            if frame_has_id_source {
                let might_have_new_data = !state.has_serial
                    || !state.has_drone_gps
                    || !state.has_operator_gps;
                if might_have_new_data {
                    tracing::debug!(
                        mac = %mac_str,
                        has_serial = state.has_serial,
                        has_drone_gps = state.has_drone_gps,
                        has_operator_gps = state.has_operator_gps,
                        "Dedup bypass: frame may have new data"
                    );
                    state.session_id.clone()
                } else {
                    return None; // Already have all data types, skip
                }
            } else {
                return None; // Normal dedup: too soon, no new data source
            }
        } else if now.duration_since(state.last_sent) > session_timeout {
            // Stale (>60s gap), generate new session
            Uuid::new_v4().to_string()
        } else {
            state.session_id.clone()
        }
    } else {
        Uuid::new_v4().to_string()
    };
    // Temporarily insert — preserve existing data flags so accumulated knowledge
    // (e.g. DJI serial/GPS) isn't wiped when an empty RemoteID beacon passes through.
    // Flags get OR'd with this detection's actual data at end of process_frame.
    let (keep_serial, keep_gps, keep_op_gps) = last_seen
        .get(&mac_str)
        .map(|s| (s.has_serial, s.has_drone_gps, s.has_operator_gps))
        .unwrap_or((false, false, false));
    last_seen.insert(mac_str.clone(), PcapDedupState {
        last_sent: now,
        session_id: session_id.clone(),
        has_serial: keep_serial,
        has_drone_gps: keep_gps,
        has_operator_gps: keep_op_gps,
        is_controller: false,
    });

    // Periodic cleanup of stale entries (every 1000 detections)
    *cleanup_counter += 1;
    if *cleanup_counter % 1000 == 0 {
        last_seen.retain(|_, state| now.duration_since(state.last_sent) < session_timeout);
    }

    // Build detection
    let now_ns = chrono::Utc::now().timestamp_nanos_opt().unwrap_or(0);
    let channel = frame_channel;

    let session_id_clone = session_id.clone();
    let mut detection = proto::Detection {
        tap_id: tap_id.to_string(),
        timestamp_ns: now_ns,
        mac_address: mac_str,
        session_id,
        rssi: parsed.radiotap.rssi,
        channel,
        frequency_mhz: parsed.radiotap.frequency as i32,
        ssid: parsed.ssid.clone().unwrap_or_default(),
        beacon_interval_tu: parsed.beacon_interval.unwrap_or(0) as u32,
        raw_frame: packet_data.to_vec(),
        ..Default::default()
    };

    // Determine source and enrich from matched data
    let mut source = proto::DetectionSource::SourceUnknown as i32;
    let mut manufacturer = String::new();
    let mut designation = String::new();
    let mut confidence: f32 = 0.0;
    let mut is_controller = false;

    // OUI enrichment
    if let Some(oui_desc) = oui_match {
        manufacturer = decode::oui::IntelDatabase::manufacturer_from_oui(oui_desc).to_string();
        confidence = 0.30;
        if oui_desc.contains("(controller)") {
            is_controller = true;
        }
    }

    // SSID enrichment
    if let Some(ssid_pat) = ssid_match {
        if manufacturer.is_empty() {
            manufacturer = ssid_pat.manufacturer.clone();
        }
        if !ssid_pat.model_hint.is_empty() {
            designation = ssid_pat.model_hint.clone();
        }
        if ssid_pat.is_controller {
            is_controller = true;
            confidence = confidence.max(0.65);
            // SSID is stable across MAC randomization — use as identifier
            if let Some(ref ssid) = parsed.ssid {
                detection.identifier = ssid.clone();
            }
            // Controller IS at operator location — set to TAP position
            if tap_lat != 0.0 && tap_lon != 0.0 {
                detection.operator_latitude = tap_lat;
                detection.operator_longitude = tap_lon;
                detection.operator_location_type = proto::OperatorLocationType::OpLocFixed as i32;
            }
        } else {
            confidence = confidence.max(0.40);
        }
        if oui_match.is_some() {
            confidence = confidence.max(0.50); // OUI + SSID combined
        }
    }

    // DJI SSID model match — runs independently of ssid_match
    if let Some(ssid) = &parsed.ssid {
        if let Some(model) = intel.match_dji_ssid_model(ssid) {
            designation = model.to_string();
            manufacturer = "DJI".to_string();
            confidence = confidence.max(0.55);
        }
    }

    // RemoteID decoding
    if let Some(vie) = remoteid_ie {
        source = match parsed.frame_type {
            FrameType::Beacon => proto::DetectionSource::SourceWifiBeacon as i32,
            FrameType::ProbeResponse => proto::DetectionSource::SourceWifiProbeResp as i32,
            FrameType::Action => proto::DetectionSource::SourceWifiNan as i32,
            _ => proto::DetectionSource::SourceWifiBeacon as i32,
        };
        confidence = 0.85;

        // Store raw RemoteID payload
        detection.remoteid_payload = vie.data.clone();

        let rid_result = decode::remoteid::decode_remoteid(&vie.data);
        if rid_result.is_none() {
            warn!(
                mac = %format_mac(&parsed.src_mac),
                data_len = vie.data.len(),
                data_hex = format!("{:02X?}", &vie.data[..vie.data.len().min(16)]),
                frame_type = ?parsed.frame_type,
                "RemoteID vendor IE found but decode FAILED"
            );
        }
        if let Some(rid) = rid_result {
            if let Some(id) = &rid.uas_id {
                detection.identifier = id.clone();
                detection.serial_number = id.clone();

                // Route UTM-assigned IDs to utm_id field (id_type 4 per ASTM F3411)
                if rid.id_type == 4 {
                    detection.utm_id = id.clone();
                }

                // Try serial prefix lookup
                if let Some(prefix) = intel.match_serial(id) {
                    manufacturer = prefix.manufacturer.clone();
                    designation = prefix.model.clone();
                }
            }

            // Map ua_type to uav_type string and uav_category enum
            detection.uav_type = ua_type_to_string(rid.ua_type).to_string();
            detection.uav_category = ua_type_to_category(rid.ua_type) as i32;

            if let Some(lat) = rid.latitude {
                detection.latitude = lat;
                confidence = 0.95; // RemoteID with location = highest
            }
            if let Some(lon) = rid.longitude {
                detection.longitude = lon;
            }
            if let Some(alt) = rid.altitude_geodetic {
                detection.altitude_geodetic = alt;
            }
            if let Some(alt) = rid.altitude_pressure {
                detection.altitude_pressure = alt;
            }
            if let Some(h) = rid.height_agl {
                detection.height_agl = h;
            }
            detection.height_reference = rid.height_reference as i32;
            if let Some(s) = rid.speed {
                detection.speed = s;
            }
            if let Some(vs) = rid.vertical_speed {
                detection.vertical_speed = vs;
            }
            if let Some(h) = rid.heading {
                detection.heading = h;
                // RemoteID heading IS track/ground course per ODID spec
                detection.track_direction = h;
            }
            detection.operational_status = rid.operational_status as i32;
            if let Some(lat) = rid.operator_latitude {
                detection.operator_latitude = lat;
            }
            if let Some(lon) = rid.operator_longitude {
                detection.operator_longitude = lon;
            }
            if let Some(alt) = rid.operator_altitude {
                detection.operator_altitude = alt;
            }
            // ASTM wire: 0=TakeOff, 1=LiveGNSS, 2=Fixed; proto: 0=Unknown, 1=TakeOff, 2=LiveGNSS, 3=Fixed
            detection.operator_location_type = match rid.operator_location_type {
                0 => proto::OperatorLocationType::OpLocTakeoff as i32,
                1 => proto::OperatorLocationType::OpLocLiveGnss as i32,
                2 => proto::OperatorLocationType::OpLocFixed as i32,
                _ => proto::OperatorLocationType::OpLocUnknown as i32,
            };
            if let Some(id) = &rid.operator_id {
                detection.operator_id = id.clone();
            }

            // Populate registration from SelfID extraction
            if let Some(reg) = &rid.registration {
                detection.registration = reg.clone();
            }
        }
    }

    // DJI DroneID decoding
    if let Some(vie) = dji_ie {
        source = proto::DetectionSource::SourceDjiOcusync as i32;
        confidence = confidence.max(0.80);

        // Store raw DJI IE as remoteid_payload too
        if detection.remoteid_payload.is_empty() {
            detection.remoteid_payload = vie.data.clone();
        }

        let dji_result = decode::dji::decode_dji_droneid(&vie.data, vie.oui_type);
        if dji_result.is_none() {
            warn!(
                mac = %format_mac(&parsed.src_mac),
                oui = format!("{:02X}:{:02X}:{:02X}", vie.oui[0], vie.oui[1], vie.oui[2]),
                oui_type = format!("0x{:02X}", vie.oui_type),
                data_len = vie.data.len(),
                data_hex = format!("{:02X?}", &vie.data[..vie.data.len().min(16)]),
                "DJI vendor IE found but decode FAILED - check format"
            );
        }
        if let Some(dji) = dji_result {
            if let Some(serial) = &dji.serial_number {
                detection.serial_number = serial.clone();
                if detection.identifier.is_empty() {
                    detection.identifier = serial.clone();
                }

                // Serial prefix lookup
                if let Some(prefix) = intel.match_serial(serial) {
                    manufacturer = prefix.manufacturer.clone();
                    designation = prefix.model.clone();
                }
            }
            // Extract model code from serial number (chars 4-6)
            // DJI serials: "1581" + 3-char model code + digits, e.g. "1581F1Y1234567"
            // The dji_model_codes DB maps "F1Y" → "Mini 3 Pro", etc.
            if let Some(serial) = &dji.serial_number {
                if let Some(model_code) = serial.get(4..7) {
                    if let Some(model) = intel.match_dji_model_code(model_code) {
                        designation = model.to_string();
                        manufacturer = "DJI".to_string();
                    }
                }
            }
            // Ensure DJI manufacturer even if only OUI matched
            if manufacturer.is_empty() {
                manufacturer = "DJI".to_string();
            }

            // Map DJI state_info to operational_status
            // bit 0 = motor on, bits 1-2 = in air (0=ground, 1+=airborne)
            let motor_on = dji.state_info & 0x01 != 0;
            let in_air = (dji.state_info >> 1) & 0x03;
            if in_air > 0 {
                detection.operational_status =
                    proto::OperationalStatus::OpStatusAirborne as i32;
            } else if motor_on {
                detection.operational_status =
                    proto::OperationalStatus::OpStatusGround as i32;
            }
            // DJI drones are always multirotor
            detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;

            if let Some(lat) = dji.latitude {
                detection.latitude = lat;
                confidence = 0.90; // DJI with location
            }
            if let Some(lon) = dji.longitude {
                detection.longitude = lon;
            }
            // DJI reports barometric altitude, map to altitude_pressure (not geodetic)
            if let Some(alt) = dji.altitude_pressure {
                detection.altitude_pressure = alt;
                // Note: We intentionally do NOT set altitude_geodetic here
                // as DJI barometric altitude is MSL-referenced, not WGS84 ellipsoid
            }
            if let Some(h) = dji.height_agl {
                detection.height_agl = h;
            }
            if let Some(heading) = dji.heading {
                detection.heading = heading;
            }
            // Compute ground speed and track direction from north/east components
            if let (Some(vn), Some(ve)) = (dji.speed_north, dji.speed_east) {
                detection.speed = (vn * vn + ve * ve).sqrt();
                // Track direction = direction of travel from velocity components
                if detection.speed > 0.1 {
                    let track = ve.atan2(vn).to_degrees();
                    detection.track_direction = ((track % 360.0) + 360.0) % 360.0;
                }
            }
            if let Some(vu) = dji.speed_up {
                detection.vertical_speed = vu;
            }
            // Operator position from home coordinates
            if let Some(lat) = dji.home_latitude {
                detection.operator_latitude = lat;
                detection.operator_location_type =
                    proto::OperatorLocationType::OpLocTakeoff as i32;
            }
            if let Some(lon) = dji.home_longitude {
                detection.operator_longitude = lon;
            }
        }
    }

    // Parrot drone detection — vendor IE is high-confidence, OUI-only requires SSID validation
    // Parrot makes non-drone products (car kits, speakers) that share OUIs with drones.
    if parrot_ie.is_some() {
        if manufacturer.is_empty() {
            manufacturer = "Parrot".to_string();
        }
        source = proto::DetectionSource::SourceWifiBeacon as i32;
        confidence = confidence.max(0.60);
        detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        info!(mac = %format_mac(&parsed.src_mac), "Parrot drone detected via vendor IE");
    } else if oui_manufacturer == Some("Parrot") && has_drone_ssid(&parsed.ssid) {
        if manufacturer.is_empty() {
            manufacturer = "Parrot".to_string();
        }
        source = proto::DetectionSource::SourceWifiBeacon as i32;
        confidence = confidence.max(0.50);
        detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        info!(mac = %format_mac(&parsed.src_mac), ssid = ?parsed.ssid, "Parrot drone detected via OUI+SSID");
    }

    // Autel drone detection — vendor IE is high-confidence, OUI-only requires SSID validation
    if autel_ie.is_some() {
        if manufacturer.is_empty() {
            manufacturer = "Autel".to_string();
        }
        source = proto::DetectionSource::SourceWifiBeacon as i32;
        confidence = confidence.max(0.60);
        detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        info!(mac = %format_mac(&parsed.src_mac), "Autel drone detected via vendor IE");
    } else if oui_manufacturer == Some("Autel") && has_drone_ssid(&parsed.ssid) {
        if manufacturer.is_empty() {
            manufacturer = "Autel".to_string();
        }
        source = proto::DetectionSource::SourceWifiBeacon as i32;
        confidence = confidence.max(0.50);
        detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        info!(mac = %format_mac(&parsed.src_mac), ssid = ?parsed.ssid, "Autel drone detected via OUI+SSID");
    }

    // Skydio drone detection (OUI-only — require SSID validation)
    if oui_manufacturer == Some("Skydio") && has_drone_ssid(&parsed.ssid) {
        if manufacturer.is_empty() {
            manufacturer = "Skydio".to_string();
        }
        source = proto::DetectionSource::SourceWifiBeacon as i32;
        confidence = confidence.max(0.50);
        detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        info!(mac = %format_mac(&parsed.src_mac), ssid = ?parsed.ssid, "Skydio drone detected via OUI+SSID");
    }

    // Yuneec drone detection (OUI-only — require SSID validation)
    if oui_manufacturer == Some("Yuneec") && has_drone_ssid(&parsed.ssid) {
        if manufacturer.is_empty() {
            manufacturer = "Yuneec".to_string();
        }
        source = proto::DetectionSource::SourceWifiBeacon as i32;
        confidence = confidence.max(0.50);
        detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        info!(mac = %format_mac(&parsed.src_mac), ssid = ?parsed.ssid, "Yuneec drone detected via OUI+SSID");
    }

    // OUI-based manufacturer fallback — only for manufacturers where OUI alone
    // is reliable (DJI). For Parrot/Autel/Skydio/Yuneec, bare OUI without
    // vendor IE or drone SSID is NOT enough (they make non-drone products).
    if manufacturer.is_empty() {
        if let Some(mfr) = oui_manufacturer {
            match mfr {
                "Parrot" | "Autel" | "Skydio" | "Yuneec" => {
                    // Already handled above with SSID validation — don't set here
                    debug!(mac = %format_mac(&parsed.src_mac), oui_mfr = mfr, ssid = ?parsed.ssid,
                        "OUI-only match for multi-product manufacturer, skipping without SSID match");
                }
                _ => {
                    manufacturer = mfr.to_string();
                    confidence = confidence.max(0.40);
                }
            }
        }
    }

    // Behavioral suspect drone detection — catch unknown drones by behavior
    if manufacturer.is_empty() && is_suspect_drone {
        confidence = (behavior_score * 0.6).min(0.60).max(0.20);
        source = proto::DetectionSource::SourceWifiBeacon as i32;

        // DJI RC controller detection — PROJ SSIDs get special treatment:
        // - Mark as controller (not drone)
        // - Set manufacturer to DJI
        // - Use SSID as stable identifier (MAC randomizes, SSID doesn't)
        // - Set operator location to TAP position (controller IS at operator)
        if behavior_reason == "ssid_dji_controller" || behavior_reason == "ssid_dji_enterprise_rc" {
            manufacturer = "DJI".to_string();
            designation = if behavior_reason == "ssid_dji_enterprise_rc" {
                "DJI Enterprise RC".to_string()
            } else {
                "DJI RC Controller".to_string()
            };
            is_controller = true;
            confidence = confidence.max(0.65);
            // SSID is stable across MAC randomization — use as identifier
            if let Some(ref ssid) = parsed.ssid {
                detection.identifier = ssid.clone();
            }
            // Controller IS at operator location — set to TAP position
            if tap_lat != 0.0 && tap_lon != 0.0 {
                detection.operator_latitude = tap_lat;
                detection.operator_longitude = tap_lon;
                detection.operator_location_type = proto::OperatorLocationType::OpLocFixed as i32;
            }
            // Log ALL vendor IEs from controller beacon — DJI may embed drone GPS telemetry
            for vie in &parsed.vendor_ies {
                info!(
                    mac = %format_mac(&parsed.src_mac),
                    ssid = %parsed.ssid.as_deref().unwrap_or(""),
                    oui = format!("{:02X}:{:02X}:{:02X}", vie.oui[0], vie.oui[1], vie.oui[2]),
                    oui_type = format!("0x{:02X}", vie.oui_type),
                    data_len = vie.data.len(),
                    data_hex = format!("{:02X?}", &vie.data[..vie.data.len().min(64)]),
                    "DJI controller vendor IE"
                );
            }
            // Log extended frame data for controller fingerprinting
            let rates_str: Vec<String> = parsed.supported_rates.iter()
                .map(|r| format!("{:.1}", (*r & 0x7F) as f32 * 0.5))
                .collect();
            let ext_rates_str: Vec<String> = parsed.extended_rates.iter()
                .map(|r| format!("{:.1}", (*r & 0x7F) as f32 * 0.5))
                .collect();
            info!(
                mac = %format_mac(&parsed.src_mac),
                ssid = %parsed.ssid.as_deref().unwrap_or(""),
                vendor_ie_count = parsed.vendor_ies.len(),
                raw_frame_len = packet_data.len(),
                seq_num = parsed.sequence_number,
                capability_info = format!("0x{:04X}", parsed.capability_info.unwrap_or(0)),
                ds_channel = ?parsed.ds_channel,
                country = %parsed.country_code.as_deref().unwrap_or(""),
                supported_rates = %rates_str.join(","),
                extended_rates = %ext_rates_str.join(","),
                ht_cap = format!("0x{:04X}", parsed.ht_capabilities.unwrap_or(0)),
                has_rsn = parsed.rsn_info.is_some(),
                "DJI controller frame analysis"
            );
            // Check if DJI vendor IEs are present — parse for drone telemetry
            if let Some(dji_vie) = parsed.vendor_ies.iter().find(|v| is_dji_droneid_ie(v)) {
                if let Some(dji_data) = decode::dji::decode_dji_droneid(&dji_vie.data, dji_vie.oui_type) {
                    info!(
                        mac = %format_mac(&parsed.src_mac),
                        serial = ?dji_data.serial_number,
                        lat = ?dji_data.latitude,
                        lon = ?dji_data.longitude,
                        alt = ?dji_data.altitude_pressure,
                        home_lat = ?dji_data.home_latitude,
                        home_lon = ?dji_data.home_longitude,
                        "DJI controller contains DroneID telemetry!"
                    );
                    // Set drone location from controller's DJI IE
                    if let (Some(lat), Some(lon)) = (dji_data.latitude, dji_data.longitude) {
                        if lat != 0.0 && lon != 0.0 {
                            detection.latitude = lat;
                            detection.longitude = lon;
                            if let Some(alt) = dji_data.altitude_pressure {
                                detection.altitude_pressure = alt;
                            }
                        }
                    }
                    if let (Some(lat), Some(lon)) = (dji_data.home_latitude, dji_data.home_longitude) {
                        if lat != 0.0 && lon != 0.0 {
                            detection.operator_latitude = lat;
                            detection.operator_longitude = lon;
                            detection.operator_location_type = proto::OperatorLocationType::OpLocTakeoff as i32;
                        }
                    }
                    if let Some(ref serial) = dji_data.serial_number {
                        if !serial.is_empty() {
                            detection.serial_number = serial.clone();
                            detection.identifier = serial.clone();
                        }
                    }
                }
            }
        } else {
            manufacturer = "UNKNOWN".to_string();
            designation = "SUSPECT".to_string();
            detection.uav_category = proto::UavCategory::UavCatHelicopter as i32;
        }

        let is_58ghz = frame_channel >= 149 && frame_channel <= 165;
        let is_laa = (parsed.src_mac[0] & 0x02) != 0;
        warn!(
            mac = %format_mac(&parsed.src_mac),
            ssid = %parsed.ssid.as_deref().unwrap_or(""),
            channel = frame_channel,
            is_58ghz = is_58ghz,
            is_locally_administered = is_laa,
            beacon_interval = parsed.beacon_interval.unwrap_or(0),
            behavior_score = behavior_score,
            behavior_reason = behavior_reason,
            "SUSPECT drone detected via behavioral analysis"
        );
    }

    // Generate identifier if still empty (use MAC)
    if detection.identifier.is_empty() {
        detection.identifier = format_mac(&parsed.src_mac);
    }

    // Set source — prefer specific (RemoteID/DJI) over generic (beacon)
    if source == proto::DetectionSource::SourceUnknown as i32 {
        source = match parsed.frame_type {
            FrameType::Beacon => proto::DetectionSource::SourceWifiBeacon as i32,
            FrameType::ProbeResponse => proto::DetectionSource::SourceWifiProbeResp as i32,
            _ => proto::DetectionSource::SourceWifiBeacon as i32,
        };
    }

    detection.source = source;
    detection.manufacturer = manufacturer.clone();
    detection.designation = designation.clone();
    detection.confidence = confidence;
    detection.is_controller = is_controller;
    // Extended 802.11 fingerprint fields
    detection.country_code = parsed.country_code.clone().unwrap_or_default();
    detection.ht_capabilities = parsed.ht_capabilities.unwrap_or(0) as u32;

    // Update dedup state with what data this detection actually carries.
    // OR flags with existing state to preserve knowledge from prior frames
    // (e.g. DJI DroneID provides serial+GPS, then empty RemoteID beacons
    // should NOT wipe those flags — otherwise RemoteID bypasses dedup).
    let has_serial = !detection.serial_number.is_empty();
    let has_drone_gps = detection.latitude != 0.0 || detection.longitude != 0.0;
    let has_operator_gps = detection.operator_latitude != 0.0 || detection.operator_longitude != 0.0;
    let (prev_serial, prev_drone_gps, prev_operator_gps) = last_seen
        .get(&detection.mac_address)
        .map(|s| (s.has_serial, s.has_drone_gps, s.has_operator_gps))
        .unwrap_or((false, false, false));
    last_seen.insert(detection.mac_address.clone(), PcapDedupState {
        last_sent: now,
        session_id: session_id_clone,
        has_serial: has_serial || prev_serial,
        has_drone_gps: has_drone_gps || prev_drone_gps,
        has_operator_gps: has_operator_gps || prev_operator_gps,
        is_controller,
    });

    // Log raw frame-level detection at DEBUG (before sanitize/merge in flush handler).
    // The INFO-level DETECTION log is in the flush handler after sanitize+merge,
    // so it accurately reflects what's published to NATS.
    debug!(
        mac = %detection.mac_address,
        id = %detection.identifier,
        manufacturer = %manufacturer,
        model = %designation,
        rssi = detection.rssi,
        channel = detection.channel,
        freq_mhz = detection.frequency_mhz,
        source = source,
        confidence = detection.confidence,
        ssid = %detection.ssid,
        op_lat = format!("{:.6}", detection.operator_latitude),
        op_lon = format!("{:.6}", detection.operator_longitude),
        drone_lat = format!("{:.6}", detection.latitude),
        drone_lon = format!("{:.6}", detection.longitude),
        seq = parsed.sequence_number,
        ds_ch = ?parsed.ds_channel,
        "FRAME_DETECTION"
    );

    // Final gate: drop bare OUI-only matches from multi-product manufacturers.
    // These companies make non-drone products (car kits, speakers, IoT) that share
    // OUIs with their drones. Without vendor IE, drone SSID, or behavioral evidence,
    // it's a false positive. NAN/RemoteID/DJI DroneID frames always set manufacturer
    // via their respective detection blocks above — they are NOT affected by this gate.
    if detection.manufacturer.is_empty()
        && matches!(oui_manufacturer, Some("Parrot") | Some("Autel") | Some("Skydio") | Some("Yuneec"))
        && !is_suspect_drone
    {
        debug!(mac = %format_mac(&parsed.src_mac), ssid = ?parsed.ssid, oui = ?oui_manufacturer,
            "Dropping bare OUI match from multi-product manufacturer (no vendor IE, no drone SSID)");
        return None;
    }

    Some(detection)
}

/// Heartbeat loop — sends tap status every 5 seconds
async fn heartbeat_loop(
    nats: &NatsPublisher,
    config: &Config,
    packets_captured: &Arc<AtomicU64>,
    bytes_captured: &Arc<AtomicU64>,
    current_channel: &Arc<AtomicI32>,
    pcap_received: &Arc<AtomicU32>,
    pcap_dropped: &Arc<AtomicU32>,
    buffer_metrics: &Arc<publish::buffer::BufferMetrics>,
    baseline_metrics: &Arc<BaselineMetrics>,
    distinct_channels: &Arc<AtomicU32>,
    ble_stats: &Arc<BleStats>,
    ble_enabled: bool,
    ble_adapter: &str,
    start_time: Instant,
) {
    let mut interval = tokio::time::interval(tokio::time::Duration::from_secs(5));
    let mut sys = sysinfo::System::new();
    let mut last_packets: u64 = 0;
    let mut last_time = Instant::now();

    loop {
        interval.tick().await;

        sys.refresh_cpu_usage();
        sys.refresh_memory();

        // Read CPU temperature
        let temp = read_cpu_temp().unwrap_or(0.0);

        let uptime = start_time.elapsed().as_secs();
        let pkts = packets_captured.load(Ordering::Relaxed);
        let bytes = bytes_captured.load(Ordering::Relaxed);
        let dets = DETECTIONS_SENT.load(Ordering::Relaxed);
        let chan = current_channel.load(Ordering::Relaxed);
        let kern_recv = pcap_received.load(Ordering::Relaxed);
        let kern_drop = pcap_dropped.load(Ordering::Relaxed);

        // Buffer metrics for reliable delivery monitoring
        let buf_size = buffer_metrics.buffer_size.load(Ordering::Relaxed);
        let buf_dropped = buffer_metrics.buffer_dropped.load(Ordering::Relaxed);
        let pub_retries = buffer_metrics.publish_retries.load(Ordering::Relaxed);
        let nats_disconnects = buffer_metrics.nats_disconnects.load(Ordering::Relaxed);
        let nats_reconnects = buffer_metrics.nats_reconnects.load(Ordering::Relaxed);

        // BSSID baseline metrics
        let total_bssids = baseline_metrics.total_bssids.load(Ordering::Relaxed);
        let new_bssids = BssidBaseline::reset_interval_counter(baseline_metrics);

        // Compute rolling packets per second
        let now = Instant::now();
        let elapsed = now.duration_since(last_time).as_secs_f32();
        let pps = if elapsed > 0.0 {
            (pkts - last_packets) as f32 / elapsed
        } else {
            0.0
        };
        last_packets = pkts;
        last_time = now;

        let distinct_ch = distinct_channels.load(Ordering::Relaxed);

        let heartbeat = proto::TapHeartbeat {
            tap_id: config.tap.id.clone(),
            tap_name: config.tap.name.clone(),
            timestamp_ns: chrono::Utc::now().timestamp_nanos_opt().unwrap_or(0),
            latitude: config.tap.latitude,
            longitude: config.tap.longitude,
            cpu_percent: sys.global_cpu_usage(),
            memory_percent: {
                let total = sys.total_memory();
                if total > 0 { (sys.used_memory() as f32 / total as f32) * 100.0 } else { 0.0 }
            },
            temperature_celsius: temp,
            wifi_interface: config.capture.interface.clone(),
            version: env!("CARGO_PKG_VERSION").to_string(),
            stats: Some(proto::TapStats {
                packets_captured: pkts,
                packets_filtered: pkts,
                detections_sent: dets,
                bytes_captured: bytes,
                current_channel: chan,
                packets_per_second: pps,
                uptime_seconds: uptime,
                pcap_kernel_received: kern_recv,
                pcap_kernel_dropped: kern_drop,
                // Buffer metrics for reliable delivery
                buffer_size: buf_size,
                buffer_dropped: buf_dropped,
                publish_retries: pub_retries,
                nats_disconnects: nats_disconnects,
                nats_reconnects: nats_reconnects,
                // BSSID baseline
                total_bssids,
                new_bssids_interval: new_bssids,
                // Channel diversity
                distinct_channels: distinct_ch,
                // tshark removed — zero fields for wire compatibility
                tshark_lines: 0,
                tshark_detections: 0,
                tshark_restarts: 0,
                // BLE scanner
                ble_advertisements: ble_stats.advertisements.load(Ordering::Relaxed),
                ble_detections: ble_stats.detections.load(Ordering::Relaxed),
                ble_scanning: ble_stats.scanning.load(Ordering::Relaxed),
                ble_interface: if ble_enabled { ble_adapter.to_string() } else { String::new() },
            }),
            ..Default::default()
        };

        if let Err(e) = nats.publish_heartbeat(&heartbeat).await {
            warn!(error = %e, "Failed to send heartbeat");
        }
    }
}

/// Read CPU temperature from thermal zone
fn read_cpu_temp() -> Option<f32> {
    std::fs::read_to_string("/sys/class/thermal/thermal_zone0/temp")
        .ok()
        .and_then(|s| s.trim().parse::<f32>().ok())
        .map(|t| t / 1000.0)
}

/// Map ASTM F3411 ua_type to a human-readable string
fn ua_type_to_string(ua_type: u8) -> &'static str {
    match ua_type {
        0 => "None",
        1 => "Aeroplane",
        2 => "Helicopter/Multirotor",
        3 => "Gyroplane",
        4 => "Hybrid Lift (VTOL)",
        5 => "Ornithopter",
        6 => "Glider",
        7 => "Kite",
        8 => "Free Balloon",
        9 => "Captive Balloon",
        10 => "Airship",
        11 => "Free Fall/Parachute",
        12 => "Rocket",
        13 => "Ground Obstacle",
        14 => "Other",
        _ => "Unknown",
    }
}

/// Map ASTM F3411 ua_type to proto UAVCategory enum
fn ua_type_to_category(ua_type: u8) -> proto::UavCategory {
    match ua_type {
        0 => proto::UavCategory::UavCatUndeclared,
        1 => proto::UavCategory::UavCatAeroplane,
        2 => proto::UavCategory::UavCatHelicopter,
        3 => proto::UavCategory::UavCatGyroplane,
        4 => proto::UavCategory::UavCatHybridLift,
        5 => proto::UavCategory::UavCatOrnithopter,
        6 => proto::UavCategory::UavCatGlider,
        7 => proto::UavCategory::UavCatKite,
        8 => proto::UavCategory::UavCatFreeBalloon,
        9 => proto::UavCategory::UavCatCaptiveBalloon,
        10 => proto::UavCategory::UavCatAirship,
        11 => proto::UavCategory::UavCatFreeFall,
        12 => proto::UavCategory::UavCatRocket,
        13 => proto::UavCategory::UavCatGroundObstacle,
        14 => proto::UavCategory::UavCatOther,
        _ => proto::UavCategory::UavCatUnknown,
    }
}
