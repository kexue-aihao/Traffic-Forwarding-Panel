# aaPanel Deployment

aaPanel uses a conventional Linux host flow: install MySQL from the panel, create the database/user, upload the matching `trafficpanel-linux-amd64` or `trafficpanel-linux-arm64` binary, then run `scripts/install-linux.sh`.

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

Use aaPanel website reverse proxy to forward HTTPS traffic to `127.0.0.1:8080`.

