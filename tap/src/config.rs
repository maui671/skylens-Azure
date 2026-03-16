use anyhow::{Context, Result};
use serde::Deserialize;
use std::path::Path;

#[derive(Debug, Deserialize, Clone)]
pub struct Config {
    pub tap: TapConfig,
    pub capture: CaptureConfig,
    pub nats: NatsConfig,
    #[serde(default)]
    pub logging: LoggingConfig,
    #[serde(default)]
    pub ble: BleConfig,
}

#[derive(Debug, Deserialize, Clone)]
pub struct TapConfig {
    pub id: String,
    pub name: String,
    pub latitude: f64,
    pub longitude: f64,
}

#[derive(Debug, Deserialize, Clone)]
pub struct CaptureConfig {
    pub interface: String,
    #[serde(default = "default_channels")]
    pub channels: Vec<i32>,
    #[serde(default = "default_hop_interval")]
    pub hop_interval_ms: u64,
    #[serde(default = "default_dedup_interval")]
    pub dedup_interval_ms: u64,
    /// Passive mode: skip monitor mode setup and channel hopping.
    /// Use when another program already controls the interface.
    #[serde(default)]
    pub passive: bool,
    /// MAC addresses to ignore (suppress known false positive sources).
    /// Format: "AA:BB:CC:DD:EE:FF" (case-insensitive, colon-separated)
    #[serde(default)]
    pub mac_denylist: Vec<String>,
    /// Force iw subprocess for channel switching instead of nl80211 netlink.
    /// Enable this for drivers where nl80211 silently fails for some channels
    /// (e.g. RTL8812AU 88XXau driver fails on UNII-3 channels 149-165).
    #[serde(default)]
    pub force_iw: bool,
    /// Enable tshark co-process for deep protocol parsing (OpenDroneID, DJI).
    /// Requires tshark installed. Falls back gracefully if not found.
    #[serde(default = "default_tshark")]
    pub tshark: bool,
    /// Enable widest-first channel width hopping on 5 GHz (80 MHz → 40 → 20 fallback).
    /// Reduces 5 GHz from 24 hops to ~6 hops per cycle. Disable if driver doesn't support wider widths.
    #[serde(default = "default_width_hopping")]
    pub channel_width_hopping: bool,
    /// Kernel ring buffer size in MB for pcap capture (default: 16).
    /// Increase on TAPs with plenty of RAM to handle burst traffic without kernel drops.
    #[serde(default = "default_pcap_buffer_mb")]
    pub pcap_buffer_mb: u32,
}

#[derive(Debug, Deserialize, Clone)]
pub struct NatsConfig {
    pub url: String,
    /// Additional NATS URLs to mirror detections and heartbeats to.
    /// Mirror publishes are fire-and-forget (no buffering or retries).
    #[serde(default)]
    pub mirror_urls: Vec<String>,
    #[serde(default = "default_reconnect_interval")]
    pub reconnect_interval_ms: u64,
    #[serde(default)]
    pub buffer: BufferConfig,
}

#[derive(Debug, Deserialize, Clone)]
pub struct BufferConfig {
    /// Maximum number of detections to buffer when offline (default: 10000)
    #[serde(default = "default_buffer_max_size")]
    pub max_size: usize,
    /// Maximum retries before dropping a detection (default: 100)
    #[serde(default = "default_buffer_max_retries")]
    pub max_retries: u64,
    /// Initial retry delay in milliseconds (default: 1000)
    #[serde(default = "default_buffer_initial_retry_ms")]
    pub initial_retry_delay_ms: u64,
    /// Maximum retry delay in milliseconds (default: 30000)
    #[serde(default = "default_buffer_max_retry_ms")]
    pub max_retry_delay_ms: u64,
    /// Buffer fullness warning threshold 0.0-1.0 (default: 0.8)
    #[serde(default = "default_buffer_warning_threshold")]
    pub warning_threshold: f32,
}

impl Default for BufferConfig {
    fn default() -> Self {
        Self {
            max_size: default_buffer_max_size(),
            max_retries: default_buffer_max_retries(),
            initial_retry_delay_ms: default_buffer_initial_retry_ms(),
            max_retry_delay_ms: default_buffer_max_retry_ms(),
            warning_threshold: default_buffer_warning_threshold(),
        }
    }
}

#[derive(Debug, Deserialize, Clone)]
pub struct LoggingConfig {
    #[serde(default = "default_log_level")]
    pub level: String,
}

impl Default for LoggingConfig {
    fn default() -> Self {
        Self {
            level: default_log_level(),
        }
    }
}

#[derive(Debug, Deserialize, Clone)]
pub struct BleConfig {
    /// Enable BLE scanning for OpenDroneID (ASTM F3411) advertisements.
    /// Requires bluetoothd running and a BLE adapter (e.g., Pi 5 built-in hci0).
    #[serde(default)]
    pub enabled: bool,
    /// BLE adapter name (default: "hci0")
    #[serde(default = "default_ble_adapter")]
    pub adapter: String,
}

impl Default for BleConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            adapter: default_ble_adapter(),
        }
    }
}

fn default_ble_adapter() -> String {
    "hci0".to_string()
}

fn default_channels() -> Vec<i32> {
    vec![1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 36, 40, 44, 48, 149, 153, 157, 161, 165]
}

fn default_hop_interval() -> u64 {
    250
}

fn default_dedup_interval() -> u64 {
    500 // 500ms: 2x position updates vs 1s, NAN cycle completes in ~200-600ms
}

fn default_reconnect_interval() -> u64 {
    1000
}

fn default_log_level() -> String {
    "info".to_string()
}

fn default_buffer_max_size() -> usize {
    10_000
}

fn default_buffer_max_retries() -> u64 {
    100
}

fn default_buffer_initial_retry_ms() -> u64 {
    1_000
}

fn default_buffer_max_retry_ms() -> u64 {
    30_000
}

fn default_buffer_warning_threshold() -> f32 {
    0.8
}

fn default_tshark() -> bool {
    false
}

fn default_width_hopping() -> bool {
    true
}

fn default_pcap_buffer_mb() -> u32 {
    32
}

impl Config {
    pub fn load(path: &Path) -> Result<Self> {
        let content = std::fs::read_to_string(path)
            .with_context(|| format!("Failed to read config file: {}", path.display()))?;
        let config: Config =
            toml::from_str(&content).with_context(|| "Failed to parse config TOML")?;
        Ok(config)
    }
}
