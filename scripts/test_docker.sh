#!/usr/bin/env bash
# Run on a disposable Linux Docker host; uses real containers and HTTPS.
# Redirected logs belong to the test user, while sudo only runs Docker operations.
# shellcheck disable=SC2024
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
# A fresh install must complete without a domain, terminal, or stdin prompts.
# The invoking test user owns this private output directory; only Docker setup needs root.
# shellcheck disable=SC2024
sudo bash "$repo_dir/scripts/install-docker.sh" \
    --port 18080 --dir "$install_dir" --bundle "$bundle" </dev/null > "$work/install-output"
password=$(sed -n 's/^管理员密码：//p' "$work/install-output")
[[ $password =~ ^[a-f0-9]{48}$ ]]
sudo grep -Fx 'TFP_ORIGIN=' "$install_dir/.env"
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
for ((attempt = 0; attempt < 30; attempt++)); do
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
curl "${curl_options[@]}" -b "$work/cookies" \
    -H "Origin: $origin" -H 'X-Requested-With: fetch' -H 'Content-Type: application/json' \
    --data '{"enabled":false,"plan_id":""}' "$origin/api/v1/auto-renew" | jq -e '.enabled == false' >/dev/null
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

# A different image ID exercises replacement without introducing a database
# migration into these installer tests. Application migrations have their own tests.
case $(uname -m) in
    x86_64) arch=amd64;;
    aarch64|arm64) arch=arm64;;
esac
version=$(cat "$repo_dir/VERSION")
image="traffic-forwarding-panel:${version}-${arch}"
old_image="traffic-forwarding-panel:0.0.0-${arch}"
archive="traffic-forwarding-panel_${version}_docker_${arch}.tar.gz"
docker build --build-arg "BASE_IMAGE=$image" -t "$old_image" - <<'DOCKER'
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
LABEL org.opencontainers.image.version="0.0.0"
DOCKER
sudo sed -i "s|^TFP_IMAGE=.*|TFP_IMAGE=$old_image|; s|^TFP_ORIGIN=.*|TFP_ORIGIN=$origin|" "$install_dir/.env"
printf 'CUSTOM_OPTION="spaces and # literal"\n' | sudo tee -a "$install_dir/.env" >/dev/null
printf 'preserve this configuration\n' | sudo tee "$install_dir/config/upgrade-marker" >/dev/null
sudo tee "$install_dir/compose.override.yaml" >/dev/null <<'YAML'
services:
  panel:
    labels:
      tfp.upgrade-preserved: "yes"
YAML
sudo docker compose --project-directory "$install_dir" up -d --wait --wait-timeout 120
old_container=$(docker inspect --format '{{.Id}}' traffic-forwarding-panel)
sudo cp "$install_dir/.env" "$work/old.env"
sudo cp "$install_dir/compose.override.yaml" "$work/override.yaml"
sudo sha256sum "$install_dir/compose.yaml" "$install_dir/compose.override.yaml" "$install_dir/config/upgrade-marker" > "$work/preserved-files"
sudo sed '/^TFP_IMAGE=/d' "$install_dir/.env" > "$work/preserved-env"

# Reject a corrupt bundle before stopping the old container.
mkdir "$work/corrupt"
cp "$bundle/compose.yaml" "$bundle/docker-SHA256SUMS" "$work/corrupt/"
printf corrupt > "$work/corrupt/$archive"
if sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" --bundle "$work/corrupt" </dev/null > "$work/corrupt-output" 2>&1; then
    echo 'Corrupt upgrade bundle was accepted' >&2; exit 1
fi
[[ $(docker inspect --format '{{.Id}}' traffic-forwarding-panel) == "$old_container" ]]
docker exec traffic-forwarding-panel /panel -healthcheck
sudo cmp "$install_dir/.env" "$work/old.env"

# An older installer must not downgrade a newer configured installation.
sudo sed -i "s|^TFP_IMAGE=.*|TFP_IMAGE=traffic-forwarding-panel:999.0.0-$arch|" "$install_dir/.env"
if sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" </dev/null > "$work/downgrade-output" 2>&1; then
    echo 'Automatic downgrade was accepted' >&2; exit 1
fi
[[ $(docker inspect --format '{{.Id}}' traffic-forwarding-panel) == "$old_container" ]]
sudo cp "$work/old.env" "$install_dir/.env"

# A failed backup must resume the old service and leave its image selection intact.
mkdir "$work/fail-bin"
printf '#!/usr/bin/env bash\nexit 9\n' > "$work/fail-bin/tar"
chmod +x "$work/fail-bin/tar"
if sudo env "PATH=$work/fail-bin:$PATH" bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" --bundle "$bundle" </dev/null > "$work/backup-failure-output" 2>&1; then
    echo 'Upgrade ignored a failed backup' >&2; exit 1
fi
[[ $(docker inspect --format '{{.Id}}' traffic-forwarding-panel) == "$old_container" ]]
docker exec traffic-forwarding-panel /panel -healthcheck
sudo cmp "$install_dir/.env" "$work/old.env"

# Successful upgrade keeps credentials, sessions, configuration and a cold backup.
sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" --bundle "$bundle" </dev/null > "$work/upgrade-output"
grep -q '升级完成' "$work/upgrade-output"
backup=$(sed -n 's/.*升级前备份：//p' "$work/upgrade-output")
sudo tar -xOf "$backup" ./.env | grep -Fx "TFP_IMAGE=$old_image"
sudo tar -tf "$backup" | grep -Fx './data/panel.db'
[[ $(sudo stat -c '%a' "$(dirname "$backup")") == 700 ]]
[[ $(docker inspect --format '{{.Id}}' traffic-forwarding-panel) != "$old_container" ]]
[[ $(docker inspect --format '{{.Image}}' traffic-forwarding-panel) == "$(docker image inspect --format '{{.Id}}' "$image")" ]]
[[ $(docker exec traffic-forwarding-panel /panel -version) == "$version" ]]
[[ $(docker inspect --format '{{index .Config.Labels "tfp.upgrade-preserved"}}' traffic-forwarding-panel) == yes ]]
sudo sha256sum -c "$work/preserved-files"
sudo sed '/^TFP_IMAGE=/d' "$install_dir/.env" | cmp - "$work/preserved-env"
curl "${curl_options[@]}" -b "$work/cookies" "$origin/api/v1/auth/session" | jq -e '.user.username == "admin"' >/dev/null
curl "${curl_options[@]}" -H "Origin: $origin" -H 'X-Requested-With: fetch' -H 'Content-Type: application/json' \
    --data-binary @"$work/login.json" "$origin/api/v1/auth/login" | jq -e '.user.role == "admin"' >/dev/null

current_container=$(docker inspect --format '{{.Id}}' traffic-forwarding-panel)
backup_count=$(sudo find "$work" -name installation.tar | wc -l)
sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" </dev/null
[[ $(docker inspect --format '{{.Id}}' traffic-forwarding-panel) == "$current_container" ]]
[[ $(sudo find "$work" -name installation.tar | wc -l) == "$backup_count" ]]

# After a new process starts, a failed health check must not silently put an old
# binary against a potentially migrated database. Keep the backup for recovery.
sudo cp "$work/old.env" "$install_dir/.env"
sudo docker compose --project-directory "$install_dir" up -d --wait --wait-timeout 120 panel
sudo tee -a "$install_dir/compose.override.yaml" >/dev/null <<'YAML'
    healthcheck:
      test: ["CMD", "/panel", "-invalid-upgrade-healthcheck"]
      interval: 1s
      timeout: 1s
      retries: 1
      start_period: 0s
YAML
if sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" --bundle "$bundle" </dev/null > "$work/health-failure-output" 2>&1; then
    echo 'Upgrade reported success despite a failed health check' >&2; exit 1
fi
grep -q '新容器未通过验证' "$work/health-failure-output"
sudo grep -Fx "TFP_IMAGE=$image" "$install_dir/.env"
[[ $(docker inspect --format '{{.Image}}' traffic-forwarding-panel) == "$(docker image inspect --format '{{.Id}}' "$image")" ]]
sudo cp "$work/override.yaml" "$install_dir/compose.override.yaml"
sudo bash "$repo_dir/scripts/install-docker.sh" --dir "$install_dir" </dev/null
curl "${curl_options[@]}" -b "$work/cookies" "$origin/api/v1/auth/session" | jq -e '.user.username == "admin"' >/dev/null
printf 'Install, HTTPS, upgrade, backup/health failures, checksum rejection, downgrade rejection and idempotent repeat passed on %s\n' "$(uname -m)"
