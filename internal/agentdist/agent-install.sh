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
#        -C /etc/ssl/exit.crt -K /etc/ssl/exit.key -w 'tcp|10.20.0.11:27015'
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

# 出口模式
EXIT_SERVER_NAME=""
EXIT_LISTEN="0.0.0.0:9443"
EXIT_TOKEN=""
EXIT_CERT=""
EXIT_KEY=""
EXIT_ALLOW=""
EXIT_TRANSPORT="tls"
EXIT_UNIT="/etc/systemd/system/tfp-exit.service"

die() {
  printf '错误：%s\n' "$1" >&2
  exit 1
}
note() { printf '  %s\n' "$1"; }

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
  -S <服务名>        出口对外域名，证书必须包含它（必填）
  -e <出口令牌>      至少 16 字符；入口建规则时要填同一个值（必填）
  -C <证书路径>      出口 TLS 证书 PEM（必填）
  -K <私钥路径>      出口 TLS 私钥 PEM（必填）
  -w <允许目标>      host:port 精确匹配，逗号分隔，如 tcp|127.0.0.1:8080,udp|127.0.0.1:5353（必填）
  -l <监听地址>      默认 0.0.0.0:9443
  -p <承载>          tls / ws / wss / http，默认 tls

其它
  -x              卸载：停止并删除服务，保留状态目录
  -h              显示本帮助
USAGE
}

while getopts ":t:u:n:a:c:s:m:S:e:C:K:w:l:p:xh" opt; do
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
    w) EXIT_ALLOW="$OPTARG" ;;
    l) EXIT_LISTEN="$OPTARG" ;;
    p) EXIT_TRANSPORT="$OPTARG" ;;
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
    agent|exit) ;;
    *) die "模式只能是 agent 或 exit，收到 $MODE" ;;
  esac
  [ -n "$TOKEN" ] || { usage >&2; die "缺少 -t 接入凭据"; }
  [ -n "$PANEL_URL" ] || { usage >&2; die "缺少 -u 面板地址"; }
  case "$PANEL_URL" in
    https://*) ;;
    http://127.0.0.1*|http://localhost*) ;;
    *) die "面板地址必须是 https://（仅本机回环允许 http://）" ;;
  esac
  PANEL_URL="${PANEL_URL%/}"
  if [ "$MODE" = "exit" ]; then
    [ -n "$EXIT_SERVER_NAME" ] || { usage >&2; die "出口模式缺少 -S 服务名"; }
    [ -n "$EXIT_TOKEN" ] || { usage >&2; die "出口模式缺少 -e 出口令牌"; }
    [ "${#EXIT_TOKEN}" -ge 16 ] || die "出口令牌至少 16 字符"
    [ -n "$EXIT_CERT" ] || { usage >&2; die "出口模式缺少 -C 证书路径"; }
    [ -n "$EXIT_KEY" ] || { usage >&2; die "出口模式缺少 -K 私钥路径"; }
    [ -n "$EXIT_ALLOW" ] || { usage >&2; die "出口模式缺少 -w 允许目标"; }
    case "$EXIT_TRANSPORT" in
      tls|ws|wss|http) ;;
      *) die "出口承载只能是 tls / ws / wss / http，收到 $EXIT_TRANSPORT" ;;
    esac
    # 这一串会被原样写进 systemd 单元的 ExecStart，只允许它需要的字符。
    case "$EXIT_ALLOW" in
      *[!A-Za-z0-9\|.,:\[\]-]*) die "允许目标里含有非法字符：$EXIT_ALLOW" ;;
    esac
    case "$EXIT_LISTEN" in
      *[!A-Za-z0-9.:\[\]]*) die "监听地址格式不对：$EXIT_LISTEN" ;;
    esac
    [ "${#NODE_NAME}" -le 128 ] || die "设备名超过 128 字符，出口身份取的就是它"
    [ -r "$EXIT_CERT" ] || die "读不到证书 $EXIT_CERT"
    [ -r "$EXIT_KEY" ] || die "读不到私钥 $EXIT_KEY"
  fi
fi

[ "$(id -u)" -eq 0 ] || die "需要 root 权限：请用 sudo 重新执行"

[ ! -e /var/lib/tfp-agent-uninstall ] || die "设备正在远程卸载并等待面板确认，请稍后重试；可用 journalctl -u tfp-agent-uninstall 查看进度"

# ── 卸载 ────────────────────────────────────────────────────────────
if [ "$UNINSTALL" = "yes" ]; then
  echo "正在卸载…"
  systemctl disable --now tfp-agent.service 2>/dev/null || true
  systemctl disable --now tfp-exit.service 2>/dev/null || true
  rm -f "$UNIT_PATH" "$EXIT_UNIT" "$BIN_PATH"
  rm -f "$ENV_DIR/agent.env" "$ENV_DIR/exit.env" "$ENV_DIR/managed-install"
  systemctl daemon-reload
  echo "已卸载。状态目录 $STATE_DIR 保留 —— 里面是节点身份，删除它等于让本机重新注册。"
  exit 0
fi

command -v systemctl >/dev/null 2>&1 || die "本机没有 systemd，无法安装为服务；请改用 go build 自行部署 Agent"
command -v curl >/dev/null 2>&1 || die "本机没有 curl，Agent 需要它探测公网 IPv4/IPv6 地址"
curl -fsSL --max-time 20 -o /dev/null "$PANEL_URL/" 2>/dev/null \
  || die "无法访问面板 $PANEL_URL，请检查地址、网络与证书"

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
[ "$(uname -s)" = "Linux" ] || die "本脚本只支持 Linux 设备"

NODE_NAME="${NODE_NAME:-$(hostname)}"

if [ "$MODE" = "exit" ]; then
  echo "出口设备接入"
  note "面板：$PANEL_URL"
  note "服务名：$EXIT_SERVER_NAME"
  note "承载：$EXIT_TRANSPORT   监听：$EXIT_LISTEN"
else
  echo "入口设备接入"
  note "面板：$PANEL_URL"
  note "设备名：$NODE_NAME"
fi
note "架构：linux/$ARCH"
echo

# ── 下载 Agent ──────────────────────────────────────────────────────
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "正在下载 Agent…"
curl -fLsS --max-time 300 -o "$TMP/agent" "$PANEL_URL/download/agent/linux/$ARCH" \
  || die "下载失败：面板上可能没有 linux/$ARCH 的 Agent 产物"

# 面板在产物缺失时返回的是 JSON 错误体，直接装上去会得到一个「不是可执行文件」
# 的服务，表现为 systemd 无限重启。这里先认魔术字节，把失败挡在安装之前。
if [ "$(head -c 4 "$TMP/agent" | od -An -tx1 | tr -d ' \n')" != "7f454c46" ]; then
  die "下载到的不是可执行文件（面板返回的可能是错误信息）：$(head -c 200 "$TMP/agent")"
fi
install -m 0755 "$TMP/agent" "$BIN_PATH"
note "已安装 $BIN_PATH"

# ── 证书 ────────────────────────────────────────────────────────────
CA_ARG=""
if [ -n "$CA_URL" ]; then
  install -d -m 0755 "$ENV_DIR"
  curl -fLsS --max-time 30 -o "$ENV_DIR/ca.pem" "$CA_URL" || die "下载 CA 证书失败"
  chmod 0644 "$ENV_DIR/ca.pem"
  CA_ARG=" -ca $ENV_DIR/ca.pem"
  note "已安装根证书 $ENV_DIR/ca.pem"
fi

install -d -m 0755 "$ENV_DIR"
install -d -m 0700 "$STATE_DIR"
# 令牌只落在这一个 0600 文件里，不进 unit、不进命令行（命令行对同机其他用户可见）
umask 077
printf 'TFP_ENROLLMENT_TOKEN=%s\n' "$TOKEN" > "$ENV_DIR/agent.env"
chmod 0600 "$ENV_DIR/agent.env"

touch "$ENV_DIR/managed-install"
chmod 0600 "$ENV_DIR/managed-install"

# ── 注册用的 Agent ──────────────────────────────────────────────────
#
# 出口模式也要装它：出口服务本身不跟面板通信，没有这个 agent，设备不会出现
# 在控制台里，出口列表的「在线」列也就永远是离线。
cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=流量转发控制台 Agent
Documentation=$PANEL_URL
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_DIR/agent.env
ExecStart=$BIN_PATH -panel $PANEL_URL -name $NODE_NAME -state $STATE_DIR/agent-state.json -disk / -enable-terminal -enable-uninstall$CA_ARG
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$UNIT_PATH"
systemctl daemon-reload
systemctl enable tfp-agent.service >/dev/null 2>&1
systemctl restart tfp-agent.service

# ── 出口服务 ────────────────────────────────────────────────────────
if [ "$MODE" = "exit" ]; then
  # 证书先验一次。载不进去的话，服务会以「无限重启」的形式失败，而 journalctl
  # 里的错误比现在这一行难读得多；证书没覆盖 -S 那个名字的话，入口侧握手会失败，
  # 而那时候排查要跨越两台机器。openssl 不在就跳过，不把它变成硬依赖。
  if command -v openssl >/dev/null 2>&1; then
    openssl x509 -in "$EXIT_CERT" -noout -checkend 0 >/dev/null 2>&1 \
      || die "证书已过期或无法解析：$EXIT_CERT"
    openssl x509 -in "$EXIT_CERT" -noout -checkhost "$EXIT_SERVER_NAME" >/dev/null 2>&1 \
      || die "证书不覆盖 $EXIT_SERVER_NAME —— 入口会拒绝这个出口。换一张含该域名的证书，或用 -S 指定证书里已有的名字"
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
ExecStart=$BIN_PATH -mode exit -exit-id $NODE_NAME -listen $EXIT_LISTEN -transport $EXIT_TRANSPORT -cert $EXIT_CERT -key $EXIT_KEY -allow '$EXIT_ALLOW'$CA_ARG
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
  chmod 0644 "$EXIT_UNIT"
  systemctl daemon-reload
  systemctl enable tfp-exit.service >/dev/null 2>&1
  systemctl restart tfp-exit.service
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
if [ "$MODE" = "exit" ]; then
  if ! systemctl is-active --quiet tfp-exit.service; then
    die "出口服务启动失败，最近日志：
$(journalctl -u tfp-exit.service -n 20 --no-pager 2>/dev/null || true)"
  fi
  echo "出口已就绪。"
  note "出口服务：systemctl status tfp-exit    日志：journalctl -u tfp-exit -f"
  note "注册 Agent：systemctl status tfp-agent  日志：journalctl -u tfp-agent -f"
  echo
  echo "还差一步：到控制台「出口管理 → 新增」建一条记录，填上"
  note "出口服务器 = 本机（节点「$NODE_NAME」）"
  note "承载 = $EXIT_TRANSPORT"
  note "端点 = $EXIT_SERVER_NAME:${EXIT_LISTEN##*:}"
  note "服务名 = $EXIT_SERVER_NAME"
  note "令牌 = 与 -e 传入的同一个值"
  echo "入口规则选这条出口后，流量才会真正走隧道。"
elif [ "$REGISTERED" = "yes" ]; then
  echo "接入完成。"
  note "服务：systemctl status tfp-agent"
  note "日志：journalctl -u tfp-agent -f"
  note "控制台「服务器」页应已出现节点「$NODE_NAME」"
else
  echo "服务已在运行，但 ${WAIT_SECONDS} 秒内未完成注册。"
  note "请查看：journalctl -u tfp-agent -n 50 --no-pager"
  note "常见原因：令牌已过期（有效期 15 分钟）或已被使用、面板地址不可达、"
  note "          面板使用私有 CA 而缺少 -c 参数"
fi
