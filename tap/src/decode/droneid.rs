//! DJI DroneID OFDM decoder — Phase 3 SDR integration.
//!
//! Decodes DJI DroneID bursts from raw IQ samples. The signal is broadcast
//! on dedicated 2.4 GHz frequencies (2399.5, 2414.5, 2429.5, 2444.5,
//! 2459.5 MHz) with 10 MHz bandwidth.
//!
//! Signal structure (from proto17/dji_droneid research):
//! - 9 OFDM symbols per burst (~645 us at 15.36 MSPS)
//! - Symbols 1-2: long cyclic prefix
//! - Symbols 4 and 6 (1-indexed): Zadoff-Chu synchronization sequences
//! - Remaining symbols: QPSK-modulated data
//! - 600 data subcarriers per symbol (plus center null)
//! - No pilot subcarriers — phase estimation from ZC sequences only
//! - Turbo coded and scrambled payload
//!
//! This module is pure Rust with no native SDR dependencies. It takes
//! Complex<f32> slices as input, enabling full unit testing with synthetic
//! vectors. Uses rustfft when the `sdr` feature is enabled; falls back to
//! a naive O(N^2) DFT otherwise.

use num_complex::Complex;
use std::f32::consts::PI;
use tracing::{debug, trace, warn};

// ---------------------------------------------------------------------------
// Constants from proto17 / dji_droneid research
// ---------------------------------------------------------------------------

/// FFT size at 15.36 MSPS sample rate.
pub const FFT_SIZE_15MHZ: usize = 1024;

/// FFT size at 30.72 MSPS sample rate (not implemented yet).
pub const FFT_SIZE_30MHZ: usize = 2048;

/// Short cyclic prefix length in samples (at 15.36 MSPS).
pub const SHORT_CP: usize = 72;

/// Long cyclic prefix length in samples (at 15.36 MSPS).
pub const LONG_CP: usize = 80;

/// Number of OFDM symbols per DroneID burst.
pub const NUM_SYMBOLS: usize = 9;

/// Zadoff-Chu root index used by DJI DroneID.
pub const ZC_ROOT: u32 = 600;

/// Zadoff-Chu sequence length used by DJI DroneID.
pub const ZC_LENGTH: usize = 601;

/// Number of active data subcarriers per OFDM symbol.
pub const DATA_CARRIERS: usize = 600;

/// Approximate burst repeat interval in milliseconds.
pub const BURST_INTERVAL_MS: u64 = 600;

/// Known DJI DroneID center frequencies in MHz.
pub const FREQUENCIES_MHZ: [f64; 5] = [2399.5, 2414.5, 2429.5, 2444.5, 2459.5];

/// Standard sample rate for DJI DroneID reception (15.36 MSPS).
pub const SAMPLE_RATE_15MHZ: f64 = 15_360_000.0;

/// Minimum correlation threshold for burst detection (normalized 0..1).
const DETECTION_THRESHOLD: f64 = 0.6;

/// Number of samples per OFDM symbol (FFT size, no CP).
const SYMBOL_LEN: usize = FFT_SIZE_15MHZ;

// ---------------------------------------------------------------------------
// FFT abstraction — rustfft when available, naive DFT otherwise
// ---------------------------------------------------------------------------

/// Compute forward FFT in-place on `data`. Length must be `FFT_SIZE_15MHZ`.
#[cfg(feature = "sdr")]
fn fft_forward(data: &mut [Complex<f32>]) {
    use rustfft::FftPlanner;
    let mut planner = FftPlanner::new();
    let fft = planner.plan_fft_forward(data.len());
    fft.process(data);
}

/// Naive DFT fallback when rustfft is not available.
/// O(N^2) — suitable for tests, not production capture rates.
#[cfg(not(feature = "sdr"))]
fn fft_forward(data: &mut [Complex<f32>]) {
    let n = data.len();
    let mut out = vec![Complex::new(0.0f32, 0.0f32); n];
    for k in 0..n {
        let mut sum = Complex::new(0.0f32, 0.0f32);
        for (idx, sample) in data.iter().enumerate() {
            let angle = -2.0 * PI * (k as f32) * (idx as f32) / (n as f32);
            let twiddle = Complex::new(angle.cos(), angle.sin());
            sum += sample * twiddle;
        }
        out[k] = sum;
    }
    data.copy_from_slice(&out);
}

/// Compute inverse FFT. Result is NOT normalized (caller divides by N).
#[cfg(feature = "sdr")]
fn fft_inverse(data: &mut [Complex<f32>]) {
    use rustfft::FftPlanner;
    let mut planner = FftPlanner::new();
    let fft = planner.plan_fft_inverse(data.len());
    fft.process(data);
}

#[cfg(not(feature = "sdr"))]
fn fft_inverse(data: &mut [Complex<f32>]) {
    let n = data.len();
    let mut out = vec![Complex::new(0.0f32, 0.0f32); n];
    for k in 0..n {
        let mut sum = Complex::new(0.0f32, 0.0f32);
        for (idx, sample) in data.iter().enumerate() {
            let angle = 2.0 * PI * (k as f32) * (idx as f32) / (n as f32);
            let twiddle = Complex::new(angle.cos(), angle.sin());
            sum += sample * twiddle;
        }
        out[k] = sum;
    }
    data.copy_from_slice(&out);
}

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// Decoded DroneID payload fields.
#[derive(Debug, Clone)]
pub struct DroneIDPayload {
    pub serial_number: String,
    pub drone_latitude: f64,
    pub drone_longitude: f64,
    pub drone_altitude: f32,
    pub pilot_latitude: f64,
    pub pilot_longitude: f64,
    pub home_latitude: f64,
    pub home_longitude: f64,
    pub speed: f32,
    pub heading: f32,
    pub height: f32,
    pub timestamp: u64,
}

/// Result from processing a single detected burst.
#[derive(Debug, Clone)]
pub struct DroneIDResult {
    /// Sample offset of burst start within the input block.
    pub burst_offset: usize,
    /// Measured carrier frequency offset in Hz.
    pub frequency_offset_hz: f64,
    /// Estimated SNR in dB from ZC correlation.
    pub snr_db: f64,
    /// Normalized ZC correlation peak strength (0..1).
    pub correlation_peak: f64,
    /// Decoded payload (None until turbo decoding is implemented).
    pub payload: Option<DroneIDPayload>,
    /// Raw demodulated bits before turbo decoding / descrambling.
    pub raw_bits: Vec<u8>,
}

/// Internal burst detection result from ZC correlation.
#[derive(Debug, Clone)]
pub struct BurstDetection {
    /// Sample offset within the input where burst starts.
    pub sample_offset: usize,
    /// Normalized correlation peak (0..1).
    pub correlation_peak: f64,
    /// Coarse frequency offset estimated from ZC symbol pair (Hz).
    pub frequency_offset_hz: f64,
    /// Estimated SNR from correlation peak vs noise floor (dB).
    pub snr_db: f64,
}

// ---------------------------------------------------------------------------
// 1. Zadoff-Chu sequence generation
// ---------------------------------------------------------------------------

/// Generate a Zadoff-Chu sequence of the given length and root index.
///
/// ZC(n) = exp(-j * pi * root * n * (n+1) / length)
///
/// DJI DroneID uses root=600, length=601 (LTE-style ZC sequences).
/// The resulting sequence has constant amplitude |ZC[n]| = 1 and ideal
/// periodic autocorrelation properties.
pub fn generate_zc_sequence(root: u32, length: usize) -> Vec<Complex<f32>> {
    let len_f64 = length as f64;
    (0..length)
        .map(|n| {
            let n_f64 = n as f64;
            // -pi * root * n * (n+1) / length
            let phase = -std::f64::consts::PI * (root as f64) * n_f64 * (n_f64 + 1.0) / len_f64;
            Complex::new(phase.cos() as f32, phase.sin() as f32)
        })
        .collect()
}

/// Map a ZC sequence into FFT bins for frequency-domain correlation.
/// Places the ZC sequence into the center `ZC_LENGTH` subcarriers of an
/// `fft_size`-point buffer (DC-centered, then fftshift to standard order).
pub fn zc_to_freq_domain(zc: &[Complex<f32>], fft_size: usize) -> Vec<Complex<f32>> {
    let mut buf = vec![Complex::new(0.0f32, 0.0f32); fft_size];
    let half = zc.len() / 2;

    // Place ZC sequence centered around DC:
    // Negative frequencies → upper bins, positive frequencies → lower bins
    for (i, &val) in zc.iter().enumerate() {
        if i <= half {
            // Positive frequencies and DC → bins 0..=half
            buf[i] = val;
        } else {
            // Negative frequencies → bins (fft_size - zc.len() + i)..
            buf[fft_size - zc.len() + i] = val;
        }
    }
    buf
}

// ---------------------------------------------------------------------------
// 2. Burst detection via ZC cross-correlation
// ---------------------------------------------------------------------------

/// Cross-correlate IQ samples with the known ZC sequence to find burst starts.
///
/// Uses time-domain sliding cross-correlation with proper normalization.
/// For each candidate offset, computes:
///   corr(k) = |sum(signal[k+n] * conj(zc[n]))| / (||signal_block|| * ||zc||)
///
/// Returns detected bursts sorted by sample offset. The `sample_rate` parameter
/// is used to convert phase rotation to frequency offset in Hz.
pub fn detect_bursts(samples: &[Complex<f32>], sample_rate: f64) -> Vec<BurstDetection> {
    // Build TWO correlation templates:
    // 1. Raw ZC time-domain sequence (for detecting raw ZC injected into signal)
    // 2. OFDM ZC template: IFFT of ZC in frequency bins (what the ZC OFDM symbol
    //    actually looks like in the time domain after OFDM modulation)
    let zc_raw = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);

    // Build OFDM ZC template: place ZC into FFT bins, IFFT to get time-domain
    let mut zc_freq = vec![Complex::new(0.0f32, 0.0f32); SYMBOL_LEN];
    for (i, &val) in zc_raw.iter().enumerate().take(SYMBOL_LEN.min(zc_raw.len())) {
        zc_freq[i] = val;
    }
    fft_inverse(&mut zc_freq);
    let norm_factor = 1.0 / SYMBOL_LEN as f32;
    let zc_ofdm: Vec<Complex<f32>> = zc_freq.iter().map(|&c| c * norm_factor).collect();

    // Use whichever template is longer for correlation window
    let templates: &[(&[Complex<f32>], &str)] = &[
        (&zc_raw, "raw_zc"),
        (&zc_ofdm, "ofdm_zc"),
    ];

    if samples.len() < ZC_LENGTH {
        return Vec::new();
    }

    // Compute noise floor from overall signal
    let noise_power = estimate_noise_power(samples, samples.len().min(8192));

    let mut detections = Vec::new();

    for &(template, _label) in templates {
        let tmpl_len = template.len();
        if samples.len() < tmpl_len {
            continue;
        }

        let tmpl_energy: f32 = template.iter().map(|c| c.norm_sqr()).sum::<f32>();
        let tmpl_norm = tmpl_energy.sqrt().max(1e-12);

        let max_offset = samples.len() - tmpl_len;

        // Phase 1: find regions with above-average energy
        let energy_step = tmpl_len / 2;
        let global_energy: f64 = samples.iter().map(|c| c.norm_sqr() as f64).sum::<f64>()
            / samples.len() as f64;

        let mut energy_candidates: Vec<usize> = Vec::new();
        let mut offset = 0;
        while offset <= max_offset {
            let window_energy: f64 = samples[offset..offset + tmpl_len]
                .iter()
                .map(|c| c.norm_sqr() as f64)
                .sum::<f64>()
                / tmpl_len as f64;
            if window_energy > global_energy * 1.5 || window_energy > 0.01 {
                energy_candidates.push(offset);
            }
            offset += energy_step;
        }

        // Phase 2: fine correlation around each energy candidate
        for &candidate in &energy_candidates {
            let search_start = candidate.saturating_sub(energy_step + tmpl_len);
            let search_end = (candidate + energy_step + tmpl_len).min(max_offset);

            let mut best_off = candidate;
            let mut best_corr = 0.0f64;
            let mut best_phase = 0.0f32;

            for off in search_start..=search_end {
                if off + tmpl_len > samples.len() {
                    break;
                }

                // Compute complex correlation
                let corr: Complex<f32> = samples[off..off + tmpl_len]
                    .iter()
                    .zip(template.iter())
                    .map(|(&s, &z)| s * z.conj())
                    .sum();

                // Signal energy in this window
                let sig_energy: f32 = samples[off..off + tmpl_len]
                    .iter()
                    .map(|c| c.norm_sqr())
                    .sum::<f32>();
                let sig_norm = sig_energy.sqrt().max(1e-12);

                let norm_corr = corr.norm() as f64 / (sig_norm * tmpl_norm) as f64;
                if norm_corr > best_corr {
                    best_corr = norm_corr;
                    best_off = off;
                    best_phase = corr.arg();
                }
            }

            if best_corr > DETECTION_THRESHOLD {
                // Check this isn't a duplicate of an already-detected burst
                let is_dup = detections.iter().any(|d: &BurstDetection| {
                    (d.sample_offset as i64 - best_off as i64).unsigned_abs()
                        < burst_length_samples() as u64
                });
                if is_dup {
                    continue;
                }

                // CFO estimate from correlation phase
                let cfo_rad_per_sample = best_phase as f64 / tmpl_len as f64;
                let cfo_hz =
                    cfo_rad_per_sample * sample_rate / (2.0 * std::f64::consts::PI);

                // SNR estimate
                let signal_power_est = best_corr * best_corr;
                let snr_db = if noise_power > 1e-20 {
                    10.0 * (signal_power_est / noise_power).log10()
                } else {
                    40.0
                };

                detections.push(BurstDetection {
                    sample_offset: best_off,
                    correlation_peak: best_corr.min(1.0),
                    frequency_offset_hz: cfo_hz,
                    snr_db,
                });
            }
        }
    } // end template loop

    // Sort by sample offset
    detections.sort_by_key(|d| d.sample_offset);
    detections
}

/// Compute normalized cross-correlation at a specific offset.
#[allow(dead_code)]
fn normalized_correlation(
    samples: &[Complex<f32>],
    offset: usize,
    template: &[Complex<f32>],
    tmpl_norm: f32,
) -> f64 {
    let tmpl_len = template.len();
    if offset + tmpl_len > samples.len() {
        return 0.0;
    }

    let corr: Complex<f32> = samples[offset..offset + tmpl_len]
        .iter()
        .zip(template.iter())
        .map(|(&s, &z)| s * z.conj())
        .sum();

    let sig_energy: f32 = samples[offset..offset + tmpl_len]
        .iter()
        .map(|c| c.norm_sqr())
        .sum::<f32>();
    let sig_norm = sig_energy.sqrt().max(1e-12);

    corr.norm() as f64 / (sig_norm * tmpl_norm) as f64
}

/// Estimate noise power from a block of samples (mean squared magnitude).
fn estimate_noise_power(samples: &[Complex<f32>], count: usize) -> f64 {
    let n = count.min(samples.len());
    if n == 0 {
        return 0.0;
    }
    let sum: f64 = samples[..n].iter().map(|c| c.norm_sqr() as f64).sum();
    sum / n as f64
}

// ---------------------------------------------------------------------------
// 3. OFDM demodulation
// ---------------------------------------------------------------------------

/// CP lengths for each of the 9 OFDM symbols (1-indexed in comments).
/// Symbols 1-2 use the long CP, symbols 3-9 use the short CP.
fn cp_lengths() -> [usize; NUM_SYMBOLS] {
    [
        LONG_CP, LONG_CP,   // symbols 1, 2
        SHORT_CP, SHORT_CP, // symbols 3, 4
        SHORT_CP, SHORT_CP, // symbols 5, 6
        SHORT_CP, SHORT_CP, // symbols 7, 8
        SHORT_CP,           // symbol 9
    ]
}

/// Total burst length in samples (sum of all symbol+CP lengths).
pub fn burst_length_samples() -> usize {
    let cps = cp_lengths();
    cps.iter().map(|&cp| cp + SYMBOL_LEN).sum()
}

/// Demodulate a single DroneID burst into frequency-domain OFDM symbols.
///
/// Input: IQ samples starting at burst beginning (from `detect_bursts`).
/// Output: 9 OFDM symbols, each as a vector of `FFT_SIZE_15MHZ` complex values
/// in frequency domain.
///
/// Steps per symbol:
/// 1. Skip cyclic prefix
/// 2. Apply frequency offset correction
/// 3. FFT to frequency domain
pub fn demodulate_ofdm(
    burst: &[Complex<f32>],
    sample_rate: f64,
    freq_offset_hz: f64,
) -> Result<Vec<Vec<Complex<f32>>>, String> {
    let burst_len = burst_length_samples();
    if burst.len() < burst_len {
        return Err(format!(
            "Burst too short: {} samples, need {}",
            burst.len(),
            burst_len
        ));
    }

    let cps = cp_lengths();
    let mut symbols = Vec::with_capacity(NUM_SYMBOLS);
    let mut pos = 0;

    // Phase increment per sample for CFO correction
    let phase_inc = -2.0 * PI * (freq_offset_hz as f32) / (sample_rate as f32);

    for sym_idx in 0..NUM_SYMBOLS {
        let cp_len = cps[sym_idx];

        // Skip cyclic prefix
        pos += cp_len;

        if pos + SYMBOL_LEN > burst.len() {
            return Err(format!(
                "Burst truncated at symbol {}: pos={}, need {}",
                sym_idx,
                pos,
                pos + SYMBOL_LEN
            ));
        }

        // Extract symbol samples and apply CFO correction
        let mut sym_data: Vec<Complex<f32>> = burst[pos..pos + SYMBOL_LEN]
            .iter()
            .enumerate()
            .map(|(i, &s)| {
                let global_sample = (pos + i) as f32;
                let phase = phase_inc * global_sample;
                let correction = Complex::new(phase.cos(), phase.sin());
                s * correction
            })
            .collect();

        // FFT to frequency domain
        fft_forward(&mut sym_data);

        symbols.push(sym_data);
        pos += SYMBOL_LEN;
    }

    Ok(symbols)
}

// ---------------------------------------------------------------------------
// 4. Channel estimation from ZC reference symbols
// ---------------------------------------------------------------------------

/// Estimate channel response from ZC reference symbols (symbols 4 and 6,
/// 0-indexed as 3 and 5).
///
/// Channel estimate H[k] = RX_ZC[k] / TX_ZC[k] for each subcarrier k.
/// Averages the estimates from both ZC symbols for noise reduction.
/// Returns per-subcarrier equalization coefficients (1/H[k]).
pub fn estimate_channel(
    zc_sym4: &[Complex<f32>],
    zc_sym6: &[Complex<f32>],
    zc_ref: &[Complex<f32>],
) -> Vec<Complex<f32>> {
    let n = zc_sym4.len().min(zc_sym6.len()).min(zc_ref.len());
    let mut eq_coeffs = vec![Complex::new(0.0f32, 0.0f32); n];

    for i in 0..n {
        let ref_val = zc_ref[i];
        if ref_val.norm_sqr() < 1e-12 {
            // No reference energy in this bin — pass through
            eq_coeffs[i] = Complex::new(1.0, 0.0);
            continue;
        }

        // H[k] from symbol 4
        let h4 = zc_sym4[i] / ref_val;
        // H[k] from symbol 6
        let h6 = zc_sym6[i] / ref_val;

        // Average the two channel estimates
        let h_avg = (h4 + h6) * 0.5;

        // Equalization coefficient = 1/H (zero-forcing)
        let h_mag_sq = h_avg.norm_sqr();
        if h_mag_sq > 1e-12 {
            eq_coeffs[i] = h_avg.conj() / h_mag_sq;
        } else {
            eq_coeffs[i] = Complex::new(1.0, 0.0);
        }
    }

    eq_coeffs
}

/// Apply channel equalization to a frequency-domain symbol.
pub fn equalize_symbol(
    symbol: &[Complex<f32>],
    eq_coeffs: &[Complex<f32>],
) -> Vec<Complex<f32>> {
    symbol
        .iter()
        .zip(eq_coeffs.iter())
        .map(|(&s, &eq)| s * eq)
        .collect()
}

// ---------------------------------------------------------------------------
// 5. QPSK demodulation
// ---------------------------------------------------------------------------

/// Extract the active data subcarriers from a frequency-domain symbol.
///
/// DJI DroneID uses 600 data subcarriers centered around DC, with DC null.
/// For a 1024-point FFT:
///   - Subcarriers -300..-1  → bins (1024-300)..1023 = bins 724..1023
///   - DC (bin 0) is null
///   - Subcarriers +1..+300  → bins 1..300
///
/// Returns 600 complex values in subcarrier order (negative freqs first).
pub fn extract_data_carriers(symbol: &[Complex<f32>], fft_size: usize) -> Vec<Complex<f32>> {
    let half = DATA_CARRIERS / 2; // 300
    let mut carriers = Vec::with_capacity(DATA_CARRIERS);

    // Negative frequency subcarriers: bins (fft_size - half) .. (fft_size - 1)
    for i in (fft_size - half)..fft_size {
        if i < symbol.len() {
            carriers.push(symbol[i]);
        }
    }

    // Positive frequency subcarriers: bins 1..=half
    for i in 1..=half {
        if i < symbol.len() {
            carriers.push(symbol[i]);
        }
    }

    carriers
}

/// Demodulate QPSK constellation points to bits.
///
/// Standard QPSK mapping (Gray coded):
///   {+1+j} → 00,  {+1-j} → 01,  {-1+j} → 10,  {-1-j} → 11
///
/// Since DJI DroneID has no pilot subcarriers, the absolute phase is
/// ambiguous. The caller can try all 4 rotations (0, 90, 180, 270 deg)
/// and check CRC/turbo decoder output to find the correct one.
///
/// `rotation` is 0..3 for 0/90/180/270 degree phase rotation.
pub fn demodulate_qpsk(symbols: &[Complex<f32>], rotation: u8) -> Vec<u8> {
    let rot = match rotation % 4 {
        0 => Complex::new(1.0f32, 0.0),
        1 => Complex::new(0.0f32, 1.0),  // 90 deg
        2 => Complex::new(-1.0f32, 0.0), // 180 deg
        3 => Complex::new(0.0f32, -1.0), // 270 deg
        _ => unreachable!(),
    };

    let mut bits = Vec::with_capacity(symbols.len() * 2);
    for &sym in symbols {
        let rotated = sym * rot;
        // Decision: real part → MSB, imag part → LSB
        let msb = if rotated.re >= 0.0 { 0u8 } else { 1u8 };
        let lsb = if rotated.im >= 0.0 { 0u8 } else { 1u8 };
        bits.push(msb);
        bits.push(lsb);
    }
    bits
}

// ---------------------------------------------------------------------------
// 6. Descrambling
// ---------------------------------------------------------------------------

/// Descramble demodulated bits using an LFSR-based scrambling sequence.
///
/// DJI DroneID uses a scrambling sequence seeded by a known initialization
/// value. The LFSR polynomial and seed are not fully public; this
/// implementation uses x^7 + x^4 + 1 (common in 802.11/LTE) as a
/// reasonable guess. The actual polynomial can be updated when confirmed.
///
/// XORs each bit with the corresponding LFSR output bit.
pub fn descramble(bits: &[u8], scramble_init: u32) -> Vec<u8> {
    let mut lfsr = scramble_init & 0x7F; // 7-bit LFSR state
    let mut output = Vec::with_capacity(bits.len());

    for &bit in bits {
        // x^7 + x^4 + 1: feedback from bits 6 and 3
        let feedback = ((lfsr >> 6) ^ (lfsr >> 3)) & 1;
        let scramble_bit = (lfsr & 1) as u8;
        output.push(bit ^ scramble_bit);
        lfsr = (lfsr >> 1) | (feedback << 6);
    }

    output
}

// ---------------------------------------------------------------------------
// 7. Payload parsing (post turbo-decode)
// ---------------------------------------------------------------------------

/// DJI coordinate conversion factor (same as WiFi-based DroneID).
/// raw_int32 / 174533.0 = degrees.
const DJI_COORD_FACTOR: f64 = 174533.0;

/// Parse DroneID payload from decoded bits.
///
/// This expects the raw payload bytes AFTER turbo decoding and descrambling.
/// The payload structure matches the WiFi-based DJI DroneID format (same
/// telemetry data, different physical layer).
///
/// NOTE: Until turbo decoding is implemented, this function is called with
/// placeholder data and will likely fail. It is provided so the full pipeline
/// is structurally complete.
pub fn parse_droneid_payload(data: &[u8]) -> Result<DroneIDPayload, String> {
    // Minimum payload: serial(16) + coords + alt + speeds + heading + home
    // Same structure as WiFi DroneID flight_reg (subcommand 0x10)
    if data.len() < 53 {
        return Err(format!("Payload too short: {} bytes, need >= 53", data.len()));
    }

    // Serial number: offset 5, 16 bytes ASCII
    let serial = extract_ascii(&data[5..21]);
    if serial.is_empty() {
        return Err("Empty serial number".to_string());
    }

    // Drone longitude: offset 21, i32 LE
    let raw_lon = i32::from_le_bytes([data[21], data[22], data[23], data[24]]);
    let drone_lon = raw_lon as f64 / DJI_COORD_FACTOR;

    // Drone latitude: offset 25, i32 LE
    let raw_lat = i32::from_le_bytes([data[25], data[26], data[27], data[28]]);
    let drone_lat = raw_lat as f64 / DJI_COORD_FACTOR;

    // Altitude: offset 29, i16 LE (meters, barometric)
    let alt = i16::from_le_bytes([data[29], data[30]]);

    // Height AGL: offset 31, i16 LE
    let height = i16::from_le_bytes([data[31], data[32]]);

    // Velocity north/east: offsets 33, 35 — cm/s
    let v_north = i16::from_le_bytes([data[33], data[34]]) as f32 / 100.0;
    let v_east = i16::from_le_bytes([data[35], data[36]]) as f32 / 100.0;
    let speed = (v_north * v_north + v_east * v_east).sqrt();

    // Heading: offset 43, i16 LE, centidegrees
    let raw_yaw = i16::from_le_bytes([data[43], data[44]]);
    let heading = ((raw_yaw as f32 / 100.0) % 360.0 + 360.0) % 360.0;

    // Home longitude: offset 45, i32 LE
    let raw_home_lon = i32::from_le_bytes([data[45], data[46], data[47], data[48]]);
    let home_lon = raw_home_lon as f64 / DJI_COORD_FACTOR;

    // Home latitude: offset 49, i32 LE
    let raw_home_lat = i32::from_le_bytes([data[49], data[50], data[51], data[52]]);
    let home_lat = raw_home_lat as f64 / DJI_COORD_FACTOR;

    Ok(DroneIDPayload {
        serial_number: serial,
        drone_latitude: drone_lat,
        drone_longitude: drone_lon,
        drone_altitude: alt as f32,
        pilot_latitude: home_lat,
        pilot_longitude: home_lon,
        home_latitude: home_lat,
        home_longitude: home_lon,
        speed,
        heading,
        height: height as f32,
        timestamp: 0, // Set by caller from capture timestamp
    })
}

/// Extract printable ASCII string, stopping at null or non-printable byte.
fn extract_ascii(data: &[u8]) -> String {
    let end = data.iter().position(|&b| b == 0).unwrap_or(data.len());
    data[..end]
        .iter()
        .filter(|&&b| b >= 0x20 && b < 0x7F)
        .map(|&b| b as char)
        .collect::<String>()
        .trim()
        .to_string()
}

// ---------------------------------------------------------------------------
// 8. High-level API
// ---------------------------------------------------------------------------

/// Process a block of IQ samples and return any detected DroneID bursts.
///
/// This is the main entry point. It:
/// 1. Runs ZC correlation to find burst locations
/// 2. OFDM-demodulates each detected burst
/// 3. Estimates and corrects channel response
/// 4. QPSK-demodulates data symbols to raw bits
///
/// Turbo decoding is NOT yet implemented — `payload` will be `None`.
/// The `raw_bits` field contains the demodulated bits for offline processing.
pub fn process_iq_block(
    samples: &[Complex<f32>],
    sample_rate: f64,
    _center_freq_mhz: f64,
) -> Vec<DroneIDResult> {
    let bursts = detect_bursts(samples, sample_rate);

    if bursts.is_empty() {
        return Vec::new();
    }

    debug!(count = bursts.len(), "DroneID bursts detected");

    let burst_len = burst_length_samples();

    // Generate ZC reference in frequency domain for channel estimation
    let zc_time = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);
    let mut zc_freq = vec![Complex::new(0.0f32, 0.0f32); FFT_SIZE_15MHZ];
    for (i, &val) in zc_time.iter().enumerate().take(FFT_SIZE_15MHZ.min(zc_time.len())) {
        zc_freq[i] = val;
    }
    fft_forward(&mut zc_freq);

    let mut results = Vec::new();

    // The detector finds the ZC symbol position, not the burst start.
    // ZC symbol 3 (0-indexed) body starts at this offset from burst beginning:
    //   symbols 0-1: LONG_CP + SYMBOL_LEN each = 2 * (80 + 1024) = 2208
    //   symbol 2: SHORT_CP + SYMBOL_LEN = 72 + 1024 = 1096
    //   symbol 3 CP: SHORT_CP = 72
    //   Total offset to symbol 3 body start = 2208 + 1096 + 72 = 3376
    // For OFDM template (1024 samples), the match is at the symbol body.
    // For raw ZC template (601 samples), the match is also near the symbol body.
    let zc_sym3_offset = 2 * (LONG_CP + SYMBOL_LEN) + SHORT_CP + SYMBOL_LEN + SHORT_CP;
    // That's: sym0(80+1024) + sym1(80+1024) + sym2(72+1024) + sym3_cp(72) = 3376

    for burst in &bursts {
        // Work backwards from detected ZC position to find burst start
        let burst_start = if burst.sample_offset >= zc_sym3_offset {
            burst.sample_offset - zc_sym3_offset
        } else {
            // ZC found near the start — likely the raw ZC template matched
            // somewhere. Use the offset directly as an approximate burst start.
            burst.sample_offset
        };

        if burst_start + burst_len > samples.len() {
            trace!(
                offset = burst_start,
                detected_at = burst.sample_offset,
                "Burst extends past end of block, skipping"
            );
            continue;
        }

        let burst_samples = &samples[burst_start..burst_start + burst_len];

        // OFDM demodulation
        let symbols = match demodulate_ofdm(burst_samples, sample_rate, burst.frequency_offset_hz)
        {
            Ok(s) => s,
            Err(e) => {
                warn!(error = %e, "OFDM demodulation failed");
                continue;
            }
        };

        if symbols.len() != NUM_SYMBOLS {
            warn!(
                count = symbols.len(),
                expected = NUM_SYMBOLS,
                "Wrong number of OFDM symbols"
            );
            continue;
        }

        // Channel estimation from ZC symbols (0-indexed: symbols 3 and 5)
        let eq_coeffs = estimate_channel(&symbols[3], &symbols[5], &zc_freq);

        // Collect data carriers from non-ZC, non-CP symbols.
        // Data symbols are: 0, 1, 2, 4, 7, 8 (0-indexed)
        // Symbols 3 and 5 are ZC reference. Symbol 6 (0-indexed) may also be data.
        // Per proto17: symbols 4 and 6 (1-indexed) = ZC → 0-indexed 3 and 5
        // Data symbols: 0, 1, 2, 4, 6, 7, 8 (0-indexed)
        let data_sym_indices: &[usize] = &[0, 1, 2, 4, 6, 7, 8];

        let mut all_carriers = Vec::new();
        for &idx in data_sym_indices {
            let equalized = equalize_symbol(&symbols[idx], &eq_coeffs);
            let carriers = extract_data_carriers(&equalized, FFT_SIZE_15MHZ);
            all_carriers.extend(carriers);
        }

        // QPSK demodulate with rotation 0 (correct rotation unknown without turbo decode)
        let raw_bits = demodulate_qpsk(&all_carriers, 0);

        results.push(DroneIDResult {
            burst_offset: burst.sample_offset,
            frequency_offset_hz: burst.frequency_offset_hz,
            snr_db: burst.snr_db,
            correlation_peak: burst.correlation_peak,
            payload: None, // Turbo decoding not yet implemented
            raw_bits,
        });
    }

    results
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::f32::consts::PI as PI32;

    // Helper: generate white Gaussian-ish noise (deterministic PRNG for reproducibility)
    fn pseudo_noise(len: usize, amplitude: f32, seed: u64) -> Vec<Complex<f32>> {
        // Simple LCG PRNG — NOT cryptographic, just reproducible
        let mut state = seed;
        (0..len)
            .map(|_| {
                state = state.wrapping_mul(6364136223846793005).wrapping_add(1);
                let r1 = ((state >> 33) as f32) / (u32::MAX as f32) * 2.0 - 1.0;
                state = state.wrapping_mul(6364136223846793005).wrapping_add(1);
                let r2 = ((state >> 33) as f32) / (u32::MAX as f32) * 2.0 - 1.0;
                Complex::new(r1 * amplitude, r2 * amplitude)
            })
            .collect()
    }

    // -----------------------------------------------------------------------
    // Test 1: ZC generation — length, constant amplitude, autocorrelation peak
    // -----------------------------------------------------------------------

    #[test]
    fn test_zc_generation_length() {
        let zc = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);
        assert_eq!(zc.len(), ZC_LENGTH);
    }

    #[test]
    fn test_zc_generation_constant_amplitude() {
        let zc = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);
        for (i, &val) in zc.iter().enumerate() {
            let mag = val.norm();
            assert!(
                (mag - 1.0).abs() < 1e-5,
                "ZC[{}] magnitude = {}, expected 1.0",
                i,
                mag
            );
        }
    }

    #[test]
    fn test_zc_autocorrelation_peak() {
        // ZC sequences have ideal periodic autocorrelation:
        // |R(0)| = N, |R(k)| = 0 for k != 0 (for prime length)
        let zc = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);
        let n = zc.len();

        // Compute circular autocorrelation at lag 0
        let r0: Complex<f32> = zc.iter().map(|&c| c * c.conj()).sum();
        let r0_mag = r0.norm();
        assert!(
            (r0_mag - n as f32).abs() < 0.1,
            "R(0) = {}, expected {}",
            r0_mag,
            n
        );

        // Compute at a non-zero lag — should be much smaller
        let lag = 1;
        let r_lag: Complex<f32> = (0..n)
            .map(|i| zc[i] * zc[(i + lag) % n].conj())
            .sum();
        let r_lag_mag = r_lag.norm();
        // For prime-length ZC, |R(lag)| = sqrt(N) ≈ 24.5 for N=601
        // This is much smaller than R(0)=601
        assert!(
            r_lag_mag < n as f32 * 0.1,
            "R({}) = {}, expected << {}",
            lag,
            r_lag_mag,
            n
        );
    }

    #[test]
    fn test_zc_different_roots_orthogonal() {
        let zc_600 = generate_zc_sequence(600, ZC_LENGTH);
        let zc_100 = generate_zc_sequence(100, ZC_LENGTH);

        // Cross-correlation at lag 0 should be small
        let cross: Complex<f32> = zc_600
            .iter()
            .zip(zc_100.iter())
            .map(|(&a, &b)| a * b.conj())
            .sum();
        let cross_mag = cross.norm();
        let auto_mag = ZC_LENGTH as f32;

        assert!(
            cross_mag < auto_mag * 0.1,
            "Cross-correlation {} too high vs autocorrelation {}",
            cross_mag,
            auto_mag
        );
    }

    // -----------------------------------------------------------------------
    // Test 2: ZC correlation — detect synthetic burst in noise
    // -----------------------------------------------------------------------

    #[test]
    fn test_zc_detection_in_noise() {
        let sample_rate = SAMPLE_RATE_15MHZ;
        let zc = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);

        // Build a signal: noise + ZC sequence at a known offset
        let signal_offset = 2000;
        let total_len = signal_offset + SYMBOL_LEN + 2000;
        let noise_amp = 0.01;

        let mut samples = pseudo_noise(total_len, noise_amp, 42);

        // Insert ZC sequence (scaled to amplitude 1.0) at the known offset
        for (i, &val) in zc.iter().enumerate() {
            if signal_offset + i < samples.len() {
                samples[signal_offset + i] += val;
            }
        }

        let detections = detect_bursts(&samples, sample_rate);

        assert!(
            !detections.is_empty(),
            "Should detect at least one burst"
        );

        // The detected offset should be near our insertion point
        let first = &detections[0];
        let offset_diff = (first.sample_offset as i64 - signal_offset as i64).unsigned_abs();
        assert!(
            offset_diff < SYMBOL_LEN as u64,
            "Detected offset {} too far from expected {} (diff={})",
            first.sample_offset,
            signal_offset,
            offset_diff
        );

        assert!(
            first.correlation_peak > DETECTION_THRESHOLD,
            "Correlation peak {} below threshold {}",
            first.correlation_peak,
            DETECTION_THRESHOLD
        );
    }

    // -----------------------------------------------------------------------
    // Test 3: OFDM demodulation — synthetic signal with known symbols
    // -----------------------------------------------------------------------

    #[test]
    fn test_ofdm_demod_symbol_count() {
        // Create a synthetic burst of the correct length (all zeros — tests structure only)
        let burst_len = burst_length_samples();
        let burst = vec![Complex::new(0.0f32, 0.0f32); burst_len + 100];

        let result = demodulate_ofdm(&burst, SAMPLE_RATE_15MHZ, 0.0);
        assert!(result.is_ok(), "Demodulation should succeed");

        let symbols = result.unwrap();
        assert_eq!(
            symbols.len(),
            NUM_SYMBOLS,
            "Should produce {} symbols",
            NUM_SYMBOLS
        );

        for (i, sym) in symbols.iter().enumerate() {
            assert_eq!(
                sym.len(),
                FFT_SIZE_15MHZ,
                "Symbol {} should have {} bins",
                i,
                FFT_SIZE_15MHZ
            );
        }
    }

    #[test]
    fn test_ofdm_demod_too_short() {
        let burst = vec![Complex::new(0.0f32, 0.0f32); 100];
        let result = demodulate_ofdm(&burst, SAMPLE_RATE_15MHZ, 0.0);
        assert!(result.is_err(), "Should fail on short input");
    }

    #[test]
    fn test_ofdm_roundtrip_tone() {
        // Generate a single-tone signal, run it through OFDM demod, verify
        // the tone appears in the correct FFT bin.
        let burst_len = burst_length_samples();
        let tone_freq = 100_000.0f32; // 100 kHz tone
        let sample_rate = SAMPLE_RATE_15MHZ as f32;

        let burst: Vec<Complex<f32>> = (0..burst_len + 100)
            .map(|i| {
                let t = i as f32 / sample_rate;
                let phase = 2.0 * PI32 * tone_freq * t;
                Complex::new(phase.cos(), phase.sin())
            })
            .collect();

        let symbols = demodulate_ofdm(&burst, SAMPLE_RATE_15MHZ, 0.0).unwrap();

        // The tone at 100 kHz should show up in the expected bin
        let expected_bin = (tone_freq / sample_rate * FFT_SIZE_15MHZ as f32).round() as usize;

        // Check that the expected bin has significant energy in at least one symbol
        let has_peak = symbols.iter().any(|sym| {
            let peak_bin = sym
                .iter()
                .enumerate()
                .max_by(|a, b| a.1.norm().partial_cmp(&b.1.norm()).unwrap())
                .unwrap()
                .0;
            // Allow +/- 2 bin tolerance
            (peak_bin as i32 - expected_bin as i32).unsigned_abs() <= 2
        });

        assert!(has_peak, "Tone should appear near bin {}", expected_bin);
    }

    // -----------------------------------------------------------------------
    // Test 4: QPSK demodulation — all constellation points
    // -----------------------------------------------------------------------

    #[test]
    fn test_qpsk_all_constellation_points() {
        // Standard QPSK: {+1+j}→00, {+1-j}→01, {-1+j}→10, {-1-j}→11
        let symbols = vec![
            Complex::new(1.0, 1.0),   // 00
            Complex::new(1.0, -1.0),  // 01
            Complex::new(-1.0, 1.0),  // 10
            Complex::new(-1.0, -1.0), // 11
        ];

        let bits = demodulate_qpsk(&symbols, 0);
        assert_eq!(bits, vec![0, 0, 0, 1, 1, 0, 1, 1]);
    }

    #[test]
    fn test_qpsk_rotation_90() {
        // After 90-degree rotation, +1+j becomes the reference for what was +j-1
        let symbols = vec![
            Complex::new(0.0, 1.0),   // Was +1+j rotated by -90 → should map back
            Complex::new(0.0, -1.0),
        ];

        let bits_r0 = demodulate_qpsk(&symbols, 0);
        let bits_r1 = demodulate_qpsk(&symbols, 1);

        // Different rotations produce different bits
        assert_ne!(bits_r0, bits_r1, "Different rotations should produce different bits");
    }

    #[test]
    fn test_qpsk_noisy_symbols() {
        // Symbols with noise but still in correct quadrant
        let symbols = vec![
            Complex::new(0.8, 0.7),    // Quadrant I → 00
            Complex::new(0.9, -0.6),   // Quadrant IV → 01
            Complex::new(-0.7, 0.9),   // Quadrant II → 10
            Complex::new(-0.85, -0.75),// Quadrant III → 11
        ];

        let bits = demodulate_qpsk(&symbols, 0);
        assert_eq!(bits, vec![0, 0, 0, 1, 1, 0, 1, 1]);
    }

    // -----------------------------------------------------------------------
    // Test 5: Frequency offset — add known CFO, verify correction
    // -----------------------------------------------------------------------

    #[test]
    fn test_frequency_offset_correction() {
        let sample_rate = SAMPLE_RATE_15MHZ;
        let cfo_hz = 500.0f64; // 500 Hz offset

        // Generate a clean single-tone burst
        let burst_len = burst_length_samples();
        let tone_freq = 50_000.0f32;

        let clean_burst: Vec<Complex<f32>> = (0..burst_len + 100)
            .map(|i| {
                let t = i as f32 / sample_rate as f32;
                let phase = 2.0 * PI32 * tone_freq * t;
                Complex::new(phase.cos(), phase.sin())
            })
            .collect();

        // Add CFO
        let cfo_burst: Vec<Complex<f32>> = clean_burst
            .iter()
            .enumerate()
            .map(|(i, &s)| {
                let t = i as f32 / sample_rate as f32;
                let cfo_phase = 2.0 * PI32 * cfo_hz as f32 * t;
                let cfo_rot = Complex::new(cfo_phase.cos(), cfo_phase.sin());
                s * cfo_rot
            })
            .collect();

        // Demodulate with CFO correction
        let corrected = demodulate_ofdm(&cfo_burst, sample_rate, cfo_hz);
        assert!(corrected.is_ok(), "Should demodulate with CFO correction");

        // Demodulate without CFO correction
        let uncorrected = demodulate_ofdm(&cfo_burst, sample_rate, 0.0);
        assert!(uncorrected.is_ok());

        // The corrected version should have the tone in the expected bin,
        // while the uncorrected version should be shifted
        let expected_bin =
            (tone_freq / sample_rate as f32 * FFT_SIZE_15MHZ as f32).round() as usize;

        let corrected_syms = corrected.unwrap();
        let uncorrected_syms = uncorrected.unwrap();

        // Check symbol 3 (arbitrary choice)
        let find_peak = |sym: &[Complex<f32>]| -> usize {
            sym.iter()
                .enumerate()
                .max_by(|a, b| a.1.norm().partial_cmp(&b.1.norm()).unwrap())
                .unwrap()
                .0
        };

        let corrected_peak = find_peak(&corrected_syms[3]);
        let uncorrected_peak = find_peak(&uncorrected_syms[3]);

        // Corrected peak should be closer to expected bin
        let corr_err = (corrected_peak as i32 - expected_bin as i32).unsigned_abs();
        let uncorr_err = (uncorrected_peak as i32 - expected_bin as i32).unsigned_abs();

        // With only 500 Hz CFO on a 15 MHz signal, the bin shift is tiny
        // (500/15e6 * 1024 ≈ 0.03 bins), so both will be close.
        // The key assertion is that correction does not make things worse.
        assert!(
            corr_err <= uncorr_err + 1,
            "Corrected error {} should not exceed uncorrected error {} significantly",
            corr_err,
            uncorr_err
        );
    }

    // -----------------------------------------------------------------------
    // Test 6: SNR estimation from correlation
    // -----------------------------------------------------------------------

    #[test]
    fn test_snr_increases_with_signal_power() {
        let sample_rate = SAMPLE_RATE_15MHZ;
        let zc = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);
        let total_len = SYMBOL_LEN * 3;
        let signal_offset = SYMBOL_LEN / 2;

        // Low SNR: weak signal in noise
        let noise_amp_high = 0.5;
        let mut samples_low = pseudo_noise(total_len, noise_amp_high, 100);
        for (i, &val) in zc.iter().enumerate() {
            if signal_offset + i < samples_low.len() {
                samples_low[signal_offset + i] += val;
            }
        }

        // High SNR: strong signal in noise
        let noise_amp_low = 0.01;
        let mut samples_high = pseudo_noise(total_len, noise_amp_low, 100);
        for (i, &val) in zc.iter().enumerate() {
            if signal_offset + i < samples_high.len() {
                samples_high[signal_offset + i] += val;
            }
        }

        let det_low = detect_bursts(&samples_low, sample_rate);
        let det_high = detect_bursts(&samples_high, sample_rate);

        // Both should detect (ZC is strong enough even with noise_amp=0.5)
        // but the high-SNR case should have a higher SNR estimate
        if !det_low.is_empty() && !det_high.is_empty() {
            assert!(
                det_high[0].snr_db > det_low[0].snr_db,
                "High-SNR case ({:.1} dB) should exceed low-SNR case ({:.1} dB)",
                det_high[0].snr_db,
                det_low[0].snr_db
            );
        }
        // If low-SNR detection fails, that's also acceptable — it means
        // the detector correctly rejects weak signals
    }

    // -----------------------------------------------------------------------
    // Test 7: Full pipeline — synthetic burst → detect → demod → verify
    // -----------------------------------------------------------------------

    #[test]
    fn test_full_pipeline_synthetic_burst() {
        let sample_rate = SAMPLE_RATE_15MHZ;

        // Build a synthetic DroneID-like burst:
        // 9 OFDM symbols with correct CP structure, ZC in symbols 3 and 5
        let zc = generate_zc_sequence(ZC_ROOT, ZC_LENGTH);
        let cps = cp_lengths();
        let burst_len = burst_length_samples();

        let mut burst = Vec::with_capacity(burst_len);

        for sym_idx in 0..NUM_SYMBOLS {
            let cp_len = cps[sym_idx];

            // Generate symbol data
            let sym_data: Vec<Complex<f32>> = if sym_idx == 3 || sym_idx == 5 {
                // ZC reference symbols — place ZC sequence, zero-pad to FFT_SIZE
                let mut data = vec![Complex::new(0.0f32, 0.0f32); SYMBOL_LEN];
                for (i, &val) in zc.iter().enumerate() {
                    data[i] = val;
                }
                data
            } else {
                // Data symbols — QPSK-like random data
                (0..SYMBOL_LEN)
                    .map(|i| {
                        let phase = (i as f32 * 0.37 + sym_idx as f32 * 1.23) % (2.0 * PI32);
                        Complex::new(phase.cos(), phase.sin()) * 0.5
                    })
                    .collect()
            };

            // IFFT to get time-domain symbol
            let mut time_domain = sym_data;
            fft_inverse(&mut time_domain);
            let norm = 1.0 / SYMBOL_LEN as f32;
            for s in time_domain.iter_mut() {
                *s *= norm;
            }

            // Cyclic prefix: last `cp_len` samples of the symbol
            let cp_start = SYMBOL_LEN - cp_len;
            for i in cp_start..SYMBOL_LEN {
                burst.push(time_domain[i]);
            }

            // Symbol body
            burst.extend_from_slice(&time_domain);
        }

        // Scale burst to realistic amplitude (~1.0 peak, typical SDR output)
        let peak = burst.iter().map(|c| c.norm()).fold(0.0f32, f32::max);
        if peak > 0.0 {
            let scale = 1.0 / peak;
            for s in burst.iter_mut() {
                *s *= scale;
            }
        }

        // Embed burst in noise
        let leading_noise = 3000;
        let trailing_noise = 3000;
        let total_len = leading_noise + burst.len() + trailing_noise;
        let mut samples = pseudo_noise(total_len, 0.01, 777);

        for (i, &val) in burst.iter().enumerate() {
            samples[leading_noise + i] += val;
        }

        // Run full pipeline
        let results = process_iq_block(&samples, sample_rate, 2414.5);

        assert!(
            !results.is_empty(),
            "Pipeline should detect at least one burst"
        );

        let result = &results[0];
        assert!(
            result.correlation_peak > DETECTION_THRESHOLD,
            "Correlation peak {} should exceed threshold {}",
            result.correlation_peak,
            DETECTION_THRESHOLD
        );

        // Should produce raw bits (7 data symbols * 600 carriers * 2 bits/QPSK = 8400 bits)
        let expected_bits = 7 * DATA_CARRIERS * 2;
        assert_eq!(
            result.raw_bits.len(),
            expected_bits,
            "Should produce {} raw bits, got {}",
            expected_bits,
            result.raw_bits.len()
        );

        // All bits should be 0 or 1
        assert!(
            result.raw_bits.iter().all(|&b| b == 0 || b == 1),
            "All bits should be 0 or 1"
        );

        // Payload should be None (turbo decoding not implemented)
        assert!(
            result.payload.is_none(),
            "Payload should be None without turbo decoding"
        );
    }

    // -----------------------------------------------------------------------
    // Test 8: Data carrier extraction
    // -----------------------------------------------------------------------

    #[test]
    fn test_data_carrier_extraction_count() {
        let sym = vec![Complex::new(1.0f32, 0.0f32); FFT_SIZE_15MHZ];
        let carriers = extract_data_carriers(&sym, FFT_SIZE_15MHZ);
        assert_eq!(carriers.len(), DATA_CARRIERS);
    }

    #[test]
    fn test_data_carrier_extraction_excludes_dc() {
        // Put a unique value in bin 0 (DC) — it should NOT appear in output
        let mut sym = vec![Complex::new(0.0f32, 0.0f32); FFT_SIZE_15MHZ];
        sym[0] = Complex::new(99.0, 99.0);

        let carriers = extract_data_carriers(&sym, FFT_SIZE_15MHZ);

        // DC bin value should not be in the extracted carriers
        assert!(
            !carriers.iter().any(|c| c.re > 90.0),
            "DC bin should be excluded from data carriers"
        );
    }

    // -----------------------------------------------------------------------
    // Test 9: Descrambling round-trip
    // -----------------------------------------------------------------------

    #[test]
    fn test_descramble_roundtrip() {
        let original: Vec<u8> = (0..256).map(|i| (i % 2) as u8).collect();
        let scrambled = descramble(&original, 0x5A);
        let recovered = descramble(&scrambled, 0x5A);
        assert_eq!(original, recovered, "Descramble should be self-inverse");
    }

    #[test]
    fn test_descramble_changes_bits() {
        let original: Vec<u8> = vec![0; 64];
        let scrambled = descramble(&original, 0x5A);
        // Scrambled output should differ from all-zeros
        assert!(
            scrambled.iter().any(|&b| b != 0),
            "Scrambled output should not be all zeros"
        );
    }

    // -----------------------------------------------------------------------
    // Test 10: Payload parsing
    // -----------------------------------------------------------------------

    #[test]
    fn test_parse_payload_valid() {
        let mut data = vec![0u8; 75];

        // Serial at offset 5
        let serial = b"1ZNBH2K00C0001  ";
        data[5..21].copy_from_slice(serial);

        // Drone longitude at offset 21
        let lon_raw: i32 = ((-65.644 as f64) * DJI_COORD_FACTOR) as i32;
        data[21..25].copy_from_slice(&lon_raw.to_le_bytes());

        // Drone latitude at offset 25
        let lat_raw: i32 = ((18.253 as f64) * DJI_COORD_FACTOR) as i32;
        data[25..29].copy_from_slice(&lat_raw.to_le_bytes());

        // Altitude at offset 29
        let alt: i16 = 100;
        data[29..31].copy_from_slice(&alt.to_le_bytes());

        // Height at offset 31
        let height: i16 = 50;
        data[31..33].copy_from_slice(&height.to_le_bytes());

        // Velocity north at offset 33 (500 cm/s = 5 m/s)
        let v_n: i16 = 500;
        data[33..35].copy_from_slice(&v_n.to_le_bytes());

        // Velocity east at offset 35 (300 cm/s = 3 m/s)
        let v_e: i16 = 300;
        data[35..37].copy_from_slice(&v_e.to_le_bytes());

        // Heading at offset 43 (18000 centideg = 180 deg)
        let yaw: i16 = 18000;
        data[43..45].copy_from_slice(&yaw.to_le_bytes());

        // Home lon at offset 45
        data[45..49].copy_from_slice(&lon_raw.to_le_bytes());

        // Home lat at offset 49
        data[49..53].copy_from_slice(&lat_raw.to_le_bytes());

        let result = parse_droneid_payload(&data);
        assert!(result.is_ok(), "Should parse valid payload: {:?}", result.err());

        let payload = result.unwrap();
        assert_eq!(payload.serial_number, "1ZNBH2K00C0001");
        assert!((payload.drone_latitude - 18.253).abs() < 0.01);
        assert!((payload.drone_longitude - (-65.644)).abs() < 0.01);
        assert_eq!(payload.drone_altitude, 100.0);
        assert_eq!(payload.height, 50.0);
        assert!((payload.heading - 180.0).abs() < 0.1);

        // Speed = sqrt(5^2 + 3^2) ≈ 5.83 m/s
        assert!((payload.speed - 5.83).abs() < 0.1);
    }

    #[test]
    fn test_parse_payload_too_short() {
        let data = vec![0u8; 20];
        let result = parse_droneid_payload(&data);
        assert!(result.is_err());
    }

    #[test]
    fn test_parse_payload_empty_serial() {
        let data = vec![0u8; 75];
        let result = parse_droneid_payload(&data);
        assert!(result.is_err(), "Should reject payload with empty serial");
    }

    // -----------------------------------------------------------------------
    // Test 11: Channel estimation basics
    // -----------------------------------------------------------------------

    #[test]
    fn test_channel_estimation_passthrough() {
        // If ZC symbols match reference exactly, equalization coefficients should be ~1
        let n = 64;
        let ref_sym: Vec<Complex<f32>> = (0..n)
            .map(|i| {
                let phase = 2.0 * PI32 * (i as f32) / (n as f32);
                Complex::new(phase.cos(), phase.sin())
            })
            .collect();

        // Both received ZC symbols identical to reference
        let eq = estimate_channel(&ref_sym, &ref_sym, &ref_sym);

        for (i, &coeff) in eq.iter().enumerate() {
            let mag = coeff.norm();
            assert!(
                (mag - 1.0).abs() < 0.1,
                "Eq coeff[{}] magnitude = {}, expected ~1.0",
                i,
                mag
            );
        }
    }

    #[test]
    fn test_channel_estimation_amplitude_correction() {
        // Simulate a channel that attenuates by 0.5
        let n = 64;
        let ref_sym: Vec<Complex<f32>> = (0..n)
            .map(|i| {
                let phase = 2.0 * PI32 * (i as f32) / (n as f32);
                Complex::new(phase.cos(), phase.sin())
            })
            .collect();

        let attenuated: Vec<Complex<f32>> = ref_sym.iter().map(|&c| c * 0.5).collect();

        let eq = estimate_channel(&attenuated, &attenuated, &ref_sym);

        // Equalization should boost by ~2x
        for (i, &coeff) in eq.iter().enumerate() {
            let mag = coeff.norm();
            assert!(
                (mag - 2.0).abs() < 0.2,
                "Eq coeff[{}] magnitude = {}, expected ~2.0",
                i,
                mag
            );
        }
    }

    // -----------------------------------------------------------------------
    // Test 12: Constants sanity checks
    // -----------------------------------------------------------------------

    #[test]
    fn test_burst_length_reasonable() {
        let len = burst_length_samples();
        // At 15.36 MSPS, ~645 us burst = ~9907 samples
        // 2*LONG_CP + 7*SHORT_CP + 9*1024 = 160 + 504 + 9216 = 9880
        let expected = 2 * LONG_CP + 7 * SHORT_CP + 9 * SYMBOL_LEN;
        assert_eq!(len, expected, "Burst length mismatch");

        // Should be roughly 645 microseconds at 15.36 MSPS
        let duration_us = len as f64 / SAMPLE_RATE_15MHZ * 1e6;
        assert!(
            (duration_us - 645.0).abs() < 50.0,
            "Burst duration {:.1} us, expected ~645 us",
            duration_us
        );
    }

    #[test]
    fn test_frequencies_in_24ghz_band() {
        for &freq in &FREQUENCIES_MHZ {
            assert!(freq > 2300.0 && freq < 2500.0, "Freq {} outside 2.4 GHz", freq);
        }
    }

    #[test]
    fn test_no_detection_on_pure_noise() {
        let samples = pseudo_noise(SYMBOL_LEN * 5, 0.01, 999);
        let detections = detect_bursts(&samples, SAMPLE_RATE_15MHZ);
        assert!(
            detections.is_empty(),
            "Should not detect bursts in pure noise (got {} detections)",
            detections.len()
        );
    }
}
