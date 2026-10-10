#!/usr/bin/env bash
#
# 设备接入脚本 —— 由流量转发控制台托管，在**目标设备**上执行。
#
# 入口（默认）：
#   bash <(curl -fLsS https://panel.example.com/download/agent-install.sh) \
#        -t '<设备组接入密钥>' -u 'https://panel.example.com'
#
# 出口（隧道端点）：
#   bash <(curl -fLsS https://panel.example.com/download/agent-install.sh) \
#        -t '<设备组接入密钥>' -u 'https://panel.example.com' \
#        -m exit -S 'exit.example.com' -e '<出口令牌>' \
#        -C /etc/ssl/exit.crt -K /etc/ssl/exit.key
#
# 两种模式都装成 systemd 服务（开机自启、崩溃重拉），并在启动后确认结果。
# 出口模式会**同时**装注册用的 agent：出口服务本身不跟面板通信，没有那个
# agent，设备不会出现在控制台里，出口列表也就永远显示离线。
#
# 凭据是一次性的还是长期的要分清：设备组的接入密钥长期不变，命令可以一直留着；
# 而「服务器 → 生成接入凭据」给出的一次性令牌 15 分钟过期、只能用一次。
#
# 重新执行本脚本 = 重装/换令牌；`-x` 卸载。

set -euo pipefail

TOKEN=""
PANEL_URL=""
NODE_NAME=""
ARCH_OVERRIDE=""
CA_URL=""
STATE_DIR="/var/lib/tfp-agent"
BIN_PATH="/usr/local/bin/tfp-agent"
ENV_DIR="/etc/tfp-agent"
UNIT_PATH="/etc/systemd/system/tfp-agent.service"
MODE="agent"
UNINSTALL="no"
WAIT_SECONDS=30
SERVICE_PROFILES=""
INSPECTION_PROFILES=""
BUSINESS_PROFILES=""
MIGRATE_SERVICES="no"

# 出口模式
EXIT_SERVER_NAME=""
EXIT_LISTEN="0.0.0.0:9443"
EXIT_UDP_LISTEN=""
EXIT_TOKEN=""
EXIT_CERT=""
EXIT_KEY=""
EXIT_CERT_MODE="provided"
EXIT_TRANSPORT="tls"
EXIT_OBFUSCATION=""
EXIT_OBFUSCATION_PARAMS=""
EXIT_UNIT="/etc/systemd/system/tfp-exit.service"
RENEW_UNIT="/etc/systemd/system/tfp-cert-renew.service"
RENEW_TIMER="/etc/systemd/system/tfp-cert-renew.timer"
IP_CERT_NAME="tfp-exit-public-ip"
CERTBOT_BIN=""
PYTHON_BIN=""
PACKAGE_MANAGER=""
SYSTEM_NAME="Linux"
ACME_CONFIG="$ENV_DIR/acme"
ACME_WORK="$STATE_DIR/acme"
ACME_LOG="/var/log/tfp-agent-acme"
UNINSTALL_SCRIPT=""

die() {
  printf '错误：%s\n' "$1" >&2
  exit 1
}
note() { printf '  %s\n' "$1"; }

detect_system() {
  local ID="" ID_LIKE="" PRETTY_NAME=""
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
  elif [ -r /usr/lib/os-release ]; then
    # shellcheck disable=SC1091
    . /usr/lib/os-release
  fi
  SYSTEM_NAME="${PRETTY_NAME:-${ID:-Linux}}"
  case " $ID $ID_LIKE " in
    *' debian '*|*' ubuntu '*) PACKAGE_MANAGER="apt-get" ;;
    *' rhel '*|*' fedora '*|*' centos '*)
      if command -v dnf >/dev/null 2>&1; then PACKAGE_MANAGER="dnf"; else PACKAGE_MANAGER="yum"; fi ;;
    *' arch '*) PACKAGE_MANAGER="pacman" ;;
    *' suse '*|*' opensuse '*) PACKAGE_MANAGER="zypper" ;;
  esac
  if [ -z "$PACKAGE_MANAGER" ]; then
    local candidate
    for candidate in apt-get dnf yum pacman zypper; do
      if command -v "$candidate" >/dev/null 2>&1; then PACKAGE_MANAGER="$candidate"; break; fi
    done
  fi
}

install_packages() {
  command -v "$PACKAGE_MANAGER" >/dev/null 2>&1 \
    || die "$SYSTEM_NAME 缺少依赖（$*），请先通过系统包管理器安装"
  note "正在通过 $PACKAGE_MANAGER 安装依赖：$*"
  case "$PACKAGE_MANAGER" in
    apt-get) DEBIAN_FRONTEND=noninteractive apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
    dnf|yum) "$PACKAGE_MANAGER" install -y "$@" ;;
    # 不单独刷新 Arch 索引，避免制造部分升级；过期镜像/索引由用户先完整升级。
    pacman) pacman -S --needed --noconfirm "$@" ;;
    zypper) zypper --non-interactive install "$@" ;;
    *) die "无法识别系统包管理器，请先安装：$*" ;;
  esac || die "依赖安装失败；请检查软件仓库与网络（Arch 请先执行 pacman -Syu），然后重试"
}

ensure_base_tools() {
  local tool bundle have_ca="no" packages=()
  for bundle in "${SSL_CERT_FILE:-}" "${CURL_CA_BUNDLE:-}" /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/ca-bundle.pem; do
    if [ -n "$bundle" ] && [ -s "$bundle" ]; then have_ca="yes"; break; fi
  done
  if ! command -v curl >/dev/null 2>&1; then packages+=(curl); fi
  if [ "$have_ca" = "no" ]; then packages+=(ca-certificates); fi
  for tool in install mktemp head od tr chmod mkdir rm mv touch uname; do
    if ! command -v "$tool" >/dev/null 2>&1; then packages+=(coreutils); break; fi
  done
  if ! command -v grep >/dev/null 2>&1; then packages+=(grep); fi
  if [ "$MODE" != "agent" ] && ! command -v openssl >/dev/null 2>&1; then packages+=(openssl); fi
  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" != "Disabled" ] && ! command -v restorecon >/dev/null 2>&1; then
    packages+=(policycoreutils)
  fi
  if [ "${#packages[@]}" -gt 0 ]; then install_packages "${packages[@]}"; fi
  for tool in curl install mktemp head od tr chmod mkdir rm mv touch uname grep; do
    command -v "$tool" >/dev/null 2>&1 || die "依赖安装后仍缺少 $tool"
  done
  if [ "$MODE" != "agent" ]; then command -v openssl >/dev/null 2>&1 || die "依赖安装后仍缺少 openssl"; fi
  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" != "Disabled" ]; then
    command -v restorecon >/dev/null 2>&1 || die "SELinux 已启用，但依赖安装后仍缺少 restorecon"
  fi
}

restore_contexts() {
  if command -v restorecon >/dev/null 2>&1; then
    restorecon -F "$@" || die "无法恢复 SELinux 文件标签，请检查目标路径策略"
  fi
}

select_python() {
  local candidate
  for candidate in python3 python3.14 python3.13 python3.12 python3.11 python3.10; do
    command -v "$candidate" >/dev/null 2>&1 || continue
    if "$candidate" -c 'import ssl, sys, venv; sys.exit(sys.version_info < (3, 10))' >/dev/null 2>&1; then
      PYTHON_BIN="$(command -v "$candidate")"
      return 0
    fi
  done
  return 1
}

install_python() {
  local package="${PYTHON_BIN##*/}" candidate
  case "$PACKAGE_MANAGER" in
    apt-get)
      package="${package:-python3}"
      install_packages "$package" "$package-venv" openssl ;;
    dnf|yum)
      if [ -z "$package" ]; then
        for candidate in python3.12 python3.11 python3.10; do
          if "$PACKAGE_MANAGER" -q list "$candidate" >/dev/null 2>&1; then package="$candidate"; break; fi
        done
      fi
      package="${package:-python3}"
      install_packages "$package" "$package-pip" openssl ;;
    pacman) install_packages python python-pip openssl ;;
    zypper) install_packages python3 python3-pip openssl ;;
    *) die "请先安装 Python 3.10 或更高版本（含 venv、pip）和 openssl，再运行公网 IP 证书安装" ;;
  esac
}

ensure_ip_certbot() {
  if ! select_python; then
    install_python
    select_python || die "$SYSTEM_NAME 的 Python 不满足 IP 证书要求；请安装 Python 3.10 或更高版本（含 venv、pip）。普通 Agent 和自备证书出口不需要 Python"
  fi
  command -v openssl >/dev/null 2>&1 || install_packages openssl
  local candidate help
  for candidate in "$(command -v certbot || true)" "$ENV_DIR/certbot/bin/certbot"; do
    [ -x "$candidate" ] || continue
    help="$("$candidate" --help all 2>/dev/null)" || continue
    if [[ "$help" == *--ip-address* && "$help" == *--required-profile* ]]; then
      CERTBOT_BIN="$candidate"
      return
    fi
  done
  note "正在安装支持 IP 证书的 Certbot（独立 Python 环境）"
  if ! "$PYTHON_BIN" -m venv "$ENV_DIR/certbot"; then
    install_python
    "$PYTHON_BIN" -m venv "$ENV_DIR/certbot" || die "无法创建 Certbot Python 环境"
  fi
  "$ENV_DIR/certbot/bin/python" -m pip install --upgrade 'certbot>=5.4,<6' \
    || die "安装 Certbot 失败，请确保 Python 3.10 或更高版本，并检查 PyPI 网络连接"
  CERTBOT_BIN="$ENV_DIR/certbot/bin/certbot"
}

detect_public_ip() {
  local family url raw address
  for family in 4 6; do
    if [ "$family" = 4 ]; then url="https://api.ipify.org"; else url="https://api6.ipify.org"; fi
    raw="$(curl -"$family" -fsS --noproxy '*' --connect-timeout 5 --max-time 15 "$url" 2>/dev/null)" || continue
    address="$("$PYTHON_BIN" -c 'import ipaddress, sys; ip = ipaddress.ip_address(sys.argv[1].strip()); sys.exit(1) if not ip.is_global or ip.is_multicast or ip.version != int(sys.argv[2]) else print(ip)' "$raw" "$family" 2>/dev/null)" || continue
    EXIT_SERVER_NAME="$address"
    if [ "$family" = 6 ] && [[ "$EXIT_LISTEN" == 0.0.0.0:* ]]; then
      EXIT_LISTEN="[::]:${EXIT_LISTEN##*:}"
    fi
    return
  done
  die "无法探测本机公网 IPv4/IPv6；请检查外网连接。公网 IP 证书不支持私网地址"
}

configure_ip_renewal() {
  cat > "$RENEW_UNIT" <<UNIT
[Unit]
Description=Traffic Forwarding Panel public IP certificate renewal
After=network-online.target
Wants=network-online.target
ConditionPathExists=$EXIT_CERT

[Service]
Type=oneshot
ExecStart=$CERTBOT_BIN renew --non-interactive --cert-name $IP_CERT_NAME --config-dir $ACME_CONFIG --work-dir $ACME_WORK --logs-dir $ACME_LOG --no-random-sleep-on-renew
TimeoutStartSec=15min
UNIT
  cat > "$RENEW_TIMER" <<UNIT
[Unit]
Description=Traffic Forwarding Panel public IP certificate renewal schedule

[Timer]
OnCalendar=*-*-* 00,06,12,18:00:00
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  chmod 0644 "$RENEW_UNIT" "$RENEW_TIMER"
  restore_contexts "$RENEW_UNIT" "$RENEW_TIMER"
  systemctl daemon-reload
  systemctl enable --now tfp-cert-renew.timer >/dev/null 2>&1
  systemctl is-active --quiet tfp-cert-renew.timer || die "证书自动续签定时器启动失败"
}

# 装完把文件落在哪写清楚：机器出问题时运营方要能直接找到它们，而不是回头翻
# 安装脚本。
print_paths() {
  echo
  echo "本机文件位置："
  note "Agent 程序    $BIN_PATH"
  note "节点身份      $STATE_DIR/agent-state.json"
  note "环境文件      $ENV_DIR/agent.env"
  note "服务单元      $UNIT_PATH"
  if [ "$MODE" = "exit" ] || [ "$MODE" = "secure-direct" ]; then
    note "出口单元      $EXIT_UNIT"
    if [ "$EXIT_CERT_MODE" = "public-ip" ]; then
      note "IP 证书       $EXIT_CERT"
      note "自动续签      $RENEW_TIMER"
      note "续签日志      journalctl -u tfp-cert-renew.service"
    fi
  fi
  if [ -n "$CA_ARG" ]; then
    note "根证书        $ENV_DIR/ca.pem"
  fi
  if [ -n "$UNINSTALL_SCRIPT" ]; then
    note "卸载脚本      $UNINSTALL_SCRIPT"
    note "              卸载：sudo bash $UNINSTALL_SCRIPT（加 -p 连节点身份一起删）"
  fi
}

usage() {
  cat <<'USAGE'
用法：bash <(curl -fLsS <面板地址>/download/agent-install.sh) -t <凭据> -u <面板地址> [选项]

入口设备（默认）
  -t <凭据>       设备组接入密钥，或「服务器 → 生成接入凭据」的一次性令牌（必填）
  -u <面板地址>   控制台对外地址，例如 https://panel.example.com（必填）
  -n <设备名>     在控制台显示的节点名，默认取本机 hostname
  -a <架构>       覆盖自动探测的架构（amd64 / arm64 / arm / 386）
  -c <CA 地址>    面板使用私有 CA 时，提供 PEM 根证书的下载地址
  -s <状态目录>   Agent 状态目录，默认 /var/lib/tfp-agent

出口设备（隧道端点）
  -m exit            以出口身份安装（会同时装注册用的 agent）
  -m secure-direct   以安全直连目标 Agent 身份安装（仅 TCP）
  -S <服务名>        证书覆盖的域名/IP（provided / auto 必填；public-ip 自动探测）
  -e <出口令牌>      至少 16 字符；入口建规则时要填同一个值（必填）
  -C <证书路径>      出口 TLS 证书 PEM（provided 必填）
  -K <私钥路径>      出口 TLS 私钥 PEM（provided 必填）
  -q <证书模式>      provided / public-ip / auto，默认 provided
                    public-ip 自动申请 Let's Encrypt IP 证书并续签，需要公网 TCP/80 可达
  -l <监听地址>      默认 0.0.0.0:9443
  -D <UDP监听地址>   启用 QUIC DATAGRAM，例如 0.0.0.0:9443；仅普通出口
  -p <承载>          tls / ws / wss / http / secure-direct，默认 tls
  -O <混淆策略>      secure-direct 必填：random-padding / timing-perturb / tls-mimic
  -P <JSON>          混淆参数 JSON（可选）

托管隧道服务
  -F <本地JSON>    安装证书、CA 标签与监听地址许可 profile（仅 agent 模式）
  -I <本地JSON>    安装私有协议检测凭据 profile（仅 agent 模式，0600）
  -B <本地JSON>    安装受控业务 TLS/上游身份 profile（仅 agent 模式，0600）
  -M              配合 -F 停用旧 tfp-exit 服务，迁移到普通 Agent 托管

其它
  系统            Debian / Ubuntu、RHEL / Rocky / AlmaLinux / Fedora、Arch / Manjaro
                  需要运行中的 systemd；缺少依赖时使用 apt、dnf、yum 或 pacman 安装
  -x              卸载：停止并删除服务，保留状态目录
  -h              显示本帮助
USAGE
}

while getopts ":t:u:n:a:c:s:m:S:e:C:K:q:w:l:D:p:O:P:F:I:B:Mxh" opt; do
  case "$opt" in
    t) TOKEN="$OPTARG" ;;
    u) PANEL_URL="$OPTARG" ;;
    n) NODE_NAME="$OPTARG" ;;
    a) ARCH_OVERRIDE="$OPTARG" ;;
    c) CA_URL="$OPTARG" ;;
    s) STATE_DIR="$OPTARG" ;;
    m) MODE="$OPTARG" ;;
    S) EXIT_SERVER_NAME="$OPTARG" ;;
    e) EXIT_TOKEN="$OPTARG" ;;
    C) EXIT_CERT="$OPTARG" ;;
    K) EXIT_KEY="$OPTARG" ;;
    q) EXIT_CERT_MODE="$OPTARG" ;;
    w) : ;; # 兼容旧接入命令；目标由转发规则配置，不再使用白名单。
    l) EXIT_LISTEN="$OPTARG" ;;
    D) EXIT_UDP_LISTEN="$OPTARG" ;;
    p) EXIT_TRANSPORT="$OPTARG" ;;
    O) EXIT_OBFUSCATION="$OPTARG" ;;
    P) EXIT_OBFUSCATION_PARAMS="$OPTARG" ;;
    F) SERVICE_PROFILES="$OPTARG" ;;
    I) INSPECTION_PROFILES="$OPTARG" ;;
    B) BUSINESS_PROFILES="$OPTARG" ;;
    M) MIGRATE_SERVICES="yes" ;;
    x) UNINSTALL="yes" ;;
    h) usage; exit 0 ;;
    \?) die "未知参数 -$OPTARG（用 -h 查看用法）" ;;
    :) die "参数 -$OPTARG 缺少取值" ;;
  esac
done

# 先做与机器状态无关的确定性检查：参数拼错和权限不足是两回事，先把便宜的
# 那个报出来，操作方不必 sudo 一次才发现地址写错了。
if [ "$UNINSTALL" != "yes" ]; then
  case "$MODE" in
    agent|exit|secure-direct) ;;
    *) die "模式只能是 agent、exit 或 secure-direct，收到 $MODE" ;;
  esac
  [ -n "$TOKEN" ] || { usage >&2; die "缺少 -t 接入凭据"; }
  [ -n "$PANEL_URL" ] || { usage >&2; die "缺少 -u 面板地址"; }
  case "$PANEL_URL" in
    https://*) ;;
    http://127.0.0.1*|http://localhost*) ;;
    *) die "面板地址必须是 https://（仅本机回环允许 http://）" ;;
  esac
  PANEL_URL="${PANEL_URL%/}"
  if [ "$MODE" = "exit" ] || [ "$MODE" = "secure-direct" ]; then
    [ -n "$EXIT_TOKEN" ] || { usage >&2; die "出口模式缺少 -e 出口令牌"; }
    [ "${#EXIT_TOKEN}" -ge 16 ] || die "出口令牌至少 16 字符"
    case "$EXIT_CERT_MODE" in provided|public-ip|auto) ;; *) die "证书模式只能是 provided、public-ip 或 auto" ;; esac
    if [ "$EXIT_CERT_MODE" != "public-ip" ]; then
      [ -n "$EXIT_SERVER_NAME" ] || { usage >&2; die "出口模式缺少 -S 服务名"; }
    fi
    if [ "$EXIT_CERT_MODE" = "provided" ]; then
      [ -n "$EXIT_CERT" ] || { usage >&2; die "出口模式缺少 -C 证书路径"; }
      [ -n "$EXIT_KEY" ] || { usage >&2; die "出口模式缺少 -K 私钥路径"; }
    fi
    case "$EXIT_TRANSPORT" in
      tls|ws|wss|http|secure-direct) ;;
      *) die "出口承载只能是 tls / ws / wss / http / secure-direct，收到 $EXIT_TRANSPORT" ;;
    esac
    case "$EXIT_LISTEN" in
      *[!A-Za-z0-9.:\[\]]*) die "监听地址格式不对：$EXIT_LISTEN" ;;
    esac
    if [ -n "$EXIT_UDP_LISTEN" ]; then
      [ "$MODE" = "exit" ] || die "原生 UDP 监听仅适用于普通出口"
      case "$EXIT_UDP_LISTEN" in *[!A-Za-z0-9.:\[\]]*) die "UDP 监听地址格式不对" ;; esac
      [ "${#EXIT_TOKEN}" -le 128 ] || die "UDP 出口令牌最多 128 字符"
    fi
    [ "${#NODE_NAME}" -le 128 ] || die "设备名超过 128 字符，出口身份取的就是它"
    if [ "$EXIT_CERT_MODE" = "provided" ]; then
      [ -r "$EXIT_CERT" ] || die "读不到证书 $EXIT_CERT"
      [ -r "$EXIT_KEY" ] || die "读不到私钥 $EXIT_KEY"
    fi
    if [ "$MODE" = "secure-direct" ]; then
      [ "$EXIT_TRANSPORT" = "secure-direct" ] || die "secure-direct 模式必须使用 -p secure-direct"
      [ -n "$EXIT_OBFUSCATION" ] || die "secure-direct 模式必须指定 -O 混淆策略"
      case "$EXIT_OBFUSCATION" in random-padding|timing-perturb|tls-mimic) ;; *) die "不支持的混淆策略 $EXIT_OBFUSCATION" ;; esac
      case "$EXIT_OBFUSCATION_PARAMS" in *[!A-Za-z0-9_.,:{}\"\ \[\]-]*) die "混淆参数 JSON 含有非法字符" ;; esac
    fi
  fi
fi

if [ -n "$SERVICE_PROFILES" ]; then
  [ "$MODE" = "agent" ] || die "托管 profile 仅用于 agent 模式"
  case "$SERVICE_PROFILES" in /*) ;; *) die "profile 必须使用本地绝对路径" ;; esac
  [ -r "$SERVICE_PROFILES" ] || die "无法读取本地 service profile"
fi
[ "$MIGRATE_SERVICES" != "yes" ] || [ -n "$SERVICE_PROFILES" ] || die "-M 需要同时指定 -F"
for LOCAL_PROFILE in "$INSPECTION_PROFILES" "$BUSINESS_PROFILES"; do
  [ -n "$LOCAL_PROFILE" ] || continue
  [ "$MODE" = "agent" ] || die "协议检测与业务 profile 仅用于 agent 模式"
  case "$LOCAL_PROFILE" in /*) ;; *) die "profile 必须使用本地绝对路径" ;; esac
  [ -r "$LOCAL_PROFILE" ] || die "无法读取本地协议检测或业务 profile"
done

[ "$(id -u)" -eq 0 ] || die "需要 root 权限：请用 sudo 重新执行"
[ "$(uname -s)" = "Linux" ] || die "本脚本只支持 Linux 设备"
command -v systemctl >/dev/null 2>&1 || die "本机没有 systemd，无法安装为服务；请自行部署 Agent"
systemctl show --property=Version >/dev/null 2>&1 || die "systemd 未运行或无法连接；容器/chroot、WSL 需先启用 systemd"

[ ! -e /var/lib/tfp-agent-uninstall ] || die "设备正在远程卸载并等待面板确认，请稍后重试；可用 journalctl -u tfp-agent-uninstall 查看进度"

# ── 卸载 ────────────────────────────────────────────────────────────
if [ "$UNINSTALL" = "yes" ]; then
  echo "正在卸载…"
  systemctl disable --now tfp-agent.service 2>/dev/null || true
  systemctl disable --now tfp-exit.service 2>/dev/null || true
  systemctl disable --now tfp-cert-renew.timer tfp-cert-renew.service 2>/dev/null || true
  rm -f "$UNIT_PATH" "$EXIT_UNIT" "$RENEW_UNIT" "$RENEW_TIMER" "$BIN_PATH" "$BIN_PATH.tfp-next" "$BIN_PATH.tfp-previous"
  rm -f "$ENV_DIR/agent.env" "$ENV_DIR/exit.env" "$ENV_DIR/managed-install" "$ENV_DIR/managed-exit" "$ENV_DIR/release-key.pub"
  rm -f "$STATE_DIR/agent-state.json.upgrade.json" "$STATE_DIR/agent-state.json.upgrade.json.result" "$STATE_DIR/agent-state.json.upgrade.json.health" "$STATE_DIR/agent-state.json.upgrade.json.lock"
  systemctl daemon-reload
  echo "已卸载。状态目录 $STATE_DIR 保留 —— 里面是节点身份，删除它等于让本机重新注册。"
  exit 0
fi

if [ -n "$SERVICE_PROFILES" ] && systemctl is-active --quiet tfp-exit.service; then
  [ "$MIGRATE_SERVICES" = "yes" ] || die "旧出口服务仍占用端口；迁移时使用 -M -F，先配置相同端口的本地许可"
  systemctl disable --now tfp-exit.service
fi

# ── 架构探测 ────────────────────────────────────────────────────────
if [ -n "$ARCH_OVERRIDE" ]; then
  ARCH="$ARCH_OVERRIDE"
else
  case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    armv7l|armv6l) ARCH="arm" ;;
    i386|i686) ARCH="386" ;;
    *) die "无法识别的架构 $(uname -m)，请用 -a 指定" ;;
  esac
fi
case "$ARCH" in amd64|arm64|arm|386) ;; *) die "不支持的架构 $ARCH" ;; esac

detect_system
note "系统：$SYSTEM_NAME"
ensure_base_tools
TMP="$(mktemp -d)"
BIN_TMP=""
trap 'rm -rf "$TMP"; if [ -n "$BIN_TMP" ]; then rm -f "$BIN_TMP"; fi' EXIT
CURL_CA_ARGS=()
if [ -n "$CA_URL" ]; then
  curl -fLsS --max-time 30 -o "$TMP/ca.pem" "$CA_URL" || die "下载 CA 证书失败"
  CURL_CA_ARGS=(--cacert "$TMP/ca.pem")
fi
curl "${CURL_CA_ARGS[@]}" -fsSL --max-time 20 -o /dev/null "$PANEL_URL/" 2>/dev/null \
  || die "无法访问面板 $PANEL_URL，请检查地址、网络与证书"

NODE_NAME="${NODE_NAME:-$(uname -n)}"

if [ "$MODE" = "exit" ] || [ "$MODE" = "secure-direct" ]; then
  if [ "$MODE" = "secure-direct" ]; then
    echo '安全直连目标设备接入'
  else
    echo '出口设备接入'
  fi
  note "面板：$PANEL_URL"
  if [ "$EXIT_CERT_MODE" != "public-ip" ]; then note "服务名：$EXIT_SERVER_NAME"; fi
  note "承载：$EXIT_TRANSPORT   监听：$EXIT_LISTEN"
else
  echo "入口设备接入"
  note "面板：$PANEL_URL"
  note "设备名：$NODE_NAME"
fi

if [ "$MODE" = "exit" ] || [ "$MODE" = "secure-direct" ]; then
  if [ "$EXIT_CERT_MODE" = "public-ip" ]; then
    ensure_ip_certbot
    detect_public_ip
    note "证书公网 IP：$EXIT_SERVER_NAME"
    install -d -m 0700 "$ACME_CONFIG" "$ACME_WORK" "$ACME_LOG"
    "$CERTBOT_BIN" certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email \
      --server https://acme-v02.api.letsencrypt.org/directory --required-profile shortlived \
      --ip-address "$EXIT_SERVER_NAME" --cert-name "$IP_CERT_NAME" --renew-with-new-domains \
      --config-dir "$ACME_CONFIG" --work-dir "$ACME_WORK" --logs-dir "$ACME_LOG" \
      || die "公网 IP 证书申请失败；请确保本机公网 IP 的 TCP/80 可达且端口未被占用（包括云安全组、系统防火墙和 NAT 转发）"
    EXIT_CERT="$ACME_CONFIG/live/$IP_CERT_NAME/fullchain.pem"
    EXIT_KEY="$ACME_CONFIG/live/$IP_CERT_NAME/privkey.pem"
    chmod 0600 "$EXIT_KEY"
  elif [ "$EXIT_CERT_MODE" = "auto" ]; then
    [ "$EXIT_SERVER_NAME" != "" ] || die "公网证书模式需要 -S 域名"
    case "$EXIT_SERVER_NAME" in *[!A-Za-z0-9.-]*) die "公网证书模式只接受 DNS 域名" ;; esac
    if command -v certbot >/dev/null 2>&1; then
      certbot certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email -d "$EXIT_SERVER_NAME" \
        || die "certbot 公网证书申请失败；请确保 DNS 指向本机且 TCP/80 可达"
      EXIT_CERT="/etc/letsencrypt/live/$EXIT_SERVER_NAME/fullchain.pem"
      EXIT_KEY="/etc/letsencrypt/live/$EXIT_SERVER_NAME/privkey.pem"
    elif command -v acme.sh >/dev/null 2>&1; then
      export ACME_HOME="${HOME:-/root}/.acme.sh"
      acme.sh --issue --standalone -d "$EXIT_SERVER_NAME" || die "acme.sh 公网证书申请失败；请确保 TCP/80 可达"
      mkdir -p "/etc/tfp-agent/certs/$EXIT_SERVER_NAME"
      acme.sh --install-cert -d "$EXIT_SERVER_NAME" --fullchain-file "/etc/tfp-agent/certs/$EXIT_SERVER_NAME/fullchain.pem" --key-file "/etc/tfp-agent/certs/$EXIT_SERVER_NAME/privkey.pem" || die "安装公网证书失败"
      EXIT_CERT="/etc/tfp-agent/certs/$EXIT_SERVER_NAME/fullchain.pem"
      EXIT_KEY="/etc/tfp-agent/certs/$EXIT_SERVER_NAME/privkey.pem"
    else
      die "公网证书模式需要预装 certbot 或 acme.sh；未获取证书时不会降级为明文"
    fi
    chmod 0600 "$EXIT_KEY"
  fi
fi
note "架构：linux/$ARCH"
echo

# ── 下载 Agent ──────────────────────────────────────────────────────
echo "正在下载 Agent…"
curl "${CURL_CA_ARGS[@]}" -fLsS --max-time 300 -o "$TMP/agent" "$PANEL_URL/download/agent/linux/$ARCH" \
  || die "下载失败：面板上可能没有 linux/$ARCH 的 Agent 产物"

# 面板在产物缺失时返回的是 JSON 错误体，直接装上去会得到一个「不是可执行文件」
# 的服务，表现为 systemd 无限重启。这里先认魔术字节，把失败挡在安装之前。
if [ "$(head -c 4 "$TMP/agent" | od -An -tx1 | tr -d ' \n')" != "7f454c46" ]; then
  die "下载到的不是可执行文件（面板返回的可能是错误信息）：$(head -c 200 "$TMP/agent")"
fi
# Validate any existing pin before changing the running program or unit.
UPGRADE_ARG=""
if [[ "$PANEL_URL" == https://* ]]; then
  if curl "${CURL_CA_ARGS[@]}" -fLsS --max-time 20 -o "$TMP/release-key.pub" "$PANEL_URL/download/agent-release-key"; then
    RELEASE_KEY=$(tr -d '\r\n' < "$TMP/release-key.pub")
    [[ "$RELEASE_KEY" =~ ^[A-Za-z0-9+/]{43}=$ ]] || die "面板升级公钥格式不正确"
    if [ -f "$ENV_DIR/release-key.pub" ] && [ "$(tr -d '\r\n' < "$ENV_DIR/release-key.pub")" != "$RELEASE_KEY" ]; then
      die "面板升级公钥已变化，请核实面板数据库或备份；现有信任公钥不会被覆盖"
    fi
  else
    rm -f "$TMP/release-key.pub"
    note "面板尚未提供升级公钥；自动升级暂不可用"
  fi
else
  note "面板地址未使用 HTTPS；自动升级暂不可用"
fi
install -d -m 0755 "${BIN_PATH%/*}"
# Replace the inode, rather than opening the running executable for writing.
BIN_TMP="$(mktemp "$BIN_PATH.install.XXXXXX")"
install -m 0755 "$TMP/agent" "$BIN_TMP"
mv -f "$BIN_TMP" "$BIN_PATH"
BIN_TMP=""
restore_contexts "$BIN_PATH"
note "已安装 $BIN_PATH"

# ── 证书 ────────────────────────────────────────────────────────────
CA_ARG=""
if [ -n "$CA_URL" ]; then
  install -d -m 0755 "$ENV_DIR"
  install -m 0644 "$TMP/ca.pem" "$ENV_DIR/ca.pem"
  CA_ARG=" -ca $ENV_DIR/ca.pem"
  note "已安装根证书 $ENV_DIR/ca.pem"
fi

install -d -m 0755 "$ENV_DIR"
install -d -m 0700 "$STATE_DIR"
# Pin the installation's public key on first setup. Reinstall must not silently
# trust a rotated key; the operator must resolve a mismatch explicitly.
if [ -f "$TMP/release-key.pub" ]; then
  printf '%s\n' "$RELEASE_KEY" > "$ENV_DIR/release-key.pub"
  chmod 0644 "$ENV_DIR/release-key.pub"
fi
if [[ "$PANEL_URL" == https://* ]] && [ -f "$ENV_DIR/release-key.pub" ]; then
  UPGRADE_ARG=" -release-key $ENV_DIR/release-key.pub"
fi
PROFILE_ARG=""
if [ -n "$SERVICE_PROFILES" ]; then
  if [ "$SERVICE_PROFILES" != "$ENV_DIR/service-profiles.json" ]; then
    install -m 0600 "$SERVICE_PROFILES" "$ENV_DIR/service-profiles.json"
  else
    chmod 0600 "$ENV_DIR/service-profiles.json"
  fi
  restore_contexts "$ENV_DIR/service-profiles.json"
fi
if [ -f "$ENV_DIR/service-profiles.json" ]; then
  PROFILE_ARG=" -service-profiles $ENV_DIR/service-profiles.json"
fi
for PROFILE_KIND in inspection business; do
  if [ "$PROFILE_KIND" = inspection ]; then LOCAL_PROFILE="$INSPECTION_PROFILES"; else LOCAL_PROFILE="$BUSINESS_PROFILES"; fi
  PROFILE_TARGET="$ENV_DIR/$PROFILE_KIND-profiles.json"
  if [ -n "$LOCAL_PROFILE" ]; then
    if [ "$LOCAL_PROFILE" != "$PROFILE_TARGET" ]; then install -m 0600 "$LOCAL_PROFILE" "$PROFILE_TARGET"; else chmod 0600 "$PROFILE_TARGET"; fi
    restore_contexts "$PROFILE_TARGET"
  fi
  if [ -f "$PROFILE_TARGET" ]; then
    chmod 0600 "$PROFILE_TARGET"
    PROFILE_ARG="$PROFILE_ARG -$PROFILE_KIND-profiles $PROFILE_TARGET"
  fi
done
# 令牌只落在这一个 0600 文件里，不进 unit、不进命令行（命令行对同机其他用户可见）
umask 077
printf 'TFP_ENROLLMENT_TOKEN=%s\n' "$TOKEN" > "$ENV_DIR/agent.env"
chmod 0600 "$ENV_DIR/agent.env"

touch "$ENV_DIR/managed-install"
chmod 0600 "$ENV_DIR/managed-install"
# Adopt only the exit unit generated by the official installer. Custom units
# remain outside the automatic upgrade lifecycle.
if [ -f "$EXIT_UNIT" ] \
  && grep -q '^Description=流量转发控制台出口（隧道端点）$' "$EXIT_UNIT" \
  && grep -q "^ExecStart=$BIN_PATH -mode \\(exit\\|secure-direct\\) " "$EXIT_UNIT"; then
  printf 'tfp-exit.service\n' > "$ENV_DIR/managed-exit"
  chmod 0600 "$ENV_DIR/managed-exit"
fi

# ── 注册用的 Agent ──────────────────────────────────────────────────
#
# 出口模式也要装它：出口服务本身不跟面板通信，没有这个 agent，设备不会出现
# 在控制台里，出口列表的「在线」列也就永远是离线。
# 顺手把卸载脚本放到本机：面板不可达或 Agent 起不来时，本机这份仍然能用。
# 拉不到不算失败 —— 它只是方便，不该拦住接入。
if curl "${CURL_CA_ARGS[@]}" -fLsS --max-time 20 -o "$TMP/agent-uninstall.sh" "$PANEL_URL/download/agent-uninstall.sh" 2>/dev/null; then
  install -m 0700 "$TMP/agent-uninstall.sh" "$ENV_DIR/agent-uninstall.sh"
  UNINSTALL_SCRIPT="$ENV_DIR/agent-uninstall.sh"
fi

cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=流量转发控制台 Agent
Documentation=$PANEL_URL
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_DIR/agent.env
ExecStart=$BIN_PATH -panel $PANEL_URL -name $NODE_NAME -state $STATE_DIR/agent-state.json -disk / -enable-terminal -enable-uninstall$CA_ARG$PROFILE_ARG$UPGRADE_ARG
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$UNIT_PATH"
restore_contexts "$ENV_DIR" "$STATE_DIR" "$ENV_DIR/agent.env" "$ENV_DIR/managed-install" "$UNIT_PATH"
systemctl daemon-reload
systemctl enable tfp-agent.service >/dev/null 2>&1
systemctl restart tfp-agent.service

# ── 出口服务 ────────────────────────────────────────────────────────
if [ "$MODE" = "exit" ] || [ "$MODE" = "secure-direct" ]; then
  # 证书先验一次。载不进去的话，服务会以「无限重启」的形式失败，而 journalctl
  # 里的错误比现在这一行难读得多；证书没覆盖 -S 那个名字的话，入口侧握手会失败，
  # 而那时候排查要跨越两台机器。openssl 不在就跳过，不把它变成硬依赖。
  if command -v openssl >/dev/null 2>&1; then
    openssl x509 -in "$EXIT_CERT" -noout -checkend 0 >/dev/null 2>&1 \
      || die "证书已过期或无法解析：$EXIT_CERT"
    CHECK_NAME="-checkhost"
    if [[ "$EXIT_SERVER_NAME" == *:* || "$EXIT_SERVER_NAME" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      CHECK_NAME="-checkip"
    fi
    openssl x509 -in "$EXIT_CERT" -noout "$CHECK_NAME" "$EXIT_SERVER_NAME" >/dev/null 2>&1 \
      || die "证书不覆盖 $EXIT_SERVER_NAME —— 入口会拒绝这个出口。换一张含该域名/IP 的证书，或用 -S 指定证书里已有的身份"
    note "证书校验通过（覆盖 $EXIT_SERVER_NAME）"
  else
    echo "提示：未安装 openssl，跳过证书有效期与域名校验。" >&2
  fi

  umask 077
  printf 'TFP_EXIT_TOKEN=%s\n' "$EXIT_TOKEN" > "$ENV_DIR/exit.env"
  chmod 0600 "$ENV_DIR/exit.env"

  # 不传 -server-name：普通出口不用它（只有反向出口用）。出口的身份由证书决定，
  # 入口拿规则里的 tunnel.server_name 去校验，所以 -S 的作用是在这里提前验一次
  # 证书覆不覆盖那个名字，而不是喂给出口进程。
  cat > "$EXIT_UNIT" <<UNIT
[Unit]
Description=流量转发控制台出口（隧道端点）
Documentation=$PANEL_URL
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_DIR/exit.env
ExecStart=$BIN_PATH -mode $MODE -exit-id $NODE_NAME -listen $EXIT_LISTEN -transport $EXIT_TRANSPORT -cert $EXIT_CERT -key $EXIT_KEY -obfuscation-strategy '$EXIT_OBFUSCATION' -obfuscation-params '$EXIT_OBFUSCATION_PARAMS'$CA_ARG -udp-listen '$EXIT_UDP_LISTEN'
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
  chmod 0644 "$EXIT_UNIT"
  printf 'tfp-exit.service\n' > "$ENV_DIR/managed-exit"
  chmod 0600 "$ENV_DIR/managed-exit"
  restore_contexts "$ENV_DIR/exit.env" "$EXIT_UNIT"
  systemctl daemon-reload
  systemctl enable tfp-exit.service >/dev/null 2>&1
  systemctl restart tfp-exit.service
  if [ "$EXIT_CERT_MODE" = "public-ip" ]; then
    configure_ip_renewal
  elif [ -f "$RENEW_TIMER" ]; then
    systemctl disable --now tfp-cert-renew.timer tfp-cert-renew.service
    rm -f "$RENEW_UNIT" "$RENEW_TIMER"
    systemctl daemon-reload
  fi
fi

# ── 确认 ────────────────────────────────────────────────────────────
# 注册成功与否不能只看状态文件在不在：Agent 一打开存储就会建出 checkpoint，
# 那时还没注册。真正可靠的信号是 identity 事件落了盘 —— 它可能还在 WAL 里，
# 所以两个文件都要看。
echo
echo "等待 Agent 注册…"
DEADLINE=$((SECONDS + WAIT_SECONDS))
REGISTERED="no"
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  if grep -qs '"kind":"identity"' "$STATE_DIR"/agent-state.json* 2>/dev/null \
     || grep -qs '"identity":{"node_id":"' "$STATE_DIR"/agent-state.json 2>/dev/null; then
    REGISTERED="yes"
    break
  fi
  if ! systemctl is-active --quiet tfp-agent.service; then
    echo
    die "服务启动失败，最近日志：
$(journalctl -u tfp-agent.service -n 20 --no-pager 2>/dev/null || true)"
  fi
  sleep 1
done

echo
if [ "$MODE" = "exit" ] || [ "$MODE" = "secure-direct" ]; then
  if ! systemctl is-active --quiet tfp-exit.service; then
    die "出口服务启动失败，最近日志：
$(journalctl -u tfp-exit.service -n 20 --no-pager 2>/dev/null || true)"
  fi
  echo "出口已就绪。"
  note "出口服务：systemctl status tfp-exit    日志：journalctl -u tfp-exit -f"
  note "注册 Agent：systemctl status tfp-agent  日志：journalctl -u tfp-agent -f"
  print_paths
  echo
  echo "还差一步：到控制台「出口管理 → 新增」建一条记录，填上"
  note "出口服务器 = 本机（节点「$NODE_NAME」）"
  note "承载 = $EXIT_TRANSPORT"
  EXIT_ENDPOINT="$EXIT_SERVER_NAME:${EXIT_LISTEN##*:}"
  if [[ "$EXIT_SERVER_NAME" == *:* ]]; then EXIT_ENDPOINT="[$EXIT_SERVER_NAME]:${EXIT_LISTEN##*:}"; fi
  note "端点 = $EXIT_ENDPOINT"
  if [ "$EXIT_CERT_MODE" = "public-ip" ]; then
    note "TLS 服务名 = 留空（直接校验端点 IP）"
    note "证书有效期约 6 天半；每 6 小时自动检查续签，新连接自动加载新证书"
  else
    note "服务名 = $EXIT_SERVER_NAME"
  fi
  note "令牌 = 与 -e 传入的同一个值"
  echo "入口规则选这条出口后，流量才会真正走隧道。"
elif [ "$REGISTERED" = "yes" ]; then
  echo "接入完成。"
  note "服务：systemctl status tfp-agent"
  note "日志：journalctl -u tfp-agent -f"
  note "控制台「服务器」页应已出现节点「$NODE_NAME」"
  print_paths
else
  echo "服务已在运行，但 ${WAIT_SECONDS} 秒内未完成注册。"
  note "请查看：journalctl -u tfp-agent -n 50 --no-pager"
  note "常见原因：令牌已过期（有效期 15 分钟）或已被使用、面板地址不可达、"
  note "          面板使用私有 CA 而缺少 -c 参数"
  print_paths
fi
