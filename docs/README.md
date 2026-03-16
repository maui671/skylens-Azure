# Skylens

**Real-time Wi-Fi drone detection and airspace monitoring.**

Skylens detects, identifies, tracks, and classifies unmanned aerial vehicles using distributed Raspberry Pi sensors and a central command node. It decodes standard RemoteID (ASTM F3411), proprietary DJI DroneID, and identifies unknown drones through Wi-Fi fingerprinting and behavioral analysis.

```
                           ┌─────────────────────────────┐
                           │       SKYLENS NODE (Go)     │
                           │                             │ 
    ┌──────────┐           │  ┌────────┐  ┌───────────┐  │         ┌──────────────┐
    │  TAP-001 │──NATS──>──│  │  Intel │  │   Spoof  │   │────────>│  Dashboard   │
    │  (Pi 5)  │  protobuf │  │ Engine │  │ Detector │   │  HTTP   │  (Browser)   │
    └──────────┘           │  └────┬───┘  └─────┬─────┘  │         │              │
                           │       │            │        │         │  Live Map    │
    ┌──────────┐           │  ┌────┴────────────┴─────┐  │  WS     │  Alerts      │
    │  TAP-002 │──NATS──>──│  │    State Manager      │──│────────>│  Fleet View  │
    │  (Pi 5)  │  protobuf │  │  (sharded, real-time) │  │         │  Analytics   │
    └──────────┘           │  └────┬──────────────────┘  │         └──────────────┘
                           │       │                     │
    ┌──────────┐           │  ┌────┴─────┐  ┌────────┐   │
    │  TAP-003 │──NATS──>──│  │PostgreSQL│  │ Redis  │   │
    │  (Pi 5)  │  protobuf │  │(history) │  │(cache) │   │
    └──────────┘           │  └──────────┘  └────────┘   │
                           └─────────────────────────────┘
```

---

## Table of Contents

- [How It Works](#how-it-works)
- [What It Detects](#what-it-detects)
- [Detection Pipeline](#detection-pipeline)
- [Dashboard](#dashboard)
- [Getting Started](#getting-started)
  - [Node Setup](#1-node-setup)
  - [TAP Setup](#2-tap-setup)
- [Configuration](#configuration)
- [NATS Protocol](#nats-protocol)
- [API Reference](#api-reference)
- [Docker Deployment](#docker-deployment)
- [Development](#development)
- [Project Structure](#project-structure)
- [License](#license)

---

## How It Works

Skylens operates as a distributed sensor network. **TAPs** (Tactical Access Points) are Raspberry Pi 5s with USB Wi-Fi adapters in monitor mode. They passively capture 802.11 management frames, run them through an 8-check detection engine, and publish matches over NATS using Protocol Buffers.

The **Node** is the central processor. It receives detections from all TAPs, enriches them with manufacturer/model identification, estimates distance via RSSI propagation modeling, performs multi-TAP trilateration when possible, runs spoof detection and trust scoring, and maintains real-time drone state. Everything is streamed to the browser dashboard over WebSocket.

### Key Capabilities

- **Multi-protocol detection**: RemoteID (ASTM F3411), DJI DroneID (OcuSync), Wi-Fi beacons, NAN, BLE
- **68+ DJI models identified** by product type code from DroneID telemetry
- **333 SSID patterns** and **58 OUI prefixes** for Wi-Fi fingerprinting
- **RSSI-based distance estimation** with per-model RF calibration and per-environment propagation tuning
- **Multi-TAP trilateration** using weighted least squares (3+ TAPs) or circle intersection (2 TAPs)
- **Spoof detection** with trust scoring: coordinate jumps, OUI/SSID mismatch, impossible speed, RSSI anomalies
- **5-tier classification**: HOSTILE / SUSPECT / UNKNOWN / NEUTRAL / FRIENDLY
- **Telegram alerts** for new drone detections, spoof events, and zone violations
- **Geofence zones** with real-time violation alerts on the map
- **Sightings history** with session grouping and detailed per-drone analytics
- **PWA support** for mobile access in the field

---

## What It Detects

| Protocol | Method | Data Extracted |
|----------|--------|----------------|
| **OpenDroneID** (ASTM F3411) | Beacon vendor IE, NAN action frames, tshark | Serial, GPS, speed, heading, altitude, operator position |
| **DJI DroneID** (OcuSync) | Proprietary vendor IE decode | Serial, GPS, barometric altitude, home point, model ID |
| **DJI Wi-Fi Beacons** | SSID pattern matching + OUI | Manufacturer, model, RSSI for distance estimation |
| **Parrot / Autel / Skydio** | Vendor IE + OUI matching | Manufacturer ID, RSSI |
| **Unknown UAVs** | OUI + behavioral analysis | MAC, RSSI, channel activity. Needs multi-TAP correlation |
| **DJI Controllers** | SSID pattern (PROJ*) + behavior | Controller identification, linked to drone sessions |

### Confidence Scoring

Every detection carries a confidence score reflecting data quality:

| Score | Meaning |
|-------|---------|
| 0.20 - 0.30 | Behavioral suspect only (channel, beacon interval, MAC analysis) |
| 0.30 | OUI match — known drone manufacturer MAC prefix |
| 0.40 | SSID regex match — drone naming pattern |
| 0.50 | OUI + SSID combined |
| 0.55 | DJI SSID model match (e.g., `DJI-MAVIC3-A1B2`) |
| 0.60 | Vendor IE match (Parrot, Autel, Skydio) |
| 0.80 | DJI DroneID decoded (no GPS fix) |
| 0.85 | OpenDroneID decoded (no GPS fix) |
| 0.90 | DJI DroneID with GPS coordinates |
| 0.95 | OpenDroneID with GPS coordinates |

---

## Detection Pipeline

### TAP Side (Rust)

Every captured management frame runs through 8 parallel detection checks:

```
 802.11 Management Frame
         │
    ┌────┴────┐
    │ 8-Check │
    │ Engine  │
    └────┬────┘
         │
  ┌──┬──┬──┬──┬──┬──┬──┐
  │  │  │  │  │  │  │  │
 OUI SS RID DJI PAR AUT MFR BHV
  │  │  │  │  │  │  │  │
  └──┴──┴──┴──┴──┴──┴──┘
         │
    ANY MATCH?
    ├── YES → Protobuf encode → NATS publish
    └── NO  → Frame discarded (<1μs)
```

| # | Check | What It Matches |
|---|-------|-----------------|
| 1 | **OUI Match** | Source MAC against 58 IEEE-verified drone manufacturer OUIs |
| 2 | **SSID Match** | Beacon/Probe SSID against 333 regex patterns |
| 3 | **OpenDroneID** | ASTM F3411 vendor IEs — BasicID, Location, System, OperatorID, SelfID, Auth |
| 4 | **DJI DroneID** | Proprietary OcuSync vendor IEs — Flight Registration (0x10), Flight Purpose (0x11) |
| 5 | **Parrot IE** | Parrot-specific vendor information elements |
| 6 | **Autel IE** | Autel-specific vendor information elements |
| 7 | **OUI Manufacturer** | Skydio, Yuneec, and other OUI-only identifiable drones |
| 8 | **Behavioral** | Multi-signal scoring: 5.8GHz activity, SSID patterns, beacon intervals, MAC analysis |

**MAC randomization protection**: Locally-administered MACs (LAA bit set) are excluded from OUI matching to prevent false positives from phones and laptops whose random MACs collide with drone vendor OUIs.

### Node Side (Go)

```
NATS Detection  →  Dedup Cache  →  Intel Engine  →  State Manager  →  WebSocket
                                       │                   │
                                  RSSI Distance       Spoof Detector
                                  Model Lookup         Trust Score
                                  Trilateration        Classification
                                       │                   │
                                  PostgreSQL            Telegram
                                  (history)             (alerts)
```

- **Intel Engine**: RSSI-to-distance estimation using log-normal propagation modeling with per-model TX power calibration, per-TAP environment profiles (open field, suburban, urban, indoor), and live calibration from ground-truth data
- **State Manager**: 16-shard FNV-1a hash map for concurrent drone state. Tracks active/lost status, flight trails, detection counts, last-seen timestamps
- **Spoof Detector**: 16-shard trust scoring with coordinate jump detection, OUI/SSID mismatch checking, impossible speed analysis, and RSSI anomaly detection
- **Trilateration**: Weighted least squares position estimation from 3+ TAPs with error ellipse and geometric dilution of precision. Falls back to circle-circle intersection for 2-TAP geometry

---

## Dashboard

The dashboard is a real-time command center accessible from any browser. It's built as a PWA so it works on mobile devices in the field.

### Pages

| Page | Description |
|------|-------------|
| **Dashboard** | Overview with threat assessment, active drone count, system health |
| **Airspace** | Full-screen map with live drone positions, trails, range rings, geofence zones, MGRS grid |
| **Fleet** | Sortable drone table with classification, signal strength, sightings history, tags |
| **TAPs** | Sensor health matrix — packets/sec, temperature, channel, buffer depth, uptime |
| **Alerts** | Alert stream with acknowledge/clear actions. Telegram integration status |
| **Analytics** | Detection trends, activity heatmaps, historical analysis |
| **System** | Server stats, database size, memory usage, connection counts |
| **Settings** | Map defaults, coordinate format (DD/DMS/MGRS), controller visibility, RF environment |
| **Admin** | User management — create, edit, delete users. Role assignment (Admin/Operator/Viewer) |

### Authentication

- JWT tokens in httpOnly cookies
- CSRF protection on all state-changing endpoints
- Role-based access control: **Admin** (full access), **Operator** (manage drones/alerts), **Viewer** (read-only)
- WebSocket authentication via one-time tickets (no JWT in URL)
- Account lockout after failed attempts
- Default login: `admin` / `admin` (change immediately after first login)

---

## Getting Started

### Prerequisites

| Component | Requirement |
|-----------|-------------|
| **Node server** | Go 1.24+, PostgreSQL 15+, Redis 7+, NATS 2.10+ |
| **TAP sensors** | Raspberry Pi 5 (4GB+), USB Wi-Fi adapter (monitor mode capable), Rust toolchain |
| **Network** | TAPs must reach the NATS server (VPN recommended for remote deployments) |

### 1. Node Setup

```bash
# Clone the repository
git clone https://github.com/K13094/skylens.git
cd skylens

# Copy and edit the configuration
cp configs/config.example.yaml configs/config.yaml
```

Edit `configs/config.yaml` with your database, Redis, and NATS connection details:

```yaml
server:
  http_port: 8080
  websocket_port: 8081
  allowed_origins: ["*"]    # Lock this down in production

nats:
  url: "nats://localhost:4222"

database:
  host: localhost
  port: 5432
  name: skylens
  user: skylens
  password: "your-secure-password"

redis:
  url: "redis://localhost:6379"

detection:
  lost_threshold_sec: 300       # Mark drone lost after 5min silence
  max_history_hours: 8760       # Retain 1 year of detection history
  single_tap_mode: true         # Enable single-TAP distance estimation
  spoof_check_enabled: true     # Enable spoof detection & trust scoring

propagation:
  global_environment: "suburban" # RF environment: open_field, suburban, urban, indoor

auth:
  enabled: true
  jwt_secret: "generate-a-random-secret-here"
```

```bash
# Build the node binary
go build -o skylens-node ./cmd/skylens-node/

# Run
./skylens-node --config configs/config.yaml
```

The dashboard is now at `http://localhost:8080`. Log in with `admin` / `admin`.

#### Database Setup

Skylens auto-creates all tables on first run. Just create an empty database:

```sql
CREATE USER skylens WITH PASSWORD 'your-secure-password';
CREATE DATABASE skylens OWNER skylens;
```

#### NATS Server

Install and run NATS. A basic configuration is included:

```bash
# Install NATS (see https://nats.io/download/)
```

#### Systemd Service (Production)

A systemd unit file is included for running Skylens Node as a service:

```bash
# Copy and edit the service file
# Edit paths and user as needed for your environment

sudo systemctl daemon-reload
sudo systemctl enable skylens-node
sudo systemctl start skylens-node
```

Or use the Makefile:

```bash
make install-service    # Install systemd service
make start              # Start the service
make logs               # View logs
```

### 2. TAP Setup

Each TAP is a Raspberry Pi 5 with a USB Wi-Fi adapter running the `skylens-tap` binary.

> **Full TAP deployment guide**: See [tap/README.md](tap/README.md) for hardware selection, driver patching, channel strategy, systemd setup, and troubleshooting.

Quick start:

```bash
# On the Raspberry Pi
cd skylens/tap

# Install dependencies
sudo apt update && sudo apt install -y libpcap-dev protobuf-compiler build-essential iw tshark

# Install Rust
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source $HOME/.cargo/env

# Build
cargo build --release

# Configure
sudo mkdir -p /etc/skylens
sudo cp config.toml /etc/skylens/config.toml
# Edit: set tap_id, GPS coordinates, interface name, NATS URL
```

Edit `/etc/skylens/config.toml`:

```toml
[tap]
id = "tap-001"
name = "North Sensor"
latitude = 40.7128          # Your TAP's GPS latitude
longitude = -74.0060        # Your TAP's GPS longitude

[capture]
interface = "wlan1"         # Your Wi-Fi adapter interface
channels = [1, 6, 11, 36, 40, 44, 48, 149, 153, 157, 161, 165]
hop_interval_ms = 200

[nats]
url = "nats://your-node-ip:4222"
```

```bash
# Run (requires root for monitor mode)
sudo ./target/release/skylens-tap /etc/skylens/config.toml
```

#### Wi-Fi Adapter Selection

| Adapter | Chipset | Driver | Patch Required | Notes |
|---------|---------|--------|----------------|-------|
| ALFA AWUS036ACHM | MT7612U | mt76 (mainline) | No | Best choice — works out of the box |
| MT7921U dongles | MT7921U | mt7921u (mainline) | No | Wi-Fi 6, NAN + RemoteID capable |
| ALFA AWUS036ACH | RTL8812AU | 88XXau (DKMS) | **Yes** | Beacon filter bug — see [Realtek fix](tap/docs/REALTEK_BEACON_FIX.md) |
| ALFA AWUS1900 | RTL8814AU | 8814au (DKMS) | **Yes** | Beacon filter bug — see [Realtek fix](tap/docs/REALTEK_BEACON_FIX.md) |

> **Important**: Realtek adapters silently drop ALL beacon frames in monitor mode due to a hardware register bug. Without the patch, RemoteID and DroneID detection is impossible. MediaTek adapters work without modification.

---

## Configuration

### Node Configuration (`configs/config.yaml`)

| Section | Key | Description | Default |
|---------|-----|-------------|---------|
| `server` | `http_port` | HTTP/dashboard port | `8080` |
| `server` | `websocket_port` | WebSocket real-time updates port | `8081` |
| `server` | `allowed_origins` | CORS origins | `["*"]` |
| `nats` | `url` | NATS server URL | Required |
| `database` | `host`, `port`, `name`, `user`, `password` | PostgreSQL connection | Required |
| `redis` | `url` | Redis connection URL | Required |
| `detection` | `lost_threshold_sec` | Seconds before marking drone lost | `300` |
| `detection` | `max_history_hours` | Detection history retention | `8760` (1yr) |
| `detection` | `single_tap_mode` | Enable RSSI distance with one TAP | `true` |
| `detection` | `spoof_check_enabled` | Enable spoof detection | `true` |
| `propagation` | `global_environment` | RF environment model | `suburban` |
| `propagation` | `tap_environments` | Per-TAP environment overrides | `{}` |
| `auth` | `enabled` | Enable authentication | `true` |
| `auth` | `jwt_secret` | JWT signing secret | Required |

### TAP Configuration (`config.toml`)

| Section | Key | Description | Default |
|---------|-----|-------------|---------|
| `tap` | `id` | Unique TAP identifier (used in NATS subjects) | Required |
| `tap` | `name` | Human-readable name | Required |
| `tap` | `latitude`, `longitude` | TAP GPS position (WGS84) | Required |
| `capture` | `interface` | Wi-Fi interface name | Required |
| `capture` | `channels` | Channel list to hop through | All 2.4 + 5 GHz |
| `capture` | `hop_interval_ms` | Dwell time per channel (ms) | `200` |
| `capture` | `passive` | Skip monitor mode setup (another tool owns the interface) | `false` |
| `capture` | `force_iw` | Use `iw` subprocess instead of nl80211 netlink | `false` |
| `capture` | `mac_denylist` | Known false positive MACs to suppress | `[]` |
| `nats` | `url` | NATS server URL | Required |
| `nats.buffer` | `max_size` | Max buffered detections during NATS outage | `10000` |
| `logging` | `level` | Log level: trace, debug, info, warn, error | `info` |

### RF Propagation Environments

The propagation model affects RSSI-to-distance estimation accuracy:

| Environment | Path Loss Exponent | Best For |
|-------------|-------------------|----------|
| `open_field` | 2.0 | Airports, open areas, farmland |
| `suburban` | 2.8 | Residential areas, light foliage |
| `urban` | 3.5 | Dense buildings, urban canyons |
| `indoor` | 4.0 | Inside buildings |

Per-TAP overrides let you tune each sensor for its specific environment.

---

## NATS Protocol

All messages are Protocol Buffer encoded. Schema: [`proto/skylens.proto`](proto/skylens.proto)

| Subject | Direction | Message | Rate |
|---------|-----------|---------|------|
| `skylens.detections.{tap_id}` | TAP → Node | `Detection` | Per detection event |
| `skylens.heartbeats.{tap_id}` | TAP → Node | `TapHeartbeat` | Every 5s per TAP |
| `skylens.commands.{tap_id}` | Node → TAP | `TapCommand` | On demand |
| `skylens.commands.broadcast` | Node → All TAPs | `TapCommand` | On demand |
| `skylens.acks.{tap_id}` | TAP → Node | `TapCommandAck` | Reply to commands |
| `skylens.alerts` | Node → Subscribers | `Alert` | On alert events |

### TAP Commands

Commands can be sent to individual TAPs or broadcast to all:

| Command | Description |
|---------|-------------|
| `Ping` | Latency check — TAP responds with `TapCommandAck` including `latency_ns` |
| `SetChannels` | Hot-reload channel list and hop interval without restart |
| `SetFilter` | Change BPF capture filter (default: `type mgt`) |
| `Restart` | Graceful (drain NATS) or hard restart |
| `UpdateConfig` | Hot-reload key-value configuration pairs |

### Reliable Delivery

TAPs buffer detections locally during NATS outages (up to 10,000 by default) and replay them with exponential backoff on reconnect. Heartbeats are not buffered — only the latest matters.

---

## API Reference

### Authentication

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/auth/login` | Login with username/password, returns JWT in httpOnly cookie |
| POST | `/api/auth/refresh` | Refresh JWT token |
| GET | `/api/auth/csrf` | Get CSRF token |
| POST | `/api/auth/ws-ticket` | Get one-time WebSocket auth ticket (30s expiry) |

### Core Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/drones` | List all tracked drones (active + lost) |
| GET | `/api/drones/{id}` | Get specific drone details |
| GET | `/api/taps` | List all TAP sensors with health data |
| GET | `/api/stats` | Current detection statistics |
| GET | `/api/status` | Overall system status |
| GET | `/api/threat` | Threat assessment summary |
| GET | `/api/fleet` | Fleet overview |
| GET | `/api/trails` | All GPS flight trails |
| GET | `/api/trails/{id}` | Trail for specific drone |

### Alerts

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/alerts` | List all alerts |
| POST | `/api/alert/{id}/ack` | Acknowledge an alert |
| POST | `/api/alerts/ack-all` | Acknowledge all alerts |
| POST | `/api/alerts/clear` | Clear all alerts |

### UAV Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/uav/{id}/sightings` | Sightings history grouped by session |
| POST | `/api/uav/{id}/history` | Get detection history |
| POST | `/api/uav/{id}/hide` | Hide a drone from the display |
| POST | `/api/uav/{id}/delete` | Delete a drone |
| POST | `/api/uavs/hide-lost` | Hide all lost drones |
| POST | `/api/uavs/unhide-all` | Unhide all drones |

### TAP Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/tap/{id}/ping` | Ping a specific TAP |
| POST | `/api/tap/{id}/restart` | Restart a specific TAP |
| POST | `/api/tap/{id}/command` | Send command to a TAP |
| POST | `/api/taps/broadcast/{cmd}` | Broadcast command to all TAPs |

### Analytics

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/detections/history` | Historical detection data |
| GET | `/api/suspects` | Behavioral suspects list |
| POST | `/api/suspects/{mac}/confirm` | Confirm a suspect as drone |
| POST | `/api/suspects/{mac}/dismiss` | Dismiss a suspect |
| GET | `/api/signatures` | Learned detection signatures |

### System

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/system/stats` | System performance statistics |
| GET | `/metrics` | Prometheus metrics endpoint |
| GET | `/health` | Health check (liveness) |
| GET | `/ready` | Readiness check |
| GET | `/api/events` | Server-Sent Events stream |
| POST | `/api/intel/update` | Update drone intelligence database |
| POST | `/api/telegram/test` | Test Telegram notification |

### WebSocket

Connect to `ws://host:8081/ws?ticket={ticket}` for real-time updates.

Get a ticket first:
```bash
# Get a one-time ticket (requires valid JWT cookie)
curl -X POST http://localhost:8080/api/auth/ws-ticket -b cookies.txt

# Connect with the ticket
wscat -c "ws://localhost:8081/ws?ticket=<ticket>"
```

Events pushed over WebSocket:
- `drone_new` — New drone detected
- `drone_update` — Drone state changed (position, signal, classification)
- `drone_lost` — Drone lost contact
- `tap_update` — TAP heartbeat received
- `alert` — New alert generated

---

## Docker Deployment

A Docker Compose stack is included for quick deployment:

```bash
# Copy and edit the compose file

# Set environment variables
export POSTGRES_PASSWORD=your-db-password
export JWT_SECRET=your-jwt-secret

# Start the full stack
```

The compose stack includes PostgreSQL 16, Redis 7, NATS 2.10, and the Skylens Node with health checks and dependency ordering.


---

## Development

```bash
# Build all binaries
make build-all

# Build just the node
go build -o skylens-node ./cmd/skylens-node/

# Build the flight simulator (generates synthetic detections for testing)
go build -o flight-sim ./cmd/flight-sim/

# Run tests
go test ./internal/...

# Run with debug logging
./skylens-node --config configs/config.yaml --log-level debug --log-format json

# Build TAP (on Pi or cross-compile)
cd tap && cargo build --release && cargo test

# Update drone intelligence database (fetches latest IEEE OUIs)
make intel-update

# Regenerate protobuf (after editing proto/skylens.proto)
protoc --go_out=. --go_opt=paths=source_relative proto/skylens.proto
```

### Flight Simulator

The flight simulator (`cmd/flight-sim/`) generates realistic drone detection scenarios over NATS for testing without physical hardware. It simulates a full flight lifecycle — approach, circle, RemoteID loss, and departure — with realistic RSSI modeling.

```bash
go build -o flight-sim ./cmd/flight-sim/
./flight-sim
```

### Intel Updater

The intel updater (`cmd/intel-updater/`) fetches the latest IEEE OUI database and updates the drone detection signatures:

```bash
go build -o intel-updater ./cmd/intel-updater/
./intel-updater              # Update and auto-commit
./intel-updater -dry-run     # Preview changes without writing
```

---

## Project Structure

```
skylens/
├── cmd/
│   ├── skylens-node/           # Node entry point, config loading, component startup
│   ├── flight-sim/             # Detection simulator for testing
│   └── intel-updater/          # IEEE OUI database updater
├── internal/
│   ├── api/                    # HTTP server, WebSocket, REST endpoints
│   │   └── dashboard/static/   # Embedded web dashboard (HTML/CSS/JS)
│   ├── auth/                   # JWT authentication, RBAC, CSRF, middleware
│   ├── geo/                    # MGRS coordinate conversion
│   ├── intel/                  # RSSI calibration, trilateration, fingerprinting,
│   │                           # DJI parser, RemoteID parser, drone model DB
│   ├── notify/                 # Telegram alert notifications
│   ├── processor/              # State manager, spoof detector, suspect correlator,
│   │                           # flight trails
│   ├── receiver/               # NATS subscription, detection pipeline
│   └── storage/                # PostgreSQL + Redis data layer
├── proto/
│   └── skylens.proto           # Shared protobuf schema (TAP + Node)
├── tap/                        # Rust — Raspberry Pi sensor
│   ├── src/
│   │   ├── capture/            # libpcap, channel hopping (nl80211), BLE
│   │   ├── decode/             # 802.11 parser, DJI decoder, RemoteID decoder,
│   │   │                       # OUI/SSID matching, BSSID baseline
│   │   └── publish/            # NATS publisher, offline buffer
│   ├── intel/                  # Drone signature database
│   ├── deploy/                 # Per-TAP deployment configs
│   └── docs/                   # TAP installation, Realtek fix, operations brief
├── configs/
│   └── config.example.yaml     # Node configuration template
│   └── docker-compose.yml      # Full stack: PostgreSQL + Redis + NATS + Node
├── scripts/
│   ├── live-monitor.sh         # Live monitoring dashboard
│   └── preflight-check.sh      # Pre-deployment validation
├── test/                       # Integration tests, test plans, E2E auth tests
├── docs/
│   └── TEST_DAY_CHECKLIST.md   # Field test preparation checklist
├── Makefile                    # Build, test, deploy, service management
```

## License

Private. All rights reserved.





