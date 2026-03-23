#!/bin/bash
set -e

echo "===== FULL SKYLENS UNINSTALL ====="

systemctl stop skylens-node 2>/dev/null || true
systemctl disable skylens-node 2>/dev/null || true

systemctl stop postgresql-16 2>/dev/null || true
systemctl disable postgresql-16 2>/dev/null || true

systemctl stop redis 2>/dev/null || true
systemctl disable redis 2>/dev/null || true

systemctl stop nats 2>/dev/null || true
systemctl disable nats 2>/dev/null || true

rm -f /etc/systemd/system/skylens-node.service
rm -f /etc/systemd/system/nats.service

systemctl daemon-reexec
systemctl daemon-reload

rm -rf /opt/skylens
rm -rf /etc/skylens

dnf remove -y postgresql16 postgresql16-server postgresql16-libs redis || true

rm -rf /var/lib/pgsql
rm -rf /usr/local/go
rm -f /usr/local/bin/nats-server

echo
echo "DONE — repo preserved at /home/tdcadmin/skylens"
