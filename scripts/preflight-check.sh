#!/bin/bash
#
# Skylens Pre-Flight Check Script
# Run this before test day to verify all systems are operational
#
# Usage: ./preflight-check.sh [node-ip]
#

set -e

NODE_IP="${1:-localhost}"
PASSED=0
FAILED=0
WARNINGS=0

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

print_header() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  $1${NC}"
    echo -e "${BLUE}========================================${NC}"
}

check_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
    ((PASSED++))
}

check_fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    ((FAILED++))
}

check_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
    ((WARNINGS++))
}

check_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

# ============================================================================
# NODE HEALTH CHECKS
# ============================================================================

print_header "NODE HEALTH CHECKS"

if systemctl is-active --quiet skylens-node 2>/dev/null; then
else
    echo "       Fix: sudo systemctl start skylens-node"
fi

# Check HTTP API health
HTTP_HEALTH=$(curl -s --connect-timeout 5 "http://${NODE_IP}:8080/health" 2>/dev/null || echo "FAILED")
if echo "$HTTP_HEALTH" | grep -q '"status":"ok"'; then
    check_pass "HTTP API is healthy"
else
    check_fail "HTTP API health check failed"
    echo "       Response: $HTTP_HEALTH"
fi

# Check readiness
HTTP_READY=$(curl -s --connect-timeout 5 "http://${NODE_IP}:8080/ready" 2>/dev/null || echo "FAILED")
if echo "$HTTP_READY" | grep -q '"status":"ready"'; then
    check_pass "Node is ready"
else
    check_warn "Node readiness check indicates issues"
    echo "       Response: $HTTP_READY"
fi

# Check WebSocket port
if nc -z "${NODE_IP}" 8081 2>/dev/null; then
    check_pass "WebSocket port 8081 is open"
else
    check_fail "WebSocket port 8081 is NOT open"
fi

# ============================================================================
# NATS CHECKS
# ============================================================================

print_header "NATS CONNECTIVITY"

# Check NATS server
if systemctl is-active --quiet nats-server 2>/dev/null; then
    check_pass "NATS server is running"
else
    # Try nats-server as alternative service name
    if systemctl is-active --quiet nats 2>/dev/null; then
        check_pass "NATS server is running (as 'nats')"
    else
        check_fail "NATS server is NOT running"
        echo "       Fix: sudo systemctl start nats-server"
    fi
fi

# Check NATS port
if nc -z localhost 4222 2>/dev/null; then
    check_pass "NATS port 4222 is open"
else
    check_fail "NATS port 4222 is NOT open"
fi

# Test NATS publish (if nats CLI available)
if command -v nats &> /dev/null; then
    if nats pub test.preflight "preflight-check" 2>/dev/null; then
        check_pass "NATS publish test successful"
    else
        check_fail "NATS publish test failed"
    fi
else
    check_warn "NATS CLI not installed - skipping publish test"
    echo "       Install: go install github.com/nats-io/natscli/nats@latest"
fi

# ============================================================================
# DATABASE CHECKS
# ============================================================================

print_header "DATABASE CHECKS"

# Check PostgreSQL
if systemctl is-active --quiet postgresql 2>/dev/null; then
    check_pass "PostgreSQL is running"
else
    check_warn "PostgreSQL service not detected (may be running differently)"
fi

# Test PostgreSQL connection
if psql -U skylens -d skylens -c "SELECT 1" &>/dev/null; then
    check_pass "PostgreSQL connection successful"

    # Check tables exist
    TABLE_COUNT=$(psql -U skylens -d skylens -t -c "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'" 2>/dev/null | tr -d ' ')
    if [ "$TABLE_COUNT" -ge 5 ]; then
        check_pass "Database schema exists ($TABLE_COUNT tables)"
    else
        check_warn "Database schema may be incomplete ($TABLE_COUNT tables)"
    fi
else
    check_warn "PostgreSQL connection failed (database may be optional)"
fi

# Check Redis
if systemctl is-active --quiet redis 2>/dev/null || systemctl is-active --quiet redis-server 2>/dev/null; then
    check_pass "Redis is running"
else
    check_warn "Redis service not detected"
fi

# Test Redis connection
if redis-cli ping 2>/dev/null | grep -q "PONG"; then
    check_pass "Redis connection successful"
else
    check_warn "Redis connection failed (cache may be optional)"
fi

# ============================================================================
# API ENDPOINT CHECKS
# ============================================================================

print_header "API ENDPOINT CHECKS"

endpoints=(
    "/api/status"
    "/api/drones"
    "/api/taps"
    "/api/alerts"
    "/api/fleet"
    "/api/threat"
    "/api/stats"
)

for endpoint in "${endpoints[@]}"; do
    response=$(curl -s --connect-timeout 5 -o /dev/null -w "%{http_code}" "http://${NODE_IP}:8080${endpoint}" 2>/dev/null || echo "000")
    if [ "$response" = "200" ]; then
        check_pass "GET ${endpoint} -> 200 OK"
    else
        check_fail "GET ${endpoint} -> ${response}"
    fi
done

# ============================================================================
# DASHBOARD PAGES
# ============================================================================

print_header "DASHBOARD PAGES"

pages=(
    "/"
    "/airspace"
    "/fleet"
    "/taps"
    "/alerts"
    "/settings"
    "/system"
)

for page in "${pages[@]}"; do
    response=$(curl -s --connect-timeout 5 -o /dev/null -w "%{http_code}" "http://${NODE_IP}:8080${page}" 2>/dev/null || echo "000")
    if [ "$response" = "200" ]; then
        check_pass "Page ${page} -> 200 OK"
    else
        check_fail "Page ${page} -> ${response}"
    fi
done

# ============================================================================
# TEST INJECTION
# ============================================================================

print_header "TEST INJECTION VERIFICATION"

# Inject test drone
check_info "Injecting test drone..."
INJECT_RESULT=$(curl -s -X POST "http://${NODE_IP}:8080/api/test/drone?preset=dji-mini" 2>/dev/null || echo "FAILED")
if echo "$INJECT_RESULT" | grep -q "identifier"; then
    check_pass "Test drone injection successful"

    # Verify drone appears in list
    sleep 1
    DRONE_COUNT=$(curl -s "http://${NODE_IP}:8080/api/drones" 2>/dev/null | grep -c "identifier" || echo "0")
    if [ "$DRONE_COUNT" -ge 1 ]; then
        check_pass "Test drone visible in /api/drones"
    else
        check_fail "Test drone NOT visible in /api/drones"
    fi

    # Clear test data
    CLEAR_RESULT=$(curl -s -X POST "http://${NODE_IP}:8080/api/test/clear" 2>/dev/null || echo "FAILED")
    if echo "$CLEAR_RESULT" | grep -q "ok"; then
        check_pass "Test data cleared successfully"
    else
        check_warn "Test data clear may have failed"
    fi
else
    check_fail "Test drone injection failed"
    echo "       Response: $INJECT_RESULT"
fi

# ============================================================================
# SYSTEM RESOURCES
# ============================================================================

print_header "SYSTEM RESOURCES"

# CPU usage
CPU_USAGE=$(top -bn1 | grep "Cpu(s)" | awk '{print $2}' | cut -d'%' -f1 || echo "0")
check_info "CPU Usage: ${CPU_USAGE}%"
if (( $(echo "$CPU_USAGE < 80" | bc -l 2>/dev/null || echo 1) )); then
    check_pass "CPU usage is acceptable"
else
    check_warn "CPU usage is high (${CPU_USAGE}%)"
fi

# Memory usage
MEM_TOTAL=$(free -m | awk '/^Mem:/{print $2}')
MEM_USED=$(free -m | awk '/^Mem:/{print $3}')
MEM_PERCENT=$((MEM_USED * 100 / MEM_TOTAL))
check_info "Memory: ${MEM_USED}MB / ${MEM_TOTAL}MB (${MEM_PERCENT}%)"
if [ "$MEM_PERCENT" -lt 80 ]; then
    check_pass "Memory usage is acceptable"
else
    check_warn "Memory usage is high (${MEM_PERCENT}%)"
fi

# Disk space
DISK_PERCENT=$(df / | awk 'NR==2 {print $5}' | tr -d '%')
check_info "Disk usage: ${DISK_PERCENT}%"
if [ "$DISK_PERCENT" -lt 90 ]; then
    check_pass "Disk space is acceptable"
else
    check_warn "Disk space is low (${DISK_PERCENT}%)"
fi

# ============================================================================
# SUMMARY
# ============================================================================

print_header "PREFLIGHT CHECK SUMMARY"

echo ""
echo -e "  ${GREEN}Passed:${NC}   $PASSED"
echo -e "  ${RED}Failed:${NC}   $FAILED"
echo -e "  ${YELLOW}Warnings:${NC} $WARNINGS"
echo ""

if [ "$FAILED" -eq 0 ]; then
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  ALL CRITICAL CHECKS PASSED${NC}"
    echo -e "${GREEN}  System is ready for test day!${NC}"
    echo -e "${GREEN}============================================${NC}"
    exit 0
else
    echo -e "${RED}============================================${NC}"
    echo -e "${RED}  $FAILED CRITICAL CHECK(S) FAILED${NC}"
    echo -e "${RED}  Review failures above before proceeding${NC}"
    echo -e "${RED}============================================${NC}"
    exit 1
fi
