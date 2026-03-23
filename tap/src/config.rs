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
    #[serde(default)]
    pub sdr: SdrConfig,
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

/// SDR configuration for RF energy detection.
#[derive(Debug, Deserialize, Clone)]
pub struct SdrConfig {
    /// Enable SDR scanning (requires SoapySDR device).
    #[serde(default)]
    pub enabled: bool,
    /// SoapySDR device string (e.g., "hackrf", "driver=rtlsdr", "driver=lime")
    #[serde(default = "default_sdr_device")]
    pub device: String,
    /// Sample rate in Hz (default: 20 MSPS)
    #[serde(default = "default_sdr_sample_rate")]
    pub sample_rate: u64,
    /// RF gain in dB (default: 40)
    #[serde(default = "default_sdr_gain")]
    pub gain: u32,
    /// FFT size in points (default: 1024). Must be power of 2.
    #[serde(default)]
    pub fft_size: Option<usize>,
    /// Detection threshold in dB above noise floor (default: 10.0)
    #[serde(default)]
    pub threshold_db: Option<f64>,
    /// Minimum adjacent FFT bins to count as a signal (default: 3)
    #[serde(default)]
    pub min_cluster_bins: Option<usize>,
    /// Frequency bands to scan
    #[serde(default = "default_sdr_bands")]
    pub bands: Vec<SdrBand>,
}

impl Default for SdrConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            device: default_sdr_device(),
            sample_rate: default_sdr_sample_rate(),
            gain: default_sdr_gain(),
            fft_size: None,
            threshold_db: None,
            min_cluster_bins: None,
            bands: default_sdr_bands(),
        }
    }
}

/// A frequency band for SDR scanning.
#[derive(Debug, Deserialize, Clone)]
pub struct SdrBand {
    /// Center frequency in MHz
    pub center_mhz: u64,
    /// Dwell time on this band in milliseconds
    #[serde(default = "default_sdr_dwell_ms")]
    pub dwell_ms: u64,
    /// Human-readable label for logging and detection metadata
    #[serde(default)]
    pub label: String,
}

fn default_sdr_device() -> String {
    "hackrf".to_string()
}

fn default_sdr_sample_rate() -> u64 {
    20_000_000 // 20 MSPS
}

fn default_sdr_gain() -> u32 {
    40
}

fn default_sdr_dwell_ms() -> u64 {
    500
}

fn default_sdr_bands() -> Vec<SdrBand> {
    vec![
        SdrBand {
            center_mhz: 2414,
            dwell_ms: 2000,
            label: "2.4ghz_droneid".to_string(),
        },
        SdrBand {
            center_mhz: 5800,
            dwell_ms: 500,
            label: "5.8ghz_fpv".to_string(),
        },
    ]
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
