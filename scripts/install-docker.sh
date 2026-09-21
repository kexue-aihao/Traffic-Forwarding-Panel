#!/usr/bin/env bash
set -euo pipefail
umask 077

VERSION="0.1.1"
RELEASE_URL="https://github.com/kexue-aihao/Traffic-Forwarding-Panel/releases/download/v${VERSION}"
install_dir=/opt/traffic-forwarding-panel
port=18080
admin="admin"
bundle=
password_stdin=false

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
用法：sudo bash install-docker.sh [选项]
  --port 18080                 宿主机回环端口
  --dir /opt/traffic-forwarding-panel
  --admin admin               首次管理员用户名
  --bundle /path/to/downloads  使用已下载镜像包、compose.yaml 和 docker-SHA256SUMS
  --password-stdin             从标准输入读取首次管理员密码（不写入配置）
重复运行仅启动现有安装，不重设密码、不覆盖数据库或配置。
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

if [[ -f $install_dir/.initialized ]]; then
    [[ -f $install_dir/compose.yaml && -f $install_dir/.env ]] || die '现有安装缺少 compose.yaml 或 .env，请先恢复配置'
    cd "$install_dir"
    docker compose up -d --wait --wait-timeout 120
    printf '现有安装已启动，数据、账号和配置保持不变。配置目录：%s\n' "$install_dir"
    exit 0
fi
[[ ! -e $install_dir/.env && ! -e $install_dir/compose.yaml && ! -e $install_dir/data/panel.db ]] || die '发现未完成或已有安装，已保留原文件。请查看部署文档的恢复说明，不会重新初始化。'
[[ ! -L $install_dir ]] || die '安装目录不能是符号链接'
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

case $(uname -m) in
    x86_64) arch=amd64;;
    aarch64|arm64) arch=arm64;;
    *) die '仅支持 x86_64 和 ARM64';;
esac
image="traffic-forwarding-panel:${VERSION}-${arch}"
archive="traffic-forwarding-panel_${VERSION}_docker_${arch}.tar.gz"
download_dir=$(mktemp -d)
trap 'rm -rf -- "$download_dir"' EXIT
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

install -d -m 700 "$install_dir"
install -d -m 700 -o 65532 -g 65532 "$install_dir/data" "$install_dir/config"
install -m 600 "$download_dir/compose.yaml" "$install_dir/compose.yaml"
printf 'TFP_IMAGE=%s\nTFP_ORIGIN=\nTFP_PORT=%s\nTFP_PAYMENTS_FILE=\n' \
    "$image" "$port" > "$install_dir/.env"
cd "$install_dir"
printf '%s\n' "$password" | docker compose run --rm -T --no-deps panel -init-admin "$admin"
touch .initialized
docker compose up -d --wait --wait-timeout 120
printf '\n部署完成。\n管理员账号：%s\n管理员密码：%s\n请保存密码，登录后可修改。\n1Panel 反向代理地址：http://127.0.0.1:%s\n域名和 HTTPS 在 1Panel 配置，保留 Host 并设置 X-Forwarded-Proto。\n管理员入口：你的站点地址/admin\n配置与数据：%s\n' \
    "$admin" "$password" "$port" "$install_dir"
unset password
