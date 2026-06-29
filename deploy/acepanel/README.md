# AcePanel Deployment

AcePanel's official repository is [acepanel/panel](https://github.com/acepanel/panel). Its upstream project is a lightweight Go-based server operations panel, and the official installation documentation lists `amd64` and `arm64` as supported architectures.

Traffic Panel can be deployed in AcePanel as a native host process managed by systemd:

1. Install AcePanel and sign in to the panel.
2. Create a MySQL database and user from AcePanel.
3. Upload the correct `trafficpanel-linux-amd64` or `trafficpanel-linux-arm64` binary and rename it to `trafficpanel`.
3. Run `scripts/install-linux.sh`.
4. Configure an AcePanel website reverse proxy to `127.0.0.1:8080`.

Example:

```sh
cp dist/trafficpanel-linux-amd64 ./trafficpanel
MYSQL_HOST=127.0.0.1 \
MYSQL_DATABASE=traffic_panel \
MYSQL_USER=trafficpanel \
MYSQL_PASSWORD='replace-me' \
BASE_URL=https://panel.example.com \
MASTER_SECRET='replace-with-long-random' \
ADMIN_PASSWORD='replace-me' \
sh scripts/install-linux.sh
```

Node hosts can run `scripts/install-node-linux.sh` after the node is created in the admin console.
