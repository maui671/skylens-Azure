#!/bin/bash
set -Eeuo pipefail
trap 'echo "[ERROR] Failed at line $LINENO"; exit 1' ERR

SERVICE_USER="tdcadmin"
OPT_DIR="/opt/skylens"
CONFIG_DIR="$OPT_DIR/configs"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
BINARY="$OPT_DIR/skylens-node"

CERT_DIR="/etc/skylens/certs"
CERT="$CERT_DIR/skylens.crt"
KEY="$CERT_DIR/skylens.key"

echo "===== Skylens Install ====="

generate() {
    head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "$1"
}

DB_PASSWORD=$(generate 32)
JWT_SECRET=$(generate 64)

echo "[INFO] DB PASS: ${DB_PASSWORD:0:4}****"

echo "===== Packages ====="
dnf install -y curl wget git jq openssl redis tar >> /dev/null

echo "===== Go 1.24 ====="
rm -rf /usr/local/go
curl -L https://go.dev/dl/go1.24.0.linux-amd64.tar.gz -o /tmp/go.tgz
tar -C /usr/local -xzf /tmp/go.tgz
export PATH="/usr/local/go/bin:$PATH"
go version

echo "===== PostgreSQL ====="
dnf install -y https://download.postgresql.org/pub/repos/yum/reporpms/EL-9-x86_64/pgdg-redhat-repo-latest.noarch.rpm >> /dev/null || true
dnf -y module disable postgresql >> /dev/null || true
dnf install -y postgresql16-server postgresql16 >> /dev/null

/usr/pgsql-16/bin/postgresql-16-setup initdb || true
systemctl enable --now postgresql-16
sleep 3

su - postgres -c "psql -c \"CREATE ROLE skylens LOGIN PASSWORD '$DB_PASSWORD';\"" || true
su - postgres -c "psql -c \"ALTER ROLE skylens WITH PASSWORD '$DB_PASSWORD';\""
su - postgres -c "psql -c \"CREATE DATABASE skylens OWNER skylens;\"" || true

echo "===== Redis ====="
systemctl enable --now redis

echo "===== NATS (FIX FOR YOUR ISSUE) ====="
curl -L https://github.com/nats-io/nats-server/releases/download/v2.10.11/nats-server-v2.10.11-linux-amd64.tar.gz -o /tmp/nats.tgz
tar -xzf /tmp/nats.tgz -C /tmp
cp /tmp/nats-server-*/nats-server /usr/local/bin/
chmod +x /usr/local/bin/nats-server

cat > /etc/systemd/system/nats.service <<EOF
[Unit]
Description=NATS Server
After=network.target

[Service]
ExecStart=/usr/local/bin/nats-server -p 4222
Restart=always

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now nats

echo "===== Directories ====="
mkdir -p $OPT_DIR $CONFIG_DIR $CERT_DIR
chown -R $SERVICE_USER:$SERVICE_USER /opt/skylens /etc/skylens || true

echo "===== Build ====="
cd /home/$SERVICE_USER/skylens
export PATH="/usr/local/go/bin:$PATH"
go build -o $BINARY ./cmd/skylens-node
chmod +x $BINARY

echo "===== TLS ====="
openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
-keyout $KEY -out $CERT -subj "/CN=$(hostname)"

chmod 640 $KEY
chmod 644 $CERT

echo 
echo
echo "===== Firewall ===="
firewall-cmd --permanent --add-port=443/tcp
firewall-cmd --permanent --add-port=8088/tcp
firewall-cmd --reload
echo  "Firewall complete"
echo 

echo "===== Config ====="
cat > $CONFIG_FILE <<EOF
server:
  https_port: 443
  tls_cert_file: $CERT
  tls_key_file: $KEY

nats:
  url: nats://127.0.0.1:4222

database:
  host: 127.0.0.1
  port: 5432
  name: skylens
  user: skylens
  password: $DB_PASSWORD
  ssl_mode: disable

redis:
  url: redis://127.0.0.1:6379

auth:
  enabled: true
  jwt_secret: $JWT_SECRET
EOF

echo "===== Systemd ====="
cat > /etc/systemd/system/skylens-node.service <<EOF
[Unit]
Description=Skylens Node
After=network.target postgresql-16.service redis.service nats.service

[Service]
User=$SERVICE_USER
WorkingDirectory=$OPT_DIR
ExecStart=$BINARY -config $CONFIG_FILE
Restart=always
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

chown -R tdcadmin:tdcadmin /opt/skylens
chown -R tdcadmin:tdcadmin /etc/skylens

systemctl daemon-reload
systemctl enable --now skylens-node

sleep 5

echo "===== VERIFY ====="

if ! systemctl is-active --quiet skylens-node; then
    echo "FAILED — dumping logs"
    journalctl -u skylens-node -n 50 --no-pager
    exit 1
fi

echo
echo "===== SUCCESS ====="
echo "https://$(hostname -I | awk '{print $1}')"
echo "admin / admin"
