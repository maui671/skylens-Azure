#!/bin/bash
#
# Skylens Node Auto-Install Script
# Takes a fresh Rocky Linux 9 / RHEL 9 / AlmaLinux 9 box to a fully running Skylens Node.
#
# Usage: sudo bash install-node.sh
#
# All prompts are collected upfront, then the install runs autonomously.
# Safe to re-run — each phase checks if work is already done.
#

# ============================================================================
# Constants
# ============================================================================

NATS_VERSION="2.10.24"
GO_VERSION="1.24.0"
SKYLENS_REPO="github.com/K13094/skylens"
SKYLENS_REPO_SSH="git@github.com:K13094/skylens.git"

# Clone to the invoking user's home directory (not /root when using sudo)
if [[ -n "${SUDO_USER:-}" ]]; then
    USER_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
else
    USER_HOME="$HOME"
fi
BUILD_DIR="${USER_HOME}/skylens"
LOG_FILE="/var/log/skylens-install.log"
CONFIG_DIR="/etc/skylens"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
BINARY_PATH="/usr/bin/skylens-node"

# ============================================================================
# Colors + output helpers
# ============================================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

PHASE_TOTAL=9
PHASE_CURRENT=0

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG_FILE" 2>/dev/null
}

info() {
    echo -e "${BLUE}[INFO]${NC} $*"
    log "INFO: $*"
}

ok() {
    echo -e "${GREEN}[  OK]${NC} $*"
    log "OK: $*"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
    log "WARN: $*"
}

fail() {
    echo -e "${RED}[FAIL]${NC} $*"
    log "FAIL: $*"
}

die() {
    fail "$*"
    fail "Installation log: ${LOG_FILE}"
    echo ""
    echo "  Last 20 lines of log:"
    tail -20 "$LOG_FILE" 2>/dev/null | sed 's/^/    /'
    echo ""
    exit 1
}

phase() {
    ((PHASE_CURRENT++)) || true
    echo ""
    echo -e "${CYAN}${BOLD}[${PHASE_CURRENT}/${PHASE_TOTAL}] $*${NC}"
    echo -e "${CYAN}$(printf '%.0s─' {1..60})${NC}"
    log "===== PHASE ${PHASE_CURRENT}/${PHASE_TOTAL}: $* ====="
}

# Run a command, log output to file AND show on screen, return REAL exit code.
# Usage: run_cmd <description> <command...>
# This is the key function — no pipes masking exit codes.
run_cmd() {
    local desc="$1"
    shift
    local tmpout
    tmpout=$(mktemp /tmp/skylens-cmd-XXXXXX)

    info "${desc}"
    # Run command, capture exit code properly
    "$@" > "$tmpout" 2>&1
    local rc=$?

    # Show last few lines on screen, full output to log
    cat "$tmpout" >> "$LOG_FILE"
    tail -5 "$tmpout" | sed 's/^/    /'
    rm -f "$tmpout"
    return $rc
}

# Same as run_cmd but doesn't die on failure, returns exit code
try_cmd() {
    local desc="$1"
    shift
    local tmpout
    tmpout=$(mktemp /tmp/skylens-cmd-XXXXXX)

    info "${desc}"
    "$@" > "$tmpout" 2>&1
    local rc=$?

    cat "$tmpout" >> "$LOG_FILE"
    if [[ $rc -ne 0 ]]; then
        tail -10 "$tmpout" | sed 's/^/    /'
    else
        tail -3 "$tmpout" | sed 's/^/    /'
    fi
    rm -f "$tmpout"
    return $rc
}

generate_password() {
    openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'
}

generate_jwt_secret() {
    openssl rand -base64 32 2>/dev/null || head -c 32 /dev/urandom | base64
}

# ============================================================================
# Pre-checks
# ============================================================================

if [[ $EUID -ne 0 ]]; then
    fail "This script must be run as root (use sudo)"
    exit 1
fi

if ! grep -qiE 'rocky|rhel|red hat|centos|alma' /etc/os-release 2>/dev/null; then
    fail "This script requires Rocky Linux 9 / RHEL 9 / AlmaLinux 9"
    exit 1
fi

MAJOR_VERSION=$(rpm -E '%{rhel}' 2>/dev/null || echo "0")
if [[ "$MAJOR_VERSION" != "9" ]]; then
    fail "This script requires EL 9 (detected: EL ${MAJOR_VERSION})"
    exit 1
fi

# Detect distro for RHEL-specific workarounds
IS_RHEL=false
if grep -qi 'red hat\|rhel' /etc/os-release 2>/dev/null; then
    IS_RHEL=true
fi

# Start logging
mkdir -p "$(dirname "$LOG_FILE")"
echo "=== Skylens Node Install $(date) ===" > "$LOG_FILE"
echo "OS: $(cat /etc/redhat-release 2>/dev/null || echo unknown)" >> "$LOG_FILE"
echo "Kernel: $(uname -r)" >> "$LOG_FILE"
echo "Arch: $(arch)" >> "$LOG_FILE"
echo "IS_RHEL: ${IS_RHEL}" >> "$LOG_FILE"

# ============================================================================
# Interactive prompts (all upfront)
# ============================================================================

echo ""
echo -e "${CYAN}${BOLD}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}${BOLD}║          Skylens Node — Auto Installer                  ║${NC}"
echo -e "${CYAN}${BOLD}║          Rocky Linux 9 / RHEL 9 / AlmaLinux 9          ║${NC}"
echo -e "${CYAN}${BOLD}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  Detected: $(cat /etc/redhat-release 2>/dev/null || echo 'Unknown EL9')"
echo -e "  Build dir: ${BUILD_DIR}"
echo ""

# HTTP port
read -rp "HTTP port [80]: " HTTP_PORT
HTTP_PORT="${HTTP_PORT:-80}"

# WebSocket port
WS_PORT=8081

# Database password
DEFAULT_DB_PASS=$(generate_password)
read -rp "PostgreSQL password for 'skylens' user [auto-generate]: " DB_PASSWORD
DB_PASSWORD="${DB_PASSWORD:-$DEFAULT_DB_PASS}"

# JWT secret
DEFAULT_JWT=$(generate_jwt_secret)
read -rp "JWT secret [auto-generate]: " JWT_SECRET
JWT_SECRET="${JWT_SECRET:-$DEFAULT_JWT}"

# Tailscale
echo ""
read -rp "Install Tailscale? (y/n) [y]: " INSTALL_TAILSCALE
INSTALL_TAILSCALE="${INSTALL_TAILSCALE:-y}"

# GitHub access — only needed if repo not already cloned
CLONE_METHOD=""
GITHUB_PAT=""

if [[ -d "${BUILD_DIR}/.git" ]]; then
    ok "Repository already cloned at ${BUILD_DIR} — skipping GitHub auth"
    CLONE_METHOD="exists"
else
    echo ""
    info "Checking GitHub access for private repo..."

    if command -v gh &>/dev/null && gh auth status &>/dev/null 2>&1; then
        ok "GitHub CLI authenticated"
        CLONE_METHOD="gh"
    elif ssh -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=5 -T git@github.com 2>&1 | grep -q "successfully authenticated"; then
        ok "SSH key authenticated with GitHub"
        CLONE_METHOD="ssh"
    else
        warn "No GitHub CLI or SSH key detected"
        echo ""
        echo "  The Skylens repo is private. You need one of:"
        echo "    1. GitHub CLI (gh auth login)"
        echo "    2. SSH key added to GitHub"
        echo "    3. Personal Access Token (PAT) with repo scope"
        echo ""
        read -rp "Enter GitHub PAT (or press Enter to skip clone): " GITHUB_PAT
        if [[ -n "$GITHUB_PAT" ]]; then
            CLONE_METHOD="pat"
        else
            warn "No GitHub access — you'll need to clone the repo manually"
            echo "  git clone https://<PAT>@${SKYLENS_REPO}.git ${BUILD_DIR}"
            echo "  Then re-run this script."
            CLONE_METHOD="skip"
        fi
    fi
fi

echo ""
echo -e "${CYAN}─── Configuration Summary ───${NC}"
echo "  HTTP port:      ${HTTP_PORT}"
echo "  WebSocket port: ${WS_PORT}"
echo "  DB password:    ${DB_PASSWORD:0:4}****"
echo "  JWT secret:     ${JWT_SECRET:0:8}..."
echo "  Tailscale:      ${INSTALL_TAILSCALE}"
echo "  Clone method:   ${CLONE_METHOD}"
echo "  Build dir:      ${BUILD_DIR}"
echo "  RHEL:           ${IS_RHEL}"
echo ""
read -rp "Proceed with installation? (y/n) [y]: " CONFIRM
CONFIRM="${CONFIRM:-y}"
if [[ "$CONFIRM" != "y" && "$CONFIRM" != "Y" ]]; then
    info "Installation cancelled."
    exit 0
fi

echo ""
info "Starting installation — all output logged to ${LOG_FILE}"
info "This will take 5-10 minutes depending on network speed."
echo ""

# ============================================================================
# Phase 1: System packages
# ============================================================================

phase "System packages"

# --- RHEL-specific: fix repos FIRST before any dnf commands ---
if [[ "$IS_RHEL" == true ]]; then
    info "RHEL detected — setting up repos before installing anything..."

    # Check subscription
    if command -v subscription-manager &>/dev/null; then
        SUB_STATUS=$(subscription-manager status 2>&1 || true)
        log "subscription-manager status: ${SUB_STATUS}"
        if echo "$SUB_STATUS" | grep -qi "unknown\|invalid\|not registered"; then
            warn "RHEL subscription may not be active"
            echo "  If package installs fail, run: subscription-manager register"
        else
            ok "RHEL subscription active"
        fi
    fi

    # Enable CRB (CodeReady Builder) — needed for dependencies
    info "Enabling CRB repo..."
    dnf config-manager --set-enabled crb >> "$LOG_FILE" 2>&1 || \
        subscription-manager repos --enable "codeready-builder-for-rhel-9-$(arch)-rpms" >> "$LOG_FILE" 2>&1 || \
        warn "Could not enable CRB repo (some packages may fail)"

    # Install EPEL — needed for Redis and other packages on RHEL
    if ! rpm -q epel-release &>/dev/null; then
        info "Installing EPEL repository..."
        dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm >> "$LOG_FILE" 2>&1 || \
            warn "EPEL install failed (Redis may not be available)"
    else
        ok "EPEL already installed"
    fi
fi

# Skip dnf update — it's slow, can hang, and isn't needed for a fresh install.
# The user can run 'dnf update' themselves after install.
info "Skipping dnf update (run manually after install if desired)"

# Install packages one at a time with REAL exit code checking
PACKAGES=(
    git gcc make wget curl tar unzip jq
    nmap-ncat
    firewalld tuned
    policycoreutils-python-utils
    selinux-policy-targeted
)

info "Installing base packages..."
INSTALL_FAILED=0
for pkg in "${PACKAGES[@]}"; do
    if rpm -q "$pkg" &>/dev/null; then
        ok "  ${pkg} — already installed"
        continue
    fi
    if try_cmd "  Installing ${pkg}..." dnf install -y "$pkg"; then
        ok "  ${pkg} — installed"
    else
        warn "  ${pkg} — FAILED (continuing)"
        ((INSTALL_FAILED++)) || true
    fi
done

if [[ $INSTALL_FAILED -eq 0 ]]; then
    ok "All base packages installed"
else
    warn "${INSTALL_FAILED} package(s) failed — check log"
fi

# Enable tuned
systemctl enable --now tuned >> "$LOG_FILE" 2>&1 || true
ok "Phase 1 complete"

# ============================================================================
# Phase 2: Tailscale
# ============================================================================

phase "Tailscale"

if [[ "$INSTALL_TAILSCALE" == "y" || "$INSTALL_TAILSCALE" == "Y" ]]; then
    if command -v tailscale &>/dev/null; then
        ok "Tailscale already installed ($(tailscale version 2>/dev/null | head -1))"
    else
        if try_cmd "Installing Tailscale..." bash -c 'curl -fsSL https://tailscale.com/install.sh | sh'; then
            systemctl enable tailscaled >> "$LOG_FILE" 2>&1 || true
            systemctl start tailscaled >> "$LOG_FILE" 2>&1 || true
            ok "Tailscale installed"
        else
            warn "Tailscale install failed (non-critical — install manually later)"
        fi
    fi

    if tailscale status &>/dev/null 2>&1; then
        TS_IP=$(tailscale ip -4 2>/dev/null || echo "unknown")
        ok "Tailscale connected (IP: ${TS_IP})"
    else
        warn "Tailscale installed but not authenticated"
        info "Run 'tailscale up --auth-key=tskey-...' after install"
    fi
else
    info "Tailscale skipped"
fi

# ============================================================================
# Phase 3: PostgreSQL 16
# ============================================================================

phase "PostgreSQL 16"

if command -v psql &>/dev/null && psql --version 2>/dev/null | grep -q "16"; then
    ok "PostgreSQL 16 already installed"
else
    # Install PGDG repo
    if ! rpm -q pgdg-redhat-repo &>/dev/null; then
        if ! try_cmd "Installing PGDG repository..." dnf install -y --nogpgcheck https://download.postgresql.org/pub/repos/yum/reporpms/EL-9-x86_64/pgdg-redhat-repo-latest.noarch.rpm; then
            warn "PGDG repo RPM install had issues — trying anyway"
        fi
    else
        ok "PGDG repo already installed"
    fi

    # Fix $releasever in ALL PGDG repo files.
    # RHEL 9.0 expands $releasever to "9.0" but PGDG mirrors only have "9".
    # Rocky/Alma also benefit from this fix for minor version mismatches.
    info "Pinning PGDG repos to EL major version..."
    RHEL_MAJOR=$(rpm -E '%{rhel}' 2>/dev/null || echo "9")
    FIXED_REPOS=0
    for repofile in /etc/yum.repos.d/pgdg-redhat-all.repo /etc/yum.repos.d/pgdg*.repo; do
        if [[ -f "$repofile" ]]; then
            if grep -q '\$releasever' "$repofile" 2>/dev/null; then
                sed -i "s/\\\$releasever/${RHEL_MAJOR}/g" "$repofile"
                ((FIXED_REPOS++)) || true
                log "Fixed releasever in $(basename "$repofile")"
            fi
        fi
    done
    if [[ $FIXED_REPOS -gt 0 ]]; then
        ok "Fixed \$releasever in ${FIXED_REPOS} repo file(s) → pinned to EL${RHEL_MAJOR}"
    else
        ok "PGDG repos already pinned (no \$releasever found)"
    fi

    # Disable built-in postgresql module (conflicts with PGDG)
    info "Disabling built-in postgresql module..."
    dnf -y module disable postgresql >> "$LOG_FILE" 2>&1 || true

    # Clean and rebuild cache with fixed repos
    info "Rebuilding dnf cache..."
    dnf clean all >> "$LOG_FILE" 2>&1 || true
    if ! try_cmd "Running dnf makecache..." dnf makecache; then
        warn "dnf makecache had issues — install may still work"
    fi

    # Verify PG16 is available before trying to install
    info "Checking postgresql16 availability..."
    if dnf list available postgresql16-server 2>/dev/null | grep -q postgresql16-server; then
        ok "postgresql16-server found in repos"
    else
        fail "postgresql16-server NOT found in any repo!"
        echo ""
        echo "  Available PGDG repos:"
        dnf repolist 2>/dev/null | grep -i pgdg | sed 's/^/    /' || echo "    (none)"
        echo ""
        echo "  Repo files in /etc/yum.repos.d/:"
        ls -la /etc/yum.repos.d/pgdg* 2>/dev/null | sed 's/^/    /' || echo "    (none)"
        echo ""
        echo "  Contents of PGDG repo file:"
        head -20 /etc/yum.repos.d/pgdg-redhat-all.repo 2>/dev/null | sed 's/^/    /' || echo "    (file not found)"
        echo ""
        die "Cannot find postgresql16-server — PGDG repo is broken. See debug info above."
    fi

    # Install PostgreSQL 16
    if ! run_cmd "Installing PostgreSQL 16..." dnf install -y postgresql16-server postgresql16; then
        echo ""
        fail "PostgreSQL 16 install failed!"
        echo ""

        if [[ "$IS_RHEL" == true ]]; then
            info "On RHEL, trying to enable CRB repo and retry..."
            dnf config-manager --set-enabled crb >> "$LOG_FILE" 2>&1 || true
            subscription-manager repos --enable "codeready-builder-for-rhel-9-$(arch)-rpms" >> "$LOG_FILE" 2>&1 || true

            if ! run_cmd "Retrying PostgreSQL 16..." dnf install -y postgresql16-server postgresql16; then
                die "PostgreSQL 16 install failed on retry"
            fi
        else
            die "PostgreSQL 16 install failed"
        fi
    fi
    ok "PostgreSQL 16 installed"
fi

# Init database
if [[ ! -f /var/lib/pgsql/16/data/PG_VERSION ]]; then
    if ! run_cmd "Initializing PostgreSQL database..." /usr/pgsql-16/bin/postgresql-16-setup initdb; then
        die "PostgreSQL initdb failed"
    fi
    ok "Database initialized"
else
    ok "Database already initialized"
fi

# IMPORTANT: Start PostgreSQL with DEFAULT pg_hba.conf first.
# The default uses "peer" auth which lets `su - postgres` run psql
# without a password. We MUST create the role/database BEFORE
# changing pg_hba.conf, otherwise postgres can't auth to itself.

info "Starting PostgreSQL 16 (with default auth)..."
systemctl enable postgresql-16 >> "$LOG_FILE" 2>&1 || true
systemctl restart postgresql-16 >> "$LOG_FILE" 2>&1
sleep 2

if ! systemctl is-active --quiet postgresql-16; then
    fail "PostgreSQL 16 failed to start!"
    journalctl -u postgresql-16 --no-pager -n 15 2>/dev/null | sed 's/^/    /'
    die "PostgreSQL 16 is not running"
fi
ok "PostgreSQL 16 running"

# Create role and database FIRST — while peer auth still works
# Use full path to psql (might not be in postgres user's PATH)
PG_PSQL="/usr/pgsql-16/bin/psql"
if [[ ! -f "$PG_PSQL" ]]; then
    # Fallback: find it
    PG_PSQL=$(find /usr -name psql -path "*/pgsql-16/*" 2>/dev/null | head -1)
    if [[ -z "$PG_PSQL" ]]; then
        PG_PSQL=$(command -v psql 2>/dev/null || echo "psql")
    fi
fi
info "Using psql: ${PG_PSQL}"

info "Creating PostgreSQL role 'skylens'..."
if ! su -s /bin/bash - postgres -c "${PG_PSQL} -tAc \"SELECT 1 FROM pg_roles WHERE rolname='skylens'\"" 2>/dev/null | grep -q 1; then
    if su -s /bin/bash - postgres -c "${PG_PSQL} -c \"CREATE ROLE skylens WITH LOGIN PASSWORD '${DB_PASSWORD}'\"" >> "$LOG_FILE" 2>&1; then
        ok "Role 'skylens' created"
    else
        fail "Failed to create role. Trying alternative method..."
        # Try with sudo -u instead of su
        if sudo -u postgres "${PG_PSQL}" -c "CREATE ROLE skylens WITH LOGIN PASSWORD '${DB_PASSWORD}'" >> "$LOG_FILE" 2>&1; then
            ok "Role 'skylens' created (via sudo)"
        else
            die "Failed to create PostgreSQL role 'skylens'"
        fi
    fi
else
    ok "Role 'skylens' already exists"
fi

info "Creating database 'skylens'..."
if ! su -s /bin/bash - postgres -c "${PG_PSQL} -tAc \"SELECT 1 FROM pg_database WHERE datname='skylens'\"" 2>/dev/null | grep -q 1; then
    if su -s /bin/bash - postgres -c "${PG_PSQL} -c \"CREATE DATABASE skylens OWNER skylens\"" >> "$LOG_FILE" 2>&1; then
        ok "Database 'skylens' created"
    else
        if sudo -u postgres "${PG_PSQL}" -c "CREATE DATABASE skylens OWNER skylens" >> "$LOG_FILE" 2>&1; then
            ok "Database 'skylens' created (via sudo)"
        else
            die "Failed to create database 'skylens'"
        fi
    fi
else
    ok "Database 'skylens' already exists"
fi

# NOW configure pg_hba.conf — after role/database exist.
# Keep "local all postgres peer" so postgres superuser can still connect.
# Change everything else to scram-sha-256 for password auth.
PG_HBA="/var/lib/pgsql/16/data/pg_hba.conf"
if [[ -f "$PG_HBA" ]]; then
    info "Configuring pg_hba.conf..."
    # Back up original
    cp "$PG_HBA" "${PG_HBA}.bak" 2>/dev/null || true

    # Write a clean pg_hba.conf that:
    # 1. Keeps peer auth for postgres superuser (local socket)
    # 2. Uses scram-sha-256 for everything else (local socket + TCP)
    cat > "$PG_HBA" << 'PGHBA'
# Skylens pg_hba.conf — generated by install-node.sh
# TYPE  DATABASE  USER       ADDRESS        METHOD

# Postgres superuser: peer auth via local socket (no password needed)
local   all       postgres                  peer

# All other local socket connections: password
local   all       all                       scram-sha-256

# IPv4 local connections: password
host    all       all        127.0.0.1/32   scram-sha-256

# IPv6 local connections: password
host    all       all        ::1/128        scram-sha-256
PGHBA
    ok "pg_hba.conf configured (peer for postgres, scram-sha-256 for all others)"

    # Restart to apply new pg_hba.conf
    info "Restarting PostgreSQL to apply auth changes..."
    systemctl restart postgresql-16 >> "$LOG_FILE" 2>&1
    sleep 2

    if ! systemctl is-active --quiet postgresql-16; then
        fail "PostgreSQL failed to restart after pg_hba.conf change!"
        warn "Restoring backup pg_hba.conf..."
        cp "${PG_HBA}.bak" "$PG_HBA" 2>/dev/null || true
        systemctl restart postgresql-16 >> "$LOG_FILE" 2>&1
        warn "Restored original pg_hba.conf"
    else
        ok "PostgreSQL restarted with new auth config"
    fi
else
    warn "pg_hba.conf not found at ${PG_HBA}"
fi

# Verify the skylens user can connect with password over TCP
info "Verifying skylens database connection..."
if PGPASSWORD="${DB_PASSWORD}" "${PG_PSQL}" -h 127.0.0.1 -U skylens -d skylens -c "SELECT 1" >> "$LOG_FILE" 2>&1; then
    ok "Database connection verified (skylens user, password auth)"
else
    warn "Cannot verify connection — skylens-node will retry on startup"
fi

# ============================================================================
# Phase 4: Redis
# ============================================================================

phase "Redis"

if command -v redis-server &>/dev/null; then
    ok "Redis already installed"
else
    if ! try_cmd "Installing Redis..." dnf install -y redis; then
        if [[ "$IS_RHEL" == true ]]; then
            info "Redis not in AppStream — trying with EPEL..."
            # Make sure EPEL is installed
            dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm >> "$LOG_FILE" 2>&1 || true
            if ! run_cmd "Installing Redis from EPEL..." dnf install -y redis; then
                die "Failed to install Redis"
            fi
        else
            die "Failed to install Redis"
        fi
    fi
    ok "Redis installed"
fi

# Configure Redis
REDIS_CONF=""
for candidate in /etc/redis/redis.conf /etc/redis.conf; do
    [[ -f "$candidate" ]] && REDIS_CONF="$candidate" && break
done

if [[ -n "$REDIS_CONF" ]]; then
    if ! grep -q "^maxmemory 512mb" "$REDIS_CONF" 2>/dev/null; then
        info "Configuring Redis (512MB LRU)..."
        sed -i '/^# maxmemory /d; /^maxmemory /d' "$REDIS_CONF"
        sed -i '/^# maxmemory-policy /d; /^maxmemory-policy /d' "$REDIS_CONF"
        echo "maxmemory 512mb" >> "$REDIS_CONF"
        echo "maxmemory-policy allkeys-lru" >> "$REDIS_CONF"
        ok "Redis configured"
    else
        ok "Redis already configured"
    fi
else
    warn "Redis config not found — using defaults"
fi

systemctl enable --now redis >> "$LOG_FILE" 2>&1 || die "Failed to start Redis"
ok "Redis running"

# ============================================================================
# Phase 5: NATS 2.10
# ============================================================================

phase "NATS 2.10"

if [[ -f /usr/local/bin/nats-server ]] && /usr/local/bin/nats-server --version 2>&1 | grep -q "${NATS_VERSION}"; then
    ok "NATS ${NATS_VERSION} already installed"
else
    NATS_URL="https://github.com/nats-io/nats-server/releases/download/v${NATS_VERSION}/nats-server-v${NATS_VERSION}-linux-amd64.tar.gz"
    if ! run_cmd "Downloading NATS ${NATS_VERSION}..." wget -q "$NATS_URL" -O /tmp/nats-server.tar.gz; then
        die "Failed to download NATS — check network"
    fi
    tar -xzf /tmp/nats-server.tar.gz -C /tmp >> "$LOG_FILE" 2>&1
    cp "/tmp/nats-server-v${NATS_VERSION}-linux-amd64/nats-server" /usr/local/bin/nats-server
    chmod +x /usr/local/bin/nats-server
    rm -rf /tmp/nats-server.tar.gz "/tmp/nats-server-v${NATS_VERSION}-linux-amd64"
    ok "NATS ${NATS_VERSION} installed"
fi

# System user
id nats &>/dev/null || useradd -r -s /sbin/nologin nats >> "$LOG_FILE" 2>&1 || true

# Config
mkdir -p /etc/nats
if [[ ! -f /etc/nats/nats.conf ]]; then
    cat > /etc/nats/nats.conf << 'NATSEOF'
# Skylens NATS Configuration
port: 4222
http_port: 8222
max_payload: 1048576
log_file: "/var/log/nats/nats.log"
logtime: true
max_connections: 256
max_subscriptions: 1024
NATSEOF
    ok "NATS config created"
else
    ok "NATS config exists"
fi

mkdir -p /var/log/nats
chown nats:nats /var/log/nats

# Systemd unit
if [[ ! -f /etc/systemd/system/nats.service ]]; then
    cat > /etc/systemd/system/nats.service << 'NATSUNIT'
[Unit]
Description=NATS Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nats
Group=nats
ExecStart=/usr/local/bin/nats-server -c /etc/nats/nats.conf
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
NATSUNIT
    systemctl daemon-reload >> "$LOG_FILE" 2>&1
    ok "NATS systemd unit created"
else
    ok "NATS systemd unit exists"
fi

systemctl enable --now nats >> "$LOG_FILE" 2>&1 || die "Failed to start NATS"
ok "NATS running"

# ============================================================================
# Phase 6: Go 1.24
# ============================================================================

phase "Go ${GO_VERSION}"

if [[ -d /usr/local/go ]] && /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
    ok "Go ${GO_VERSION} already installed"
else
    if ! run_cmd "Downloading Go ${GO_VERSION}..." wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tar.gz; then
        die "Failed to download Go — check network"
    fi
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go.tar.gz >> "$LOG_FILE" 2>&1
    rm -f /tmp/go.tar.gz
    ok "Go ${GO_VERSION} installed"
fi

export PATH="/usr/local/go/bin:${PATH}"

if [[ ! -f /etc/profile.d/go.sh ]]; then
    echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/go.sh
    ok "Go added to system PATH"
fi

go version >> "$LOG_FILE" 2>&1 || die "Go binary not working"
ok "Go ready: $(go version | awk '{print $3}')"

# ============================================================================
# Phase 7: Clone & Build
# ============================================================================

phase "Clone & Build"

if [[ -d "${BUILD_DIR}/.git" ]]; then
    info "Updating existing repo at ${BUILD_DIR}..."
    cd "$BUILD_DIR"
    git pull >> "$LOG_FILE" 2>&1 || warn "git pull failed — using existing code"
    ok "Repository updated"
elif [[ "$CLONE_METHOD" == "skip" ]]; then
    die "No repo at ${BUILD_DIR} and no GitHub access. Clone manually and re-run."
else
    info "Cloning to ${BUILD_DIR}..."
    rm -rf "$BUILD_DIR"
    mkdir -p "$(dirname "$BUILD_DIR")"

    case "$CLONE_METHOD" in
        gh)
            if ! run_cmd "Cloning via GitHub CLI..." gh repo clone K13094/skylens "$BUILD_DIR"; then
                die "gh clone failed"
            fi
            ;;
        ssh)
            if ! run_cmd "Cloning via SSH..." git clone "$SKYLENS_REPO_SSH" "$BUILD_DIR"; then
                die "SSH clone failed"
            fi
            ;;
        pat)
            info "Cloning via PAT..."
            if ! git clone "https://${GITHUB_PAT}@${SKYLENS_REPO}.git" "$BUILD_DIR" >> "$LOG_FILE" 2>&1; then
                fail "Clone failed!"
                echo "  Make sure your PAT has 'repo' scope"
                echo "  Make sure you're added as a collaborator on K13094/skylens"
                die "Git clone failed with PAT"
            fi
            ok "Repository cloned"
            ;;
        exists)
            ok "Repository exists"
            ;;
    esac
fi

# Chown to the invoking user so they can git pull later without sudo
if [[ -n "${SUDO_USER:-}" ]]; then
    chown -R "${SUDO_USER}:${SUDO_USER}" "$BUILD_DIR" 2>/dev/null || true
fi

# Build
cd "$BUILD_DIR"
info "Building skylens-node..."
info "  Source: ${BUILD_DIR}"
info "  Target: ${BINARY_PATH}"

if ! run_cmd "Compiling..." CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BINARY_PATH" ./cmd/skylens-node/; then
    echo ""
    fail "Build failed! Last 30 lines:"
    tail -30 "$LOG_FILE" | sed 's/^/    /'
    die "Go build failed"
fi

# SELinux
if command -v restorecon &>/dev/null; then
    restorecon -v "$BINARY_PATH" >> "$LOG_FILE" 2>&1 || true
fi

ok "Binary: ${BINARY_PATH} ($(du -h "$BINARY_PATH" | awk '{print $1}'))"

# ============================================================================
# Phase 8: Configure
# ============================================================================

phase "Configure"

mkdir -p "$CONFIG_DIR"
mkdir -p "${CONFIG_DIR}/certs"

if [[ ! -f "$CONFIG_FILE" ]]; then
    info "Writing ${CONFIG_FILE}..."
    cat > "$CONFIG_FILE" << CFGEOF
# Skylens Node Configuration
# Generated by install-node.sh on $(date)

server:
  http_port: ${HTTP_PORT}
  websocket_port: ${WS_PORT}

nats:
  url: "nats://localhost:4222"

database:
  host: "localhost"
  port: 5432
  name: "skylens"
  user: "skylens"
  password: "${DB_PASSWORD}"
  ssl_mode: "disable"

redis:
  url: "redis://localhost:6379"

detection:
  lost_threshold_sec: 1800
  evict_after_min: 0
  trust_decay_rate: 0.1
  max_history_hours: 8760
  max_displayed_drones: 500
  spoof_check_enabled: true
  single_tap_mode: true
  single_tap_min_observations: 5
  single_tap_min_time_span_sec: 30
  single_tap_min_mobility: 0.5

propagation:
  global_environment: "open_field"
  tap_environments: {}
  tap_rssi_offsets: {}

auth:
  enabled: true
  jwt_secret: "${JWT_SECRET}"

telegram:
  enabled: false
  bot_token: ""
  chat_id: ""
  notify_new_drone: true
  notify_spoofing: true
  notify_drone_lost: true
  notify_tap_status: true

tak:
  enabled: false
  address: ""
  use_tls: true
  cert_file: ""
  key_file: ""
  ca_file: ""
  rate_limit_sec: 3
  stale_seconds: 30
  send_controllers: false
CFGEOF
    chmod 640 "$CONFIG_FILE"
    ok "Config written"
else
    ok "Config exists — not overwriting"
fi

# Systemd unit
cat > /etc/systemd/system/skylens-node.service << 'SVCEOF'
[Unit]
Description=Skylens Node - UAV Airspace Monitor
Documentation=https://github.com/K13094/skylens
After=network-online.target nats.service postgresql-16.service redis.service
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/bin/skylens-node \
    -config /etc/skylens/config.yaml \
    -log-format json \
    -log-level info
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
Environment=GOMAXPROCS=4
StandardOutput=journal
StandardError=journal
SyslogIdentifier=skylens-node
LimitNOFILE=65536
LimitNPROC=4096
TimeoutStartSec=30
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
SVCEOF
systemctl daemon-reload >> "$LOG_FILE" 2>&1
ok "Systemd unit created"

# Sysctl tuning
SYSCTL_FILE="/etc/sysctl.d/99-skylens.conf"
if [[ ! -f "$SYSCTL_FILE" ]]; then
    cat > "$SYSCTL_FILE" << 'SYSCTLEOF'
net.core.somaxconn = 8192
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 87380 16777216
vm.swappiness = 1
SYSCTLEOF
    sysctl -p "$SYSCTL_FILE" >> "$LOG_FILE" 2>&1 || true
    ok "Sysctl tuning applied"
else
    ok "Sysctl tuning exists"
fi

# File limits
LIMITS_FILE="/etc/security/limits.d/99-skylens.conf"
if [[ ! -f "$LIMITS_FILE" ]]; then
    cat > "$LIMITS_FILE" << 'LIMITSEOF'
* soft nofile 65536
* hard nofile 65536
LIMITSEOF
    ok "Limits configured"
else
    ok "Limits exist"
fi

# Tuned profile
tuned-adm profile network-latency >> "$LOG_FILE" 2>&1 || true
ok "Tuned: $(tuned-adm active 2>/dev/null | awk '{print $NF}' || echo 'default')"

# Firewall
if ! systemctl is-active --quiet firewalld 2>/dev/null; then
    systemctl enable --now firewalld >> "$LOG_FILE" 2>&1 || true
fi

if systemctl is-active --quiet firewalld 2>/dev/null; then
    firewall-cmd --permanent --zone=public --add-port="${HTTP_PORT}/tcp" >> "$LOG_FILE" 2>&1 || true
    firewall-cmd --permanent --zone=public --add-port=4222/tcp >> "$LOG_FILE" 2>&1 || true
    firewall-cmd --permanent --zone=public --add-port="${WS_PORT}/tcp" >> "$LOG_FILE" 2>&1 || true
    firewall-cmd --permanent --zone=public --add-port=8222/tcp >> "$LOG_FILE" 2>&1 || true
    if ip link show tailscale0 &>/dev/null; then
        firewall-cmd --permanent --zone=trusted --add-interface=tailscale0 >> "$LOG_FILE" 2>&1 || true
    fi
    firewall-cmd --reload >> "$LOG_FILE" 2>&1 || true
    ok "Firewall configured"
fi

# ============================================================================
# Phase 9: Start & Verify
# ============================================================================

phase "Start & Verify"

systemctl enable skylens-node >> "$LOG_FILE" 2>&1 || true
systemctl restart skylens-node >> "$LOG_FILE" 2>&1 || true
info "Waiting for skylens-node to start..."
sleep 4

# Service checks
SERVICES_OK=true
echo ""
for svc in postgresql-16 redis nats skylens-node; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        ok "${svc} — running"
    else
        fail "${svc} — NOT RUNNING"
        journalctl -u "$svc" --no-pager -n 5 2>/dev/null | sed 's/^/    /'
        SERVICES_OK=false
    fi
done

echo ""

# Health check
HTTP_RESULT=$(curl -s --connect-timeout 5 "http://localhost:${HTTP_PORT}/health" 2>/dev/null || echo "UNREACHABLE")
if echo "$HTTP_RESULT" | grep -q '"status"'; then
    ok "Health check passed"
else
    fail "Health check failed: ${HTTP_RESULT}"
    echo "  skylens-node logs:"
    journalctl -u skylens-node --no-pager -n 15 2>/dev/null | tail -10 | sed 's/^/    /'
    SERVICES_OK=false
fi

# NATS check
NATS_RESULT=$(curl -s --connect-timeout 5 "http://localhost:8222/connz" 2>/dev/null || echo "")
if echo "$NATS_RESULT" | grep -q '"num_connections"'; then
    NATS_CONNS=$(echo "$NATS_RESULT" | jq -r '.num_connections // 0' 2>/dev/null || echo "?")
    ok "NATS healthy (${NATS_CONNS} connections)"
else
    warn "NATS monitoring not responding"
fi

# Schema check
TABLE_COUNT=$(PGPASSWORD="${DB_PASSWORD}" psql -h localhost -U skylens -d skylens -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" 2>/dev/null || echo "0")
if [[ "$TABLE_COUNT" -gt 0 ]]; then
    ok "Database schema: ${TABLE_COUNT} tables"
else
    warn "No tables yet — skylens-node may still be initializing"
fi

# ============================================================================
# Summary
# ============================================================================

echo ""
echo -e "${CYAN}${BOLD}╔══════════════════════════════════════════════════════════╗${NC}"
if [[ "$SERVICES_OK" == true ]]; then
    echo -e "${GREEN}${BOLD}║          Installation Complete                          ║${NC}"
else
    echo -e "${YELLOW}${BOLD}║          Installation Complete (with warnings)          ║${NC}"
fi
echo -e "${CYAN}${BOLD}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""

PRIMARY_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "localhost")
TS_IP=$(tailscale ip -4 2>/dev/null || echo "not connected")

echo -e "  ${BOLD}Dashboard:${NC}       http://${PRIMARY_IP}:${HTTP_PORT}"
echo -e "  ${BOLD}WebSocket:${NC}       ws://${PRIMARY_IP}:${WS_PORT}/ws"
echo -e "  ${BOLD}NATS:${NC}            nats://${PRIMARY_IP}:4222"
echo -e "  ${BOLD}NATS Monitor:${NC}    http://${PRIMARY_IP}:8222"
if [[ "$TS_IP" != "not connected" ]]; then
    echo -e "  ${BOLD}Tailscale IP:${NC}    ${TS_IP}"
fi
echo ""
echo -e "  ${BOLD}Default login:${NC}   admin / admin"
echo -e "  ${BOLD}Config:${NC}          ${CONFIG_FILE}"
echo -e "  ${BOLD}Binary:${NC}          ${BINARY_PATH}"
echo -e "  ${BOLD}Source:${NC}          ${BUILD_DIR}"
echo -e "  ${BOLD}Log:${NC}             ${LOG_FILE}"
echo ""
echo -e "  ${BOLD}Next steps:${NC}"
if [[ "$TS_IP" == "not connected" ]]; then
    echo "    1. Connect Tailscale:  tailscale up --auth-key=tskey-..."
    echo "    2. Point TAPs NATS at this node's Tailscale IP"
else
    echo "    1. Point TAPs NATS at ${TS_IP}:4222"
fi
echo "    2. Change default admin password in dashboard"
echo "    3. Edit ${CONFIG_FILE} for Telegram, TAK, etc."
echo ""
echo -e "  ${BOLD}Commands:${NC}"
echo "    journalctl -u skylens-node -f"
echo "    systemctl restart skylens-node"
echo "    curl http://localhost:${HTTP_PORT}/health"
echo ""
