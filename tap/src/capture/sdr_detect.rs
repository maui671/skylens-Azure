//! Signal detection logic for SDR energy detection.
//!
//! Operates on Power Spectral Density (PSD) output from FFT processing.
//! Detects energy above a configurable threshold, groups adjacent bins
//! into signal clusters, and classifies modulation type using simple
//! spectral shape heuristics.
//!
//! This is Phase 1: energy detection only. No protocol decoding.

use crate::proto;
use std::time::{SystemTime, UNIX_EPOCH};

/// A detected RF signal cluster from PSD analysis.
#[derive(Debug, Clone)]
pub struct SignalCluster {
    /// Center frequency in MHz
    pub center_freq_mhz: f64,
    /// Estimated bandwidth in MHz
    pub bandwidth_mhz: f64,
    /// Peak power in dBm (calibrated from PSD)
    pub peak_power_dbm: f64,
    /// Mean power across the cluster in dBm
    pub mean_power_dbm: f64,
    /// Number of FFT bins in this cluster
    pub bin_count: usize,
    /// Estimated noise floor in dBm at time of detection
    pub noise_floor_dbm: f64,
    /// SNR in dB (peak - noise floor)
    pub snr_db: f64,
    /// Classified modulation type
    pub modulation: Modulation,
    /// Classified protocol guess
    pub protocol: RfProtocol,
    /// Band label from config (e.g., "5.8ghz_fpv_high")
    pub band_label: String,
}

/// Simple modulation classification from spectral shape.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Modulation {
    /// Flat-topped wide spectrum (digital video, OcuSync, HDZero)
    Ofdm,
    /// Asymmetric humped spectrum (analog FPV video)
    Fm,
    /// Narrow chirp-like signature (ELRS, Crossfire, LoRa)
    Lora,
    /// Frequency-hopping spread spectrum (detected as recurring narrow bursts)
    Fhss,
    /// Cannot classify
    Unknown,
}

impl Modulation {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Ofdm => "ofdm",
            Self::Fm => "fm",
            Self::Lora => "lora",
            Self::Fhss => "fhss",
            Self::Unknown => "unknown",
        }
    }
}

/// RF protocol classification based on frequency + modulation + bandwidth.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RfProtocol {
    /// DJI OcuSync / O3 / O4 video link
    DjiOcusync,
    /// DJI DroneID burst (2.4 GHz OFDM, 10 MHz BW)
    DjiDroneId,
    /// Analog 5.8 GHz FPV video (FM)
    AnalogFpv,
    /// Digital FPV video (HDZero, Walksnail, DJI FPV)
    DigitalFpv,
    /// ExpressLRS control link
    Elrs,
    /// TBS Crossfire control link
    Crossfire,
    /// Generic control link (FHSS pattern)
    ControlLink,
    /// Cannot classify protocol
    Unknown,
}

impl RfProtocol {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::DjiOcusync => "dji_ocusync",
            Self::DjiDroneId => "dji_droneid",
            Self::AnalogFpv => "analog_fpv",
            Self::DigitalFpv => "digital_fpv",
            Self::Elrs => "elrs",
            Self::Crossfire => "crossfire",
            Self::ControlLink => "control_link",
            Self::Unknown => "unknown",
        }
    }

    /// Map to DetectionSource enum
    pub fn detection_source(&self) -> proto::DetectionSource {
        match self {
            Self::DjiOcusync => proto::DetectionSource::SourceRfOcusync,
            Self::DjiDroneId => proto::DetectionSource::SourceRfDjiDroneid,
            Self::AnalogFpv => proto::DetectionSource::SourceRfFpvAnalog,
            Self::DigitalFpv => proto::DetectionSource::SourceRfFpvDigital,
            Self::Elrs | Self::Crossfire | Self::ControlLink => {
                proto::DetectionSource::SourceRfControlLink
            }
            Self::Unknown => proto::DetectionSource::SourceRfEnergy,
        }
    }
}

/// PSD bin with frequency and power.
#[derive(Debug, Clone, Copy)]
struct PsdBin {
    freq_mhz: f64,
    power_dbm: f64,
}

/// Compute noise floor from PSD using the median (robust to signals).
///
/// Median is preferred over mean because strong signals skew the mean
/// upward. The median stays at the true noise floor even when 20-30%
/// of bins contain signals.
pub fn compute_noise_floor(psd_dbm: &[f64]) -> f64 {
    if psd_dbm.is_empty() {
        return -120.0; // reasonable default for no data
    }
    let mut sorted = psd_dbm.to_vec();
    sorted.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    sorted[sorted.len() / 2]
}

/// Detect signals above threshold in PSD data.
///
/// Groups adjacent above-threshold bins into signal clusters and
/// computes center frequency, bandwidth, peak power, and modulation.
///
/// # Arguments
/// * `psd_dbm` - Power spectral density in dBm, one value per FFT bin
/// * `center_freq_mhz` - SDR center frequency for this capture
/// * `sample_rate_hz` - SDR sample rate in Hz
/// * `threshold_db` - Detection threshold in dB above noise floor
/// * `band_label` - Label for the band being scanned
/// * `min_cluster_bins` - Minimum adjacent bins to count as a signal (default: 3)
pub fn detect_signals(
    psd_dbm: &[f64],
    center_freq_mhz: f64,
    sample_rate_hz: f64,
    threshold_db: f64,
    band_label: &str,
    min_cluster_bins: usize,
) -> Vec<SignalCluster> {
    if psd_dbm.is_empty() {
        return Vec::new();
    }

    let n = psd_dbm.len();
    let noise_floor = compute_noise_floor(psd_dbm);
    let threshold = noise_floor + threshold_db;
    let bin_width_mhz = (sample_rate_hz / n as f64) / 1_000_000.0;

    // Build PSD bins with frequency mapping.
    // FFT output is ordered: [DC, +1, +2, ..., +N/2-1, -N/2, ..., -2, -1]
    // Map to actual frequencies centered on center_freq_mhz.
    let bins: Vec<PsdBin> = (0..n)
        .map(|i| {
            let freq_offset = if i < n / 2 {
                i as f64 * bin_width_mhz
            } else {
                (i as f64 - n as f64) * bin_width_mhz
            };
            PsdBin {
                freq_mhz: center_freq_mhz + freq_offset,
                power_dbm: psd_dbm[i],
            }
        })
        .collect();

    // Sort bins by frequency for contiguous cluster detection
    let mut sorted_bins = bins.clone();
    sorted_bins.sort_by(|a, b| {
        a.freq_mhz
            .partial_cmp(&b.freq_mhz)
            .unwrap_or(std::cmp::Ordering::Equal)
    });

    // Find contiguous groups of bins above threshold
    let mut clusters: Vec<SignalCluster> = Vec::new();
    let mut cluster_start: Option<usize> = None;

    for (i, bin) in sorted_bins.iter().enumerate() {
        if bin.power_dbm > threshold {
            if cluster_start.is_none() {
                cluster_start = Some(i);
            }
        } else if let Some(start) = cluster_start {
            // End of cluster
            if i - start >= min_cluster_bins {
                if let Some(c) = build_cluster(
                    &sorted_bins[start..i],
                    noise_floor,
                    band_label,
                    center_freq_mhz,
                ) {
                    clusters.push(c);
                }
            }
            cluster_start = None;
        }
    }

    // Handle cluster that extends to end of array
    if let Some(start) = cluster_start {
        if sorted_bins.len() - start >= min_cluster_bins {
            if let Some(c) = build_cluster(
                &sorted_bins[start..],
                noise_floor,
                band_label,
                center_freq_mhz,
            ) {
                clusters.push(c);
            }
        }
    }

    clusters
}

/// Build a SignalCluster from a group of contiguous PSD bins.
fn build_cluster(
    bins: &[PsdBin],
    noise_floor: f64,
    band_label: &str,
    tune_center_mhz: f64,
) -> Option<SignalCluster> {
    if bins.is_empty() {
        return None;
    }

    let first_freq = bins.first().unwrap().freq_mhz;
    let last_freq = bins.last().unwrap().freq_mhz;
    let bandwidth = last_freq - first_freq;
    let center_freq = (first_freq + last_freq) / 2.0;

    let peak_power = bins
        .iter()
        .map(|b| b.power_dbm)
        .fold(f64::NEG_INFINITY, f64::max);

    let mean_power = {
        // Average in linear power domain, convert back to dB
        let sum_linear: f64 = bins.iter().map(|b| 10.0_f64.powf(b.power_dbm / 10.0)).sum();
        10.0 * (sum_linear / bins.len() as f64).log10()
    };

    let snr = peak_power - noise_floor;
    let modulation = classify_modulation(bins, peak_power, mean_power, bandwidth);
    let protocol = classify_protocol(center_freq, bandwidth, &modulation, tune_center_mhz);

    Some(SignalCluster {
        center_freq_mhz: center_freq,
        bandwidth_mhz: bandwidth,
        peak_power_dbm: peak_power,
        mean_power_dbm: mean_power,
        bin_count: bins.len(),
        noise_floor_dbm: noise_floor,
        snr_db: snr,
        modulation,
        protocol,
        band_label: band_label.to_string(),
    })
}

/// Classify modulation type from spectral shape.
///
/// Uses simple heuristics based on:
/// - Flatness ratio: ratio of mean power to peak power (in linear domain)
/// - Spectral asymmetry: difference between left and right halves
/// - Bandwidth: narrow vs wide signals
fn classify_modulation(
    bins: &[PsdBin],
    peak_power: f64,
    mean_power: f64,
    bandwidth_mhz: f64,
) -> Modulation {
    if bins.len() < 3 {
        return Modulation::Unknown;
    }

    // Flatness: ratio of mean to peak in linear domain.
    // OFDM has flat top, so mean/peak is close to 1.0 (> 0.7).
    // FM has a hump, so mean/peak is lower (0.3 - 0.7).
    let peak_linear = 10.0_f64.powf(peak_power / 10.0);
    let mean_linear = 10.0_f64.powf(mean_power / 10.0);
    let flatness = if peak_linear > 0.0 {
        mean_linear / peak_linear
    } else {
        0.0
    };

    // Narrow signal (< 0.5 MHz) -> likely LoRa/chirp or control link
    if bandwidth_mhz < 0.5 {
        return Modulation::Lora;
    }

    // Very narrow (< 1 MHz) with low flatness -> FHSS burst
    if bandwidth_mhz < 1.0 && flatness < 0.3 {
        return Modulation::Fhss;
    }

    // Spectral asymmetry: compare left and right halves of the cluster.
    // FM (analog FPV) has asymmetric spectrum due to FM deviation.
    let mid = bins.len() / 2;
    let left_mean: f64 = bins[..mid].iter().map(|b| b.power_dbm).sum::<f64>() / mid as f64;
    let right_mean: f64 = bins[mid..].iter().map(|b| b.power_dbm).sum::<f64>()
        / (bins.len() - mid) as f64;
    let asymmetry = (left_mean - right_mean).abs();

    // Wide + flat -> OFDM (digital video)
    if flatness > 0.6 && bandwidth_mhz > 5.0 {
        return Modulation::Ofdm;
    }

    // Wide + asymmetric + not flat -> FM (analog FPV)
    if asymmetry > 3.0 && flatness < 0.5 && bandwidth_mhz > 5.0 {
        return Modulation::Fm;
    }

    // Medium bandwidth + somewhat flat -> OFDM
    if flatness > 0.5 && bandwidth_mhz > 2.0 {
        return Modulation::Ofdm;
    }

    // Medium bandwidth + asymmetric -> FM
    if asymmetry > 2.0 && bandwidth_mhz > 2.0 {
        return Modulation::Fm;
    }

    Modulation::Unknown
}

/// Classify protocol from center frequency, bandwidth, and modulation.
///
/// Uses known frequency allocations for drone protocols:
/// - 2400-2483 MHz: DJI DroneID (10 MHz OFDM), ELRS, Crossfire
/// - 868-928 MHz: ELRS (900 MHz), Crossfire
/// - 5650-5925 MHz: Analog FPV, DJI OcuSync, HDZero, Walksnail
fn classify_protocol(
    center_freq_mhz: f64,
    bandwidth_mhz: f64,
    modulation: &Modulation,
    _tune_center_mhz: f64,
) -> RfProtocol {
    // 2.4 GHz band (2400-2500 MHz)
    if (2380.0..2500.0).contains(&center_freq_mhz) {
        return match modulation {
            // DJI DroneID: ~10 MHz OFDM bursts at specific frequencies
            // (2399.5, 2414.5, 2429.5, 2444.5, 2459.5 MHz)
            Modulation::Ofdm if bandwidth_mhz > 5.0 && bandwidth_mhz < 15.0 => {
                RfProtocol::DjiDroneId
            }
            // Wide OFDM on 2.4 GHz -> likely OcuSync 2.0 video link
            Modulation::Ofdm if bandwidth_mhz >= 15.0 => RfProtocol::DjiOcusync,
            // Narrow/chirp on 2.4 GHz -> ELRS or Crossfire
            Modulation::Lora => RfProtocol::Elrs,
            Modulation::Fhss => RfProtocol::ControlLink,
            _ => RfProtocol::Unknown,
        };
    }

    // 900 MHz band (860-930 MHz)
    if (860.0..930.0).contains(&center_freq_mhz) {
        return match modulation {
            Modulation::Lora => RfProtocol::Elrs,
            Modulation::Fhss => RfProtocol::Crossfire,
            _ => RfProtocol::ControlLink,
        };
    }

    // 5.8 GHz band (5650-5950 MHz)
    if (5650.0..5950.0).contains(&center_freq_mhz) {
        return match modulation {
            // Analog FPV: ~15-20 MHz FM
            Modulation::Fm => RfProtocol::AnalogFpv,
            // Digital FPV: 10-40 MHz OFDM (DJI FPV, HDZero, Walksnail)
            Modulation::Ofdm if bandwidth_mhz > 15.0 => RfProtocol::DjiOcusync,
            Modulation::Ofdm => RfProtocol::DigitalFpv,
            _ => RfProtocol::Unknown,
        };
    }

    RfProtocol::Unknown
}

/// Generate a synthetic ID for multi-TAP correlation.
///
/// Format: `rf:<center_freq_khz>:<bandwidth_khz>`
/// Two TAPs seeing the same RF signal at the same frequency/bandwidth
/// will generate the same synthetic ID, enabling the node to correlate them.
///
/// Center frequency is quantized to 500 kHz steps and bandwidth to 1 MHz
/// steps to handle slight measurement variations between TAPs.
pub fn generate_synthetic_id(center_freq_mhz: f64, bandwidth_mhz: f64) -> String {
    // Quantize: center to nearest 500 kHz, bandwidth to nearest 1 MHz
    let center_khz = ((center_freq_mhz * 1000.0 / 500.0).round() * 500.0) as i64;
    let bw_khz = ((bandwidth_mhz * 1000.0 / 1000.0).round() * 1000.0) as i64;
    format!("rf:{}:{}", center_khz, bw_khz)
}

/// Build a protobuf Detection from a signal cluster.
pub fn cluster_to_detection(
    cluster: &SignalCluster,
    tap_id: &str,
) -> proto::Detection {
    let synthetic_id = generate_synthetic_id(cluster.center_freq_mhz, cluster.bandwidth_mhz);
    let source = cluster.protocol.detection_source();
    let now_ns = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos() as i64;

    proto::Detection {
        tap_id: tap_id.to_string(),
        timestamp_ns: now_ns,
        identifier: synthetic_id.clone(),
        rssi: cluster.peak_power_dbm as i32,
        frequency_mhz: cluster.center_freq_mhz as i32,
        source: source as i32,
        confidence: snr_to_confidence(cluster.snr_db),
        rf_center_freq_mhz: cluster.center_freq_mhz,
        rf_bandwidth_mhz: cluster.bandwidth_mhz,
        rf_modulation: cluster.modulation.as_str().to_string(),
        rf_power_dbm: cluster.peak_power_dbm as i32,
        rf_protocol: cluster.protocol.as_str().to_string(),
        rf_synthetic_id: synthetic_id,
        // Generate a stable session ID from frequency + bandwidth
        session_id: uuid::Uuid::new_v5(
            &uuid::Uuid::NAMESPACE_OID,
            format!(
                "sdr:{}:{}",
                cluster.center_freq_mhz as i64,
                cluster.bandwidth_mhz as i64
            )
            .as_bytes(),
        )
        .to_string(),
        ..Default::default()
    }
}

/// Convert SNR to a confidence value (0.0 - 1.0).
/// Higher SNR = higher confidence in the detection.
fn snr_to_confidence(snr_db: f64) -> f32 {
    // Sigmoid-like mapping:
    //   10 dB SNR -> ~0.50 confidence
    //   20 dB SNR -> ~0.80 confidence
    //   30 dB SNR -> ~0.95 confidence
    let c = 1.0 / (1.0 + (-0.15 * (snr_db - 10.0)).exp());
    (c as f32).clamp(0.1, 0.99)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_noise_floor_computation() {
        // 100 bins at -100 dBm with 5 bins at -60 dBm (signals)
        let mut psd = vec![-100.0; 100];
        psd[40] = -60.0;
        psd[41] = -60.0;
        psd[42] = -60.0;
        psd[43] = -60.0;
        psd[44] = -60.0;
        let nf = compute_noise_floor(&psd);
        assert!((nf - (-100.0)).abs() < 1.0, "noise floor should be ~-100 dBm, got {}", nf);
    }

    #[test]
    fn test_detect_signals_empty() {
        let signals = detect_signals(&[], 5800.0, 20_000_000.0, 10.0, "test", 3);
        assert!(signals.is_empty());
    }

    #[test]
    fn test_detect_signal_above_threshold() {
        // Create PSD with noise at -100 dBm and a signal at -70 dBm (30 dB above noise)
        let n = 1024;
        let mut psd = vec![-100.0; n];
        // Place a 10-bin wide signal in the middle
        for i in 500..510 {
            psd[i] = -70.0;
        }
        let signals = detect_signals(&psd, 5800.0, 20_000_000.0, 10.0, "5.8ghz_test", 3);
        assert!(!signals.is_empty(), "should detect signal above threshold");
        let s = &signals[0];
        assert!(s.peak_power_dbm > -75.0, "peak should be ~-70 dBm");
        assert!(s.snr_db > 25.0, "SNR should be ~30 dB");
    }

    #[test]
    fn test_synthetic_id_quantization() {
        // Two slightly different measurements should produce the same ID
        // Quantization: center to nearest 500 kHz, bandwidth to nearest 1 MHz
        // 5800.1 MHz * 1000 = 5800100 kHz / 500 = 11600.2 -> round -> 11600 * 500 = 5800000
        // 5800.2 MHz * 1000 = 5800200 kHz / 500 = 11600.4 -> round -> 11600 * 500 = 5800000
        let id1 = generate_synthetic_id(5800.1, 20.3);
        let id2 = generate_synthetic_id(5800.2, 19.7);
        assert_eq!(id1, id2, "quantized IDs should match for similar signals");
    }

    #[test]
    fn test_synthetic_id_different_signals() {
        let id1 = generate_synthetic_id(5800.0, 20.0);
        let id2 = generate_synthetic_id(2414.0, 10.0);
        assert_ne!(id1, id2, "different frequencies should produce different IDs");
    }

    #[test]
    fn test_snr_to_confidence() {
        let c10 = snr_to_confidence(10.0);
        let c20 = snr_to_confidence(20.0);
        let c30 = snr_to_confidence(30.0);
        assert!(c10 > 0.4 && c10 < 0.6, "10 dB SNR should be ~0.5, got {}", c10);
        assert!(c20 > 0.7, "20 dB SNR should be > 0.7, got {}", c20);
        assert!(c30 > 0.9, "30 dB SNR should be > 0.9, got {}", c30);
    }

    #[test]
    fn test_protocol_classification_24ghz() {
        // 2.4 GHz OFDM ~10 MHz -> DJI DroneID
        let p = classify_protocol(2414.5, 10.0, &Modulation::Ofdm, 2414.5);
        assert_eq!(p, RfProtocol::DjiDroneId);

        // 2.4 GHz LoRa -> ELRS
        let p = classify_protocol(2440.0, 0.3, &Modulation::Lora, 2440.0);
        assert_eq!(p, RfProtocol::Elrs);
    }

    #[test]
    fn test_protocol_classification_58ghz() {
        // 5.8 GHz FM -> Analog FPV
        let p = classify_protocol(5800.0, 18.0, &Modulation::Fm, 5800.0);
        assert_eq!(p, RfProtocol::AnalogFpv);

        // 5.8 GHz wide OFDM -> DJI OcuSync
        let p = classify_protocol(5800.0, 25.0, &Modulation::Ofdm, 5800.0);
        assert_eq!(p, RfProtocol::DjiOcusync);
    }

    #[test]
    fn test_protocol_classification_900mhz() {
        let p = classify_protocol(915.0, 0.4, &Modulation::Lora, 915.0);
        assert_eq!(p, RfProtocol::Elrs);

        let p = classify_protocol(868.0, 0.5, &Modulation::Fhss, 868.0);
        assert_eq!(p, RfProtocol::Crossfire);
    }
}
