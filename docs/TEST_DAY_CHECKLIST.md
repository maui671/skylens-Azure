# Skylens Test Day Pre-Flight Checklist

**Date:** ____________
**Location:** ____________
**Team Lead:** ____________

---

## PART 1: PRE-TEST DAY CHECKLIST

### 1.1 TAP Health Checks (Run on each Raspberry Pi)

```bash
# SSH to TAP device first
ssh pi@<tap-ip>
```

| Check | Command | Expected Result | Pass |
|-------|---------|-----------------|------|
| TAP service running | `systemctl status skylens-tap` | Active (running) | [ ] |
| WiFi interface exists | `ip link show wlan1` | State: UP | [ ] |
| Monitor mode enabled | `iw dev wlan1 info \| grep type` | `type monitor` | [ ] |
| Config file valid | `cat /home/pi/skylens-tap/config.toml` | No parse errors | [ ] |
| TAP ID configured | `grep "^id = " /home/pi/skylens-tap/config.toml` | Unique tap-XXX | [ ] |
| TAP lat/lon configured | `grep -E "latitude|longitude" /home/pi/skylens-tap/config.toml` | Valid coordinates | [ ] |
| NATS URL correct | `grep "url = " /home/pi/skylens-tap/config.toml` | Points to Node IP | [ ] |
| Can ping NATS server | `ping -c 3 <nats-server-ip>` | 0% packet loss | [ ] |
| CPU temp OK | `cat /sys/class/thermal/thermal_zone0/temp` | < 70000 (70C) | [ ] |
| Disk space OK | `df -h /` | > 1GB free | [ ] |
| Memory OK | `free -m` | > 200MB available | [ ] |

**TAP Config Verification:**
```bash
# Verify config.toml has correct settings
cat << 'EOF'
Required config.toml settings:
[tap]
id = "tap-001"              # Unique per device
name = "Runway North"       # Human-readable name
latitude = XX.XXXXXX        # Precise location
longitude = -XX.XXXXXX      # Precise location

[capture]
interface = "wlan1"         # Must be your monitor-mode interface
channels = [1,6,11,36,40,44,48,149,153,157,161,165]

[nats]
url = "nats://<node-ip>:4222"
EOF
```

---

### 1.2 Node Health Checks (Run on skylens-node server)

| Check | Command | Expected Result | Pass |
|-------|---------|-----------------|------|
| Node service running | `systemctl status skylens-node` | Active (running) | [ ] |
| HTTP API responding | `curl -s http://localhost:8080/health` | `{"status":"ok"...}` | [ ] |
| Readiness check | `curl -s http://localhost:8080/ready` | `{"status":"ready"...}` | [ ] |
| WebSocket port open | `nc -zv localhost 8081` | Connection succeeded | [ ] |
| Config file valid | `cat /home/node/skylens-node/configs/config.yaml` | Valid YAML | [ ] |

**Node Config Verification:**
```bash
# Check /home/node/skylens-node/configs/config.yaml
cat /home/node/skylens-node/configs/config.yaml
```

Expected:
```yaml
server:
  http_port: 8080
  websocket_port: 8081

nats:
  url: "nats://localhost:4222"

database:
  host: "localhost"
  port: 5432
  name: "skylens"
  user: "skylens"
  password: "skylens123"
  ssl_mode: "disable"

redis:
  url: "redis://localhost:6379"

detection:
  single_tap_mode: true          # CRITICAL for limited TAP deployments
  lost_threshold_sec: 300
  spoof_check_enabled: true
```

---

### 1.3 NATS Connectivity

| Check | Command | Expected Result | Pass |
|-------|---------|-----------------|------|
| NATS server running | `systemctl status nats-server` | Active (running) | [ ] |
| NATS port open | `nc -zv localhost 4222` | Connection succeeded | [ ] |
| NATS accepting connections | `nats server check connection` | OK | [ ] |
| Can publish test message | `nats pub test.ping "hello"` | Published successfully | [ ] |
| Subscribe to detections | `timeout 5 nats sub "skylens.detections.*" --count=1` | Subscription active | [ ] |
| Subscribe to heartbeats | `timeout 10 nats sub "skylens.heartbeats.*" --count=1` | Receives heartbeat | [ ] |

**NATS Cluster Health (if applicable):**
```bash
# Check NATS monitoring endpoint
curl -s http://localhost:8222/varz | jq '{connections, subscriptions, in_msgs, out_msgs}'
```

---

### 1.4 Database State

| Check | Command | Expected Result | Pass |
|-------|---------|-----------------|------|
| PostgreSQL running | `systemctl status postgresql` | Active (running) | [ ] |
| Can connect to DB | `psql -U skylens -d skylens -c "SELECT 1"` | Returns 1 | [ ] |
| Schema exists | `psql -U skylens -d skylens -c "\dt"` | Shows drones, detections tables | [ ] |
| Redis running | `systemctl status redis` | Active (running) | [ ] |
| Redis responding | `redis-cli ping` | PONG | [ ] |

**Database Schema Verification:**
```bash
# Connect to PostgreSQL
psql -U skylens -d skylens

# Check tables exist
\dt

# Expected tables:
#  drones
#  detections
#  tap_stats
#  rssi_calibration
#  rssi_calibration_data
#  learned_signatures
#  false_positives

# Check drone count (should be 0 or low for fresh start)
SELECT COUNT(*) FROM drones;

# Check detection count in last hour
SELECT COUNT(*) FROM detections WHERE time > NOW() - INTERVAL '1 hour';
```

**Optional: Clear old test data before test day:**
```bash
# WARNING: Only run if you want a clean slate
psql -U skylens -d skylens -c "TRUNCATE drones, detections, tap_stats CASCADE;"
redis-cli FLUSHDB
```

---

### 1.5 Dashboard Functionality

| Check | URL/Action | Expected Result | Pass |
|-------|------------|-----------------|------|
| Dashboard loads | `http://<node-ip>:8080/` | Airspace page renders | [ ] |
| Airspace page | `http://<node-ip>:8080/airspace` | Map displays | [ ] |
| Fleet page | `http://<node-ip>:8080/fleet` | Table renders | [ ] |
| Taps page | `http://<node-ip>:8080/taps` | TAP list shows | [ ] |
| Alerts page | `http://<node-ip>:8080/alerts` | Alert list renders | [ ] |
| Settings page | `http://<node-ip>:8080/settings` | Settings form renders | [ ] |
| System page | `http://<node-ip>:8080/system` | System stats display | [ ] |
| WebSocket connects | Browser DevTools Network tab | WS connection to :8081 | [ ] |

**Browser Console Check:**
```
Open DevTools (F12) -> Console tab
Should see: "WebSocket connected" or similar
Should NOT see: Red errors about WebSocket connection failures
```

---

### 1.6 API Endpoint Verification

```bash
# Run these from the Node server or any machine that can reach it
NODE_IP="localhost"  # or actual IP

# Core status endpoints
curl -s "http://${NODE_IP}:8080/health" | jq .
curl -s "http://${NODE_IP}:8080/ready" | jq .
curl -s "http://${NODE_IP}:8080/api/status" | jq '.connected, .uptime'

# Data endpoints
curl -s "http://${NODE_IP}:8080/api/drones" | jq 'length'
curl -s "http://${NODE_IP}:8080/api/taps" | jq '.[] | {id, name, status}'
curl -s "http://${NODE_IP}:8080/api/alerts" | jq 'length'
curl -s "http://${NODE_IP}:8080/api/fleet" | jq '.total_uavs'
curl -s "http://${NODE_IP}:8080/api/threat" | jq '.threat_level'
curl -s "http://${NODE_IP}:8080/api/stats" | jq .
curl -s "http://${NODE_IP}:8080/api/system/stats" | jq '{cpu, memory}'
```

---

### 1.7 Test Injection Verification

**Inject a test drone to verify the full pipeline:**

```bash
# Inject test drone
curl -X POST "http://${NODE_IP}:8080/api/test/drone?preset=dji-mini" | jq .

# Verify it appears in drone list
curl -s "http://${NODE_IP}:8080/api/drones" | jq '.[] | select(.manufacturer == "DJI")'

# Verify dashboard updates (check browser)

# Clear test data
curl -X POST "http://${NODE_IP}:8080/api/test/clear" | jq .

# Verify cleared
curl -s "http://${NODE_IP}:8080/api/drones" | jq 'length'
```

---

## PART 2: LIVE MONITORING COMMANDS

### 2.1 Real-Time Detection Stream (Terminal 1)

```bash
# Watch NATS detections live - shows every drone detection
nats sub "skylens.detections.*" --raw
```

**What to look for:**
- Binary protobuf data appearing (means detections flowing)
- Consistent stream when drones are active
- No long gaps (>30 seconds) during active flight

### 2.2 TAP Heartbeat Monitor (Terminal 2)

```bash
# Watch TAP heartbeats - should appear every 5 seconds per TAP
nats sub "skylens.heartbeats.*" --raw
```

**What to look for:**
- Regular heartbeats from each TAP (every 5 seconds)
- If heartbeat stops, TAP may be offline or NATS disconnected

### 2.3 Node Logs (Terminal 3)

```bash
# Follow skylens-node logs in real-time
journalctl -u skylens-node -f

# Or with more detail
journalctl -u skylens-node -f --output=short-precise
```

**What to look for:**
```
Detection received tap_id=tap-001 identifier=... mac=60:60:1F:...
Tap heartbeat received id=tap-001 name="Runway North"
APPROACH detected identifier=... rssi=-65 distance_est=150.2
Suspect promoted via multi-TAP correlation mac=...
```

### 2.4 TAP Logs (SSH to each TAP)

```bash
# On each TAP device
journalctl -u skylens-tap -f
```

**What to look for:**
```
DETECTION mac=60:60:1F:AA:BB:CC manufacturer=DJI model="Mini 3 Pro"
Stats packets=12345 pps=230 detections=42 nats_sent=42
Heartbeat sent
```

### 2.5 API Polling Dashboard (Terminal 4)

```bash
# Poll drone count every 5 seconds
watch -n 5 'curl -s http://localhost:8080/api/drones | jq "length"'

# Or more detailed
watch -n 5 'curl -s http://localhost:8080/api/fleet | jq "{total: .total_uavs, active: .active_uavs, threat: .threat_level}"'
```

### 2.6 WebSocket Event Monitor

```bash
# Use websocat to monitor WebSocket events
websocat ws://localhost:8081/ws

# Or with wscat
wscat -c ws://localhost:8081/ws
```

**Expected events:**
```json
{"type":"drone_update","data":{"identifier":"...","latitude":18.25,"longitude":-65.64,...}}
{"type":"drone_new","data":{...}}
{"type":"tap_status","data":{"id":"tap-001","status":"ONLINE",...}}
```

### 2.7 System Resource Monitor (Terminal 5)

```bash
# Monitor Node server resources
htop

# Or simple CPU/memory
watch -n 2 'echo "=== Node ===" && ps aux | grep skylens-node | head -1 && echo && free -m | head -2'
```

**Target metrics:**
- CPU: < 50% sustained
- Memory: < 500MB for Node
- Detection latency: < 100ms from capture to dashboard

---

## PART 3: QUICK TROUBLESHOOTING GUIDE

### Problem: Not Seeing Any Drones

**Check in this order:**

1. **TAP Capturing?**
   ```bash
   # On TAP device
   journalctl -u skylens-tap -n 50 | grep -E "DETECTION|packets"
   # Should see "Stats packets=XXXX pps=YYY"
   # pps should be > 0 if WiFi traffic exists
   ```

2. **TAP Connected to NATS?**
   ```bash
   # On TAP device
   journalctl -u skylens-tap -n 20 | grep -i nats
   # Should see "Connected to NATS" not "NATS disconnected"
   ```

3. **NATS Receiving?**
   ```bash
   # On Node server
   timeout 10 nats sub "skylens.detections.*" --count=1
   # Should receive a message within 10 seconds if drones flying
   ```

4. **Node Processing?**
   ```bash
   # On Node server
   journalctl -u skylens-node -n 50 | grep -i detection
   # Should see "Detection received" entries
   ```

5. **Is it a DJI/Parrot/known drone?**
   - Unknown manufacturers may not broadcast RemoteID
   - Check if single_tap_mode is enabled for behavioral detection
   - Try flying closer to TAP (RSSI too weak if far away)

### Problem: TAP Shows OFFLINE

```bash
# Check TAP service
ssh pi@<tap-ip> "systemctl status skylens-tap"

# Restart if needed
ssh pi@<tap-ip> "sudo systemctl restart skylens-tap"

# Check for errors
ssh pi@<tap-ip> "journalctl -u skylens-tap -n 50"

# Common issues:
# - NATS URL incorrect in config.toml
# - Network connectivity to Node
# - WiFi interface not in monitor mode
```

### Problem: Drones Appear Then Immediately Disappear

```bash
# Check lost_threshold_sec in config
grep lost_threshold_sec /home/node/skylens-node/configs/config.yaml

# Should be 300 (5 minutes) for testing
# If too low, drones timeout between detections
```

### Problem: Dashboard Not Updating

```bash
# Check WebSocket
curl -i -N -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Key: test" -H "Sec-WebSocket-Version: 13" \
  http://localhost:8081/ws

# Check Node is broadcasting events
journalctl -u skylens-node | grep -i websocket

# Restart Node if needed
sudo systemctl restart skylens-node
```

### Problem: High CPU / Memory on TAP

```bash
# On TAP device
top -b -n 1 | head -15

# Check kernel drops
journalctl -u skylens-tap | grep -i "kernel.*drop"

# If drops occurring, reduce capture scope:
# - Reduce channel list
# - Increase hop_interval_ms
# - Consider hardware upgrade
```

### Problem: Database Connection Failed

```bash
# Check PostgreSQL
sudo systemctl status postgresql
sudo -u postgres psql -c "SELECT 1"

# Check connection string
grep -A5 database /home/node/skylens-node/configs/config.yaml

# Test connection
psql -U skylens -d skylens -h localhost -c "SELECT NOW()"
```

---

## PART 4: SUCCESS METRICS

### 4.1 Detection Metrics

| Metric | Target | Measure Command |
|--------|--------|-----------------|
| Detection latency | < 100ms | Timestamp in detection vs. Node log |
| Detections per second | Depends on traffic | `journalctl -u skylens-node \| grep dps` |
| TAP packet rate | > 100 pps in active area | TAP stats in heartbeat |
| Kernel drop rate | < 1% | `journalctl -u skylens-tap \| grep kern_drop` |

### 4.2 Coverage Metrics

| Metric | Target | How to Verify |
|--------|--------|---------------|
| Detection range | 200-500m (varies by drone TX power) | Known-distance flight test |
| Multi-TAP correlation | > 80% for drones in overlap zone | Check drone.taps_seen count |
| Suspect promotion | Working for unknown drones | Check /api/suspects endpoint |

### 4.3 System Health Metrics

| Metric | Target | Measure Command |
|--------|--------|-----------------|
| Node CPU | < 50% | `top` |
| Node memory | < 500MB | `free -m` |
| TAP CPU | < 40% | SSH to TAP, run `top` |
| TAP temperature | < 65C | `cat /sys/class/thermal/thermal_zone0/temp` |
| NATS message backlog | 0 | `nats server check` |
| DB connection pool | Healthy | Node logs, no connection errors |

### 4.4 Test Flight Success Criteria

**For a successful test, verify:**

- [ ] Drone detected within 30 seconds of takeoff
- [ ] Drone position updates in real-time on dashboard map
- [ ] Drone trails/history accumulating in database
- [ ] RSSI values correlate with distance (stronger when closer)
- [ ] Drone marked as LOST within 5 minutes of landing (configurable)
- [ ] Multiple TAPs see same drone (if in overlap zone)
- [ ] No unexplained gaps in detection stream during flight
- [ ] Alerts generate for spoof/anomaly conditions (if triggered)
- [ ] System stable for full test duration (no crashes/restarts)

---

## PART 5: QUICK REFERENCE COMMANDS

### Start/Stop Services

```bash
# Node
sudo systemctl start skylens-node
sudo systemctl stop skylens-node
sudo systemctl restart skylens-node

# TAP (run on TAP device)
sudo systemctl start skylens-tap
sudo systemctl stop skylens-tap
sudo systemctl restart skylens-tap

# NATS
sudo systemctl restart nats-server

# PostgreSQL
sudo systemctl restart postgresql

# Redis
sudo systemctl restart redis
```

### Emergency Reset

```bash
# Nuclear option: restart everything
sudo systemctl restart nats-server
sudo systemctl restart skylens-node
# SSH to each TAP and restart skylens-tap
```

### View All Logs

```bash
# Tail all relevant logs
journalctl -u skylens-node -u nats-server -u postgresql -u redis -f
```

### Inject Test Data

```bash
# Quick test drone
curl -X POST "http://localhost:8080/api/test/drone?preset=dji-mini"

# Start simulation
curl -X POST "http://localhost:8080/api/test/simulate?action=start"

# Stop simulation
curl -X POST "http://localhost:8080/api/test/simulate?action=stop"

# Clear all test data
curl -X POST "http://localhost:8080/api/test/clear"
```

---

## CHECKLIST SIGN-OFF

| Section | Verified By | Time | Notes |
|---------|-------------|------|-------|
| 1.1 TAP Health | | | |
| 1.2 Node Health | | | |
| 1.3 NATS Connectivity | | | |
| 1.4 Database State | | | |
| 1.5 Dashboard | | | |
| 1.6 API Endpoints | | | |
| 1.7 Test Injection | | | |

**System Ready for Test:** [ ] YES / [ ] NO

**Blockers/Issues:**
_____________________________________________
_____________________________________________
_____________________________________________

---

*Document Version: 1.0*
*Created: 2026-02-08*
*For: Skylens UAV Detection System*
