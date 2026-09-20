# Go Agent 与独立出口

本实现使用项目自己的 v1 数据面协议，不兼容 nyanpass 节点协议。入口 Agent 从控制面注册并拉取配置；出口服务由管理员用本地证书、共享身份凭据和精确目标白名单启动。出口不会成为任意目标公开代理。

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
| `tls` | `exit.example.com:9443` | 入口与出口 TLS 1.3 |
| `ws` | `ws://exit.example.com:9443/tunnel` | WebSocket 二进制流内 TLS 1.3；外层 Upgrade 元信息可见 |
| `wss` | `wss://exit.example.com:9443/tunnel` | WebSocket 外层 TLS 1.3 |
| `http` | `exit.example.com:9443` | HTTP/1.1 CONNECT `/tunnel` 后内层 TLS 1.3；CONNECT 元信息可见 |

所有隧道均支持 TCP 和 UDP，UDP 使用明确报文帧；由于承载基于 TCP，UDP 仍存在队头阻塞。客户端到入口及出口到目标没有自动加密；业务协议自身是否加密独立判断。CONNECT 仅用于本产品入口到出口协议，尚不承诺兼容任意第三方 HTTP/CDN 代理。

## 执行、计量与故障语义

- 配置寿命不超过 24 小时，并且每次转发都检查租约有效期及剩余额度。配置/租约过期不新发数据；控制面每批 5 分钟/16 MiB 等预算以实际配置为准。
- 新配置先完整验证并预绑定新增监听器，再持久化并替换；失败保留旧配置。相同配置轮询不主动断连；规则或租约更换会关闭旧连接，客户端需要重连。
- 本版原始计量为进入转发发送缓冲前的业务有效载荷字节，上行/下行分别记录。不含隧道头；远端写入失败仍可能计入已接收的字节，不声称等同于最终送达。未知是否送达不能通过重启回退预算。
- 每次发送前把扣减和 usage 事件同步落盘；服务端只确认已经持久化的事件。相同事件可重放，未确认数据重启保留。待确认事件最多 4096 条；满额、落盘失败即拒绝后续转发。磁盘故障后需要修复并重启进程。
- 停用/替换、到期或预算不足时，先持久退休标记并停止旧租约使用；上报全部未确认计量后调用 `POST /api/v1/agent/leases/retire`，`used_bytes` 为该租约累计原始字节。服务端幂等退还未用配额，之后拉取新租约。退出进程不立即退租，避免把可能还需恢复的状态当成未使用。
- TCP 半关闭保留，单方向缓存 32 KiB。每入口规则最多 256 TCP 连接或 UDP 会话；UDP 空闲回收 30 秒，TCP 单方向空闲 2 分钟。出口最多 256 隧道会话。会话切换与目标错误不提供无损迁移。
- 协议屏蔽只实现首段明文 HTTP 方法和 SOCKS4/5 前缀识别；未知流量允许。不是 DPI 保证，TLS/HTTPS 密文内容和 URL 路径不会被解密识别。`fet` 等未知检测器拒绝应用；控制面负责合并组与规则限制。

当前同步落盘策略优先保证有限配额及崩溃恢复，不是高吞吐优化版本；上线容量需以真实机器基准验收。租约历史去重元数据随租约数量增长，需要后续保留/压缩策略；待确认流量 spool 有硬上限。多出口链、反向连接、Mux、SNI 共享端口、远程升级、全局带宽调度尚未实现。

## 探针

实际采集 gopsutil 提供的 CPU、内存、磁盘、系统负载、在线时间与网卡计数。速率为相邻样本字节差除以间隔，单位 B/s；排除回环接口，但虚拟网卡/网桥可能产生重复统计，因此不能直接用于账单。首样本、计数回绕、接口集合变化、平台不支持的指标返回 null。容器部署显示容器能够看到的系统范围，不保证宿主机全貌。

默认不调用任何第三方公网 IP 服务。可显式配置运营方控制的 HTTPS 纯 IP 回显服务，每 10 分钟观察一次：

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

`internal/integration` 使用真正的 `app.New`、SQL 数据库、HTTP 接口、Agent 与本地出口，不替换控制面处理器。默认 SQLite，也可经 `TFP_TEST_DRIVER` / `TFP_TEST_DSN` 使用有创建临时数据库权限的测试服务器。覆盖注册、管理员和用户授权、签名支付回调幂等、购买、四承载各 TCP/UDP 原始字节结算、部分租约归还与新租约、即时续费新周期、撤权、删除 ACK 释放端口、断联期间持久计量与重启补传、命名空间组策略实际阻断。支付回调是本地有效签名 fixture，没有连接商户支付平台或发生真实付款。生产双机网络、商户实付与容量目标仍需独立验收。Linux amd64/arm64 已以 `CGO_ENABLED=0` 交叉编译；这不等同于对应机器上的运行验收。
