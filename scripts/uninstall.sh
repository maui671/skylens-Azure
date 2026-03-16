#!/usr/bin/env bash
set -euo pipefail

APP_USER="tdcadmin"
APP_GROUP="tdcadmin"
DEPLOY_ROOT="/opt/skylens"
CONFIG_DIR="/etc/skylens"
DATA_DIR="/var/lib/skylens"
LOG_DIR="/var/log/skylens"
PGDATA="/var/lib/pgsql/data"

log()   { echo "[INFO] $*"; }
warn()  { echo "[WARN] $*"; }
run()   { echo "+ $*"; "$@"; }

[[ $EUID -eq 0 ]] || { echo "[ERROR] Run as root"; exit 1; }

log "Stopping services"
systemctl disable --now skylens-node 2>/dev/null || true
systemctl disable --now nginx 2>/dev/null || true
systemctl disable --now redis 2>/dev/null || true
systemctl disable --now nats-server 2>/dev/null || true
systemctl disable --now postgresql-16 2>/dev/null || true

log "Removing service files"
rm -f /etc/systemd/system/nats-server.service
rm -f /etc/systemd/system/postgresql-16.service.d/override.conf
rm -f /etc/systemd/system/redis.service.d/limit.conf
systemctl daemon-reload || true

log "Removing deployed runtime files"
rm -rf "${DEPLOY_ROOT}"
rm -rf "${CONFIG_DIR}"
rm -rf "${DATA_DIR}"
rm -f /usr/local/bin/skylens-node
rm -f /etc/nginx/conf.d/skylens.conf

log "Removing PostgreSQL data"
rm -rf "${PGDATA}"

log "Removing firewall rules"
firewall-cmd --permanent --remove-service=http 2>/dev/null || true
firewall-cmd --permanent --remove-service=https 2>/dev/null || true
firewall-cmd --permanent --remove-port=4222/tcp 2>/dev/null || true
firewall-cmd --reload 2>/dev/null || true

log "Removing packages"
dnf -y remove nginx redis nats-server postgresql16-server postgresql16 postgresql16-libs pgdg-redhat-repo || true

log "Preserving source repo at /home/tdcadmin/skylens"
log "Uninstall complete"
