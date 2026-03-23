//! SDR capture and band scanning module.
//!
//! Third capture source alongside WiFi (pcap) and BLE. Uses SoapySDR
//! to interface with any supported SDR hardware (HackRF, LimeSDR,
//! PlutoSDR, RTL-SDR). Captures IQ samples, runs FFT to compute
//! Power Spectral Density, and detects energy above a configurable
//! threshold.
//!
//! Phase 1: Energy detection with spectral classification.
//! No protocol decoding (DJI DroneID decoding is Phase 3).
//!
//! The SDR uses blocking I/O (SoapySDR read_stream is synchronous),
//! so capture runs in a dedicated std::thread, bridged to async via
//! an mpsc channel for detections.

use crate::capture::sdr_detect::{self, SignalCluster};
use crate::config::SdrConfig;
use crate::proto;

use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use num_complex::Complex;
use rustfft::FftPlanner;
use soapysdr::{Device, Direction, RxStream};
use tokio::sync::mpsc;
use tracing::{debug, error, info, warn};

/// RX direction constant for SoapySDR
const RX: Direction = Direction::Rx;

/// PLL settle time after retuning (microseconds).
/// HackRF settles in ~800us, RTL-SDR in ~5ms. Use 2ms as safe default.
const PLL_SETTLE_US: u64 = 2000;

/// Minimum time between detections of the same synthetic ID (dedup).
const DEDUP_WINDOW_SECS: u64 = 2;

/// Maximum number of dedup entries before cleanup.
const DEDUP_MAX_ENTRIES: usize = 1000;

/// Stats exposed for heartbeat reporting.
pub struct SdrStats {
    pub detections: AtomicU64,
    pub scans: AtomicU64,
    pub scanning: AtomicBool,
    pub current_freq_mhz: AtomicI32,
}

impl Default for SdrStats {
    fn default() -> Self {
        Self {
            detections: AtomicU64::new(0),
            scans: AtomicU64::new(0),
            scanning: AtomicBool::new(false),
            current_freq_mhz: AtomicI32::new(0),
        }
    }
}

/// Main SDR scan loop. Wraps the inner scanner with restart-on-error
/// and exponential backoff, matching the BLE scanner pattern.
pub async fn sdr_scan_loop(
    config: SdrConfig,
    stop: Arc<AtomicBool>,
    det_tx: mpsc::Sender<proto::Detection>,
    tap_id: String,
    stats: Arc<SdrStats>,
) {
    let mut backoff_secs = 1u64;
    const MAX_BACKOFF_SECS: u64 = 30;

    loop {
        if !stop.load(Ordering::SeqCst) {
            break;
        }

        let start = Instant::now();
        stats.scanning.store(true, Ordering::Relaxed);

        // Run the SDR scanner in a blocking thread since SoapySDR is synchronous
        let cfg = config.clone();
        let s = stop.clone();
        let tx = det_tx.clone();
        let tid = tap_id.clone();
        let st = stats.clone();

        let result = tokio::task::spawn_blocking(move || {
            run_sdr_scanner(&cfg, &s, &tx, &tid, &st)
        })
        .await;

        stats.scanning.store(false, Ordering::Relaxed);

        match result {
            Ok(Ok(())) => {
                // Clean exit (stop flag cleared)
                info!("SDR scanner stopped cleanly");
                break;
            }
            Ok(Err(e)) => {
                error!(error = %e, backoff_secs, "SDR scanner error, restarting");
            }
            Err(e) => {
                error!(error = %e, backoff_secs, "SDR scanner task panicked, restarting");
            }
        }

        // Reset backoff if scanner ran for a while (transient issue)
        if start.elapsed() > Duration::from_secs(60) {
            backoff_secs = 1;
        }

        // Sleep with stop-flag checks
        let deadline = Instant::now() + Duration::from_secs(backoff_secs);
        while Instant::now() < deadline {
            if !stop.load(Ordering::SeqCst) {
                return;
            }
            std::thread::sleep(Duration::from_millis(500));
        }

        backoff_secs = (backoff_secs * 2).min(MAX_BACKOFF_SECS);
    }
}

/// Inner SDR scanner. Opens the device, configures it, and runs
/// the band scanning loop.
fn run_sdr_scanner(
    config: &SdrConfig,
    stop: &Arc<AtomicBool>,
    det_tx: &mpsc::Sender<proto::Detection>,
    tap_id: &str,
    stats: &Arc<SdrStats>,
) -> anyhow::Result<()> {
    // Open SoapySDR device
    let dev = Device::new(&*config.device)
        .map_err(|e| anyhow::anyhow!("Failed to open SoapySDR device '{}': {}", config.device, e))?;

    let info = dev.hardware_info()
        .map_err(|e| anyhow::anyhow!("Failed to get hardware info: {}", e))?;
    info!(
        device = %config.device,
        info = ?info,
        "SDR device opened"
    );

    // Configure sample rate
    let sample_rate = config.sample_rate as f64;
    dev.set_sample_rate(RX, 0, sample_rate)
        .map_err(|e| anyhow::anyhow!("Failed to set sample rate to {}: {}", sample_rate, e))?;

    // Configure gain
    dev.set_gain(RX, 0, config.gain as f64)
        .map_err(|e| anyhow::anyhow!("Failed to set gain to {}: {}", config.gain, e))?;

    // Set bandwidth to match sample rate (maximize captured spectrum)
    let _ = dev.set_bandwidth(RX, 0, sample_rate);

    // Set initial frequency
    if let Some(first_band) = config.bands.first() {
        dev.set_frequency(RX, 0, first_band.center_mhz as f64 * 1_000_000.0, ())
            .map_err(|e| anyhow::anyhow!("Failed to set initial frequency: {}", e))?;
    }

    info!(
        sample_rate = sample_rate,
        gain = config.gain,
        bands = config.bands.len(),
        "SDR configured, starting band scan loop"
    );

    // Setup RX stream
    let mut stream: RxStream<Complex<f32>> = dev.rx_stream(&[0])
        .map_err(|e| anyhow::anyhow!("Failed to create RX stream: {}", e))?;
    stream.activate(None)
        .map_err(|e| anyhow::anyhow!("Failed to activate RX stream: {}", e))?;

    // FFT setup
    let fft_size = config.fft_size.unwrap_or(1024);
    let mut planner = FftPlanner::new();
    let fft = planner.plan_fft_forward(fft_size);

    // IQ sample buffer (one FFT block)
    let mut iq_buf = vec![Complex::new(0.0f32, 0.0f32); fft_size];
    // FFT scratch space
    let mut scratch = vec![Complex::new(0.0f32, 0.0f32); fft.get_inplace_scratch_len()];
    // PSD output buffer (dBm per bin)
    let mut psd = vec![0.0f64; fft_size];

    // Dedup state: synthetic_id -> last detection time
    let mut dedup: std::collections::HashMap<String, Instant> = std::collections::HashMap::new();
    let mut last_dedup_cleanup = Instant::now();

    let threshold_db = config.threshold_db.unwrap_or(10.0);
    let min_bins = config.min_cluster_bins.unwrap_or(3);

    // Windowing function (Hann window for sidelobe suppression)
    let window: Vec<f64> = (0..fft_size)
        .map(|i| {
            0.5 * (1.0 - (2.0 * std::f64::consts::PI * i as f64 / (fft_size - 1) as f64).cos())
        })
        .collect();

    // Main band scanning loop
    loop {
        for band in &config.bands {
            if !stop.load(Ordering::SeqCst) {
                info!("SDR scanner stop flag cleared, shutting down");
                stream.deactivate(None)?;
                return Ok(());
            }

            let center_hz = band.center_mhz as f64 * 1_000_000.0;

            // Retune to band center frequency
            dev.set_frequency(RX, 0, center_hz, ())
                .map_err(|e| anyhow::anyhow!("Failed to tune to {} MHz: {}", band.center_mhz, e))?;

            stats
                .current_freq_mhz
                .store(band.center_mhz as i32, Ordering::Relaxed);

            // Wait for PLL to settle
            std::thread::sleep(Duration::from_micros(PLL_SETTLE_US));

            // Flush stale samples from the buffer (PLL settling produces garbage)
            let _ = stream.read(&mut [&mut iq_buf], 1_000_000);

            // Dwell on this band, capturing and analyzing multiple FFT blocks
            let dwell = Duration::from_millis(band.dwell_ms);
            let dwell_start = Instant::now();
            let mut blocks_in_dwell = 0u32;

            while dwell_start.elapsed() < dwell {
                if !stop.load(Ordering::SeqCst) {
                    stream.deactivate(None)?;
                    return Ok(());
                }

                // Read one block of IQ samples (timeout 100ms)
                let n_read = match stream.read(&mut [&mut iq_buf], 100_000) {
                    Ok(n) => n,
                    Err(e) => {
                        debug!(error = %e, freq_mhz = band.center_mhz, "SDR read error, skipping block");
                        continue;
                    }
                };

                if n_read < fft_size / 2 {
                    // Not enough samples, skip
                    continue;
                }

                // Apply window function and compute FFT
                let mut fft_buf: Vec<Complex<f32>> = iq_buf[..n_read.min(fft_size)]
                    .iter()
                    .enumerate()
                    .map(|(i, &s)| {
                        let w = if i < window.len() { window[i] as f32 } else { 1.0 };
                        Complex::new(s.re * w, s.im * w)
                    })
                    .collect();

                // Zero-pad if we got fewer samples than fft_size
                fft_buf.resize(fft_size, Complex::new(0.0, 0.0));

                fft.process_with_scratch(&mut fft_buf, &mut scratch);

                // Compute PSD: |FFT[k]|^2 / N in dB scale
                // Add gain calibration offset (approximate for HackRF: -40 dBFS to dBm)
                let gain_offset_db = -(config.gain as f64);
                let n_f64 = fft_size as f64;
                for (i, bin) in fft_buf.iter().enumerate() {
                    let mag_sq = (bin.re as f64).powi(2) + (bin.im as f64).powi(2);
                    // PSD in dBFS, then apply rough calibration to dBm
                    // Reference: 0 dBFS = ~+10 dBm for HackRF at 0 gain
                    psd[i] = 10.0 * (mag_sq / n_f64).log10() + gain_offset_db - 30.0;
                    // Clamp to reasonable range
                    if psd[i] < -150.0 {
                        psd[i] = -150.0;
                    }
                }

                // Detect signals
                let signals = sdr_detect::detect_signals(
                    &psd,
                    band.center_mhz as f64,
                    sample_rate,
                    threshold_db,
                    &band.label,
                    min_bins,
                );

                // Process each detected signal
                for signal in &signals {
                    let sid = sdr_detect::generate_synthetic_id(
                        signal.center_freq_mhz,
                        signal.bandwidth_mhz,
                    );

                    // Dedup: skip if we reported this signal recently
                    if let Some(last) = dedup.get(&sid) {
                        if last.elapsed() < Duration::from_secs(DEDUP_WINDOW_SECS) {
                            continue;
                        }
                    }

                    log_detection(signal);

                    let detection = sdr_detect::cluster_to_detection(signal, tap_id);
                    match det_tx.try_send(detection) {
                        Ok(()) => {
                            stats.detections.fetch_add(1, Ordering::Relaxed);
                            dedup.insert(sid, Instant::now());
                        }
                        Err(mpsc::error::TrySendError::Full(_)) => {
                            warn!("SDR: det_tx channel full, dropping detection");
                        }
                        Err(mpsc::error::TrySendError::Closed(_)) => {
                            info!("SDR: det_tx channel closed, shutting down");
                            stream.deactivate(None)?;
                            return Ok(());
                        }
                    }
                }

                blocks_in_dwell += 1;
            }

            debug!(
                freq_mhz = band.center_mhz,
                label = %band.label,
                blocks = blocks_in_dwell,
                "Band scan complete"
            );
        }

        // One full cycle through all bands
        stats.scans.fetch_add(1, Ordering::Relaxed);

        // Periodic dedup cleanup
        if last_dedup_cleanup.elapsed() > Duration::from_secs(30) {
            let cutoff = Duration::from_secs(DEDUP_WINDOW_SECS * 3);
            dedup.retain(|_, ts| ts.elapsed() < cutoff);
            if dedup.len() > DEDUP_MAX_ENTRIES {
                // Emergency flush if too many entries (should never happen)
                dedup.clear();
            }
            last_dedup_cleanup = Instant::now();
        }
    }
}

/// Log a detected signal at info level with key metrics.
fn log_detection(signal: &SignalCluster) {
    info!(
        freq_mhz = format!("{:.1}", signal.center_freq_mhz),
        bw_mhz = format!("{:.1}", signal.bandwidth_mhz),
        peak_dbm = format!("{:.1}", signal.peak_power_dbm),
        snr_db = format!("{:.1}", signal.snr_db),
        modulation = signal.modulation.as_str(),
        protocol = signal.protocol.as_str(),
        band = %signal.band_label,
        bins = signal.bin_count,
        "SDR signal detected"
    );
}
