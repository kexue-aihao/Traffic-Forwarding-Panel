#!/usr/bin/env sh
set -eu

APP_DIR="${APP_DIR:-/opt/trafficpanel-node}"
BIN="${BIN:-/usr/local/bin/trafficpanel}"
SERVICE="${SERVICE:-trafficpanel-node}"
AGENT_SERVER_URL="${AGENT_SERVER_URL:-http://127.0.0.1:8080}"
AGENT_NODE_ID="${AGENT_NODE_ID:?AGENT_NODE_ID is required}"
AGENT_NODE_SECRET="${AGENT_NODE_SECRET:?AGENT_NODE_SECRET is required}"
AGENT_NODE_NAME="${AGENT_NODE_NAME:-node-$AGENT_NODE_ID}"
AGENT_NODE_HOST="${AGENT_NODE_HOST:-127.0.0.1}"
AGENT_NODE_PORT="${AGENT_NODE_PORT:-0}"
AGENT_UDP_IDLE_TIMEOUT="${AGENT_UDP_IDLE_TIMEOUT:-2m}"

mkdir -p "$APP_DIR"
install -m 0755 ./trafficpanel "$BIN"

cat > "$APP_DIR/env" <<EOF
TP_MODE=node
TP_AGENT_SERVER_URL=$AGENT_SERVER_URL
TP_AGENT_NODE_ID=$AGENT_NODE_ID
TP_AGENT_NODE_SECRET=$AGENT_NODE_SECRET
TP_AGENT_NODE_NAME=$AGENT_NODE_NAME
TP_AGENT_NODE_HOST=$AGENT_NODE_HOST
TP_AGENT_NODE_PORT=$AGENT_NODE_PORT
TP_AGENT_UDP_IDLE_TIMEOUT=$AGENT_UDP_IDLE_TIMEOUT
EOF

cat > "/etc/systemd/system/$SERVICE.service" <<EOF
[Unit]
Description=Traffic Panel Node
After=network.target

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
