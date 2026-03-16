# Skylens TAP — Field Sensor

High-performance Wi-Fi drone detection sensor written in Rust. Captures raw 802.11 frames via libpcap, identifies drones through 8 independent detection checks, decodes OpenDroneID (ASTM F3411) and DJI DroneID protocols, and publishes detections over NATS using Protocol Buffers.

Designed for Raspberry Pi 5 with external USB Wi-Fi adapters in monitor mode.

```
  Wi-Fi Adapter (monitor mode)
          │
     ┌────┴────┐
     │ libpcap │
     │ capture │
     └────┬────┘
          │
  ┌───────┴───────┐
  │  802.11 Frame │
  │    Parser     │
  └───────┬───────┘
          │
  ┌───────┴───────┐
  │   8-Check     │
  │   Detection   │
  │   Engine      │
  └───────┬───────┘
   │ │ │ │ │ │ │ │
  OUI SS RID DJI PAR AUT MFR BHV
          │
  ┌───────┴───────┐
  │   Protobuf    │
  │   Encode      │
  └───────┬───────┘
          │
  ┌───────┴───────┐
  │ NATS Publish  │──────> Skylens Node
  └───────────────┘
```

---

## Table of Contents

- [Quick Start](#quick-start)
- [Hardware Requirements](#hardware-requirements)
- [Installation](#installation)
- [Configuration](#configuration)
- [Detection Pipeline](#detection-pipeline)
- [Protocol Decoders](#protocol-decoders)
- [Channel Strategy](#channel-strategy)
- [NATS Protocol](#nats-protocol)
- [Systemd Service](#systemd-service)
- [Passive Mode](#passive-mode)
- [Troubleshooting](#troubleshooting)
- [Performance](#performance)
- [Project Structure](#project-structure)

---

## Quick Start

```bash
# On the Raspberry Pi — clone the monorepo
git clone https://github.com/K13094/skylens.git
cd skylens/tap

# Install system dependencies (Raspberry Pi OS / Debian)
sudo apt update && sudo apt install -y \
    libpcap-dev protobuf-compiler build-essential git iw tshark

# Install Rust (if not already installed)
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source $HOME/.cargo/env

# Build
cargo build --release
# Binary: target/release/skylens-tap (~4.3MB)

# Configure
sudo mkdir -p /etc/skylens
sudo cp config.toml /etc/skylens/config.toml
# Edit: set tap_id, GPS coordinates, Wi-Fi interface, NATS URL

# Run (requires root for monitor mode)
sudo ./target/release/skylens-tap /etc/skylens/config.toml
```

### Deploying to a Remote Pi

You don't need the full repo on every Pi. Build once, deploy the binary:

```bash
# On your build machine
cd skylens/tap && cargo build --release

# Copy binary + config + intel DB to the target Pi
scp target/release/skylens-tap pi@remote:/home/tap/skylens-tap
scp config.toml pi@remote:/etc/skylens/config.toml
scp intel/drone_models.json pi@remote:/home/tap/intel/drone_models.json

# SSH in and run
ssh pi@remote
sudo /home/tap/skylens-tap /etc/skylens/config.toml /home/tap/intel/drone_models.json
```

---

## Hardware Requirements

### Raspberry Pi 5

- **RAM**: 4GB+ recommended
- **OS**: 64-bit Raspberry Pi OS (Bookworm) or Debian 12+
- **Network**: Must reach the NATS server (Tailscale or direct LAN)
- **Power**: Standard USB-C power supply. Outdoor deployments may use PoE HAT or battery

### Wi-Fi Adapter Selection

| Adapter | Chipset | Driver | Patch Required | Recommendation |
|---------|---------|--------|----------------|----------------|
| **ALFA AWUS036ACHM** | MT7612U | mt76 (mainline) | No | **Best choice** — zero setup |
| **MT7921U dongles** | MT7921U | mt7921u (mainline) | No | Wi-Fi 6, NAN capable, RemoteID via NAN |
| **ALFA AWUS036ACH** | RTL8812AU | 88XXau (DKMS) | **Yes** | Good range, needs beacon fix patch |
| **ALFA AWUS1900** | RTL8814AU | 8814au (DKMS) | **Yes** | 4x4 MIMO, best range, needs beacon fix patch |

#### The Realtek Problem

Realtek RTL8812AU and RTL8814AU adapters have a hardware register bug that silently drops **all beacon frames** in monitor mode. Since RemoteID, DroneID, and SSID identification all depend on beacons, an unpatched Realtek adapter will detect almost nothing.

The fix is a one-line driver patch. Full instructions: **[docs/REALTEK_BEACON_FIX.md](docs/REALTEK_BEACON_FIX.md)**

#### MediaTek vs Realtek

| Feature | MediaTek (MT7612U / MT7921U) | Realtek (RTL8812AU / RTL8814AU) |
|---------|------------------------------|----------------------------------|
| Monitor mode | Works out of the box | Requires DKMS driver + patch |
| Beacon capture | Yes | Only with patch |
| NAN frames | MT7921U only | No (firmware drops them) |
| RemoteID path | NAN + Beacon | tshark fallback only |
| Range | Good | Better (RTL8814AU 4x4 MIMO) |
| Driver stability | Excellent | Occasional firmware hangs |

> **Bottom line**: MediaTek for ease of setup. Realtek for maximum range — if you patch the driver.

---

## Installation

See **[docs/INSTALL.md](docs/INSTALL.md)** for the complete step-by-step installation guide covering:

1. System package installation
2. Wi-Fi driver setup (MediaTek and Realtek)
3. Realtek beacon fix patch
4. Rust toolchain installation
5. Building from source
6. Configuration
7. Systemd service setup
8. Verification and testing
9. Multi-TAP deployment

---

## Configuration

### config.toml

```toml
[tap]
id = "tap-001"                    # Unique ID (appears in NATS subjects)
name = "North Sensor"             # Human-readable name
latitude = 40.7128                # GPS latitude (WGS84)
longitude = -74.0060              # GPS longitude (WGS84)

[capture]
interface = "wlan1"               # Wi-Fi interface name
channels = [1, 6, 11, 36, 40, 44, 48, 149, 153, 157, 161, 165]
hop_interval_ms = 200             # Channel dwell time (ms)
# passive = true                  # Set true if another tool owns the interface
# force_iw = true                 # Force iw subprocess (RTL8814AU workaround)
# mac_denylist = ["AA:BB:CC:DD:EE:FF"]  # Suppress known false positive MACs

[nats]
url = "nats://your-node-ip:4222"

[nats.buffer]
max_size = 10000                  # Max buffered detections when NATS is offline
max_retries = 100                 # Max retry attempts per detection
initial_retry_delay_ms = 1000     # Exponential backoff start
max_retry_delay_ms = 30000        # Backoff cap
warning_threshold = 0.8           # Log warning at 80% buffer capacity

[logging]
level = "info"                    # trace, debug, info, warn, error
```

### Configuration Reference

| Section | Key | Description | Default |
|---------|-----|-------------|---------|
| `tap` | `id` | Unique TAP identifier | Required |
| `tap` | `name` | Human-readable display name | Required |
| `tap` | `latitude` | GPS latitude (WGS84 degrees) | Required |
| `tap` | `longitude` | GPS longitude (WGS84 degrees) | Required |
| `capture` | `interface` | Wi-Fi interface name | Required |
| `capture` | `channels` | Channel list to hop through | 2.4 + 5 GHz |
| `capture` | `hop_interval_ms` | Time spent on each channel (ms) | `200` |
| `capture` | `passive` | Don't set monitor mode or hop channels | `false` |
| `capture` | `force_iw` | Use `iw` subprocess instead of nl80211 | `false` |
| `capture` | `mac_denylist` | MAC addresses to suppress | `[]` |
| `nats` | `url` | NATS server URL | Required |
| `nats.buffer` | `max_size` | Offline detection buffer size | `10000` |
| `nats.buffer` | `warning_threshold` | Buffer usage warning level | `0.8` |
| `logging` | `level` | Log verbosity | `info` |

---

## Detection Pipeline

Every captured 802.11 management frame runs through 8 independent checks. If **any** check fires, a `Detection` protobuf message is published to NATS.

| # | Check | Source Data | What It Matches |
|---|-------|-------------|-----------------|
| 1 | **OUI Match** | Source MAC + BSSID | 58 IEEE-verified drone manufacturer OUI prefixes |
| 2 | **SSID Match** | Beacon/Probe Response SSID | 333 regex patterns for drone Wi-Fi networks |
| 3 | **OpenDroneID** | Vendor IE (ASTM F3411) | Standard RemoteID broadcasts (Wi-Fi Beacon + NAN) |
| 4 | **DJI DroneID** | Vendor IE (DJI proprietary) | DJI OcuSync telemetry (serial, GPS, altitude, home point) |
| 5 | **Parrot IE** | Vendor IE (Parrot) | Parrot drone identification |
| 6 | **Autel IE** | Vendor IE (Autel) | Autel drone identification |
| 7 | **OUI Manufacturer** | Source MAC | Skydio, Yuneec, and other OUI-only identifiable drones |
| 8 | **Behavioral** | Multi-signal | Unknown drones via channel activity, SSID patterns, beacon intervals, MAC analysis |

### MAC Randomization Protection

Locally-administered MACs (LAA bit set in first octet) are excluded from OUI matching. This prevents false positives from phones and laptops whose randomized MACs happen to collide with drone vendor OUI prefixes (e.g., `26:37:12` matches a DJI vendor IE OUI but is almost always a randomized phone MAC).

### Confidence Scores

| Score | Detection Method |
|-------|-----------------|
| 0.20 - 0.30 | Behavioral suspect only |
| 0.30 | OUI match (known drone manufacturer MAC) |
| 0.40 | SSID regex match |
| 0.50 | OUI + SSID combined |
| 0.55 | DJI SSID model match (e.g., `DJI-MAVIC3-xxxx`) |
| 0.60 | Vendor IE match (Parrot, Autel, Skydio) |
| 0.80 | DJI DroneID decoded (no GPS fix) |
| 0.85 | OpenDroneID decoded (no GPS fix) |
| 0.90 | DJI DroneID with GPS coordinates |
| 0.95 | OpenDroneID with GPS coordinates |

---

## Protocol Decoders

### OpenDroneID (ASTM F3411)

The FAA-mandated Remote Identification standard. Drones broadcast identity and telemetry via Wi-Fi beacons or NAN (Neighbor Awareness Networking) action frames.

**Supported message types:**

| Type | Data |
|------|------|
| BasicID | Serial number, UA type, ID type |
| Location | Latitude, longitude, altitude (geodetic + pressure), speed, heading, vertical speed |
| System | Operator position, area count, classification |
| OperatorID | Operator registration ID |
| SelfID | Free-text description |
| Auth | Authentication data |
| Message Pack | Multiple messages in a single IE |

**Coordinate decoding**: `int32 / 1e7` = WGS84 degrees

**NAN parsing**: Wi-Fi NAN Service Discovery frames carry RemoteID data on channel 6 (2.4 GHz) and channel 149 (5 GHz). The TAP parses NAN action frames and extracts the ODID payload after skipping the 5-byte NAN header (OUI[3] + type[1] + msg_counter[1]).

### DJI DroneID (OcuSync)

DJI's proprietary protocol embedded in vendor-specific information elements.

**Supported formats:**
- **OcuSync format**: OUI `60:60:1F`, type `0x13`
- **Legacy format**: OUI `26:37:12`

**Decoded subcommands:**
- **Flight Registration (0x10)**: Serial number, GPS coordinates, altitude, velocity, heading, home position
- **Flight Purpose (0x11)**: Additional flight metadata

**Coordinate decoding**: `int32 / 174533.0` = radians → degrees

**Model identification**: 68+ DJI product types identified by model code lookup (Mavic 3, Mini 4 Pro, Air 2S, Phantom 4, Matrice 300, etc.)

---

## Channel Strategy

### Recommended Channel Plans

**Minimum viable** (6 channels, fast cycle):
```toml
channels = [1, 6, 11, 149, 153, 157]
```

**Standard** (12 channels, good coverage):
```toml
channels = [1, 6, 11, 36, 40, 44, 48, 149, 153, 157, 161, 165]
```

**Full spectrum** (36 channels, maximum detection):
```toml
channels = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165]
```

### Key Frequencies

| Frequency | Why It Matters |
|-----------|---------------|
| **Channel 6** (2.437 GHz) | Wi-Fi NAN discovery channel. Most RemoteID beacons land here. **Highest priority.** |
| **Channel 149** (5.745 GHz) | 5 GHz NAN discovery channel. DJI controller comms frequently here. |
| **Channels 116, 132** | DJI OcuSync video link channels (real-world verified) |
| **DFS channels (52-144)** | Required for complete coverage. Some drones use DFS frequencies. |

### Timing Considerations

- **Cycle time** = `hop_interval_ms × number of channels`
- With 36 channels at 200ms = 7.2s full cycle
- **Timing aliasing warning**: If `hop_interval_ms × channels = 5000ms`, the heartbeat always samples the same channel, giving misleading stats. Avoid exact multiples of 5 seconds.
- Priority channels (6, 149) automatically get 2x dwell time

---

## NATS Protocol

All messages are Protocol Buffer encoded. Schema defined in [`../proto/skylens.proto`](../proto/skylens.proto).

| Subject | Direction | Message | Rate |
|---------|-----------|---------|------|
| `skylens.detections.{tap_id}` | TAP → Node | `Detection` | Per detection event |
| `skylens.heartbeats.{tap_id}` | TAP → Node | `TapHeartbeat` | Every 5s |
| `skylens.commands.{tap_id}` | Node → TAP | `TapCommand` | On demand |
| `skylens.commands.broadcast` | Node → All TAPs | `TapCommand` | On demand |
| `skylens.acks.{tap_id}` | TAP → Node | `TapCommandAck` | Reply to commands |

### Reliable Delivery

Detections are buffered during NATS outages and replayed with exponential backoff on reconnect:

- Buffer holds up to 10,000 detections (configurable)
- Initial retry delay: 1 second, max: 30 seconds
- Warning logged at 80% buffer capacity
- Heartbeats are **not** buffered — only the latest one matters

### Deduplication

The TAP deduplicates detections within a configurable window (default: 1000ms per MAC). The same drone detected multiple times within the window only generates one NATS message.

### Commands

The TAP subscribes to `skylens.commands.{tap_id}` and `skylens.commands.broadcast` for remote management:

| Command | Effect |
|---------|--------|
| **Ping** | TAP responds with `TapCommandAck` including round-trip `latency_ns` |
| **SetChannels** | Hot-reload channel list and hop interval without restarting capture |
| **SetFilter** | Change BPF capture filter (default: `type mgt` for management frames only) |
| **Restart** | Graceful (drain NATS connections) or hard restart |
| **UpdateConfig** | Hot-reload key-value configuration pairs |

---

## Systemd Service

```bash
sudo tee /etc/systemd/system/skylens-tap.service << 'EOF'
[Unit]
Description=Skylens TAP - Drone Detection Sensor
After=network.target
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=notify
User=root
WorkingDirectory=/home/tap
ExecStart=/home/tap/skylens-tap /etc/skylens/config.toml /home/tap/intel/drone_models.json
Restart=always
RestartSec=5
WatchdogSec=30
Environment=RUST_LOG=info

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable skylens-tap
sudo systemctl start skylens-tap
```

### Systemd Notes

| Setting | Why |
|---------|-----|
| `Type=notify` | Binary sends `sd_notify(READY=1)` when capture starts successfully |
| `WatchdogSec=30` | Binary must send `WATCHDOG=1` pings every <15s or systemd restarts it |
| `RestartSec=5` | Wait 5s before restarting after crash |
| `StartLimitBurst=10` | Allow up to 10 restarts in 5 minutes before giving up |

> **Important**: The binary uses `sd_notify::notify(false, ...)` internally. Passing `true` unsets `$NOTIFY_SOCKET` and breaks all subsequent watchdog pings, causing systemd to kill the process after 30s.

---

## Passive Mode

Set `capture.passive = true` when another program already controls the Wi-Fi interface:

```toml
[capture]
passive = true
```

In passive mode:
- Monitor mode setup is **skipped** (the interface must already be in monitor mode)
- Channel hopper is **not started** (channel info comes from radiotap headers)
- TAP reads frames passively from the shared interface

This is useful when running alongside other capture tools (nzyme, Kismet, airodump-ng, etc.) that already manage the interface and channel hopping.

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `packets=0 pps=0` | Adapter not in monitor mode | Check `iw dev wlan1 info` shows `type monitor` |
| No beacons in tshark test | Realtek beacon filter bug | Apply patch from [docs/REALTEK_BEACON_FIX.md](docs/REALTEK_BEACON_FIX.md) |
| Service dies after 30s | Watchdog failure | Verify `sd_notify(false, ...)` not `true` in code |
| Low detection count | Missing channels | Add DFS channels (52-144) and priority channels (6, 149) |
| NATS errors in logs | Network connectivity | Check VPN status, test with `nc -zv <nats-ip> 4222` |
| `buffer_size > 0` in heartbeat | NATS disconnected | Detections are buffering locally. Check network connectivity. |
| Same channel in every heartbeat | Timing aliasing | Change `hop_interval_ms` to avoid cycle time = 5000ms multiple |
| High CPU usage | Too many channels, low dwell | Increase `hop_interval_ms` or reduce channel count |
| `Permission denied` on capture | Not running as root | Use `sudo` or set capabilities: `setcap cap_net_raw+ep skylens-tap` |

### Verification Commands

```bash
# Service status
sudo systemctl status skylens-tap

# Live logs
sudo journalctl -u skylens-tap -f

# Verify capture is running (watch for pps > 0)
sudo journalctl -u skylens-tap --no-pager -n 10 | grep Stats

# Test beacon capture (should show beacon frames)
sudo tshark -i wlan1 -c 50 -Y "wlan.fc.type_subtype == 0x08"

# Verify NATS connectivity
nc -zv <your-nats-ip> 4222

# Check adapter info
iw dev wlan1 info

# Watchdog survival test (wait 35+ seconds)
sleep 35 && sudo systemctl is-active skylens-tap
```

### Healthy Log Output

```
INFO skylens_tap: Starting Skylens TAP tap-001
INFO skylens_tap::capture::channel: Set wlan1 to monitor mode
INFO skylens_tap: BPF filter set: type mgt
INFO skylens_tap: Capture running, publishing detections to NATS
INFO skylens_tap: Stats packets=500 pps=42 detections=12 dps=1 nats_sent=12 nats_errors=0
```

### Unhealthy Log Output

```
# No packets — adapter/driver problem
INFO skylens_tap: Stats packets=0 pps=0 detections=0

# Watchdog death — sd_notify issue
systemd[1]: skylens-tap.service: Watchdog timeout (limit 30s)!

# Channel regulatory restriction — safe to ignore
WARN skylens_tap::capture::channel: Channel 144 not available (regulatory)
```

---

## Performance

| Metric | Value |
|--------|-------|
| **Binary size** | ~4.3 MB (release, LTO, stripped) |
| **Memory** | ~12 MB RSS typical |
| **CPU** | <5% on Raspberry Pi 5 at ~150 frames/sec |
| **Frame rejection** | <1 microsecond per non-matching frame |
| **Snaplen** | 2048 bytes (management frames are 100-500 bytes) |
| **Channel switching** | nl80211 netlink (primary), `iw` subprocess (fallback) |
| **5 GHz channel width** | 80 MHz wide-channel capture |

---

## Intel Database

The `intel/drone_models.json` file contains all detection signatures used by the 8-check engine:

| Category | Count | Description |
|----------|-------|-------------|
| **OUI prefixes** | 58 | IEEE-verified drone manufacturer MAC prefixes |
| **SSID patterns** | 333 | Regex patterns for drone Wi-Fi networks |
| **Serial prefixes** | 146 | Serial number to manufacturer/model mapping |
| **DJI model codes** | 68 | DJI product type identification codes |
| **DJI SSID models** | 90 | DJI SSID-based model identification patterns |

Update the database with the latest IEEE OUIs using the intel updater tool in the monorepo root:

```bash
cd skylens
go build -o intel-updater ./cmd/intel-updater/
./intel-updater
```

---

## Project Structure

```
tap/
├── Cargo.toml                  # Dependencies and build configuration
├── build.rs                    # Protobuf code generation (uses ../proto/skylens.proto)
├── config.toml                 # Configuration template
├── intel/
│   └── drone_models.json       # Detection signature database
├── deploy/
│   └── tap-003/                # Example deployment files (systemd, config, proxy)
├── docs/
│   ├── INSTALL.md              # Full installation guide
│   ├── REALTEK_BEACON_FIX.md   # Critical Realtek driver patch instructions
│   └── TAP_OPERATIONS_BRIEF.md # Operations quick reference
└── src/
    ├── main.rs                 # Entry point: capture loop, 8-check engine, heartbeat
    ├── config.rs               # TOML configuration loader
    ├── capture/
    │   ├── pcap.rs             # libpcap wrapper (snaplen=2048, BPF filter)
    │   ├── channel.rs          # Channel hopper with priority dwell (ch6, ch149)
    │   ├── nl80211.rs          # Netlink nl80211 fast channel switching + iw fallback
    │   └── ble.rs              # BLE advertisement capture (OpenDroneID over BT)
    ├── decode/
    │   ├── frame.rs            # 802.11 frame parser (radiotap, IEs, NAN SDF, FCS)
    │   ├── dji.rs              # DJI DroneID decoder (Flight Reg 0x10, Purpose 0x11)
    │   ├── remoteid.rs         # OpenDroneID / ASTM F3411 decoder (6 msg types + packs)
    │   ├── oui.rs              # Intel DB loader (OUI/SSID/serial/DJI model matching)
    │   └── baseline.rs         # BSSID baseline tracker with atomic metrics
    └── publish/
        ├── nats.rs             # NATS protobuf publisher with command subscription
        └── buffer.rs           # Offline detection buffer with exponential backoff
```

---

## Updating

```bash
# Pull latest code
cd skylens/tap
git pull

# Rebuild
cargo build --release

# Restart service
sudo systemctl restart skylens-tap

# Verify
sudo journalctl -u skylens-tap -f
```

For remote TAPs without the full repo:

```bash
# Build on your dev machine, then copy
scp target/release/skylens-tap pi@remote:/home/tap/skylens-tap
scp intel/drone_models.json pi@remote:/home/tap/intel/drone_models.json
ssh pi@remote sudo systemctl restart skylens-tap
```
