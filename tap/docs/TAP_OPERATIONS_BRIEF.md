# Skylens TAP Operations Brief

Reference document for the Skylens Node team working with TAP deployments.

## What is the TAP?

The Skylens TAP is a Rust binary that runs on a Raspberry Pi 5 with a USB WiFi adapter in monitor mode. It captures raw 802.11 management frames, decodes drone protocols (OpenDroneID/ASTM F3411 and DJI DroneID), and publishes detections as Protocol Buffer messages over NATS.

## Current Deployment

| TAP | Location | Adapter | Chipset | Driver | Mode | Tailscale IP |
|-----|----------|---------|---------|--------|------|-------------|
| tap-001 | Site A | ALFA USB | RTL8814AU | 8814au (DKMS, patched) | Active | 100.83.116.49 |
| tap-002 | Site A | ALFA USB | RTL8812AU | 88XXau (DKMS, patched) | Active | 100.115.179.117 |
| tap-003 | Site A | MT7921U | MT7921U | mt7921u (mainline) | Passive | 100.94.165.41 |

- **NATS node**: `nats://100.73.32.29:4222`
- **TAP-003** runs in passive mode on a shared interface with a NATS TCP proxy over a second Tailscale instance

## NATS Topics

| Topic | Direction | Message Type | Frequency |
|-------|-----------|-------------|-----------|
| `skylens.detections.{tap_id}` | TAP -> Node | `Detection` protobuf | On detection |
| `skylens.heartbeats.{tap_id}` | TAP -> Node | `TapHeartbeat` protobuf | Every 5s |
| `skylens.commands.{tap_id}` | Node -> TAP | `TapCommand` protobuf | On demand |
| `skylens.commands.broadcast` | Node -> TAPs | `TapCommand` protobuf | On demand |
| `skylens.acks.{tap_id}` | TAP -> Node | `TapCommandAck` protobuf | On command |

## Detection Confidence Scores

When the node receives detections, confidence indicates how the drone was identified:

| Confidence | Meaning |
|-----------|---------|
| 0.20-0.30 | Behavioral suspect only (5.8GHz + SSID, beacon interval, etc.) |
| 0.30 | OUI match only (known drone manufacturer MAC prefix) |
| 0.40 | SSID regex match only |
| 0.50 | OUI + SSID combined |
| 0.55 | DJI SSID model match (e.g., DJI-MAVIC3-A1B2) |
| 0.60 | Vendor IE match (Parrot, Autel, Skydio) |
| 0.80 | DJI DroneID protocol decoded (no GPS) |
| 0.85 | OpenDroneID/RemoteID decoded (no GPS) |
| 0.90 | DJI DroneID with GPS coordinates |
| 0.95 | OpenDroneID/RemoteID with GPS coordinates |

**Behavioral detection** requires score >= 0.40 (multiple signals). A single weak signal (5.8GHz alone or LAA MAC alone) does NOT trigger detection.

**Locally-administered MAC (LAA) rejection**: Randomized MACs are excluded from OUI matching to prevent false positives from phones whose random MACs collide with vendor IE OUIs.

**Node spoof detection thresholds**: >=0.90 (+10 trust), >=0.80 (+5 trust), <0.50 (-15 trust)

## Critical Knowledge: Realtek WiFi Adapter Bug

### The Problem

Both TAPs use ALFA-branded USB WiFi adapters with **Realtek** chipsets (RTL8812AU and RTL8814AU). These use out-of-tree Linux drivers (`88XXau` and `8814au`) that have a critical bug:

**The hardware Receive Configuration Register (`RCR`) keeps the `RCR_CBSSID_BCN` bit set in monitor mode, which tells the WiFi chip to silently drop ALL beacon frames that don't match the associated BSSID. In monitor mode there is no BSSID, so ALL beacons are dropped.**

Without beacons:
- No OpenDroneID/RemoteID detection (RemoteID is broadcast in beacon vendor IEs)
- No SSID capture from AP beacons
- No DJI DroneID from beacon vendor IEs
- Only probe requests and control frames are visible

### The Fix

Both TAPs have been patched. The fix adds a monitor-mode check to `rtw_hal_rcr_set_chk_bssid()` in the driver's `hal/hal_com.c` that clears the beacon filter bits when in monitor mode.

Full patch instructions: **`docs/REALTEK_BEACON_FIX.md`** in the skylens-tap repo.

### When to Re-Apply

The patch must be re-applied after:
- Kernel upgrades (DKMS may rebuild from unpatched source)
- Driver reinstallation
- OS reinstall

### How to Verify

```bash
# SSH to a TAP and check for beacons
sudo iw dev wlan1 set channel 6
sudo timeout 5 tshark -i wlan1 -c 200 -T fields -e wlan.fc.type_subtype 2>/dev/null \
  | sort | uniq -c | sort -rn
# Must see "0x0008" (Beacon) lines. If not, patch is missing.
```

### Alternative: MediaTek Adapters

MediaTek chipsets (MT7921U, MT7612U) with mainline `mt76` kernel drivers work correctly without patching and are the recommended choice for new deployments.

## Channel Configuration

TAPs hop across 36 channels every 200ms (priority dwell 2x on ch6 and ch149 for RemoteID):
- 2.4GHz: channels 1-11
- 5GHz UNII-1: 36, 40, 44, 48
- 5GHz DFS: 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144
- 5GHz UNII-3: 149, 153, 157, 161, 165

**DFS channels are critical** — real-world drone detections show RemoteID broadcasts on DFS frequencies.

**Timing aliasing warning**: `hop_interval_ms * num_channels` must NOT equal the 5s heartbeat interval, or the heartbeat always samples the same channel position.

## TAP Commands (Node -> TAP)

| Command | Proto Message | What it Does |
|---------|--------------|--------------|
| Ping | `PingCommand` | TAP responds with `TapCommandAck` including `latency_ns` |
| SetChannels | `SetChannelsCommand` | Hot-reload channel list and hop interval |
| SetFilter | `SetFilterCommand` | Change BPF filter (default: `"type mgt"`) |
| Restart | `RestartCommand` | Graceful (`graceful=true`: drain NATS) or hard restart |
| UpdateConfig | `UpdateConfigCommand` | Hot-reload key-value config pairs |

## Heartbeat Fields

The `TapHeartbeat` includes:
- `stats.current_channel` — current WiFi channel (verify hopping works)
- `stats.packets_per_second` — current capture rate (should be >0)
- `stats.pcap_kernel_received/dropped` — kernel-level capture stats
- `stats.buffer_size` — NATS retry buffer depth (>0 means NATS disconnected)
- `cpu_percent`, `memory_percent`, `temperature_celsius` — Pi health

## If a TAP Goes Offline

1. Check `skylens.heartbeats.{tap_id}` — if heartbeats stop, TAP is down
2. SSH to the TAP and check: `sudo systemctl status skylens-tap`
3. Check logs: `sudo journalctl -u skylens-tap -n 50`
4. Common causes:
   - Watchdog timeout (30s) — usually a code bug, check for `notify(true)` usage
   - WiFi adapter disconnected — check `lsusb` and `iw dev`
   - Tailscale VPN down — check `tailscale status`
   - NATS unreachable — detections buffer locally (up to 10k), check `buffer_size` in heartbeat

## Repo Structure

```
skylens/                     # Monorepo root
├── proto/skylens.proto      # Shared protobuf schema (TAP + Node)
├── tap/                     # TAP source (Rust)
│   ├── config.toml          # TAP configuration template
│   ├── intel/drone_models.json  # Detection signature database (58 OUIs, 333 SSIDs)
│   ├── deploy/tap-003/      # TAP-003 passive mode deploy files
│   ├── docs/
│   │   ├── INSTALL.md       # Full installation guide
│   │   ├── REALTEK_BEACON_FIX.md # Critical driver patch instructions
│   │   └── TAP_OPERATIONS_BRIEF.md # This file
│   └── src/                 # Rust source (8-check detection engine)
├── internal/                # Node source (Go)
└── cmd/                     # Node entry points (Go)
```
