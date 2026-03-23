// Package intel provides RF-specific calibration for SDR-based drone detection.
// Maps known drone RF protocols to their transmit power levels and propagation
// characteristics, enabling accurate RSSI → distance estimation even without
// WiFi-level protocol information.
package intel

import (
	"fmt"
	"math"
)

// RFProtocol identifies a drone RF protocol detected by the SDR pipeline.
type RFProtocol string

const (
	RFProtoUnknown      RFProtocol = "unknown"
	RFProtoDJIOcuSync   RFProtocol = "dji_ocusync"
	RFProtoDJIDroneID   RFProtocol = "dji_droneid"
	RFProtoAnalogFPV    RFProtocol = "analog_fpv"
	RFProtoDigitalFPV   RFProtocol = "digital_fpv"   // DJI FPV / HDZero / Walksnail
	RFProtoELRS900      RFProtocol = "elrs_900"
	RFProtoELRS2400     RFProtocol = "elrs_2400"
	RFProtoCrossfire    RFProtocol = "crossfire"
	RFProtoControlLink  RFProtocol = "control_link"   // Generic 2.4GHz FHSS (FrSky/Spektrum/Futaba)
)

// RFCalibration holds propagation parameters for an RF protocol.
type RFCalibration struct {
	Protocol    RFProtocol
	RSSI0       float64 // Reference power at 1m (dBm)
	PathLossN   float64 // Path loss exponent
	ShadowingDB float64 // Log-normal shadowing std dev (dB)
	MinDistM    float64 // Minimum credible distance (meters)
	MaxDistM    float64 // Maximum credible distance (meters)
}

// rfCalibrationTable maps RF protocols to their calibration parameters.
// Values are derived from datasheet TX power specs and open-field propagation.
var rfCalibrationTable = map[RFProtocol]RFCalibration{
	// DJI OcuSync video downlink: ~400mW (26 dBm) EIRP, OFDM
	// Typically on 5.8GHz channels (149, 132, 116 observed in field)
	RFProtoDJIOcuSync: {
		RSSI0: 26.0, PathLossN: 2.4, ShadowingDB: 6.0,
		MinDistM: 10, MaxDistM: 20000,
	},
	// DJI DroneID burst: ~400mW (26 dBm), 10MHz OFDM on 2.4GHz
	// Bursts every ~600ms, decoded gives GPS + serial
	RFProtoDJIDroneID: {
		RSSI0: 26.0, PathLossN: 2.5, ShadowingDB: 6.0,
		MinDistM: 10, MaxDistM: 15000,
	},
	// Analog FPV 5.8GHz video: power varies by transmitter
	// Most common: 600mW (27.8 dBm). Range: 25mW to 1W.
	// Using 600mW as baseline — the most common outdoor power level.
	RFProtoAnalogFPV: {
		RSSI0: 27.8, PathLossN: 2.2, ShadowingDB: 5.0,
		MinDistM: 5, MaxDistM: 5000,
	},
	// Digital FPV (DJI FPV system, HDZero, Walksnail Avatar)
	// DJI FPV: 700mW (28.5 dBm), HDZero: 25-400mW, Walksnail: up to 1W
	// Using 700mW as representative
	RFProtoDigitalFPV: {
		RSSI0: 28.5, PathLossN: 2.3, ShadowingDB: 6.0,
		MinDistM: 10, MaxDistM: 10000,
	},
	// ELRS 900MHz: typically 1W (30 dBm), LoRa modulation
	// Lower frequency = better propagation (lower path loss exponent)
	RFProtoELRS900: {
		RSSI0: 30.0, PathLossN: 2.0, ShadowingDB: 5.0,
		MinDistM: 10, MaxDistM: 40000,
	},
	// ELRS 2.4GHz: typically 100mW (20 dBm), LoRa modulation
	RFProtoELRS2400: {
		RSSI0: 20.0, PathLossN: 2.6, ShadowingDB: 6.0,
		MinDistM: 5, MaxDistM: 10000,
	},
	// TBS Crossfire 900MHz: up to 2W (33 dBm)
	RFProtoCrossfire: {
		RSSI0: 33.0, PathLossN: 2.0, ShadowingDB: 5.0,
		MinDistM: 10, MaxDistM: 50000,
	},
	// Generic 2.4GHz control link (FrSky, Spektrum, Futaba): ~100mW (20 dBm)
	RFProtoControlLink: {
		RSSI0: 20.0, PathLossN: 2.6, ShadowingDB: 7.0,
		MinDistM: 5, MaxDistM: 5000,
	},
}

// RFGenericCalibration is used when protocol is unknown.
// Conservative parameters — assumes moderate power, standard propagation.
var RFGenericCalibration = RFCalibration{
	Protocol:    RFProtoUnknown,
	RSSI0:       22.0,  // ~150mW assumed
	PathLossN:   2.5,
	ShadowingDB: 8.0,   // High uncertainty
	MinDistM:    10,
	MaxDistM:    15000,
}

// GetRFCalibration returns calibration parameters for a given RF protocol.
func GetRFCalibration(proto RFProtocol) RFCalibration {
	if cal, ok := rfCalibrationTable[proto]; ok {
		return cal
	}
	return RFGenericCalibration
}

// EstimateRFDistance estimates distance from an SDR power measurement.
// Returns (distance_m, min_m, max_m, confidence).
func EstimateRFDistance(powerDBm int32, proto RFProtocol) (distM, minM, maxM float64, confidence float64) {
	cal := GetRFCalibration(proto)

	// Log-distance path loss: d = 10^((RSSI0 - RSSI) / (10*n))
	rssiDiff := cal.RSSI0 - float64(powerDBm)
	if rssiDiff <= 0 {
		return cal.MinDistM, cal.MinDistM, cal.MinDistM * 2, 0.3
	}

	distM = math.Pow(10, rssiDiff/(10*cal.PathLossN))

	// Clamp
	if distM < cal.MinDistM {
		distM = cal.MinDistM
	}
	if distM > cal.MaxDistM {
		distM = cal.MaxDistM
	}

	// Uncertainty bounds from log-normal shadowing (1-sigma)
	shadowFactor := math.Pow(10, cal.ShadowingDB/(10*cal.PathLossN))
	minM = distM / shadowFactor
	maxM = distM * shadowFactor
	if minM < cal.MinDistM {
		minM = cal.MinDistM
	}
	if maxM > cal.MaxDistM {
		maxM = cal.MaxDistM
	}

	// Confidence: higher for known protocols, lower for unknown
	confidence = 0.5
	if proto != RFProtoUnknown {
		confidence = 0.65
	}
	// Penalize very weak signals
	if powerDBm < -90 {
		confidence *= 0.7
	}
	// Bonus for strong signals (close range, more reliable)
	if powerDBm > -60 {
		confidence = math.Min(confidence*1.2, 0.8)
	}

	return distM, minM, maxM, confidence
}

// IdentifyRFProtocol classifies an RF detection based on spectral characteristics.
// This is a simple heuristic classifier — ML-based classification comes later.
func IdentifyRFProtocol(centerFreqMHz float64, bandwidthMHz float64, modulation string) RFProtocol {
	// 900 MHz band (868-928 MHz)
	if centerFreqMHz >= 860 && centerFreqMHz <= 930 {
		if modulation == "lora" {
			if bandwidthMHz < 1 {
				return RFProtoELRS900 // or Crossfire — can't distinguish without deeper analysis
			}
		}
		return RFProtoControlLink
	}

	// 2.4 GHz band (2400-2500 MHz)
	if centerFreqMHz >= 2390 && centerFreqMHz <= 2500 {
		// DJI DroneID uses specific frequencies outside normal WiFi channels
		if centerFreqMHz >= 2395 && centerFreqMHz <= 2465 && bandwidthMHz >= 8 && bandwidthMHz <= 12 {
			if modulation == "ofdm" {
				return RFProtoDJIDroneID
			}
		}
		if modulation == "lora" {
			return RFProtoELRS2400
		}
		if modulation == "fhss" {
			return RFProtoControlLink
		}
	}

	// 5.8 GHz band (5650-5950 MHz)
	if centerFreqMHz >= 5600 && centerFreqMHz <= 5950 {
		if modulation == "fm" {
			return RFProtoAnalogFPV
		}
		if modulation == "ofdm" {
			if bandwidthMHz >= 35 {
				return RFProtoDJIOcuSync // O3 40MHz mode
			}
			if bandwidthMHz >= 15 && bandwidthMHz <= 25 {
				return RFProtoDigitalFPV // Could be DJI FPV, HDZero, or OcuSync 2.0
			}
			if bandwidthMHz >= 8 && bandwidthMHz <= 12 {
				return RFProtoDJIOcuSync // OcuSync 1.0/2.0 10MHz mode
			}
		}
		return RFProtoAnalogFPV // Default for 5.8GHz: assume FPV
	}

	return RFProtoUnknown
}

// GenerateSyntheticID creates a stable identifier for multi-TAP RF correlation.
// The ID is based on the signal's center frequency and bandwidth, rounded to
// prevent jitter from creating multiple identities for the same signal.
func GenerateSyntheticID(centerFreqMHz float64, bandwidthMHz float64, proto RFProtocol) string {
	// Round frequency to nearest 500 kHz to absorb measurement jitter
	freqKHz := int(math.Round(centerFreqMHz*1000/500) * 500)
	// Round bandwidth to nearest 1 MHz
	bwMHz := int(math.Round(bandwidthMHz))

	switch proto {
	case RFProtoAnalogFPV:
		// FPV channels are well-defined — snap to nearest standard channel
		ch := snapToFPVChannel(centerFreqMHz)
		if ch != "" {
			return "fpv:" + ch
		}
	case RFProtoDJIDroneID:
		return "dji_droneid:" + formatFreq(freqKHz)
	case RFProtoDJIOcuSync:
		return "ocusync:" + formatFreq(freqKHz)
	}

	return "rf:" + formatFreq(freqKHz) + ":" + formatBW(bwMHz)
}

func formatFreq(khz int) string {
	return fmt.Sprintf("%d", khz)
}

func formatBW(mhz int) string {
	return fmt.Sprintf("%dmhz", mhz)
}

// FPV channel table (Raceband — most common for racing)
var fpvRacebandChannels = map[int]string{
	5658: "R1", 5695: "R2", 5732: "R3", 5769: "R4",
	5806: "R5", 5843: "R6", 5880: "R7", 5917: "R8",
}

// Additional FPV bands
var fpvBandAChannels = map[int]string{
	5865: "A1", 5845: "A2", 5825: "A3", 5805: "A4",
	5785: "A5", 5765: "A6", 5745: "A7", 5725: "A8",
}

func snapToFPVChannel(centerMHz float64) string {
	centerInt := int(math.Round(centerMHz))
	tolerance := 5 // ±5 MHz

	// Find closest channel across all bands (deterministic: pick lowest frequency on tie)
	bestCh := ""
	bestDist := tolerance + 1
	for freq, ch := range fpvRacebandChannels {
		d := abs(centerInt - freq)
		if d < bestDist || (d == bestDist && freq < bestFreq(bestCh, fpvRacebandChannels, fpvBandAChannels)) {
			bestDist = d
			bestCh = ch
		}
	}
	for freq, ch := range fpvBandAChannels {
		d := abs(centerInt - freq)
		if d < bestDist || (d == bestDist && freq < bestFreq(bestCh, fpvRacebandChannels, fpvBandAChannels)) {
			bestDist = d
			bestCh = ch
		}
	}
	if bestDist <= tolerance {
		return bestCh
	}
	return ""
}

func bestFreq(ch string, tables ...map[int]string) int {
	for _, t := range tables {
		for f, c := range t {
			if c == ch {
				return f
			}
		}
	}
	return 0
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
