# 协议嗅探与禁用：实现、部署和验收

实施日期：2026-10-10；基于 v0.1.39 源码实施并纳入 v0.1.40 发布，保持 `GroupPolicyVersion=2`，新增 `InspectionVersion=1`。本文件说明源码支持范围和本轮测试证据，生产吞吐/PPS/P99 与 24 小时稳定性仍按 [UDP 性能验收手册](udp-performance-lab.md) 在 Linux 跨机环境验收。

## 配置与行为

设备组高级配置显式启用检测版本，面板只保存本地凭据标签：

```json
{
  "policy_version": 2,
  "blocked_protocol": ["socks5", "shadowsocks", "vmess"],
  "inspection": {
    "version": 1,
    "profiles": ["controlled-ss", "controlled-vmess"],
    "mode": "strict",
    "unknown": "allow"
  }
}
```

`strict` 要求节点能力与部署前提齐全，然后只阻断满足声明证据的协议匹配。缺能力、profile、相应网络或观测位置会阻止路线编译/配置准备，并显示具体原因。使用未知凭据的某条流仍可能是未知，不能把静态准备成功解释成识别任意第三方加密流量。

`observe` 仅记录，不阻断选中协议；仅观察候选的凭据、scope、网络或本地关联拓扑前提缺失时，跳过候选/关联证明并报告有界 `unavailable`，不影响无关严格策略的配置。同一候选参与严格禁用时仍强制校验；本地 JSON 格式、文件权限和业务适配本身的错误仍须修正。`observe` 与 `unknown=deny` 的矛盾组合不接受。`unknown=deny` 是独立的未知业务访问策略，可能影响普通未知业务和不透明 TLS，不计为四种代理协议的认证识别。

规范名称为 `http/socks4/socks5/shadowsocks/trojan/vmess`，历史 `socks` 保持 SOCKS4+5 联集，兼容 `app:` 输入。历史顶层裸 `http` 的载波语义保持不变。存量 v2 配置不会因新增检测版本失活；存量 UDP SOCKS 检测修正为完整 RFC1928 首部，不再以普通报文第一个字节为 4/5 就拒绝。

## 本地检测凭据

Agent 参数 `-inspection-profiles /etc/tfp-agent/inspection-profiles.json` 读取版本化私有 JSON：

```json
{
  "version": 1,
  "profiles": {
    "controlled-ss": {
      "protocol": "shadowsocks",
      "method": "aes-128-gcm",
      "password": "replace-locally",
      "targets": ["127.0.0.1:8388"]
    },
    "controlled-vmess": {
      "protocol": "vmess",
      "method": "aead",
      "uuid": "0581b063-cc43-4719-bb46-736235a2c3e9"
    },
    "controlled-trojan": {
      "protocol": "trojan",
      "password": "replace-locally"
    },
    "explicit-udp-structure": {
      "protocol": "socks5",
      "udp_mode": "structural"
    }
  }
}
```

以上均为示例占位值。Unix 文件必须是普通文件且仅所有者可访问，例如 `0600`；Windows 由本地 ACL 限制。最大 1 MiB/256 个标签，每条检测计划最多 16 项凭据，支持 `rule_ids/group_ids/targets` 限定用途。SS2022 使用精确长度的 base64 `key`，不用 password；AES 多用户支持 2–4 个冒号分隔 PSK。Trojan 的 SHA224 hash 也是可用认证凭据，应与 password 一样保密。

面板注册/ACK 只接收标签、协议、变体、网络和准备状态；不接收密钥、UUID、密码、摘要或私钥路径。ACK 明确空数组撤销准备状态并重新发布受影响配置。Agent 每轮准备读取本地配置，内容改变生成不对外暴露的本地代次；旧连接/服务状态随代次撤销。加载/准备失败保留合法当前配置，不提交部分规则。

## SOCKS5 UDP 的受控关联

显式结构模式仅凭完整 UDP 首部，可能与普通数据碰撞。较强模式可以配置本地 profile：

```json
{
  "protocol": "socks5",
  "udp_mode": "associated",
  "control_rule_ids": ["controlled-socks-tcp-rule"],
  "relay_endpoint": "203.0.113.10:1081",
  "rule_ids": ["controlled-socks-udp-rule"],
  "targets": ["192.0.2.20:1081"]
}
```

只支持同一个入口 Agent、同一账号下的明确 TCP/UDP 规则关系与固定受控 SOCKS 服务；UDP 监听必须是精确的非通配数字 IP:port，TCP/UDP 目标必须是同一明确配置的服务主机。服务实际成功返回的数字 BND 必须等于入口公开 UDP 端点，不自动改写服务响应。透明双向 observer 在 greeting、方法选择、认证、UDP ASSOCIATE 和成功 BND 真正发送后才注册关联；每报以真实源 IP/端口、规则、精确目标和策略/profile 代次复查私有证明。TCP 关闭/半关闭、撤销和超时立即失效，关联证据是会话与结构证据，不冒充密码学认证。

无关联报文保持未知，执行独立未知策略；未知首报不会阻止以后报文重新检查。registry 限总 4096、每 peer 32 项，UDP 使用精确索引，不扫全局。控制规则的 SOCKS 禁用策略仍有效，不会为建立 association 隐式放行。

域名 BND、GSSAPI、共享 TLS/业务 TLS/WS、PROXY 或负载均衡控制关系等未支持组合不能建立有效关联。严格模式拒绝准备实际目标切换拓扑；固定目标的默认失败计数/冷却参数不会误判为目标切换。受控关联可在直连入口和 TFP TLS 入口→出口的入口观察，但跨 TFP 出口无法验证原始客户端来源，不接收入口伪造关联标签；需要出口独立部署可验证协作，或明确选择 UDP 结构模式。

## 业务 TLS 与 WebSocket

Trojan 需要受控业务 TLS 终止。TFP 自身隧道 TLS/WS 解封装不等于解除业务 TLS。高级检测对象可声明：

```json
{
  "version": 1,
  "profiles": ["controlled-trojan"],
  "mode": "strict",
  "unknown": "allow",
  "business": {
    "tls_profile": "owned-service",
    "upstream_tls_profile": "verified-origin",
    "websocket": false
  }
}
```

Agent 用 `-business-profiles /etc/tfp-agent/business-profiles.json` 读取独立于 TFP 隧道证书的本地映射：

```json
{
  "owned-service": {
    "certificate": "/etc/tfp-agent/business-cert.pem",
    "private_key": "/etc/tfp-agent/business-key.pem",
    "allowed_listen": ["0.0.0.0:443"],
    "allowed_targets": ["127.0.0.1:8443"]
  },
  "verified-origin": {
    "ca": "/etc/tfp-agent/origin-ca.pem",
    "server_name": "origin.example.com",
    "allowed_targets": ["127.0.0.1:8443"]
  }
}
```

监听与目标使用精确许可，客户端正常验证业务身份，上游重建 TLS 并验证 CA/SNI。入口→出口模式通过授权 grant 绑定业务适配对象，出口只按本地授权配置重建上游 TLS，不接受入口自报“以前是 TLS”作为自己 `TLSRequired` 的证明。配置实际观察层应符合本地入口/出口策略。

`business.websocket=true` 对受控 WS 在 HTTP `101` 后检查有限的解掩码载荷；允许流保留原始帧，不把检查视图当作 socket 字节。原始 HTTP 禁用/Host/Path 策略仍执行。默认不把普通子协议名字解码为 early-data；受控早数据需显式 `websocket_early_data=true`，完整可裁决首部在 Upgrade 请求发送前检查，分段而不可裁决的早数据明确拒绝。压缩等未支持扩展不宣称覆盖，gRPC/HTTP2/SIP003 不在当前内层适配范围。

安装脚本新增 `-I <本地绝对JSON路径>` 和 `-B <本地绝对JSON路径>`，以 `0600` 复制并为服务配置对应 Agent 参数。升级 Agent、部署各检测节点的私有 profile、等待准备状态上报后，再在管理员高级配置选中协议与标签。入口与出口所用标签可相同，但各节点独立使用自己的实际凭据。

## 数据面与支持范围

| 协议/模式 | 可识别证据与范围 |
| --- | --- |
| SOCKS4/4a、SOCKS5 TCP | 完整请求/greeting 的结构证据，畸形方法数和截断不确认 |
| SOCKS5 UDP 显式结构模式 | 完整 RSV/FRAG/ATYP/ADDR/PORT；标为结构，不能称为认证身份 |
| SOCKS5 UDP 受控关联 | 本地实际 TCP association 与源端点/目标/代次证明，精确索引逐报复查 |
| SS2017 | AES128/256-GCM、ChaCha20-Poly1305，TCP 长度和载荷 tag、每 UDP 报文认证 |
| SS2022 | AES128/256-GCM、ChaCha20-Poly1305，固定/变长首部和时间窗、TCP/UDP；AES SIP023 2–4 key 最终认证 |
| VMess AEAD | 已知 UUID，AuthID 预筛后验证两个 GCM tag 和完整请求结构；TCP/内部 UDP/mux，body `none/zero` 不跳过认证 |
| Trojan | 明确业务明文位置，恒时验证已知认证 hash 和完整地址结构，TCP/内部 UDP |
| VMess legacy | 仅观察诊断，严格认证禁用不接受 legacy profile |
| 未知密钥/透传业务 TLS | 未知或不可见；默认允许，独立未知拒绝策略另算 |

检测器注册表、证据类型、准备与逐包决策在 `internal/policy/detect` 共享。TCP 多候选共享有界前缀，认证证据优先于弱结构碰撞。每流最多 65,535 字节、5 秒、256 次密码学操作，失败候选与下一所需长度缓存。进程同时限制 128 个原始检测和 128 个 WS 检测；容量耗尽作为独立资源原因，不能冒充协议匹配。UDP 每报独立认证，不将未知首报永久缓存为允许，不新增逐包 SQL/WAL。

原始直连、普通出口、UDP-over-TCP、QUIC DATAGRAM 解封装与 chain/reverse/mux 按业务流/报文调用公共检测器。v3 保留原有前缀验证，出口仍检查实际字节再转发，但可能已建立到源站的连接。启用新版独立出口检测的 hop 或授权业务适配使用 v4 分阶段能力，在源站连接前检查真实流。允许前缀完整回放一次，拒绝 gate 返回 `0, error`，保留半关闭、计量、授权和租约撤销。

各 hop 根据自己的检测与业务适配需求协商能力。只启用入口检测时，普通手动/未托管隧道和旧出口仍可使用旧 wire；预览明确显示入口覆盖，不能宣称具有独立出口检测。出口策略启用新检测时要求该 hop 本地能力、准备状态和托管服务；业务适配跨 hop 还要求各 hop 授权 grant 和 v4 能力。

WS 为完成外层握手可能已向 origin 发送合法 HTTP 请求。出口拒绝前，入口可能已经向隧道发送并计量数据；这些已经发送的字节不会自动退款。入口/出口诊断按观测位置分别展示，不能把多跳拒绝计数相加作为用户连接数。

## 验证证据

本轮独立协议验证包括：

- 固定 `sing-shadowsocks v0.2.9` 实际客户端编码，覆盖 SS2017/2022 所有上述 cipher、IPv4/IPv6/域名、TCP/UDP、SIP023 2–4 key。
- 固定 `v2fly/v2ray-core v5.54.2` 实际 VMess 请求编码，覆盖 AEAD 各 body security、内部 UDP/mux、地址形式、碎片、坏 tag、未知 UUID，CRC 单独不能确认。
- 固定官方 Xray `v26.3.27` 独立进程客户端和协议服务端，共 16 个真实 socket/目标观察用例验证 off→block→restore；直连与 TFP TLS 入口→出口覆盖 SOCKS5、SS2017 三 cipher、SS2022 AES128/256、VMess AEAD；直连增加 Trojan 业务 TLS 与 VMess WS。下载文件核对固定 SHA256。
- 实际 Agent 配置和 UDP socket 覆盖直连、UDP-over-TCP、QUIC × 六种 SS2017/2022 cipher；同一 flow 中未知包、坏 tag、合法认证包和普通包依次测试，确保逐报重新检查、阻断与解除恢复。TLS、链式中间/最终出口、反向出口和 QUIC 出口只使用自己的本地检测凭据；同时检查源站无新增报文、实际拒绝回调和认证诊断。
- 独立 SS2017、SS2022 与 VMess AEAD 编码流在入口只声明 1 字节时，出口 v3/v4 均以实际流完成认证拒绝，源站收到零业务字节；v4 还验证没有建立源站连接。WS 完整认证首部与缺失帧尾在 EOF/超时下仍拒绝，避免可识别数据误作未知回放。
- 有界解析/凭据/scope/未知/预算回归、race/vet，以及约 67 万次短时 fuzz。
- SOCKS association 实际 TCP、no-auth/user-password、BND/来源/写入证据、撤销、过期、并发，以及有界 observer fuzz；直连和 TFP TLS 入口→出口分别验证来源端口隔离、控制关闭撤销、profile 轮换撤销和计量续租保留关联。真实高级配置 API→默认 Failover→Agent Apply 跨层测试防止界面参数与数据面脱节。

Linux CI 新增固定 Xray 下载校验与同一真实客户端矩阵。三库、真实浏览器和跨平台构建继续使用仓库现有检查。本机使用 Windows 与 SQLite，未运行 PostgreSQL/MySQL 或实际 Linux 数据面；CI 的三库配置不能写成本机已验证三库。参考测试不等于生产性能达标；Linux 跨机无策略吞吐/PPS/CPU预算、启用检测开销和 24 小时稳定性应另外测量。

最终代码验证结果：

| 检查 | 结果 |
| --- | --- |
| `go test -race ./... -count=1 -timeout=8m` | 全仓通过，包括入口/出口、控制面、未知/观察模式与 WS 部分帧新增回归 |
| `TFP_TEST_XRAY=<固定校验后的二进制> go test -race ./internal/integration -run '^TestIndependentXrayClients$' -count=1 -timeout=5m` | 16 个独立客户端场景通过 |
| `go vet ./...`、`go mod tidy -diff`、`gofmt`、`git diff --check` | 通过 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64/arm64 go build ./cmd/agent ./cmd/panel` | 两种架构均编译通过，不等于 Linux 运行验收 |
| `npm run typecheck`、`npm run build` | 通过，已生成前端静态资源 |
| `npm test`、`npm run test:live` | Chromium、Firefox、WebKit 全部通过，新协议编辑纳入默认真实面板测试 |
| `scripts/test_agent_installer.py` | 19 项模拟安装测试通过 |
| `go generate ./internal/openapi` | 143 个操作，生成前后 SHA256 一致 |

生产 Agent 不链接 GPL SagerNet 实现；该依赖仅用于独立客户端测试。生产 VMess 参考 helper 与 BLAKE3 依赖为 MIT；依赖版本固定在 `go.mod/go.sum`，原始版权声明保存在 [协议依赖声明](protocol-dependency-notices.txt)，随源码与发布文档打包，Docker 镜像包含 `/licenses/protocol-dependency-notices.txt`。
