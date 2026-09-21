#!/usr/bin/env bash
# Run on a disposable Linux Docker host; uses real containers and HTTPS.
set -euo pipefail
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
bundle=$(realpath "${1:?Pass the directory containing the Docker release bundle}")
work=$(mktemp -d)
install_dir=$work/install
cleanup() {
    result=$?
    if ((result != 0)); then
        docker logs traffic-forwarding-panel 2>&1 || true
        docker logs tfp-test-proxy 2>&1 || true
    fi
    docker rm -f tfp-test-proxy >/dev/null 2>&1 || true
    if [[ -f $install_dir/compose.yaml ]]; then
        sudo docker compose --project-directory "$install_dir" down >/dev/null 2>&1 || true
    fi
    sudo rm -rf -- "$work"
}
trap cleanup EXIT
password=$(openssl rand -hex 18)
printf '%s\n' "$password" | sudo bash "$repo_dir/scripts/install-docker.sh" \
    --domain panel.test:18443 --port 18080 --dir "$install_dir" --bundle "$bundle" --password-stdin
[[ $(docker inspect --format '{{.Config.User}}' traffic-forwarding-panel) == 65532:65532 ]]
[[ $(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' traffic-forwarding-panel) == true ]]
[[ $(docker inspect --format '{{(index (index .HostConfig.PortBindings "8080/tcp") 0).HostIp}}' traffic-forwarding-panel) == 127.0.0.1 ]]
[[ $(docker exec traffic-forwarding-panel /panel -version) == "$(cat "$repo_dir/VERSION")" ]]
docker exec traffic-forwarding-panel /panel -healthcheck
[[ $(sudo stat -c '%u:%g' "$install_dir/data/panel.db") == 65532:65532 ]]

mkdir "$work/proxy"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=panel.test \
    -addext subjectAltName=DNS:panel.test -keyout "$work/proxy/key.pem" -out "$work/proxy/cert.pem" 2>/dev/null
cat > "$work/proxy/nginx.conf" <<'NGINX'
events {}
http {
    access_log off;
    server {
        listen 18443 ssl;
        ssl_certificate /etc/nginx/cert.pem;
        ssl_certificate_key /etc/nginx/key.pem;
        location / {
            proxy_pass http://127.0.0.1:18080;
            proxy_set_header Host $http_host;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "upgrade";
            proxy_buffering off;
            proxy_read_timeout 3600s;
        }
    }
}
NGINX
docker run -d --name tfp-test-proxy --network host \
    -v "$work/proxy:/etc/nginx:ro" nginx:stable-alpine >/dev/null
curl_options=(--silent --show-error --fail --noproxy '*' --cacert "$work/proxy/cert.pem" \
    --resolve panel.test:18443:127.0.0.1)
origin=https://panel.test:18443
for attempt in $(seq 1 30); do
    if curl "${curl_options[@]}" "$origin/api/v1/health" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl "${curl_options[@]}" "$origin/admin" > "$work/admin.html"
grep -q '<div id="app">' "$work/admin.html"
jq -cn --arg password "$password" '{username:"admin",password:$password}' > "$work/login.json"
curl "${curl_options[@]}" -D "$work/headers" -c "$work/cookies" \
    -H "Origin: $origin" -H 'X-Requested-With: fetch' -H 'Content-Type: application/json' \
    --data-binary @"$work/login.json" "$origin/api/v1/auth/login" | jq -e '.user.role == "admin"' >/dev/null
grep -qi 'set-cookie:.*Secure' "$work/headers"
curl "${curl_options[@]}" -b "$work/cookies" "$origin/api/v1/auth/session" | jq -e '.user.username == "admin"' >/dev/null
status=$(curl --silent --noproxy '*' --cacert "$work/proxy/cert.pem" --resolve panel.test:18443:127.0.0.1 \
    -o /dev/null -w '%{http_code}' -H 'Origin: https://other.test' -H 'X-Requested-With: fetch' \
    -H 'Content-Type: application/json' --data-binary @"$work/login.json" "$origin/api/v1/auth/login")
[[ $status == 403 ]]
set +e
curl "${curl_options[@]}" -b "$work/cookies" -N --max-time 7 \
    -D "$work/sse-headers" "$origin/api/v1/probes/events" > "$work/events"
sse_status=$?
set -e
[[ $sse_status == 28 ]]
grep -qi 'content-type: text/event-stream' "$work/sse-headers"
grep -q 'event: probes' "$work/events"

sudo docker compose --project-directory "$install_dir" restart panel
sudo docker compose --project-directory "$install_dir" up -d --wait --wait-timeout 120
curl "${curl_options[@]}" -b "$work/cookies" "$origin/api/v1/auth/session" | jq -e '.user.username == "admin"' >/dev/null
sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" </dev/null
curl "${curl_options[@]}" -b "$work/cookies" "$origin/api/v1/auth/session" | jq -e '.user.username == "admin"' >/dev/null
printf 'Docker install, HTTPS login, CSRF, SSE, restart persistence and repeat install passed on %s\n' "$(uname -m)"
