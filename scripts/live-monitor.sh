#!/bin/bash
#
# Skylens Live Monitoring Script
# Run this during test day to monitor system in real-time
#
# Usage: ./live-monitor.sh [mode]
# Modes: all (default), detections, taps, logs, stats
#

MODE="${1:-all}"
NODE_IP="${2:-localhost}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

print_header() {
    echo -e "${CYAN}========================================${NC}"
    echo -e "${CYAN}  $1${NC}"
    echo -e "${CYAN}========================================${NC}"
}

case "$MODE" in
    detections)
        print_header "LIVE DETECTION STREAM"
        echo "Subscribing to skylens.detections.* ..."
        echo "Press Ctrl+C to stop"
        echo ""
        if command -v nats &> /dev/null; then
            nats sub "skylens.detections.*" --raw
        else
            echo -e "${RED}Error: nats CLI not installed${NC}"
            echo "Install: go install github.com/nats-io/natscli/nats@latest"
            exit 1
        fi
        ;;

    taps)
        print_header "TAP HEARTBEAT MONITOR"
        echo "Subscribing to skylens.heartbeats.* ..."
        echo "Heartbeats should appear every 5 seconds per TAP"
        echo "Press Ctrl+C to stop"
        echo ""
        if command -v nats &> /dev/null; then
            nats sub "skylens.heartbeats.*" --raw
        else
            echo -e "${RED}Error: nats CLI not installed${NC}"
            exit 1
        fi
        ;;

    logs)
        print_header "NODE LOGS (REAL-TIME)"
        echo "Following skylens-node logs..."
        echo "Press Ctrl+C to stop"
        echo ""
        journalctl -u skylens-node -f --output=short-precise
        ;;

    stats)
        print_header "LIVE STATISTICS"
        echo "Polling /api/fleet every 5 seconds..."
        echo "Press Ctrl+C to stop"
        echo ""
        while true; do
            clear
            echo -e "${CYAN}=== SKYLENS LIVE STATS ===${NC}"
            echo "Time: $(date '+%H:%M:%S')"
            echo ""

            # Fleet stats
            FLEET=$(curl -s "http://${NODE_IP}:8080/api/fleet" 2>/dev/null)
            if [ -n "$FLEET" ]; then
                echo -e "${GREEN}Fleet Status:${NC}"
                echo "$FLEET" | jq -r '"  Total UAVs:  \(.total_uavs // 0)"' 2>/dev/null
                echo "$FLEET" | jq -r '"  Active UAVs: \(.active_uavs // 0)"' 2>/dev/null
                echo "$FLEET" | jq -r '"  Lost UAVs:   \(.lost_uavs // 0)"' 2>/dev/null
            fi

            echo ""

            # TAP stats
            TAPS=$(curl -s "http://${NODE_IP}:8080/api/taps" 2>/dev/null)
            if [ -n "$TAPS" ]; then
                echo -e "${GREEN}TAP Status:${NC}"
                echo "$TAPS" | jq -r '.[] | "  \(.id): \(.status // "UNKNOWN") | Detections: \(.detections_sent // 0) | PPS: \(.packets_per_second // 0)"' 2>/dev/null
            fi

            echo ""

            # Threat level
            THREAT=$(curl -s "http://${NODE_IP}:8080/api/threat" 2>/dev/null)
            if [ -n "$THREAT" ]; then
                LEVEL=$(echo "$THREAT" | jq -r '.threat_level // "UNKNOWN"' 2>/dev/null)
                case "$LEVEL" in
                    "LOW"|"NONE")
                        echo -e "${GREEN}Threat Level: $LEVEL${NC}"
                        ;;
                    "MEDIUM")
                        echo -e "${YELLOW}Threat Level: $LEVEL${NC}"
                        ;;
                    "HIGH"|"CRITICAL")
                        echo -e "${RED}Threat Level: $LEVEL${NC}"
                        ;;
                    *)
                        echo "Threat Level: $LEVEL"
                        ;;
                esac
            fi

            echo ""

            # System health
            HEALTH=$(curl -s "http://${NODE_IP}:8080/api/system/stats" 2>/dev/null)
            if [ -n "$HEALTH" ]; then
                echo -e "${GREEN}System Health:${NC}"
                echo "$HEALTH" | jq -r '"  CPU: \(.cpu_percent // 0)% | Memory: \(.memory_percent // 0)%"' 2>/dev/null
            fi

            echo ""
            echo -e "${BLUE}(Refreshing every 5 seconds - Ctrl+C to stop)${NC}"
            sleep 5
        done
        ;;

    drones)
        print_header "DRONE LIST (REAL-TIME)"
        echo "Polling /api/drones every 3 seconds..."
        echo "Press Ctrl+C to stop"
        echo ""
        while true; do
            clear
            echo -e "${CYAN}=== ACTIVE DRONES ===${NC}"
            echo "Time: $(date '+%H:%M:%S')"
            echo ""

            DRONES=$(curl -s "http://${NODE_IP}:8080/api/drones" 2>/dev/null)
            if [ -n "$DRONES" ]; then
                COUNT=$(echo "$DRONES" | jq 'length' 2>/dev/null)
                echo "Total drones: $COUNT"
                echo ""
                echo "$DRONES" | jq -r '.[] | "\(.identifier[0:20])... | \(.manufacturer // "UNK") \(.model // "") | RSSI: \(.rssi // 0) | Alt: \(.altitude_geo // 0)m | \(.status // "UNKNOWN")"' 2>/dev/null | head -20
            else
                echo "No drones detected or API unavailable"
            fi

            echo ""
            echo -e "${BLUE}(Refreshing every 3 seconds - Ctrl+C to stop)${NC}"
            sleep 3
        done
        ;;

    all)
        print_header "SKYLENS LIVE MONITOR"
        echo ""
        echo "Available monitoring modes:"
        echo ""
        echo "  ./live-monitor.sh detections  - Watch NATS detection stream"
        echo "  ./live-monitor.sh taps        - Watch TAP heartbeats"
        echo "  ./live-monitor.sh logs        - Follow Node logs"
        echo "  ./live-monitor.sh stats       - Live stats dashboard"
        echo "  ./live-monitor.sh drones      - Watch drone list"
        echo ""
        echo "For multi-terminal setup, run each in separate terminals:"
        echo ""
        echo "  Terminal 1: ./live-monitor.sh logs"
        echo "  Terminal 2: ./live-monitor.sh detections"
        echo "  Terminal 3: ./live-monitor.sh stats"
        echo ""

        # Quick status check
        echo -e "${CYAN}--- Quick Status Check ---${NC}"

        # Node health
        HEALTH=$(curl -s --connect-timeout 3 "http://${NODE_IP}:8080/health" 2>/dev/null)
        if echo "$HEALTH" | grep -q '"status":"ok"'; then
            echo -e "${GREEN}Node: HEALTHY${NC}"
        else
            echo -e "${RED}Node: UNHEALTHY OR UNREACHABLE${NC}"
        fi

        # NATS
        if nc -z localhost 4222 2>/dev/null; then
            echo -e "${GREEN}NATS: CONNECTED${NC}"
        else
            echo -e "${RED}NATS: NOT REACHABLE${NC}"
        fi

        # Drone count
        DRONES=$(curl -s --connect-timeout 3 "http://${NODE_IP}:8080/api/drones" 2>/dev/null)
        DRONE_COUNT=$(echo "$DRONES" | jq 'length' 2>/dev/null || echo "0")
        echo -e "${GREEN}Active Drones: ${DRONE_COUNT}${NC}"

        # TAP count
        TAPS=$(curl -s --connect-timeout 3 "http://${NODE_IP}:8080/api/taps" 2>/dev/null)
        TAP_COUNT=$(echo "$TAPS" | jq 'length' 2>/dev/null || echo "0")
        ONLINE_TAPS=$(echo "$TAPS" | jq '[.[] | select(.status == "ONLINE")] | length' 2>/dev/null || echo "0")
        echo -e "${GREEN}TAPs: ${ONLINE_TAPS}/${TAP_COUNT} online${NC}"

        echo ""
        ;;

    *)
        echo "Unknown mode: $MODE"
        echo "Usage: ./live-monitor.sh [detections|taps|logs|stats|drones|all]"
        exit 1
        ;;
esac
