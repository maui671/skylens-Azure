# Skylens: Distributed Real-Time UAV Detection and Tracking via Passive RF Monitoring

**Version 1.0 — March 2026**

---

## Abstract

Skylens is a distributed unmanned aerial vehicle (UAV) detection system that identifies, tracks, and classifies drones using passive radio frequency monitoring. The system operates a network of low-cost sensor nodes (TAPs) built on Raspberry Pi 5 hardware with patched WiFi adapters in monitor mode, feeding detections to a central processing node over NATS messaging. Skylens parses multiple drone communication protocols — including ASTM F3411 Remote ID, DJI's proprietary DroneID, WiFi NAN discovery, OcuSync telemetry, and BLE 4/5 Remote ID beacons — to extract identification, position, altitude, speed, and operator location from detected aircraft. A trust-based spoof detection engine scores each detection for anomalies, while weighted least-squares trilateration estimates drone positions from multi-sensor RSSI measurements. The system was field-validated in February 2026 against nzyme, a commercial drone detection product, achieving first-detection 30 seconds faster with significantly higher update rates. Three TAP sensors currently monitor airspace 24/7 with zero packet drops and sub-100ms detection-to-dashboard latency.

---

## Table of Contents

1. [Introduction](#1-introduction)
2. [System Architecture](#2-system-architecture)
3. [TAP Sensor Design](#3-tap-sensor-design)
4. [Detection Pipeline](#4-detection-pipeline)
5. [Protocol Support](#5-protocol-support)
6. [Intelligence Engine](#6-intelligence-engine)
7. [Spoof Detection and Trust Scoring](#7-spoof-detection-and-trust-scoring)
8. [Position Estimation](#8-position-estimation)
9. [RF Propagation Modeling](#9-rf-propagation-modeling)
10. [State Management and Drone Lifecycle](#10-state-management-and-drone-lifecycle)
11. [Real-Time Dashboard](#11-real-time-dashboard)
12. [Alerting and Notifications](#12-alerting-and-notifications)
13. [Persistence and Analytics](#13-persistence-and-analytics)
14. [Security Model](#14-security-model)
15. [Deployment and Operations](#15-deployment-and-operations)
16. [Field Validation Results](#16-field-validation-results)
17. [Performance Characteristics](#17-performance-characteristics)
18. [Lessons Learned](#18-lessons-learned)
19. [Future Work](#19-future-work)

---

## 1. Introduction

The rapid proliferation of consumer and commercial drones has created a significant gap in airspace awareness, particularly around sensitive installations, airports, military facilities, and critical infrastructure. While regulatory frameworks like the FAA's Remote ID rule (effective September 2023) mandate that drones broadcast identification and telemetry, enforcement depends on the ability to actually receive and process these signals. Many drones in the field lack Remote ID firmware updates, use proprietary protocols that aren't captured by standard receivers, or operate with configurations that evade simple detection methods.

Existing commercial solutions like nzyme and DroneShield address parts of this problem but tend to be expensive, closed-source, and limited to specific protocol families. Skylens was built to fill this gap with an open-architecture system that combines protocol-agnostic WiFi monitoring with deep protocol parsing, distributed sensor fusion, and real-time classification.

### Design Goals

The system was designed around several core principles:

- **Passive detection only.** No active transmission, no jamming, no interaction with detected aircraft. The system is purely a receiver.
- **Protocol diversity.** Support every known drone communication protocol — not just Remote ID, but proprietary formats like DJI DroneID and behavioral signatures for drones that don't broadcast identification.
- **Low-cost hardware.** Sensor nodes run on Raspberry Pi 5 with commercial USB WiFi adapters. Total cost per TAP is under $150.
- **Real-time operation.** Detection-to-dashboard latency under 100 milliseconds. Operators see drones appear on the map within seconds of power-on.
- **Resilience.** Sensor nodes buffer detections during network interruptions, reconnect automatically, and resume without data loss.
- **Idempotent deployment.** The entire stack — sensors, processing node, database, messaging — can be installed on fresh hardware with a single script.

---

## 2. System Architecture

Skylens follows a hub-and-spoke topology with three main components:

### 2.1 TAP Sensors (Rust)

Each TAP is a Raspberry Pi 5 running a Rust binary that captures WiFi frames in monitor mode, parses drone protocols, enriches detections with manufacturer/model intelligence, and publishes structured protobuf messages to a NATS server. TAPs also perform BLE scanning for drones broadcasting Remote ID over Bluetooth.

### 2.2 Processing Node (Go)

The central node receives detections from all TAPs via NATS, deduplicates them, runs spoof detection, performs trilateration when multiple TAPs observe the same drone, manages drone state (lifecycle, classification, controller-UAV linking), persists data to PostgreSQL, caches state in Redis, and serves the real-time dashboard and REST API.

### 2.3 Transport Layer (NATS)

NATS provides the pub/sub messaging backbone between TAPs and the Node. Subject hierarchy:

| Subject | Direction | Content |
|---------|-----------|---------|
| `skylens.detections.{tap_id}` | TAP -> Node | Protobuf Detection messages |
| `skylens.heartbeats.{tap_id}` | TAP -> Node | TAP health telemetry (every 5s) |
| `skylens.commands.{tap_id}` | Node -> TAP | Configuration commands |
| `skylens.commands.broadcast` | Node -> All TAPs | Broadcast commands |

All messages use Protocol Buffers for serialization. The detection message includes 40+ fields covering identification, position, signal characteristics, operator location, and source metadata.

### 2.4 Data Flow

```
WiFi Frames / BLE Advertisements
        |
    [TAP Sensor] -- pcap capture + BLE scan
        |
    Frame parsing, protocol decoding, fingerprinting
        |
    Deduplication (1s window, data-completeness bypass)
        |
    Protobuf serialization
        |
    NATS publish (buffered, retry with backoff)
        |
    [Node] -- NATS subscription
        |
    Detection enrichment (serial decode, model lookup)
        |
    Spoof detection (trust scoring, 13+ anomaly checks)
        |
    State management (suspect -> active -> lost lifecycle)
        |
    Trilateration (multi-TAP WLS position estimation)
        |
    PostgreSQL persistence + Redis cache
        |
    WebSocket broadcast to dashboard clients
        |
    [Dashboard] -- real-time map, fleet table, alerts
```

---

## 3. TAP Sensor Design

### 3.1 Hardware

Each TAP runs on a Raspberry Pi 5 (4GB RAM) with an external USB WiFi adapter capable of monitor mode. Current deployment uses:

| TAP | Adapter | Chipset | Notes |
|-----|---------|---------|-------|
| TAP-001 | Alfa AWUS1900 | RTL8814AU | Patched rtw88 driver, 4x4 MIMO |
| TAP-002 | Alfa AWUS036ACH | RTL8812AU | Patched rtw88 driver, 2x2 MIMO |
| TAP-003 | Generic | MT7921U | Passive mode, MediaTek driver |

The Realtek adapters required a firmware lineage fix (v33.6.0 branch) and a kernel 6.12 rebuild of the rtw88 driver to enable monitor mode beacon reception. Without this patch, these chipsets silently drop all beacon frames — which are the primary carrier for Remote ID and DJI DroneID data.

### 3.2 Capture Architecture

The TAP uses a hybrid threading model:

- **Capture thread** (std::thread): Blocking pcap reads from the kernel ring buffer. This runs on a dedicated OS thread because libpcap's `next_packet()` blocks indefinitely and would stall an async runtime.
- **Channel hopper** (tokio task): Cycles through WiFi channels using nl80211 netlink calls.
- **BLE scanner** (tokio task): Scans for BLE Remote ID advertisements via BlueZ D-Bus.
- **NATS publisher** (tokio task): Serializes and publishes detections with a 10,000-message buffer for offline resilience.
- **Heartbeat emitter** (tokio task): Publishes TAP health metrics every 5 seconds.
- **Watchdog** (tokio task): Monitors packet flow and triggers process exit after 60 seconds of zero packets, allowing systemd to restart the service.

libpcap is configured with a 16 MB kernel ring buffer, 1024-byte snap length (sufficient for management frames), 50 ms read timeout, and a BPF filter excluding control frames.

### 3.3 Channel Hopping Strategy

WiFi-based drone detection requires monitoring multiple channels because drones transmit on different frequencies depending on the protocol:

- **Remote ID beacons**: Transmitted on WiFi channel 6 via NAN (Neighbor Awareness Networking)
- **DJI DroneID**: Transmitted at 2399.5, 2414.5, 2429.5, 2444.5, and 2459.5 MHz (10 MHz bandwidth, overlapping channels 1-11)
- **OcuSync video telemetry**: Observed on channels 149, 132, and 116 (5 GHz)
- **Controller SSIDs**: Primarily channel 149 (65%), with some on channels 132 and 116

The channel hopper uses a widest-first strategy to maximize spectral coverage per hop:

1. **5 GHz**: Start with 80 MHz wide channels (VHT80). A single hop to center frequency 5210 MHz covers channels 36-48. If the driver doesn't support 80 MHz, fall back to 40 MHz (two hops), then 20 MHz (four hops).
2. **2.4 GHz**: Use 40 MHz channels (HT40+). Channel 1 at HT40+ covers DJI DroneID frequencies at 2414.5 and 2429.5 MHz. Channel 6 at HT40+ covers 2429.5, 2444.5, and 2459.5 MHz.

Dwell times are priority-weighted:

| Channel | Multiplier | Reason |
|---------|-----------|--------|
| Ch 6 | 5x base | NAN discovery channel, Remote ID beacons |
| Ch 149 | 4x base | OcuSync + NAN 5 GHz discovery |
| Ch 1 (HT40+) | 3x base | DJI DroneID burst capture (~600ms) |
| Ch 36 | 2x base | UNII-1 drone traffic |
| All others | 1x base | Standard coverage |

With a 200 ms base interval, a full cycle through all channels takes approximately 8-9 seconds. Channel switching uses nl80211 netlink sockets directly (via the `neli` crate) rather than spawning `iw` subprocesses, eliminating per-hop process creation overhead.

### 3.4 Stuck Socket Recovery

The MT7921U driver occasionally disconnects the AF_PACKET socket during nl80211 channel hops. The TAP detects this condition (zero packets received for an extended period), drops the pcap handle, verifies monitor mode is still active by reading `/sys/class/net/<iface>/type`, and reopens the capture. This recovery happens without restarting the process.

---

## 4. Detection Pipeline

Each captured frame passes through an eight-check detection pipeline. Any single check matching is sufficient to classify the frame as a drone detection.

### Check 1: OUI Match

The source MAC address is compared against a database of 100+ known drone manufacturer OUI prefixes. Locally-administered MAC addresses (LAA bit set) are excluded from OUI matching because controller MAC randomization uses the LAA range and would produce false positives against vendor IE OUIs.

**Known OUI families:**
- DJI: `60:60:1F`, `04:A8:5A`, `34:D2:62`, `48:1C:B9`, `88:29:85`, `8C:1E:D9`, `E4:7A:2C`
- Parrot: `90:03:B7`, `A0:14:3D`
- Autel: `70:88:6B`, `EC:5B:CD`, `18:D7:93`
- Skydio: `38:1D:14`

### Check 2: SSID Pattern Match

The frame's SSID is tested against 2,600+ compiled regex patterns. Key patterns:

- `^RID-` — ASTM F3411 Remote ID beacon (highest confidence: 0.70)
- `^PROJ[0-9a-fA-F]{6}$` — DJI RC controller (PROJ + 6 hex chars from MAC hash)
- `^RM [A-Z0-9]+ [0-9]+$` — DJI enterprise controller (Matrice RC Plus)
- `^DJI[-_ ]` — Generic DJI drone SSID
- Product-specific patterns: `^MAVIC`, `^PHANTOM`, `^ANAFI`, `^EVO`

### Check 3: ASTM F3411 Remote ID Vendor IE

The frame's vendor information elements (IEs) are searched for OUI `FA:0B:BC` with type `0x0D`, which indicates an ASTM F3411 Remote ID payload. If found, the full ODID message is decoded (see Section 5.1).

### Check 4: DJI DroneID Vendor IE

Vendor IEs with OUI `26:37:12`, `26:6F:48`, or `60:60:1F` indicate DJI's proprietary DroneID protocol. The payload contains serial number, GPS position, altitude, velocity, heading, and home/operator location (see Section 5.2).

### Check 5-6: Parrot / Autel Vendor IEs

Vendor IEs from Parrot (`90:03:B7`) or Autel (`70:88:6B`, `EC:5B:CD`) OUIs trigger manufacturer-specific identification.

### Check 7: Secondary OUI Lookup

A broader OUI check against the BSSID field (in addition to the source MAC checked in step 1).

### Check 8: Behavioral Heuristics

If no protocol or signature match is found, the frame is scored against behavioral indicators:

| Signal | Score |
|--------|-------|
| SSID contains "DRONE" | +0.50 |
| SSID contains "FPV" | +0.40 |
| SSID contains "UAV" | +0.45 |
| 5.8 GHz beacon | +0.15 |
| Beacon interval 40-60 TU | +0.20 |
| LAA MAC address | +0.10 |

Frames scoring 0.45 or higher are classified as drone suspects. This catches DIY drones, Chinese toy drones (SYMA, JJRC), and other aircraft that don't broadcast standard identification.

### Deduplication

After a positive detection, the frame enters a deduplication layer. The standard window is 1 second — if the same MAC address was reported within the last second, the detection is suppressed. DJI RC controllers use a 5-minute window because they broadcast constantly with little new information.

A data-completeness bypass overrides the dedup window when a frame carries data types (serial number, drone GPS, operator GPS) that haven't been captured yet for this drone. This is important because different protocols carry different data — a DJI DroneID frame might have the drone's position while a subsequent Remote ID frame might add the operator's position.

---

## 5. Protocol Support

### 5.1 ASTM F3411 Remote ID (WiFi NAN + BLE)

Remote ID is the FAA-mandated broadcast standard for drone identification, defined in ASTM F3411-22a. Drones broadcast identification, position, and operator location via WiFi NAN Service Discovery Frames on channel 6 (2.4 GHz) and channel 149 (5 GHz), and optionally via BLE 4/5 advertisements.

**WiFi NAN transport:** The TAP identifies NAN frames by their action frame structure: category `0x04` (Public Action), action code `0x09` (Vendor Specific), OUI `50:6F:9A` (WiFi Alliance), type `0x13`. Within the NAN Service Descriptor attribute (`attr_id = 0x03`), the Remote ID service is identified by its Service ID — the first 6 bytes of SHA-256("org.opendroneid.remoteid"): `[0x88, 0x69, 0x19, 0x9D, 0x92, 0x09]`.

The Service Info payload follows ASTM F3411-22a Section 7.2.1: `OUI(3) | OUI_Type(1) | Message_Counter(1) | ODID_Message(s)`. The 5-byte header must be skipped before parsing the ODID payload.

**ODID message types:**

| Type | Content |
|------|---------|
| 0x0 BasicID | Serial number, ID type (serial/registration/UTM), UA type |
| 0x1 Location | Latitude, longitude, altitude (pressure/geodetic/AGL), speed, heading, vertical speed |
| 0x2 Auth | Authentication tokens |
| 0x3 SelfID | Free-text description, from which FAA registration numbers are extracted via regex |
| 0x4 System | Operator location (takeoff/live GNSS/fixed), area radius/ceiling/floor |
| 0x5 OperatorID | Operator contact information |
| 0xF MessagePack | Multiple messages bundled in a single frame |

**BLE transport:** The TAP scans for BLE advertisements containing Service Data with UUID `0xFFFA`. BLE 4 (legacy) sends one 25-byte message per advertisement at 3-4 Hz, requiring accumulation over a 3-second window to collect all message types. BLE 5 (extended) sends a full MessagePack in a single advertisement at 1 Hz.

**Location encoding:** Coordinates are stored as int32 values divided by 10^7 to get degrees. Altitude uses uint16 with a scale of 0.5 meters and an offset of -1000m. Speed has a two-range encoding: 0-63.75 m/s at 0.25 m/s resolution (multiplier=0) and 63.75-254.25 m/s at 0.75 m/s resolution (multiplier=1).

### 5.2 DJI DroneID (Proprietary)

DJI drones broadcast a proprietary telemetry protocol via WiFi vendor IEs, independent of the ASTM Remote ID standard. Three OUI variants have been observed:

| OUI | Format |
|-----|--------|
| `26:37:12` | Standard: `oui_type` field is the subcommand |
| `26:6F:48` | Standard: same as above |
| `60:60:1F` | OcuSync: `data[0]` is the subcommand, `data[1..]` is payload |

**Subcommand 0x10 (Flight Registration)** contains the richest data:

| Offset | Field | Encoding |
|--------|-------|----------|
| 0 | Version | 1 = cleartext, 2+ = encrypted |
| 1-2 | Sequence number | uint16 LE |
| 3-4 | State info | Bitfield: bit 0 = motor on, bits 1-2 = airborne status |
| 5-20 | Serial number | 16 bytes ASCII |
| 21-28 | Longitude, Latitude | int32 LE, divide by 174533.0 for degrees |
| 29-32 | Altitude (pressure), Height AGL | int16 LE, meters |
| 33-38 | Velocity (N/E/Up) | int16 LE, cm/s |
| 39-44 | Pitch, Roll, Yaw | int16 LE, centidegrees |
| 45-52 | Home longitude, latitude | int32 LE, same encoding as drone position |
| 53 | Product type | Enum (Mavic=0, Phantom=1, etc.) |

The coordinate divisor of 174533.0 was empirically determined through reverse engineering and validated against ground-truth GPS data from calibration flights.

**Encryption (V2+):** DJI firmware updates from mid-2023 onward encrypt the coordinate, altitude, and velocity fields in the DroneID payload. Skylens detects encrypted payloads (version >= 2) and extracts only the unencrypted fields: serial number, product type, state info, and sequence number. The serial number alone is sufficient to identify the drone manufacturer and model.

**DJI DroneID RF characteristics (2.4 GHz):**

| Parameter | Value |
|-----------|-------|
| Frequencies | 2399.5, 2414.5, 2429.5, 2444.5, 2459.5 MHz |
| Bandwidth | 10 MHz per burst |
| Burst duration | ~600 ms |
| Protocol designator | proto17 |

### 5.3 OcuSync Video Telemetry

DJI drones use OcuSync for the video downlink between drone and controller. While the primary purpose is video transmission, these frames carry identification data (MAC address, SSID) that can be used for detection when DroneID is encrypted or absent.

Field observations show OcuSync traffic on channels 149 (65%), 132 (20%), and 116 (9%) — notably different from the theoretical channels 153/157/161/165 cited in most documentation.

### 5.4 DJI Controller Identification

DJI RC controllers broadcast WiFi access point beacons with distinctive SSID patterns:

- **Consumer RC (RC-N1, RC Pro, RC 2):** SSID format `PROJ` followed by 6 hexadecimal characters (e.g., `PROJa46651`). The hex suffix is derived from the controller's MAC address.
- **Enterprise RC (RC Plus):** SSID format `RM` followed by a model code and serial (e.g., `RM E70363 0570269`).

Controllers use locally-administered MAC randomization — the MAC address changes on WiFi reconnect (up to 4 different MACs per controller observed), but the SSID remains stable. This makes SSID the reliable identifier for controller tracking.

---

## 6. Intelligence Engine

### 6.1 Serial Number Decoding

DJI drones use a CTA-2063-A serial format starting with `1581F` followed by a 3-character model code and a unique identifier. The Node maintains a lookup table mapping model codes to human-readable names:

| Prefix | Model | Notes |
|--------|-------|-------|
| 1581F0A | Mavic Pro | Original Mavic |
| 1581F1Y | Mini 3 Pro | Consumer |
| 1581F3Y | Air 2S | Field calibrated |
| 1581F45 | Mavic 3 | Multiple variants |
| 1581F4X | Neo 2 | Field detected Mar 2026 |
| 1581F5K | Mavic 3 Pro | Professional |
| 1581F6Z9 | Air 3S | Field calibrated |
| 1581F895 | Phantom 4 Pro | Legacy |
| 1581F9DE | Inspire 3 | Enterprise |

The database currently maps 68+ DJI model codes, with additional prefixes for Autel (`1748`), Parrot (`1588E`), and Skydio (`1668B`).

### 6.2 OUI Database

The system maintains a curated database of 100+ OUI prefixes mapped to drone manufacturers. The OUI database is loaded at TAP startup and used for initial manufacturer identification before deeper protocol parsing occurs.

### 6.3 RSSI Calibration Per Model

Different drone models have different transmit power levels, which means the same RSSI reading at the same distance will vary by model. The system maintains per-model RSSI calibration offsets (in dB relative to a baseline) to normalize distance estimates:

| Category | Models | Offset (dB) |
|----------|--------|-------------|
| Mini family | Mini 2, Mini SE | -10.0 |
| Mini 3/4 family | Mini 3 Pro, Mini 4 Pro | -8.0 |
| FPV/Neo family | Avata, Neo, Neo 2, Flip | -3.0 to -5.0 |
| Air family | Air 2S, Air 3, Air 3S | -7.2 to +1.5 |
| Mavic family | Mavic 3, Mavic 3 Pro | 0.0 (baseline) |
| Phantom family | Phantom 4 Pro | +2.0 |
| Enterprise | Inspire 3, Matrice 30/4T | +5.0 to +8.0 |

The Air 2S offset of -7.2 dB and Air 3S offset of +1.5 dB were field-calibrated using ground-truth GPS data from calibration flights at known distances.

---

## 7. Spoof Detection and Trust Scoring

Every drone in the system carries a trust score from 0 to 100. New drones start at 100 (fully trusted). The spoof detector runs 13+ anomaly checks on each detection, applying penalties for suspicious behavior and bonuses for consistent, verified data.

### 7.1 Penalty Flags

| Flag | Penalty | Trigger |
|------|---------|---------|
| coordinate_jump | 30 | Impossible movement speed (>80 m/s between detections at >100m separation) |
| invalid_coordinates | 40 | NaN/Inf values or coordinates outside WGS84 bounds |
| duplicate_id | 35 | Same identifier observed from different MACs at >100m separation |
| oui_ssid_mismatch | 35 | MAC vendor doesn't match SSID vendor (e.g., DJI OUI but "ANAFI" SSID) |
| impossible_vendor | 40 | Generic chipset OUI (ESP32, Espressif) claiming to be an enterprise drone |
| speed_violation | 25 | Reported speed exceeds 80 m/s |
| altitude_spike | 20 | Altitude change exceeds 100 m/s |
| rssi_distance_mismatch | 25 | Very strong RSSI (-40 dBm) but claimed position is far from TAP |
| rssi_impossible | 20 | RSSI outside physically plausible range (>-20 dBm or <-100 dBm) |
| timestamp_anomaly | 15 | Detection timestamp >30s in past or >5s in future |
| randomized_mac | 15 | Locally administered MAC (common in DIY/spoofing setups) |
| low_confidence | 15 | Detection confidence below 0.50 |
| no_serial | 5 | Missing serial number |

### 7.2 Bonus System

| Bonus | Points | Trigger |
|-------|--------|---------|
| triple_match_with_serial | 20 | OUI vendor, SSID vendor, and serial all match + valid serial |
| oui_ssid_remoteid_match | 15 | OUI, SSID, and Remote ID data all consistent |
| high_confidence | 10 | Detection confidence >= 0.90 |
| oui_ssid_match | 10 | OUI vendor matches SSID vendor |
| medium_confidence | 5 | Detection confidence >= 0.80 |

### 7.3 Trust Recovery

Trust isn't permanently damaged. After 10 consecutive clean detections (no new flags), the drone's trust score recovers to 100. After 5 clean detections with flags still present, trust recovers by 5 points per cycle.

### 7.4 Classification

The trust score feeds into the drone's classification:

| Trust Range | Classification |
|-------------|---------------|
| 0-20 | HOSTILE |
| 21-50 | SUSPECT |
| 51-100 | UNKNOWN (default) |
| Manual override | NEUTRAL or FRIENDLY |

NEUTRAL and FRIENDLY classifications are operator-assigned through the dashboard for known, authorized aircraft.

### 7.5 Implementation

The spoof detector uses a 16-shard map (FNV-1a hash) to distribute lock contention across detection tracks. Each drone's detection history is maintained in a sliding window, and anomaly checks compare the current detection against the historical track. This sharded architecture ensures spoof checking doesn't become a bottleneck even at high detection rates.

---

## 8. Position Estimation

### 8.1 Trilateration Algorithm

When multiple TAP sensors observe the same drone, Skylens estimates the drone's position using weighted least-squares trilateration.

**Input:** For each TAP that detected the drone, the system provides a distance estimate (derived from RSSI via the log-distance path loss model) and the TAP's known GPS coordinates.

**Processing steps:**

1. **Coordinate transformation.** TAP positions are converted from geodetic (lat/lon) to a local East-North-Up (ENU) Cartesian frame centered on the first TAP. This avoids singularities in geodetic math and works in meters.

2. **Weight calculation.** Each TAP's measurement is weighted by the inverse of its estimated uncertainty squared, scaled by detection confidence and an inverse-distance factor:

   ```
   weight = (1 / uncertainty^2) * confidence * (1 / sqrt(distance/1000 + 1))
   ```

3. **Initial estimate.** A weighted centroid is computed from the TAP positions, then each TAP's measurement projects a point at its estimated distance along the TAP-to-centroid direction. The weighted average of these projected points provides a starting position for the iterative solver.

4. **Levenberg-Marquardt iteration.** The solver minimizes the sum of weighted squared residuals (measured distance minus predicted distance from current estimate). The Jacobian consists of direction cosines from the current estimate to each TAP. Step damping limits each iteration to 500m of movement to prevent divergence. Convergence is achieved when the step length drops below 1 meter.

5. **Error estimation.** The covariance matrix `C = sigma^2 * (J'WJ)^-1` is computed and its eigenvalues give the semi-major and semi-minor axes of the error ellipse. Geometric Dilution of Precision (GDOP) is calculated as `sqrt(trace((J'WJ)^-1))` and used to scale the confidence estimate.

6. **Outlier rejection.** Optionally, residuals exceeding 2.5 sigma are flagged as outliers and removed, then the solver runs again with the cleaned dataset.

### 8.2 Geometry Fallbacks

| TAPs Available | Method |
|---------------|--------|
| 3+ | Weighted least-squares (primary) |
| 2 | Circle-circle intersection with range adjustment |
| 1 | Range-bearing estimation from previous position |

For the two-TAP case, the system computes the intersection of two circles (each centered on a TAP with radius equal to the estimated distance). If the circles don't intersect, ranges are scaled up to achieve 1% overlap. If one circle contains the other, only the stronger-signal measurement is used. When two intersection points exist, the one closer to the drone's previous estimated position is chosen.

### 8.3 Kalman Filtering

Estimated positions are smoothed with a constant-velocity Kalman filter. The state vector is `[x, y, vx, vy]` with adaptive process noise based on measurement uncertainty. Innovation gating rejects measurements exceeding 3 sigma from the prediction, preventing erratic position jumps from single bad RSSI readings.

---

## 9. RF Propagation Modeling

Distance estimation from RSSI uses the log-distance path loss model:

```
RSSI = RSSI_0 - 10 * n * log10(d / d_0) + X_sigma
```

Where:
- **RSSI_0** is the reference RSSI at distance d_0 (1 meter), set to -20 dBm for unknown drones
- **n** is the path loss exponent (environment-dependent)
- **d_0** is the reference distance (1 meter)
- **X_sigma** is the log-normal shadowing variance

### Environment Presets

| Environment | Path Loss Exponent (n) | Shadowing Sigma (dB) |
|------------|----------------------|---------------------|
| open_field | 1.8 | 6 |
| suburban | 2.4 | 7 |
| urban | 2.7 | 6 |
| dense_urban | 3.2 | 8 |
| indoor | 3.5 | 10 |

The `open_field` preset with n=1.8 was calibrated against nzyme measurements at the production deployment site. Per-TAP environment overrides and RSSI calibration offsets are configurable — for example, TAP-003 (MT7921U in passive mode) has a +16.0 dB offset because it reads approximately 16 dB weaker than the RTL8812AU adapters.

---

## 10. State Management and Drone Lifecycle

### 10.1 Sharded State

The Node's state manager holds all active and recently-lost drones in a 16-shard concurrent map. Each shard has its own read-write mutex, and shard assignment uses FNV-1a hashing on the drone identifier. Secondary indexes provide O(1) lookup by serial number and MAC address for deduplication.

### 10.2 Drone Lifecycle

```
SUSPECT CANDIDATE
  | WiFi frame matches detection pipeline, but identity unconfirmed
  |
  v (Single-TAP promotion: 5+ observations over 30+ seconds with mobility)
  v (Multi-TAP correlation: 2+ TAPs observe same drone within 30-second window)
  |
ACTIVE
  | Fully tracked, visible on map, alerts generated
  |
  v (No detection for lost_threshold_sec, default 1800 seconds)
  |
LOST
  | Grayed on map, track preserved
  |
  v (evict_after_min, default 0 = never evict)
  |
EVICTED (removed from memory, persists in database)
```

**Single-TAP mode** is essential for deployments with limited sensor coverage. A drone detected by only one TAP can be promoted from suspect to active after meeting minimum observation thresholds (configurable: default 5 observations over 30 seconds with RSSI variance indicating mobility).

### 10.3 Track Numbering

Each promoted drone receives a monotonically increasing track number (TRK-001, TRK-002, etc.) that persists across service restarts via the database. Track numbers provide a human-friendly identifier for operational communication.

### 10.4 Controller-UAV Linking

The system correlates DJI RC controllers with their associated drones using multiple signals: manufacturer match, temporal proximity (controller and drone detected within a short window), same TAP observation, and operator location similarity. Linked controllers show a visual association in the dashboard (e.g., "DJI RC -> TRK-014").

---

## 11. Real-Time Dashboard

The dashboard is a single-page progressive web application (PWA) embedded directly into the Go binary via `go:embed`. It requires no separate web server or build step — the dashboard is served from the same binary that processes detections.

### 11.1 Pages

| Page | Purpose |
|------|---------|
| **Dashboard** | Overview: threat level, active drone count, system health, recent alerts |
| **Airspace** | Full-screen tactical map with live drone markers, flight trails, range rings, MGRS grid overlay |
| **Fleet** | Sortable table of all tracked drones with signal strength, distance, classification, sightings history |
| **TAPs** | Sensor health matrix: packets/sec, temperature, uptime, kernel drops, channel status |
| **Alerts** | Alert feed with acknowledge/dismiss actions |
| **Analytics** | Historical trends, detection heatmaps, manufacturer distribution |
| **Admin** | User management, role assignment, TAP-to-user assignment |
| **Settings** | Coordinate format (DD/DMS/MGRS), controller visibility toggle, display preferences |

### 11.2 Real-Time Updates

The dashboard connects to the Node via WebSocket using a one-time ticket authentication scheme (see Section 14). Events are batched server-side in 100ms windows (max 50 events per batch) and sent as a single JSON array. Client-side updates are throttled to ~3 Hz per drone to prevent visual flashing on the map.

Event types pushed over WebSocket:

| Event | Trigger |
|-------|---------|
| `drone_new` | First detection of a new drone |
| `drone_update` | Position, signal, or status change |
| `drone_lost` | Drone lost contact |
| `tap_update` | TAP heartbeat received |
| `alert` | New alert generated |
| `system_refresh` | Full re-fetch signal (after NATS reconnect) |

### 11.3 Map Features

The airspace map (built on Leaflet.js) displays:

- **Drone markers** with manufacturer-specific icons, color-coded by classification
- **Flight trails** showing recent position history
- **Range rings** showing RSSI-estimated distance from each detecting TAP, with uncertainty bands
- **Estimated position** from trilateration (when available) with error ellipse
- **Operator location** markers (from Remote ID System messages)
- **TAP positions** with coverage indicators
- **MGRS grid overlay** for military coordinate reference

---

## 12. Alerting and Notifications

### 12.1 Alert Types

| Alert | Trigger | Priority |
|-------|---------|----------|
| New drone | First detection of unknown aircraft | Normal |
| Spoof detected | Trust score drops below 20 or critical flags detected | High |
| Drone lost | No detection for `lost_threshold_sec` | Normal |
| TAP offline | Missing heartbeat for 90+ seconds | High |
| TAP online | TAP reconnects after being offline | Normal |

### 12.2 Telegram Integration

Alerts are optionally forwarded to a Telegram channel via the Bot API. Messages are formatted with rich HTML including drone identification, coordinates (in both decimal degrees and MGRS), classification, and timestamp. Rate limiting prevents message spam (minimum 500ms between sends). Each alert type can be independently enabled or disabled through the dashboard settings.

---

## 13. Persistence and Analytics

### 13.1 Database Schema

PostgreSQL 16 stores four primary tables:

- **drones**: Persistent registry of all detected drones with current state, classification, track number, and last-known position.
- **detections**: Time-series of individual detection events with position, RSSI, channel, source, trust score, and spoof flags. Rate-limited to prevent table explosion from stationary drones (skips insert if no position change within 30 seconds).
- **tap_stats**: Hourly snapshots of TAP health metrics.
- **alerts**: Alert history with acknowledgment state.

### 13.2 Sightings History

The API provides a sightings history endpoint that groups a drone's detections into discrete sessions separated by 5-minute gaps. Each session shows start/end time, duration, detection count, altitude range, speed range, signal strength range, and which TAPs observed the drone.

### 13.3 Redis Caching

Redis serves as a fast cache for frequently-accessed state: WebSocket tickets (30-second TTL), rate limiter counters, and session data. Configured with 512 MB maximum memory and `allkeys-lru` eviction policy.

---

## 14. Security Model

### 14.1 Authentication

The system uses JWT-based authentication with httpOnly cookies. Tokens are issued on login with a configurable expiry (default: 1 year for operational continuity). Passwords are stored as bcrypt hashes with a minimum 8-character requirement.

### 14.2 WebSocket Authentication

WebSocket connections use a one-time ticket scheme rather than passing JWTs in the URL (which would expose tokens in server logs and browser history):

1. Client POSTs to `/api/auth/ws-ticket` with a valid session cookie
2. Server issues a random ticket valid for 30 seconds
3. Client connects to `ws://host:8081/ws?ticket=XXX`
4. Server validates the ticket (one-time use), attaches the authenticated user context to the WebSocket connection

### 14.3 RBAC

Three roles with hierarchical permissions:

| Role | Capabilities |
|------|-------------|
| **Admin** | Full access: user management, system configuration, data operations |
| **Operator** | Drone tagging, classification, alert acknowledgment, TAP commands |
| **Viewer** | Read-only access to all drone, map, and analytics data |

### 14.4 Security Headers

All HTTP responses include:
- Content-Security-Policy restricting script/style/image sources
- X-Content-Type-Options: nosniff
- X-Frame-Options: DENY
- Referrer-Policy: strict-origin-when-cross-origin
- CSRF token validation on state-changing requests
- Rate limiting: 100 requests/second per IP

---

## 15. Deployment and Operations

### 15.1 Node Deployment

The processing node runs on Rocky Linux 9 with the following stack:

| Component | Version | Configuration |
|-----------|---------|---------------|
| PostgreSQL | 16 | shared_buffers=2GB, effective_cache=8GB, work_mem=64MB |
| Redis | 7 | 512MB maxmemory, allkeys-lru |
| NATS | 2.10.24 | max_payload 1MB, 256 max connections |
| Go | 1.24 | CGO_ENABLED=0, trimpath |

System tuning includes: network-latency tuned profile, somaxconn=8192, TCP buffers at 16MB, swappiness=1, and 65536 file descriptor limits.

An automated install script (`scripts/install-node.sh`) takes a fresh Rocky Linux 9 box from zero to a fully running system in under 10 minutes, handling all dependency installation, database creation, system tuning, firewall configuration, and service setup.

### 15.2 TAP Deployment

Each TAP runs Arch Linux on Raspberry Pi 5 with:
- Patched rtw88 kernel module (for Realtek adapters)
- systemd service with watchdog
- Tailscale VPN for connectivity to the Node
- Automatic restart on crash or stuck capture

### 15.3 Networking

All inter-component communication runs over Tailscale, providing encrypted point-to-point connectivity without exposing services to the public internet. The firewall restricts the public interface to necessary ports (HTTP, NATS, WebSocket, NATS monitoring) while placing the Tailscale interface in a trusted zone.

---

## 16. Field Validation Results

### 16.1 Comparison with nzyme

In a side-by-side test conducted February 18, 2026, Skylens was compared against nzyme (a commercial open-source WiFi-based detection system) monitoring the same airspace:

| Metric | Skylens | nzyme |
|--------|---------|-------|
| First detection (Mavic 3) | T+0s | T+30s |
| Update rate | ~3 Hz (WebSocket) | ~0.5 Hz (polling) |
| Protocol coverage | RemoteID + DJI DroneID + OcuSync + BLE | RemoteID only |

### 16.2 Detection Results

| Drone | Detections | Signal | Notes |
|-------|-----------|--------|-------|
| DJI Mavic 3 | 20 | -69 to -78 dBm | Strong, consistent detection |
| DJI Phantom 4 Pro V2 | 6 | -95 dBm | Weak signal, near noise floor |
| DJI Air 2S | 2 | -95 dBm | Barely caught |
| DJI Mavic 2 | 0 | N/A | No RemoteID firmware installed |
| DJI Controllers (19) | 1,261 | -50 to -97 dBm | 28 unique MACs across 19 controllers |

### 16.3 Channel Intelligence

All drone detections occurred on channel 6 only. Controller detections were distributed across 5 GHz channels: 149 (65%), 132 (20%), 116 (9%). This confirmed the optimal channel plan of `[1, 6, 11, 116, 132, 149]` with heavy dwell on channel 6.

### 16.4 Production Statistics (as of March 2026)

- **49 unique UAVs** detected and tracked (plus 189 controllers)
- **Manufacturers observed**: DJI (dominant), Autel
- **Models identified**: Mavic 3 variants, Air 2S, Air 3, Air 3S, Phantom 4 Pro, Inspire 3, Matrice 4T, Matrice 30, Avata, FPV, Mavic 2 Pro, Neo 2, Mini SE, Mini 2 SE
- **3 TAPs online 24/7** with zero NATS disconnects and zero packet drops
- **Detection rate**: 42-74 packets per second across all TAPs

---

## 17. Performance Characteristics

### 17.1 Node Resource Usage

Measured on Rocky Linux 9 VM (8 vCPU Xeon Gold 6130, 16GB RAM):

| Metric | Value |
|--------|-------|
| Load average | 0.04 |
| Process memory (skylens-node) | 27 MB |
| Total RAM usage | 845 MB / 15.7 GB (5%) |
| CPU utilization | <1% |
| Swap usage | 0 |

### 17.2 Latency

| Path | Latency |
|------|---------|
| Frame capture to NATS publish | <10 ms |
| NATS to Node processing | <5 ms |
| Processing to WebSocket broadcast | <50 ms |
| End-to-end (capture to dashboard) | <100 ms |

### 17.3 Concurrency Model

| Component | Strategy | Rationale |
|-----------|----------|-----------|
| State manager | 16-shard FNV-1a map | Distributes lock contention |
| Spoof detector | 16-shard track map | Independent trust scoring |
| NATS receiver | 8 DB worker goroutines | Bounded concurrency for persistence |
| WebSocket | 100ms batch window | Reduces per-event overhead |
| TAP capture | Dedicated OS thread | pcap blocking reads can't use async |

---

## 18. Lessons Learned

### 18.1 Realtek Driver Issues

The RTL8812AU and RTL8814AU chipsets in their stock driver configuration silently drop all WiFi beacon frames in monitor mode. This is a hardware register configuration issue in the rtw88 driver, not a kernel bug. The fix requires using a specific firmware lineage (v33.6.0) and rebuilding the rtw88 module against kernel 6.12+. Without this patch, these popular and affordable adapters are useless for drone detection.

### 18.2 DJI Encryption

DJI's mid-2023 firmware updates encrypted most of the DroneID payload, rendering direct GPS extraction impossible for updated drones. However, the serial number and product type fields remain unencrypted, so identification still works. Position data for encrypted drones comes from the separate ASTM Remote ID broadcast, which DJI implemented in parallel.

### 18.3 MAC Randomization

DJI controllers randomize their MAC addresses on WiFi reconnect. A single controller can present 4+ different MAC addresses in a session. The SSID (PROJ + 6 hex chars) remains stable and is the reliable identifier. This required the system to use SSID as the primary key for controller tracking rather than MAC address.

### 18.4 Channel 6 Dominance

Despite the theoretical frequency diversity of DJI DroneID (five frequencies spanning 2.4 GHz), every single drone detection in our deployment was captured on channel 6. The NAN discovery protocol mandates channel 6 as the primary discovery channel, and Remote ID beacons use NAN as the transport. This means channel 6 dwell time is the single most important parameter for detection probability.

### 18.5 Co-located Sensors

TAP-002 and TAP-003 are physically located at the same position (within GPS error). This creates degenerate geometry for trilateration — two circles centered at the same point. The system handles this with a circle-intersection fallback that merges co-located TAP measurements using the stronger signal's distance estimate.

---

## 19. Future Work

**5.8 GHz DJI DroneID capture.** Documentation suggests DJI DroneID may be broadcast at 5756.5 MHz, but this is unverified. Confirming and capturing on this frequency would add a second detection channel for DJI drones.

**3D trilateration.** Current position estimation is 2D (latitude/longitude). With 4+ TAPs at different elevations, altitude estimation from RSSI becomes feasible.

**Drone trajectory prediction.** The Kalman filter's velocity state could be used to project future positions, enabling predictive alerting (e.g., "drone will enter restricted airspace in 30 seconds").

**Passive radar integration.** Combining WiFi-based detection with passive radar (using ambient RF signals like DVB-T or LTE) could detect drones that don't broadcast any WiFi signals at all.

**Machine learning classification.** The current rule-based fingerprinting system (2,600+ regex patterns, 100+ OUI entries) could be augmented with a trained classifier on raw beacon fingerprints for better identification of unknown drone types.

---

## Appendix A: Protocol Reference

### A.1 Protobuf Detection Message

The Detection protobuf message transmitted from TAP to Node contains 40+ fields:

```
tap_id, timestamp_ns, mac_address, session_id, rssi, channel, frequency_mhz,
ssid, beacon_interval_tu, manufacturer, designation, confidence, identifier,
serial_number, latitude, longitude, altitude_geodetic, altitude_pressure,
height_agl, speed, vertical_speed, heading, utm_id, registration,
operator_latitude, operator_longitude, operator_altitude,
operator_location_type, operational_status, uav_category, uav_type,
detection_source, is_controller, track_direction, model, raw_frame
```

### A.2 NATS Subject Hierarchy

```
skylens.detections.{tap_id}     TAP -> Node     Detection events
skylens.heartbeats.{tap_id}     TAP -> Node     Health telemetry (5s interval)
skylens.commands.{tap_id}       Node -> TAP     Targeted commands
skylens.commands.broadcast      Node -> All      Broadcast commands
```

### A.3 Configuration Reference

**Node (YAML):** server, nats, database, redis, detection, propagation, auth, telegram

**TAP (TOML):** tap (id, name, position), capture (interface, channels, hop_interval, dedup), nats (url, buffer), ble (enabled, adapter), logging (level)

---

*This document describes Skylens as deployed and validated in production as of March 2026.*
