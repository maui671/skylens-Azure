# Skylens TAP Installation Guide

Complete guide to setting up a Skylens TAP drone detection sensor on a Raspberry Pi 5.

## Prerequisites

- Raspberry Pi 5 (4GB+ RAM) with Raspberry Pi OS (64-bit Bookworm)
- External USB WiFi adapter supporting monitor mode
- Network access to a NATS server (Tailscale VPN recommended)
- SSH access to the Pi

### Recommended WiFi Adapters

| Adapter | Chipset | Driver | Notes |
|---------|---------|--------|-------|
| ALFA AWUS036ACHM | MT7612U | mt76 (mainline) | Best choice - no driver patching needed |
| MT7921U USB dongles | MT7921U | mt7921u (mainline) | WiFi 6, no patching needed |
| ALFA AWUS036ACH | RTL8812AU | 88XXau (DKMS) | Requires beacon fix patch (see below) |
| ALFA AWUS1900 | RTL8814AU | 8814au (DKMS) | Requires beacon fix patch (see below) |

## Step 1: System Setup

```bash
# Update system
sudo apt update && sudo apt upgrade -y

# Install dependencies
sudo apt install -y \
  libpcap-dev \
  protobuf-compiler \
  build-essential \
  git \
  iw \
  tshark \
  dkms

# Install Rust
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source $HOME/.cargo/env
```

## Step 2: WiFi Adapter Driver Setup

### MediaTek Adapters (MT7612U, MT7921U)

These use mainline kernel drivers and work out of the box:

```bash
# Verify adapter is detected
lsusb | grep -i mediatek
# Should show: MediaTek Inc. Wireless_Device

# Verify driver is loaded
lsmod | grep mt76
# Should show: mt7921u, mt7921_common, mt76_connac_lib, etc.
```

### Realtek Adapters (RTL8812AU, RTL8814AU)

These require an out-of-tree driver AND a critical patch to enable beacon capture in monitor mode.

#### Install the Driver (DKMS)

```bash
# For RTL8812AU (88XXau driver)
sudo apt install -y realtek-rtl88xxau-dkms

# For RTL8814AU (8814au driver)
# Clone and install from source if not in apt
git clone https://github.com/aircrack-ng/rtl8814au.git
cd rtl8814au
sudo make dkms_install
```

#### CRITICAL: Apply the Beacon Capture Patch

**Without this patch, the adapter will NOT capture beacon frames in monitor mode. This means no RemoteID detection, no SSID capture from beacons, and no vendor IE parsing.**

See [REALTEK_BEACON_FIX.md](REALTEK_BEACON_FIX.md) for the full explanation and patch instructions.

Quick version:

```bash
# Find your DKMS source
DRIVER_SRC=$(ls -d /usr/src/realtek-rtl88xxau-* 2>/dev/null || ls -d /usr/src/rtl8814au-* 2>/dev/null)
echo "Patching: $DRIVER_SRC"

# Backup and patch
sudo cp $DRIVER_SRC/hal/hal_com.c $DRIVER_SRC/hal/hal_com.c.bak
sudo sed -i '/rcr_new = rcr;/a\
\t/* Monitor mode: accept all beacons and data regardless of BSSID */\
\tif (check_fwstate(\&adapter->mlmepriv, WIFI_MONITOR_STATE)) {\
\t\trcr_new \&= ~(RCR_CBSSID_BCN | RCR_CBSSID_DATA);\
\t\tif (rcr != rcr_new)\
\t\t\trtw_hal_set_hwreg(adapter, HW_VAR_RCR, (u8 *)\&rcr_new);\
\t\treturn;\
\t}' $DRIVER_SRC/hal/hal_com.c
sudo sed -i 's/^n\t\/\* Monitor/\t\/\* Monitor/' $DRIVER_SRC/hal/hal_com.c

# Rebuild DKMS
MODULE_NAME=$(basename $DRIVER_SRC | sed 's/-[0-9].*//')
MODULE_VER=$(basename $DRIVER_SRC | sed 's/^[^0-9]*//')
KERNEL=$(uname -r)

sudo dkms remove $MODULE_NAME/$MODULE_VER -k $KERNEL 2>/dev/null
sudo dkms build $MODULE_NAME/$MODULE_VER -k $KERNEL
sudo dkms install $MODULE_NAME/$MODULE_VER -k $KERNEL

# Reload module
MODNAME=$(lsmod | grep -o '88XXau\|8814au' | head -1)
sudo rmmod $MODNAME
sudo modprobe $MODNAME
```

#### Verify Beacon Capture

```bash
# Setup monitor mode
sudo ip link set wlan1 down
sudo iw dev wlan1 set type monitor
sudo ip link set wlan1 up
sudo iw dev wlan1 set channel 6

# Check for beacons (0x0008 = Beacon frame)
sudo timeout 5 tshark -i wlan1 -c 200 -T fields -e wlan.fc.type_subtype 2>/dev/null \
  | sort | uniq -c | sort -rn

# You MUST see "0x0008" (Beacon) lines. If not, the patch failed.
```

## Step 3: Clone and Build Skylens TAP

```bash
cd ~
git clone https://github.com/K13094/skylens.git
cd skylens/tap

# Build release binary (~70s on Pi 5)
cargo build --release
# Binary at target/release/skylens-tap
```

> **Deployment shortcut**: You only need the full repo for building. For remote TAPs, just copy the binary, config, and intel database. See the [TAP README](../tap/README.md) for deploy instructions.

## Step 4: Configure

Create or edit `config.toml`:

```toml
[tap]
id = "tap-001"          # Unique ID for this TAP
name = "Tap 1"          # Human-readable name
latitude = 0.000000     # GPS latitude (WGS84)
longitude = 0.000000    # GPS longitude (WGS84)

[capture]
interface = "wlan1"     # WiFi adapter interface name
# Full channel list: 2.4GHz (1-11) + 5GHz UNII-1 + DFS + UNII-3
channels = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165]
hop_interval_ms = 200   # Channel dwell time in ms

[nats]
url = "nats://100.73.32.29:4222"  # NATS server URL
reconnect_interval_ms = 1000

[nats.buffer]
max_size = 10000
max_retries = 100
initial_retry_delay_ms = 1000
max_retry_delay_ms = 30000
warning_threshold = 0.8

[logging]
level = "info"
```

### Channel Configuration Notes

- **DFS channels (52-144)** are critical for drone detection - many drones use these frequencies
- **hop_interval_ms** should NOT be set such that `hop_interval_ms * num_channels = heartbeat_interval (5000ms)` as this causes timing aliasing where the heartbeat always samples the same channel position
- With 36 channels at 200ms, full cycle = 7.2s (safely avoids 5s heartbeat aliasing)
- Some DFS channels may not be available depending on your regulatory domain; the hopper will log warnings for unavailable channels and skip them

## Step 5: Install Systemd Service

```bash
sudo tee /etc/systemd/system/skylens-tap.service << 'EOF'
[Unit]
Description=Skylens Tap Drone Detection
After=network.target tailscaled.service
Requires=tailscaled.service
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=notify
User=root
WorkingDirectory=/home/tap
ExecStartPre=/usr/bin/tailscale status
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

### Systemd Configuration Notes

- **Type=notify**: The binary sends `sd_notify(READY=1)` when capture starts. If it doesn't, systemd kills it.
- **WatchdogSec=30**: Binary must send `WATCHDOG=1` pings every <15s (half of 30s). The `sd-notify` crate handles this.
- **ExecStartPre**: Verifies Tailscale VPN is up before starting (NATS server is over Tailscale).
- Adjust `WorkingDirectory` and `ExecStart` paths to match your installation.

## Step 6: Verify

```bash
# Check service status
sudo systemctl status skylens-tap

# Watch logs
sudo journalctl -u skylens-tap -f

# Verify watchdog survival (wait 35+ seconds)
sleep 35 && sudo systemctl is-active skylens-tap
# Should print "active"

# Check stats in logs
sudo journalctl -u skylens-tap --no-pager -n 5 | grep Stats
# Should show packets captured, detections sent, pps > 0
```

### What Good Logs Look Like

```
INFO skylens_tap: Starting Skylens TAP tap-001
INFO skylens_tap::capture::channel: Set wlan1 to monitor mode
INFO skylens_tap: BPF filter set: type mgt
INFO skylens_tap: Capture running, publishing detections to NATS
INFO skylens_tap: Stats packets=500 pps=7 detections=12 dps=1 nats_sent=12 nats_errors=0
```

### What Bad Logs Look Like

```
# No packets = driver/adapter problem
INFO skylens_tap: Stats packets=0 pps=0 detections=0

# Watchdog failure = sd_notify bug (check MEMORY.md for fix)
systemd: skylens-tap.service: Watchdog timeout

# Channel errors = regulatory domain restrictions (safe to ignore for affected channels)
ERROR skylens_tap::capture::channel: iw set channel failed channel=144 stderr=channel is disabled
```

## Troubleshooting

### No detections at all
1. Check `pps` in Stats log. If 0, the adapter isn't capturing.
2. Run the tshark beacon test above. If no beacons, apply the Realtek patch.
3. Verify monitor mode: `iw dev wlan1 info` should show `type monitor`.

### Service dies after exactly 30 seconds
The watchdog is failing. Check if `sd_notify::notify(false, ...)` is used (NOT `true`). The first bool param controls whether to unset `$NOTIFY_SOCKET` after the call.

### Service hangs on stop/restart
The capture loop isn't checking the stop flag when `next_packet()` returns `None`. See the shutdown hang fix in the README.

### Low detection count
- Ensure DFS channels (52-144) are in the channel list
- Check `hop_interval_ms` isn't causing timing aliasing with the 5s heartbeat
- Increase channel dwell time if you're missing beacons (try 300ms)

### NATS connection errors
- Verify Tailscale is connected: `tailscale status`
- Check NATS server is reachable: `nc -zv 100.73.32.29 4222`
- Buffer config will hold detections during brief disconnections

## Updating

```bash
cd ~/skylens/tap
git pull
cargo build --release
sudo systemctl restart skylens-tap

# Verify watchdog survival
sleep 35 && sudo systemctl is-active skylens-tap
```

## Multi-TAP Deployment

For deploying to additional TAPs over SSH:

```bash
# Build on any Pi (same architecture)
cd ~/skylens/tap
cargo build --release

# Deploy binary + intel to remote TAP
scp target/release/skylens-tap tap@remote-pi:/home/tap/skylens-tap
scp intel/drone_models.json tap@remote-pi:/home/tap/intel/drone_models.json
ssh tap@remote-pi 'sudo systemctl restart skylens-tap'

# Verify
sleep 35 && ssh tap@remote-pi 'sudo systemctl is-active skylens-tap'
```

Each TAP needs its own `config.toml` with a unique `tap.id`, correct GPS coordinates, and the correct Wi-Fi interface name.
