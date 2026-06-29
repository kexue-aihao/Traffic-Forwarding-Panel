#!/usr/bin/env sh
set -eu

APP_DIR="${APP_DIR:-/opt/trafficpanel}"
BIN="${BIN:-/usr/local/bin/trafficpanel}"
SERVICE="${SERVICE:-trafficpanel}"
MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_DATABASE="${MYSQL_DATABASE:-traffic_panel}"
MYSQL_USER="${MYSQL_USER:-trafficpanel}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-trafficpanel_pass}"
HTTP_ADDR="${HTTP_ADDR:-:8080}"
BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
MASTER_SECRET="${MASTER_SECRET:-change-me}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin123456}"

mkdir -p "$APP_DIR"
install -m 0755 ./trafficpanel "$BIN"

cat > "$APP_DIR/env" <<EOF
TP_MODE=server
TP_HTTP_ADDR=$HTTP_ADDR
TP_BASE_URL=$BASE_URL
TP_DATABASE_DSN=$MYSQL_USER:$MYSQL_PASSWORD@tcp($MYSQL_HOST:$MYSQL_PORT)/$MYSQL_DATABASE?parseTime=true&charset=utf8mb4&loc=Local
TP_MASTER_SECRET=$MASTER_SECRET
TP_BOOTSTRAP_ADMIN_USER=$ADMIN_USER
TP_BOOTSTRAP_ADMIN_PASS=$ADMIN_PASSWORD
EOF

cat > "/etc/systemd/system/$SERVICE.service" <<EOF
[Unit]
Description=Traffic Panel
After=network.target mysql.service mariadb.service

[Service]
EnvironmentFile=$APP_DIR/env
ExecStart=$BIN
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now "$SERVICE"
systemctl status "$SERVICE" --no-pager

