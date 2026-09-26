#!/usr/bin/env bash
#
# 设备卸载脚本 —— 由流量转发控制台托管，在**目标设备**上执行。
#
#   bash <(curl -fLsS https://panel.example.com/download/agent-uninstall.sh)
#
# 停止并移除注册用的 Agent 与出口服务，连同它们的二进制、systemd 单元和环境
# 文件。接入脚本在安装时会把本脚本放到 /etc/tfp-agent/agent-uninstall.sh，
# 面板不可达时用那一份。
#
# 与网页端的「卸载设备」不是一回事：那条路由 Agent 自己执行并回报结果，面板
# 据此把节点从列表移除。本脚本用于面板不可达、或 Agent 已经起不来的机器 ——
# 它不会、也无法通知面板，面板上的节点记录需要到控制台里删掉。
#
# 节点身份目录默认保留：里面是本机的注册凭据，删掉等于让本机重新注册成一个
# 新节点。确实要让另一台机器接管同一个节点时，用 -p 一并删除。

set -euo pipefail

BIN_PATH="/usr/local/bin/tfp-agent"
STATE_DIR="/var/lib/tfp-agent"
ENV_DIR="/etc/tfp-agent"
UNIT_PATH="/etc/systemd/system/tfp-agent.service"
EXIT_UNIT="/etc/systemd/system/tfp-exit.service"
UNINSTALL_WORKER="/var/lib/tfp-agent-uninstall"
PURGE="no"

die() {
  printf '错误：%s\n' "$1" >&2
  exit 1
}
note() { printf '  %s\n' "$1"; }

usage() {
  cat <<'USAGE'
用法：bash <(curl -fLsS <面板地址>/download/agent-uninstall.sh) [选项]

选项：
  -p, --purge     连同节点身份目录一起删除（本机将重新注册为一个新节点）
  -h, --help      显示本帮助
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    -p | --purge) PURGE="yes" ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "未知参数：$1"
      ;;
  esac
  shift
done

[ "$(id -u)" -eq 0 ] || die "需要 root 权限：请用 sudo 重新执行"
# 远程卸载已经排上了队时不能从旁边再拆一遍：那条路会自己停服务、删文件并回报
# 面板，两边同时动同一批文件只会留下半截状态。
[ ! -e "$UNINSTALL_WORKER" ] || die "设备正在远程卸载并等待面板确认，请稍后重试；可用 journalctl -u tfp-agent-uninstall 查看进度"

echo "正在卸载…"
systemctl disable --now tfp-agent.service 2>/dev/null || true
systemctl disable --now tfp-exit.service 2>/dev/null || true
rm -f "$UNIT_PATH" "$EXIT_UNIT" "$BIN_PATH"
rm -f "$ENV_DIR/agent.env" "$ENV_DIR/exit.env" "$ENV_DIR/managed-install" "$ENV_DIR/agent-uninstall.sh"
systemctl daemon-reload

if [ "$PURGE" = "yes" ]; then
  rm -rf "$STATE_DIR"
  echo "已卸载，节点身份目录也已删除。"
else
  echo "已卸载。"
fi

echo
echo "本机剩余文件："
if [ -d "$STATE_DIR" ]; then
  note "节点身份  $STATE_DIR（重新接入会复用它；要让本机重新注册用 -p）"
fi
if [ -d "$ENV_DIR" ]; then
  note "环境目录  $ENV_DIR（空目录，可以自行删除）"
fi
echo
echo "面板上的节点记录不会自动消失：到控制台「服务器」页删掉，或等它离线。"
