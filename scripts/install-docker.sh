#!/usr/bin/env bash
set -euo pipefail
umask 077

VERSION="0.1.6"
RELEASE_URL="https://github.com/kexue-aihao/Traffic-Forwarding-Panel/releases/download/v${VERSION}"
install_dir=/opt/traffic-forwarding-panel
port=18080
admin="admin"
bundle=
password_stdin=false
existing=false
download_dir=
backup_dir=
env_temp=
restore_env=false
resume_old=false
new_started=false

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
cleanup() {
    result=$?
    trap - EXIT
    if ((result != 0)); then
        if $new_started; then
            printf '新容器未通过验证，未自动降级数据库。升级前备份：%s/installation.tar\n请检查 docker compose logs panel；回退时需同时恢复旧配置和数据库。\n' "$backup_dir" >&2
        else
            if $restore_env; then
                cp -p -- "$backup_dir/.env" "$install_dir/.env" || true
            fi
            if $resume_old; then
                printf '升级尚未启动新镜像，正在启动原容器。\n' >&2
                (cd "$install_dir" && docker compose up -d --wait --wait-timeout 120 panel) || true
            fi
        fi
    fi
    [[ -z $env_temp ]] || rm -f -- "$env_temp"
    [[ -z $download_dir ]] || rm -rf -- "$download_dir"
    exit "$result"
}
verify_running() {
    local container expected actual
    container=$(docker compose ps -q panel)
    [[ -n $container ]] || die '未找到运行中的面板容器'
    expected=$(docker image inspect "$image" --format '{{.Id}}')
    actual=$(docker inspect "$container" --format '{{.Image}}')
    [[ $actual == "$expected" ]] || die '运行中的容器未使用目标镜像，请检查 Compose 覆盖配置'
    [[ $(docker compose exec -T panel /panel -version) == "$VERSION" ]] || die '运行中的面板版本与安装脚本不匹配'
}
check_version() {
    local current=$1 newest
    [[ $current =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]] || die '无法识别现有镜像版本，请按部署文档手动升级'
    newest=$(printf '%s\n%s\n' "${current%%-*}" "${VERSION%%-*}" | sort -V | tail -n 1)
    [[ $newest == "${VERSION%%-*}" ]] || die "现有版本 $current 高于脚本版本 $VERSION，拒绝自动降级"
    if [[ ${current%%-*} == "${VERSION%%-*}" && $VERSION == *-* && $current != "$VERSION" ]]; then
        die '同版本预发布包切换需手动处理，拒绝自动降级'
    fi
}
usage() {
    cat <<'HELP'
用法：sudo bash install-docker.sh [选项]
  --port 18080                 宿主机回环端口
  --dir /opt/traffic-forwarding-panel
  --admin admin               首次管理员用户名
  --bundle /path/to/downloads  使用已下载镜像包、compose.yaml 和 docker-SHA256SUMS
  --password-stdin             从标准输入读取首次管理员密码（不写入配置）
已有旧版会先校验新镜像、停机备份，再升级容器；同版本仅启动并验证。
保留管理员、数据、端口、域名与 Compose 自定义配置，不自动降级。
默认自动生成管理员密码，安装完成后显示；域名和 HTTPS 在 1Panel 配置。
HELP
}
while (($#)); do
    case "$1" in
        --port|--dir|--admin|--bundle)
            (($# >= 2)) || die "$1 缺少参数"
            case "$1" in
                --port) port=$2;;
                --dir) install_dir=$2;;
                --admin) admin=$2;;
                --bundle) bundle=$2;;
            esac
            shift 2;;
        --password-stdin) password_stdin=true; shift;;
        -h|--help) usage; exit 0;;
        *) die "未知参数：$1";;
    esac
done
[[ $(uname -s) == Linux ]] || die '仅支持 Linux 服务器'
((EUID == 0)) || die '请使用 sudo bash install-docker.sh'
command -v docker >/dev/null || die '请先通过 1Panel 安装 Docker'
docker info >/dev/null 2>&1 || die 'Docker 未运行'
docker compose version >/dev/null 2>&1 || die '需要 Docker Compose v2'
[[ $install_dir =~ ^/([a-zA-Z0-9_.-]+/)*[a-zA-Z0-9_.-]+$ && $install_dir != / ]] || die '安装目录必须是绝对路径且不含空格'
[[ $install_dir != *'/../'* && $install_dir != */.. && $install_dir != *'/./'* && $install_dir != */. ]] || die '安装目录不能包含 . 或 .. 路径段'
if [[ ! $port =~ ^[0-9]{1,5}$ ]] || ((10#$port < 1024 || 10#$port > 65535)); then
    die '端口范围为 1024–65535'
fi
port=$((10#$port))
[[ ! -L $install_dir ]] || die '安装目录不能是符号链接'
command -v flock >/dev/null || die '需要 util-linux 提供的 flock 命令'
lock_id=$(printf '%s' "$install_dir" | sha256sum | cut -d ' ' -f 1)
exec 9>"/run/lock/traffic-forwarding-panel-${lock_id}.lock"
flock -n 9 || die '该目录已有安装或升级任务正在运行'
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

case $(uname -m) in
    x86_64) arch=amd64;;
    aarch64|arm64) arch=arm64;;
    *) die '仅支持 x86_64 和 ARM64';;
esac
image="traffic-forwarding-panel:${VERSION}-${arch}"
archive="traffic-forwarding-panel_${VERSION}_docker_${arch}.tar.gz"
# The managed .env selects the image; a caller's shell must not override it.
unset TFP_IMAGE

if [[ -f $install_dir/.initialized ]]; then
    existing=true
    [[ -f $install_dir/compose.yaml && -f $install_dir/.env ]] || die '现有安装缺少 compose.yaml 或 .env，请先恢复配置'
    [[ ! -L $install_dir/.env && ! -L $install_dir/data ]] || die '自动升级不支持链接到目录外的 .env 或 data，请手动备份和升级'
    [[ $(grep -c '^TFP_IMAGE=' "$install_dir/.env") == 1 ]] || die '.env 必须有且只有一条 TFP_IMAGE 配置'
    [[ -z $bundle ]] || bundle=$(realpath "$bundle")
    cd "$install_dir"
    current_image=$(docker compose config --images panel)
    [[ $current_image =~ ^traffic-forwarding-panel:([0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?)-(amd64|arm64)$ ]] || die '现有 Compose 使用自定义镜像，请按部署文档手动升级'
    current_version=${BASH_REMATCH[1]}
    check_version "$current_version"
    [[ $(TFP_IMAGE="$image" docker compose config --images panel) == "$image" ]] || die 'Compose 镜像未使用 .env 的 TFP_IMAGE，请先检查自定义配置'
    container=$(docker compose ps -a -q panel)
    if [[ -n $container ]]; then
        running_version=$(docker inspect "$container" --format '{{ index .Config.Labels "org.opencontainers.image.version" }}')
        check_version "$running_version"
        if [[ $current_image == "$image" && $(docker inspect "$container" --format '{{.Image}}') == "$(docker image inspect "$image" --format '{{.Id}}' 2>/dev/null)" ]]; then
            docker compose up -d --wait --wait-timeout 120 panel
            verify_running
            printf '当前已是 v%s，容器已启动并验证，账号和配置保持不变。配置目录：%s\n' "$VERSION" "$install_dir"
            exit 0
        fi
        data_mount=$(docker inspect "$container" --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}}:{{.Source}}{{end}}{{end}}')
        [[ $data_mount == "bind:$install_dir/data" ]] || die '自动备份仅支持安装目录下的 data，请对自定义数据挂载手动备份和升级'
        container_env=$(docker inspect "$container" --format '{{range .Config.Env}}{{println .}}{{end}}')
        grep -Fxq 'TFP_DATABASE=sqlite' <<< "$container_env" || die '外部数据库需单独备份，请按部署文档手动升级'
        grep -Fxq 'TFP_DSN=/data/panel.db' <<< "$container_env" || die '自定义数据库路径需单独备份，请按部署文档手动升级'
    fi
    [[ -f data/panel.db ]] || die '未找到安装目录内的数据库，请检查配置后手动升级'
    printf '准备升级：%s -> %s；账号和自定义配置将保留。\n' "$current_image" "$image"
fi
if ! $existing; then
    [[ ! -e $install_dir/.env && ! -e $install_dir/compose.yaml && ! -e $install_dir/data/panel.db ]] || die '发现未完成或已有安装，已保留原文件。请查看部署文档的恢复说明，不会重新初始化。'
    if [[ -d $install_dir && -n $(find "$install_dir" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
        die '安装目录非空，请使用新的空目录或检查现有安装'
    fi
    docker container inspect traffic-forwarding-panel >/dev/null 2>&1 && die '容器名 traffic-forwarding-panel 已被使用，请先检查现有部署'
    [[ $admin =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,63}$ ]] || die '管理员用户名需为 3–64 位字母、数字、下划线、点或短横线'
    if $password_stdin; then
        IFS= read -r password || die '无法从标准输入读取密码'
    else
        password=$(od -An -N24 -tx1 /dev/urandom | tr -d '[:space:]')
        [[ $password =~ ^[a-f0-9]{48}$ ]] || die '无法生成随机管理员密码'
    fi
    password_bytes=$(printf '%s' "$password" | wc -c)
    ((password_bytes >= 12 && password_bytes <= 72)) || die '密码长度需要 12–72 字节'
fi
download_dir=$(mktemp -d)
if [[ -n $bundle ]]; then
    for file in "$archive" compose.yaml docker-SHA256SUMS; do
        [[ -f $bundle/$file ]] || die "离线包缺少 $file"
        cp -- "$bundle/$file" "$download_dir/$file"
    done
else
    command -v curl >/dev/null || die '需要 curl 下载发布包'
    for file in "$archive" compose.yaml docker-SHA256SUMS; do
        curl --fail --show-error --location --retry 3 --connect-timeout 20 \
            "$RELEASE_URL/$file" -o "$download_dir/$file"
    done
fi
# Check each required file by its exact manifest entry; missing entries fail.
for file in "$archive" compose.yaml; do
    checksum=$(awk -v file="$file" '$2 == file {print $1}' "$download_dir/docker-SHA256SUMS")
    [[ $checksum =~ ^[a-fA-F0-9]{64}$ ]] || die "校验清单缺少或重复记录：$file"
    (cd "$download_dir"; printf '%s  %s\n' "$checksum" "$file" | sha256sum -c -)
done
docker load -i "$download_dir/$archive"
[[ $(docker image inspect "$image" --format '{{ index .Config.Labels "org.opencontainers.image.version" }}') == "$VERSION" ]] || die '镜像版本与安装脚本不匹配'

if $existing; then
    backup_dir=$(mktemp -d "${install_dir}.backup-$(date +%Y%m%d-%H%M%S)-XXXXXX")
    cp -p -- .env "$backup_dir/.env"
    printf '镜像校验通过，正在停机备份到 %s/installation.tar\n' "$backup_dir"
    if [[ -n $container && $(docker inspect "$container" --format '{{.State.Running}}') == true ]]; then
        resume_old=true
    fi
    docker compose stop panel
    tar -cpf "$backup_dir/installation.tar" -C "$install_dir" .
    env_temp=$(mktemp "$install_dir/.env.upgrade-XXXXXX")
    awk -v image="$image" '/^TFP_IMAGE=/ { print "TFP_IMAGE=" image; next } { print }' .env > "$env_temp"
    restore_env=true
    mv -- "$env_temp" .env
    env_temp=
    new_started=true
    resume_old=false
    docker compose up -d --wait --wait-timeout 120 panel
    verify_running
    printf '\n升级完成，当前运行版本：v%s\n账号、数据和自定义配置已保留。升级前备份：%s/installation.tar\n入口/出口 Agent 需另外升级。\n' "$VERSION" "$backup_dir"
    exit 0
fi

install -d -m 700 "$install_dir"
install -d -m 700 -o 65532 -g 65532 "$install_dir/data" "$install_dir/config"
install -m 600 "$download_dir/compose.yaml" "$install_dir/compose.yaml"
printf 'TFP_IMAGE=%s\nTFP_ORIGIN=\nTFP_PORT=%s\nTFP_PAYMENTS_FILE=\n' \
    "$image" "$port" > "$install_dir/.env"
cd "$install_dir"
printf '%s\n' "$password" | docker compose run --rm -T --no-deps panel -init-admin "$admin"
touch .initialized
docker compose up -d --wait --wait-timeout 120
verify_running
printf '\n部署完成。\n管理员账号：%s\n管理员密码：%s\n请保存密码，登录后可修改。\n1Panel 反向代理地址：http://127.0.0.1:%s\n域名和 HTTPS 在 1Panel 配置，保留 Host 并设置 X-Forwarded-Proto。\n管理员入口：你的站点地址/admin\n配置与数据：%s\n' \
    "$admin" "$password" "$port" "$install_dir"
unset password
