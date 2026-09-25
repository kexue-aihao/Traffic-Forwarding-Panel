# Go Agent 与独立出口

本实现使用项目自己的数据面协议：单出口使用 v1，多出口链使用 v2，不兼容 nyanpass 节点协议。入口 Agent 从控制面注册并拉取配置；出口服务由管理员用本地证书、共享身份凭据和精确目标白名单启动。出口不会成为任意目标公开代理。

## 一键接入设备

面板自己托管 Agent 产物与接入脚本，所以接入一台设备只需要一条命令 —— 不需要
在目标机上装 Go、也不需要事先把二进制拷过去。

```bash
bash <(curl -fLsS https://panel.example.com/download/agent-install.sh) \
     -t '<接入凭据>' -u 'https://panel.example.com' [-n '<设备名>']
```

脚本按本机架构从**同一个面板**下载 Agent，装到 `/usr/local/bin/tfp-agent`，
写一个 systemd 服务（开机自启、崩溃重拉），启动后确认注册成功。要求目标设备
是 Linux、有 systemd、以 root 执行。

### 三种接入方式

控制台的「设备组 → 接入设备」按场景给不同的命令。三者的差别不在于装哪个二进制
—— 都是同一个 Agent —— 而在这台设备之后扮演什么角色：

| 方式 | 设备角色 | 建规则时 | 命令里多出来的参数 |
| --- | --- | --- | --- |
| 入口直出 | 入口 | 出口选择留空 | 无 |
| 入口 | 入口 | 选一条出口 | 出口用私有 CA 时加 `-c <根证书地址>` |
| 隧道 | 出口（隧道端点） | —— | `-m exit` 与证书、令牌、白名单 |

**入口直出**把流量直接发往目标，这一段不额外加密；要让这一段也加密，把规则的承载
改成 `direct-tls`（见上面的承载表）。

**入口**把流量交给出口隧道，入口到出口全程 TLS 1.3。

**隧道**把设备接成出口端点，命令形如：

```bash
bash <(curl -fLsS https://panel.example.com/download/agent-install.sh)      -t '<设备组接入密钥>' -u 'https://panel.example.com'      -m exit -S 'exit.example.com' -e '<出口令牌>'      -C /etc/ssl/exit.crt -K /etc/ssl/exit.key      -w 'tcp|10.20.0.11:27015,udp|10.20.0.11:5353'
```

它会装**两个** systemd 服务：`tfp-exit`（隧道端点本身）和 `tfp-agent`（注册用的
Agent）。出口服务不跟面板通信，没有第二个服务，设备不会出现在控制台里、出口列表的
「在线」列就永远是离线。脚本会在装之前用 openssl 校验证书没过期、且覆盖 `-S` 给的
域名 —— 证书不匹配的话入口侧握手会失败，那时候排查要跨两台机器。

证书与私钥由你自己的 CA 签发，面板不代为签发也不保存私钥。装完还要到控制台
「出口管理 → 新增」登记一条记录，承载、端点、服务名与令牌都要和命令里的一致；
入口规则选这条出口后流量才会真正走隧道。

### 两种接入凭据

| | 设备组接入密钥（推荐） | 一次性接入令牌 |
| --- | --- | --- |
| 在哪生成 | 设备组 → 「接入设备」 | 服务器 → 「生成接入凭据」 |
| 有效期 | 长期不变 | 15 分钟 |
| 可用次数 | 不限，同组设备共用一条命令 | 一次 |
| 设备名 | 由设备自报（默认 hostname） | 由控制台指定，命令带 `-n` |
| 撤销方式 | 轮换密钥，已分发命令立即失效 | 自动作废 |

日常接入用**设备组接入密钥**：命令可以一直留着，装失败、断线、重装都直接再跑
一遍，不必回控制台。它的粒度是「允许接入本组」，不是某台机器的身份 ——
任何拿到这条命令的人都能往该组加节点，所以它出现在不该出现的地方时要立刻轮换。

一次性令牌适合「预先定好节点名、只接一台」的场景。

**两种凭据都只决定节点加入哪个组**，而组决定它拿得到哪些配置。首次注册成功后
Agent 把节点身份持久化到状态目录（默认 `/var/lib/tfp-agent`），之后重启不再需要
接入凭据 —— 所以那个目录必须保留，删除它等于让本机以新身份重新注册。

### 面板侧需要准备什么

面板默认从**自身可执行文件所在目录**发布 Agent。容器镜像已经把 `/agent` 放在
`/panel` 旁边，因此开箱即用；镜像另外放了一份 `agent-linux-<arch>` 作为显式
平台名。

要接入与面板不同架构的设备（例如面板跑 amd64、设备是 arm64），把对应产物放进
一个目录并按 `agent-linux-arm64` 命名，然后设置 `TFP_AGENT_DIR`（或
`-agent-dir`）指向它：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o /srv/tfp-agents/agent-linux-arm64 ./cmd/agent
```

面板用 `GET /download/agent/{os}/{arch}` 提供产物，用
`GET /download/agent-install.sh` 提供脚本，两者都**免登录** —— 接入命令在目标
设备上执行，那里没有会话。产物缺失时返回 404 并说明缺的是哪个平台，脚本会在
安装前校验下载结果的 ELF 魔术字节，把这种失败挡在「装出一个无限重启的服务」
之前。

### 卸载与重来

用同一个脚本加 `-x` 卸载：停止并删除服务，保留状态目录。要让本机以全新身份
重新接入，在卸载后自行删除状态目录，再跑一次接入命令。

### 出口节点

出口已纳入一键接入（见上面「三种接入方式」的**隧道**），脚本会顺手把证书有效性
与域名覆盖检查做掉。链式隧道里 A、B 两跳还需要各自的 `next-hops.json` 授权下一跳，
那一份仍然要手工放置 —— 下面「最多三个出口的链式隧道」有完整示例，`-next-hops`
参数由 `-mode exit` 直接启动时使用，安装脚本不代管。


## 构建与运行

要求 Go 1.26；命令从仓库根目录执行。下面环境变量命令使用 PowerShell，Linux 用对应的 `export`。凭据使用实际新建值，不把真实密钥提交到仓库。

```powershell
go build -o ./agent.exe ./cmd/agent
$env:TFP_ENROLLMENT_TOKEN = '<控制面一次性节点注册 token>'
./agent.exe -panel https://panel.example.com -name entry-01 -state ./private/agent-state.json
```

首次注册成功后持久保存节点身份，后续启动不再需要一次性 token。`-ca ./ca.pem` 向系统信任库增加私有 CA，同时用于面板 HTTPS 和隧道验证。仅本机回环面板 URL 允许 HTTP。不提供跳过证书校验选项。状态文件必须保留；**不能复制同一节点状态运行多个实例，不能删除状态后重用旧租约**。进程锁禁止本机两个实例共用相同状态路径；文件存储依赖本地可靠文件系统，不支持多机共享文件作为高可用。

独立出口示例（需要已签发、含 `exit.example.com` SAN 的有效证书）：

```powershell
$env:TFP_EXIT_TOKEN = '<至少16字符的随机出口凭据>'
./agent.exe -mode exit -listen 0.0.0.0:9443 -transport tls -cert ./exit.crt -key ./exit.key -allow 'tcp|127.0.0.1:8080,udp|127.0.0.1:5353'
```

出口 `-transport` 可取 `tls`、`ws`、`wss`、`http`。每个监听器选择一种承载；多个承载可运行多个出口进程。`-allow` 精确匹配 `network|host:port`，无通配符；DNS 目标由出口解析，应仅填入运营方控制的地址。证书、私钥、出口 token 均由本地运维配置，不经用户列表接口分发。

控制面 Rule 示例（真实 ID、组和租约由控制面分配）：

```json
{
  "name": "example",
  "node_id": "NODE_ID",
  "group_id": "GROUP_ID",
  "network": "tcp",
  "transport": "tls",
  "listen": "0.0.0.0:18080",
  "target": "127.0.0.1:8080",
  "enabled": true,
  "tunnel": {
    "endpoint": "exit.example.com:9443",
    "server_name": "exit.example.com",
    "token": "SAME_EXIT_TOKEN"
  }
}
```

| 承载 | endpoint | 加密位置 |
|---|---|---|
| `direct` | 无 tunnel | 不增加加密，直接连接 target |
| `direct-tls` | 无 tunnel，可带一个 `server_name` 作校验名 | 入口与**目标**之间 TLS 1.3；没有出口，加密在对端终止 |
| `tls` | `exit.example.com:9443` | 入口与出口 TLS 1.3 |
| `ws` | `ws://exit.example.com:9443/tunnel` | WebSocket 二进制流内 TLS 1.3；外层 Upgrade 元信息可见 |
| `wss` | `wss://exit.example.com:9443/tunnel` | WebSocket 外层 TLS 1.3 |
| `http` | `exit.example.com:9443` | HTTP/1.1 CONNECT `/tunnel` 后内层 TLS 1.3；CONNECT 元信息可见 |

`direct-tls` 是唯一不使用出口的加密承载：入口先连到 target，再在同一条连接上做
TLS 握手，证书由入口按 `-ca` 装进来的信任库（系统根 + 私有 CA）校验。校验名默认取
target 的主机部分；目标是纯 IP、证书签的却是域名时，在规则的 `tunnel.server_name`
里显式给出。它只支持 TCP —— UDP 要走 TLS 得用 DTLS，那是另一套协议，本版不提供。
没有跳过证书校验的开关，与其它承载一致。

节点能力里对应 `direct-tls-v1`：未声明该能力的老 Agent 上，控制面会拒绝创建
`direct-tls` 规则，而不是把规则下发过去再让它静默失败。

所有隧道均支持 TCP 和 UDP，UDP 使用明确报文帧；由于承载基于 TCP，UDP 仍存在队头阻塞。客户端到入口及出口到目标没有自动加密；业务协议自身是否加密独立判断。CONNECT 仅用于本产品入口到出口协议，尚不承诺兼容任意第三方 HTTP/CDN 代理。

## 最多三个出口的链式隧道

支持入口 Agent → 出口 A → 出口 B → 出口 C → 最终目标，出口总数为 1–3。首出口仍使用 `tunnel.endpoint/server_name/token`；`tunnel.chain` 依次列出后续 1–2 个出口，每项增加自己的 `transport`。各跳可以混用四种承载，均执行 TLS 1.3 证书及 token 验证，TCP 半关闭和 UDP 报文边界贯穿整条链。链中的出口均需运行支持 v2 的版本。

三出口 Rule 的 `transport` / `tunnel` 部分示例：

```json
{
  "transport": "tls",
  "tunnel": {
    "endpoint": "exit-a.example.com:9443",
    "server_name": "exit-a.example.com",
    "token": "EXIT_A_TOKEN_AT_LEAST_16_CHARS",
    "chain": [
      {
        "transport": "wss",
        "endpoint": "wss://exit-b.example.com:9443/tunnel",
        "server_name": "exit-b.example.com",
        "token": "EXIT_B_TOKEN_AT_LEAST_16_CHARS"
      },
      {
        "transport": "http",
        "endpoint": "exit-c.example.com:9443",
        "server_name": "exit-c.example.com",
        "token": "EXIT_C_TOKEN_AT_LEAST_16_CHARS"
      }
    ]
  }
}
```

出口还必须在本机显式授权下一跳。A 的 `private/next-hops.json` 是仅含上述 B 对象的 JSON 数组，B 的对应文件仅含 C 对象；`transport`、`endpoint`、`server_name`、`token` 四项必须与请求完全一致。C 无需下一跳文件。文件上限 64 KiB、最多 64 个授权对象；Linux/macOS 要求文件权限 `0600`（`chmod 600 private/next-hops.json`），Windows 需由部署者限制文件 ACL。各出口使用独立随机 token，示例值仅用于说明。

在 A、B、C 三台主机上分别设置自己的 `TFP_EXIT_TOKEN` 并执行对应命令。下面最终目标是 C 本机的 `127.0.0.1:8080` / `127.0.0.1:5353`；所有出口的 `-allow` 都须包含相同最终目标字符串，只有最后一跳实际连接它：

```powershell
# A：本机 next-hops.json 授权 B
./agent.exe -mode exit -exit-id exit-a -listen 0.0.0.0:9443 -transport tls -cert ./exit-a.crt -key ./exit-a.key -ca ./ca.pem -next-hops ./private/next-hops.json -allow 'tcp|127.0.0.1:8080,udp|127.0.0.1:5353'
# B：本机 next-hops.json 授权 C
./agent.exe -mode exit -exit-id exit-b -listen 0.0.0.0:9443 -transport wss -cert ./exit-b.crt -key ./exit-b.key -ca ./ca.pem -next-hops ./private/next-hops.json -allow 'tcp|127.0.0.1:8080,udp|127.0.0.1:5353'
# C：终点出口，无下一跳
./agent.exe -mode exit -exit-id exit-c -listen 0.0.0.0:9443 -transport http -cert ./exit-c.crt -key ./exit-c.key -allow 'tcp|127.0.0.1:8080,udp|127.0.0.1:5353'
```

参与链路的每个出口必须配置稳定的 `-exit-id`（1–128 字符）。不同逻辑出口使用不同 ID；同一出口通过多个域名或监听器暴露时应保持相同 ID。入口拒绝重复规范化地址，出口通过已访问的 ID 拒绝 DNS 别名造成的回环，逐跳限制整条链不超过三个出口。单出口 v1 不要求 ID。运维配置在进程启动时读取，修改后重启生效。

所有出口都是运营方信任的中继，逐跳可见业务明文及剩余链路凭据；这不是对中间出口隐藏载荷的端到端加密。每次连接仅在全链授权并连接最终目标后就绪，默认连接/握手等待上限为 10 秒。证书、token、白名单、目标连接失败或中继断开会关闭该链路，不自动重试或切换出口。上行/下行仅由入口 Agent 对业务有效载荷记账；中间出口及终点出口不创建租约、不重复计费，隧道帧头不进入计费。

## 网络诊断（LookingGlass）

控制台的「网络诊断」页可以让节点执行 ping、tcping、mtr —— 用来回答「从入口机器
看过去，目标到底通不通」，这从面板本机 ping 是看不出来的。

Agent 声明 `looking-glass-v1` 后即自动参与，**不需要 `-enable-terminal`**：这条
通道与节点运维里的远程终端是两件事。终端是一个不受限的 shell（默认关闭、每次
还要二次授权）；这里只有三种方法、参数结构化，面板校验后把主机名作为 **argv
的独立元素**交给 exec，全程不经过 shell。

- `tcping` 由 Agent 自己完成 TCP 握手（四次，逐个计时），**不依赖节点上装了
  tcping**；`ping` 与 `mtr` 调用系统命令，缺失时会明确说「节点上没有 mtr」。
- 单次诊断上限 30 秒、输出上限 32 KiB，超出即截断 —— 节点上的网络命令不该把
  Agent 挂住，也不该把面板撑爆。
- 请求两分钟内有效；同一个节点同时只跑一条。结果只写一次，重放会被拒。

## 执行、计量与故障语义

- 配置寿命不超过 24 小时，并且每次转发都检查租约有效期及剩余额度。配置/租约过期不新发数据；控制面每批 5 分钟/16 MiB 等预算以实际配置为准。
- 新配置先完整验证并预绑定新增监听器，再持久化并替换；失败保留旧配置。相同配置轮询不主动断连；规则或租约更换会关闭旧连接，客户端需要重连。
- 本版原始计量为进入转发发送缓冲前的业务有效载荷字节，上行/下行分别记录。不含隧道头；远端写入失败仍可能计入已接收的字节，不声称等同于最终送达。未知是否送达不能通过重启回退预算。
- 每次发送前把扣减和 usage 事件同步落盘；服务端只确认已经持久化的事件。相同事件可重放，未确认数据重启保留。待确认事件最多 4096 条；满额、落盘失败即拒绝后续转发。磁盘故障后需要修复并重启进程。
- 停用/替换、到期或预算不足时，先持久退休标记并停止旧租约使用；上报全部未确认计量后调用 `POST /api/v1/agent/leases/retire`，`used_bytes` 为该租约累计原始字节。服务端幂等退还未用配额，之后拉取新租约。退出进程不立即退租，避免把可能还需恢复的状态当成未使用。
- TCP 半关闭保留，单方向缓存 32 KiB。每入口规则最多 256 TCP 连接或 UDP 会话；UDP 空闲回收 30 秒，TCP 单方向空闲 2 分钟。出口最多 256 隧道会话。会话切换与目标错误不提供无损迁移。
- 协议屏蔽只实现首段明文 HTTP 方法和 SOCKS4/5 前缀识别；未知流量允许。不是 DPI 保证，TLS/HTTPS 密文内容和 URL 路径不会被解密识别。`fet` 等未知检测器拒绝应用；控制面负责合并组与规则限制。

计量使用 append WAL 和有界 group commit，发送前等待该批次 fsync 成功；上线容量仍需以真实机器基准验收。租约历史去重元数据随租约数量增长，checkpoint 达到 64 MiB 上限将停止转发，需要后续保留/压缩策略；待确认流量 spool 有硬上限。反向连接、Mux、SNI 共享端口和受控远程升级已实现，仍需 Linux 跨机、公网和断电演练验收。

反向出口用 `-mode reverse-exit -reverse-endpoint host:port -server-name exit.example.com -exit-id exit-a -allow reverse|exit-a` 启动，入口把规则的 `tunnel.reverse` 设为稳定 ID；主动载波断开后按 1 秒退避重连。`tunnel.mux=true` 复用同一出口凭据的 yamux 载波，每条载波最多 256 条流、连接池最多 64 条。

远程终端和节点升级默认关闭。终端需要 Linux 服务账号启动并声明 `terminal-v1`：`./agent -enable-terminal ...`。升级需要公钥文件：`./agent -release-key ./release-key.b64 ...`。发布签名覆盖版本、平台、架构和 SHA-256；替换后新进程必须在 60 秒内完成配置 ACK/探针健康标记，否则恢复旧二进制。

## WAL、备份与恢复

持久化格式版本为 1。`agent-state.json` 是带 sequence 与 CRC32C 的 checkpoint；`agent-state.json.wal` 是追加事件日志，文件头包含版本 magic、基准 sequence 和 CRC32C，记录包含长度、单调 sequence、头部 CRC32C、头部与载荷联合 CRC32C、JSON 事件。事件分别记录节点身份、配置、原始计量、服务端确认、退休和退休确认。旧版单 JSON 文件在首次打开时一次性迁移，保留全部 pending、used 与 retired 状态。

- 队列最多 256 请求；计量批次最多 128 笔，最多等候 1 ms 聚合。只有整批写入并 fsync 后，`Charge` 才允许调用者发送。超额度、队列满、spool 满或落盘错误不能通过继续写内存绕过。
- WAL 达 8 MiB 后创建 checkpoint：写临时文件 → fsync → rename → Unix 上目录 fsync；然后写带新基准 sequence 的新 WAL → fsync → rename → Unix 上目录 fsync。始终先完整保存 checkpoint，再替换 WAL。尚未 ACK 的事件也包含在 checkpoint，不能因轮转而丢弃。
- 恢复仅裁掉可确认的末尾不完整写入。完整记录的 CRC 错误（包括最后一条）、中间记录损坏、sequence 错误、头部错误、缺失必需 WAL 都停止启动，避免把损坏误判成“没用过额度”。完整但调用者未收到成功的记录仍保守计费，符合发送缓冲计量口径。
- **停 Agent 后备份完整状态目录**，同时保留 checkpoint 与 `.wal`；不要只备份 JSON。`.tmp` / `.wal.next` 是未完成替换的暂存文件，正常恢复依据正式 checkpoint 与 WAL，不能自行用旧文件覆盖新文件来解除额度限制。不要手工截断完整损坏记录；应从一致备份恢复并核对控制面账务。
- Windows 执行文件 fsync 与 rename，但 Go 路径未提供 Unix 式目录 fsync；实际断电与文件系统持久性保证需在目标 OS/文件系统上验收。自动化故障测试模拟写入/同步/替换边界及短写，不等同于物理断电认证。

可复现计量基准：

```powershell
go test ./internal/agent -run '^$' -bench BenchmarkDurableUsage -benchtime=500x -count=1
```

2026-09-20 Windows amd64、Intel i5-10400F、Go 1.26.5、本地测试临时目录，预置 3000 条待确认记录，每次记录 32 KiB，有 500 次操作：旧版整 JSON 写/sync/rename 单 worker 为 5.73 ms/op，WAL 为 2.04 ms/op；32 workers 旧版 5.42 ms/op，WAL 为 64.8 µs/op。旧版路径在同一基准内保留以供对照；这些数字仅反映计量持久化调用，不是实际转发带宽或生产容量。`-benchtime` 请使用不超过 1000 次，避免超过此基准保留的 4096 条 spool 上限。

## 探针

实际采集 gopsutil 提供的 CPU、内存、磁盘、系统负载、在线时间与网卡计数。速率为相邻样本字节差除以间隔，单位 B/s；排除回环接口，但虚拟网卡/网桥可能产生重复统计，因此不能直接用于账单。首样本、计数回绕、接口集合变化、平台不支持的指标返回 null。容器部署显示容器能够看到的系统范围，不保证宿主机全貌。

默认每 10 分钟在目标设备上运行固定的 `curl` 命令，通过 `https://api.ipify.org` 分别探测公网 IPv4 和 IPv6。IPv6 不可用时只上报 IPv4；命令失败不会影响 Agent。需要使用自建回显服务时，可用 `-ip-echo` 覆盖默认探测：

```powershell
./agent.exe -panel https://panel.example.com -state ./private/agent-state.json -ip-echo https://ip4.example.com/ip,https://ip6.example.com/ip -disk C:\
```

返回地址包含 IPv4/IPv6、来源 URL、时间。失败后不伪造地址；这只能证明访问该回显端点的出口地址，不代表所有业务路径相同。

## 检查

```powershell
go test ./internal/agent ./internal/tunnel ./internal/probe ./cmd/agent
go test -race ./internal/agent ./internal/tunnel ./internal/probe
go test -race -v ./internal/integration
```

四承载均在真实本机 TCP/UDP socket 上测试回显、大于单帧的数据、空 UDP 报文、TCP 半关闭、错误证书名、不受信任证书、错误出口 token 和白名单外目标。Agent 测试覆盖预算重启、防退休复活、满 spool、磁盘失败、配置冲突回滚、计量方向，以及必须确认计量后才退租。

链式测试覆盖全部 64 种三跳承载组合的 TCP/UDP 回显、跨帧数据、空/大 UDP 报文及 TCP 半关闭；在每一跳独立验证错误身份、证书及白名单拒绝；覆盖超长链、重复地址、DNS 别名回环、中继关闭后的会话回收和未完成下游握手取消。真实 Agent 入口测试验证两跳/三跳的 TCP/UDP 业务各方向仅计量一次，以及无效链配置不会替换旧配置。这些测试在同一主机的多个真实监听器上运行，不等同于生产跨机器链路或容量验收。

`internal/integration` 使用真正的 `app.New`、SQL 数据库、HTTP 接口、Agent 与本地出口，不替换控制面处理器。默认 SQLite，也可经 `TFP_TEST_DRIVER` / `TFP_TEST_DSN` 使用有创建临时数据库权限的测试服务器。覆盖注册、管理员和用户授权、签名支付回调幂等、购买、四承载各 TCP/UDP 原始字节结算、部分租约归还与新租约、即时续费新周期、撤权、删除 ACK 释放端口、断联期间持久计量与重启补传、命名空间组策略实际阻断。支付回调是本地有效签名 fixture，没有连接商户支付平台或发生真实付款。生产双机网络、商户实付与容量目标仍需独立验收。Linux amd64/arm64 已以 `CGO_ENABLED=0` 交叉编译；这不等同于对应机器上的运行验收。
