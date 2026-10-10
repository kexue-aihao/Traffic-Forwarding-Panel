# 高级设置实施交付说明

日期：2026-10-10。实施依据：[已审批技术方案](advanced-settings-implementation-plan.md)，基线 v0.1.38。14 个参数已接入控制面编译和数据面执行，纳入 [v0.1.39](releases/v0.1.39.md)；本地测试覆盖直连入口、正向出口和反向服务。用户已授权提交、推送和版本发布。Linux、PostgreSQL/MySQL 功能回归由发布 CI 执行；Linux 跨机性能与各发行版安装实机仍待隔离验收。

## 运行链路与参数覆盖

| 参数 | 已实现行为 | 主要证据 |
| --- | --- | --- |
| `allowed_host`、`blocked_host` | 名称规范化，HTTP Host/TLS SNI 检查；跨组允许列表取交集，拒绝列表合并 | policy 单元测试，直连/隧道真实报文，出口独立检查 |
| `blocked_path` | 明文 HTTP/1 按请求边界、h2c 按 HPACK HEADERS 检查；编码与歧义路径拒绝 | 同连接后续请求、正文/分块、h2 后续 stream 测试 |
| `blocked_protocol` | HTTP/1 扩展方法、h2c、SOCKS4/5；TLS HTTP ALPN 保守识别；UDP 保留明确的首部检测 | TCP/UDP 检测及 HTTP/WS 外层载波兼容回归 |
| `tls_inbound_policy`、`tls_reject_empty_sni` | TLS 原样透传；TLS-only 入口撤销 UDP，模式2限制非管理员独立端口 | 真实 TLS 握手、空 SNI、明文拒绝及控制面权限检查 |
| `disable_udp`、`udp_over_tcp` | 禁用入口和相关出口 UDP 并撤销旧会话；有出口时强制 TCP 载波，直连仍使用 UDP | 两个 Agent 的 QUIC→TCP→禁用→恢复测试 |
| `ipv6_group` | 连接授权组对端时优先 IPv6，含正向、链式和反向连接；TCP 地址竞速回退 | IPv6 loopback 真实 socket、域名证书校验与 IPv4 回退 |
| `max_fail`、`fail_timout_sec` | 加权候选、连续失败阈值、冷却、单次半开；出口目标/下一跳也执行状态机 | 真正失败连接、备用目标、冷却期间无 origin Accept、恢复 |
| `reverse_group`、`protocol` | hub 先准备并 ACK；出口主动连接；两端就绪后激活路线；四种载波、重连与撤销 | ServiceManager 四载波测试及控制面真实双 Agent 测试 |
| `tls` | TLS1.3、SNI、CA、ALPN、服务器/客户端证书 profile；同端口证书更新 | 错误 CA/SNI/ALPN/mTLS 拒绝、TCP/QUIC 证书轮换测试 |

实现代码集中在 `internal/contract/group_policy.go`、`internal/policy`、`internal/netx`、`internal/platform/group_policy_compile.go`、`managed_services.go`、`internal/agent/service_manager.go`、`failover.go` 和 `internal/tunnel/group_policy.go`。界面增加完整策略激活、草稿影响预览、托管出口/hub 设置和规则运行状态。

## 升级和激活

1. 升级入口和出口 Agent，确认节点声明完整策略能力。已激活规则遇到能力不足会撤销下发，并在 `blocked_rules` 中说明原因。
2. 在“设备组 → 高级设置”编辑参数并预览影响。存量配置不自动开启；勾选“启用完整高级策略”才提交 `policy_version=2`。
3. 出口组启用完整策略前，迁移到普通 Agent 托管出口。入口策略可用于直连或已有隧道；完整出口策略要求托管服务，避免出口独立检查缺失。
4. 保存后在规则诊断查看配置 ACK、策略 hash、实际载波、地址族、候选和拒绝连接数；反向路线还要检查出口列表中的服务就绪状态。

旧配置 `policy_version=0` 保留历史的应用屏蔽、UDP 禁用和 UDP over TCP 行为。其他完整策略不随升级静默激活。非法 Apply 在准备阶段失败，保留当前监听和服务配置；没有加载本地 profile 会给出配置错误。

## 本地服务 profile

监听许可使用精确地址，包括 IPv6 地址的方括号形式。文件只放在节点，以下路径是 Linux 部署示例：

```json
{
  "reverse-listener": {
    "certificate": "/etc/tfp-agent/tls/fullchain.pem",
    "private_key": "/etc/tfp-agent/tls/private.key",
    "allowed_listen": ["0.0.0.0:9443", "[::]:9443"]
  },
  "private-ca": {
    "ca": "/etc/tfp-agent/tls/ca.pem"
  },
  "reverse-client": {
    "certificate": "/etc/tfp-agent/tls/client.pem",
    "private_key": "/etc/tfp-agent/tls/client.key"
  }
}
```

普通 Agent 使用 `-service-profiles /etc/tfp-agent/service-profiles.json`。接入脚本支持：

```bash
bash agent-install.sh -t '<设备组接入密钥>' -u 'https://panel.example.com' \
  -F /root/service-profiles.json
```

存在旧 `tfp-exit.service` 时，使用 `-M -F /root/service-profiles.json` 显式迁移；脚本停用旧出口服务，复制 profile 为0600，systemd 启动普通 Agent 并传入 profile 参数。新服务必须具备相同端口的本地许可。Debian/Ubuntu、RHEL 系与 Arch 使用同一服务参数。

profile 标签映射在 Agent 启动时加载；修改标签、路径或监听许可后重启 Agent。证书、私钥和 CA 文件在配置轮询时重新加载，内容变化会切换服务代次；TCP/QUIC 无需先移除端口。轮换时旧业务会话被撤销并重连。无效证书会导致新配置失败，当前代次继续工作。私有 CA 应同时配置业务入口的信任，正向出口连接沿用 Agent 的 `-ca`；反向 hub 的本地连接使用该 hub 准备的 TLS policy。

## 配置正向和反向服务

正向出口在“出口管理”选中“由普通 Agent 托管服务”，选择出口组与节点，填写本地 profile、监听地址、对外端点、TLS 名称与凭据。原生 UDP 另配置 UDP 端点，其绑定地址也必须出现在本地许可中。QUIC 仍有独立的 TLS ALPN 与报文数据通道。

反向配置：

1. 入口节点加载监听证书 profile，建立托管出口资源并选中“作为入口组的反向 hub”，选择入口设备组和对应节点。
2. 出口节点加载本地 profile。建立普通托管出口资源作为该节点的关系身份资源；反向模式只启动主动 connector，不绑定此资源的正向监听。
3. 出口组高级设置开启完整策略，`reverse_group` 选择入口组，`protocol` 与 hub 的承载一致。
4. 设置 TLS 对象。`certificate_profile` 指向 hub 的服务器证书标签；`ca_profile` 在两端应指向可验证 hub 的 CA；需要 mTLS 时，hub 本地 profile 设置 `require_client_certificate:true`，并通过 `client_certificate_profile` 给入口本地客户端和出口 connector 提供客户端证书。
5. 等待 hub 监听就绪、connector 主动连接就绪，再使用该出口组建立规则。

TLS 示例：

```json
{
  "enabled": true,
  "server_name": "reverse.example.com",
  "min_version": "1.3",
  "alpn": ["tfp-reverse-v1"],
  "ca_profile": "private-ca",
  "certificate_profile": "reverse-listener",
  "client_certificate_profile": "reverse-client"
}
```

未使用 mTLS 时省略 `client_certificate_profile`。`enabled=false` 撤销关系，不会降低为明文。显式配置 ALPN 时，两端必须成功协商该值。一个物理 hub 监听只能使用一套 TLS policy；多个出口组给同一 hub 配置不同 TLS policy 会被阻断并返回 `reverse_hub_tls_policy_conflict`。`tls_simple` 使用专用关系端点，保留 yamux；它是本项目定义，不声称与未知参考协议互通。

关系凭据由控制面按出口、hub 和资源版本派生，载波凭据与每规则业务凭据分开。业务请求同时验证规则 ID、网络和目标，跨规则复用凭据会拒绝。停用账号、调整身份组/组授权、删除规则/资源或改变策略后，相关服务授权会更新，旧会话撤销。服务配置最多有效5分钟且受业务租约截止限制，节点离线不能无限沿用权限。

## 边界和实施取舍

* Host/SNI 与 Path 针对可见 TCP 业务；HTTPS Path、ECH 内层 SNI、HTTP3、DTLS 不解密。UDP 应用识别只检查公开首部。
* 直连 UDP 没有远端解封装端点，`udp_over_tcp` 不适用。反向与链式 UDP 使用 TCP 载波，原生 QUIC 覆盖普通单出口。
* IPv6 组只影响已授权节点对端的地址选择，不改变出口组、计费倍率或目标权限；外部直连目标没有组映射时不应用该列表。
* 候选限定同出口组、同倍率、同有效策略，最多16项；权重更新和候选新增保留业务连接，候选删除或路由/授权变化关闭现有会话。业务数据发送后不重播。
* 配置发布当前使用所有已接入节点的保守依赖集合；正确覆盖反向与链式撤销，代价是相关操作会引起更多节点轮询应用。可后续优化为精确依赖索引。
* 拒绝计数在内存汇总，经 ACK 上报；包括初始策略拒绝和后续 HTTP 策略拒绝，解析错误另按连接关闭处理。计数不是长期持久统计。
* 未配置入站策略的 UDP 数据路径继续使用现有原生快速路径；未增加逐包 SQL/WAL 或协议解析。5% 性能预算必须通过专门测量验收。

## 验证和待验收项

本地验证使用 Windows、Go 1.26.9、SQLite、真实 loopback TCP/UDP/IPv6 socket 和两个 Agent。覆盖策略允许/拒绝/修改/恢复、计量回归、证书与授权撤销；安装器使用临时文件系统和模拟包管理器/systemd。模拟安装通过不代表发行版实机安装已经通过。

| 已执行检查 | 结果 |
| --- | --- |
| `go test ./...` | 全量通过；最后的服务续期与能力依赖修改另完成 Agent/platform/integration 回归 |
| policy/netx/Agent/tunnel/platform/integration 的 `-race` | 全部通过；服务续期、证书、候选连续性、能力更新和反向集成另完成最终版本专项 race |
| `go vet ./...`、OpenAPI 生成、`git diff --check` | 通过 |
| 前端 typecheck、生产构建 | 通过；保留已有 `inter.woff2` 构建解析提示 |
| `npm test`、`npm run test:live` | 两套测试均在 Chromium、Firefox、WebKit 通过 |
| 安装器 Python 测试、Bash 语法检查 | 17 项通过；新加 profile 安装和显式旧出口迁移场景 |

可复跑命令：

```bash
go test ./...
go test -race ./internal/policy ./internal/netx ./internal/agent ./internal/tunnel ./internal/platform ./internal/integration
go vet ./...
go run ./internal/openapi/generate
cd web
npm run typecheck
npm run build
npm test
npm run test:live
cd ..
python -m unittest discover -s scripts -p test_agent_installer.py
bash -n internal/agentdist/agent-install.sh
git diff --check
```

三浏览器测试覆盖 Chromium、Firefox、WebKit 的完整策略激活、预览、旧配置兼容，以及托管 hub 的表单保存与回读。托管服务的实际建连证据来自 Go socket 集成测试，浏览器中的模拟节点心跳不作为执行证据。

外部验收仍需要：

| 项目 | 执行方式 |
| --- | --- |
| PostgreSQL、MySQL | 使用仓库 `internal/testdb` 的 `TFP_TEST_DRIVER`、`TFP_TEST_DSN` 接口，在隔离测试库运行 platform/integration 与 race；不要指向生产库 |
| Linux 安装/运行 | Debian、Ubuntu、RHEL/Rocky/Alma、Arch 实机或虚拟机验证 profile 权限、证书加载、systemd、双栈、SELinux、防火墙和显式迁移 |
| UDP 性能预算 | 按 [UDP 隔离验收手册](udp-performance-lab.md) 与 [UDP 性能计划](udp-performance-plan.md)，在相同硬件和拓扑比较直连、QUIC 与旧版本；重复采样 throughput/PPS/CPU/P99 |
| HTTP/反向稳定性 | 在跨机环境报告首包 P50/P95/P99、HTTP RPS/缓冲上界、断线恢复、长期 FD/goroutine/内存；保留有限租约和真实用量上报 |

本地环境没有 Linux VM、PostgreSQL/MySQL 和跨机测试网络；发布 CI 补充 Linux 与三库功能回归，最终结果见 v0.1.39 的 GitHub Actions 记录。尚不能宣布生产性能达标或完成 P5 全部验收。
