# Agent 接入脚本的 Linux 兼容性

本说明针对面板提供的 `/download/agent-install.sh`，覆盖入口、普通出口（含 `-D` 原生 UDP）和安全直连目标。Docker 面板安装器是另一套脚本。

## 系统与安装行为

| 系列 | 目标系统示例 | 依赖安装 | 公网 IP 证书额外要求 |
|---|---|---|---|
| Debian | Debian 11/12/13 及使用 systemd 的衍生系统 | `apt-get update` 后按需安装 | Python ≥3.10；Debian 11 默认 3.9，需另行提供较新 Python |
| Ubuntu | Ubuntu 22.04/24.04 及使用 systemd 的衍生系统 | 非交互 `apt-get` | 按需补齐 `python3-venv` |
| Red Hat | RHEL 8/9/10、Rocky、AlmaLinux、CentOS Stream、Fedora | 优先 `dnf`，缺少时使用 `yum` | 默认 Python 较旧时，查询已有软件源的 `python3.12/3.11/3.10` 和匹配的 pip 包 |
| Arch | Arch Linux、Manjaro 及使用 systemd 的衍生系统 | `pacman -S --needed --noconfirm` | `python`、`python-pip`、`openssl` |

表中的版本是目标验收范围，不表示已经在每个版本上完成实机测试。停更系统、缺少可用软件源或未启用 RHEL 订阅仓库的系统需要先修复软件源。脚本不会替换系统 Python、添加第三方仓库或升级整个操作系统。

统一要求：Linux、Bash、root 权限、运行中的 systemd、能访问面板下载端点。安装前通过 `systemctl show --property=Version` 检查服务管理器连接，避免在仅装有 systemctl 的容器/chroot 中误报安装成功。OpenRC/runit 等 init 系统需另行部署服务。

发行版通过 `/etc/os-release` 的 `ID/ID_LIKE` 识别；文件缺失时读取 `/usr/lib/os-release`，仍无法识别时按已存在的包管理器选择。基础依赖为 curl、系统 CA、coreutils 和 grep，缺少时才安装；出口另需 OpenSSL。设备名通过 `uname -n` 获取，不依赖 Arch 精简安装中可能缺少的 hostname 工具。

支持自动识别 amd64、arm64、arm、386；**面板必须实际提供对应架构的 Agent 产物**。当前正式发布主要提供 amd64/arm64；架构识别成功不代表存在下载文件。发布构建使用 `CGO_ENABLED=0`，避免依赖目标发行版的 glibc 版本。

## 首次接入

控制台生成的命令使用 curl 下载脚本。因此全新系统在执行命令前至少需要 Bash、curl 和有效的 CA 信任库；脚本尚未下载时不能安装自己的下载工具。按对应系统准备：

```bash
# Debian / Ubuntu（root）
apt-get update
apt-get install -y bash curl ca-certificates

# RHEL / Rocky / AlmaLinux / CentOS Stream / Fedora（root）
dnf install -y bash curl ca-certificates
# 没有 dnf 的 yum 系统使用：yum install -y bash curl ca-certificates

# Arch / Manjaro（root，软件源与已安装系统版本应一致）
pacman -S --needed --noconfirm bash curl ca-certificates
```

Arch 不使用单独的 `pacman -Sy`，也不在接入时自动执行全系统升级；软件源过期导致安装失败时，先按系统维护流程完成 `pacman -Syu`，然后重试接入。

以 root 执行面板生成的完整命令。普通用户可先用 `sudo -i` 进入 root shell 再执行，避免 `sudo bash <(curl ...)` 无法访问原 shell 的进程替换描述符。

普通入口和自备证书出口不需要 Python。只有 `-q public-ip` 需要 Python ≥3.10、SSL/venv/pip 和支持 IP 地址的 Certbot；脚本优先复用已有工具，否则安装到 `/etc/tfp-agent/certbot` 的独立环境。旧 Python 的系统若已有软件源不提供满足要求的版本，会明确终止 IP 证书安装，普通接入仍可使用。

私有 CA：`-c` 下载的根证书用于面板可达性检查、Agent 下载、卸载脚本下载及 Agent 运行。根证书下载地址本身必须可由既有信任库验证；首次下载脚本时也需要信任面板证书。可先安装受信的根证书或使用 curl 的 `--cacert` 下载脚本，禁止用 `-k` 关闭验证。

Red Hat 等启用 SELinux 的系统，脚本在启动服务前使用 `restorecon` 恢复程序、服务单元、环境文件和状态目录标签；缺少标签工具时按需安装 `policycoreutils`。自定义 SELinux 策略仍需实机验证。网络端口和防火墙按部署范围配置：普通出口的 TCP 与 `-D` UDP 监听分别放行，IP 证书申请与续签还要求公网 TCP/80 可达。

## 已有验证与实机验收

本地隔离回归执行真实 Bash 脚本，将系统目录映射到临时目录，并模拟包管理器、systemd、SELinux 和 ACME。覆盖 Debian、Ubuntu、Linux Mint、RHEL、Rocky、AlmaLinux、CentOS、Fedora、Arch、Manjaro 的识别和依赖分支，以及 yum 回退、旧 Python、venv 修复、私有 CA、架构、原生 UDP 出口、服务不可用和包安装失败。原有 IP 证书/续签/卸载回归也保留。

```bash
python3 scripts/test_agent_installer.py -v
python3 scripts/test_agent_certificates.py -v
bash -n internal/agentdist/agent-install.sh
shellcheck internal/agentdist/agent-install.sh
go test ./internal/agentdist
```

这些检查已接入仓库发布 CI，执行结果见 v0.1.38 对应的 GitHub Actions 记录。本地模拟分支和 Ubuntu CI 不等同于各发行版的真实软件源、systemd、SELinux、ACME 和网络验收。

实际验收应在各系列独立测试机执行，并保留以下记录：

1. `cat /etc/os-release`、`uname -m`、`systemctl show --property=Version`、相关包版本及仓库状态。
2. 普通入口接入后，`systemctl is-enabled tfp-agent`、`systemctl is-active tfp-agent`、面板节点注册和真实 TCP/UDP 收发；重启机器后再次检查。
3. 自备证书出口接入后，注册 Agent 与出口服务都运行，TLS/QUIC 认证和回包正常；检查 TCP 与 UDP 的监听/防火墙规则。
4. IP 证书模式验证 Python 版本、证书申请、续签 timer 与证书轮换。Red Hat 在 SELinux enforcing 状态下检查 `ls -Z`、服务日志及 AVC 记录。
5. 重装后保留原节点身份；卸载停止服务且默认保留状态和证书。任何会修改真实节点状态的验收均使用隔离测试账户和测试机。

目前开发环境为 Windows，未提供上述发行版的测试机，本次没有声称完成全发行版实机安装。
