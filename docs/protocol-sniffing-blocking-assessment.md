# Shadowsocks、Trojan、VMess、SOCKS5 嗅探与禁用评估

日期：2026-10-10。基线：v0.1.39 / `41f27b40f0a3329844adfd8e42425d7ccc1827ae`。本文件保留架构评估与已审批实施计划；后续按用户授权由主 Agent 与 3 个子 Agent 并行实施，功能和本地验证已完成，纳入 v0.1.40 发布。实际支持范围、部署和测试证据见 [实现交付说明](protocol-sniffing-implementation.md)。

后续用户批准实施，实际源码支持、部署步骤和验收证据见 [实现与验收](protocol-sniffing-implementation.md)。本评估保留批准前的架构与阶段依据；下文“待实施”描述对应当时基线。

## 1. 结论和推荐范围

现有控制面编译、入口检查、托管出口独立检查、配置准备/提交和授权撤销可以继续使用。需要将首字节分类升级为有状态、有界的公共检测引擎，并为受控业务入口提供独立的 TLS/WS 适配器。

推荐提供两类能力：明文可见协议的结构检查，以及本地配置凭据后的加密协议认证检查。默认只对满足声明证据条件的匹配执行协议禁用；未知、缺少密钥、内层不可见和不支持封装分别呈现。无密钥的任意 Shadowsocks/VMess 与透传 TLS 内的 Trojan，不能承诺准确识别并单独禁用。选中协议名称不等于任意第三方加密流量均可被识别。

尚未得到用户关于目标是否受控、是否能提供协议凭据的补充回答，先按“两类都覆盖，但在各自可验证条件内提供能力”组织方案。对于无法提供密钥或业务解密位置的场景，结果必须明确标记不可确认；这一限制不能通过增加四个复选框消除。

| 协议 | 推荐识别方式 | 可以交付的禁用范围 | 明确限制 |
| --- | --- | --- | --- |
| SOCKS5 TCP | 完整解析客户端 greeting，验证版本、非零方法数、完整方法列表 | 可见明文 greeting 匹配后，在目标拨号前阻断 | 属于结构证据，不是密码学证明；不能等待服务端响应前尚未发送的 CONNECT 请求 |
| SOCKS5 UDP | 解析 `RSV/FRAG/ATYP/ADDR/PORT`；增强模式绑定受控 TCP association | 已关联的 UDP 业务；可选独立的结构阻断模式 | 独立 UDP 首部可能与普通数据碰撞，不能标为认证确认；通用转发器当前没有 association 状态 |
| Shadowsocks AEAD 2017 | 已知 method/key，验证首部与 payload 的 AEAD tag 和请求结构；UDP 认证完整报文 | 已配置凭据和宣布支持的 cipher 的 TCP/UDP | 未知密钥、未支持 cipher、外部 TLS/插件封装不可确认 |
| Shadowsocks 2022 | 按 SIP022 认证 fixed/variable header，验证 type/timestamp；按对应布局认证 UDP | 已配置 PSK、受支持 cipher 的 TCP/UDP；SIP023 多用户独立验收 | 不得只凭 identity header 映射确认；需要最终认证验证 |
| Shadowsocks legacy stream | 已知 key 解密后的地址结构检查 | 只列为兼容诊断，默认不提供可靠协议禁用 | 没有 AEAD 完整性，随机错误解密也可能形成合法地址 |
| VMess AEAD | 已知 UUID，验证 AuthID 预筛及 ALength/AHeader 的 GCM tags，再检查请求结构 | 受支持版本及明文可见载波内的已知 UUID 流 | AuthID CRC32 不是最终认证；`security=none/zero` 不意味着认证头可无密钥识别 |
| VMess legacy | UUID 与有限时间窗验证身份 token，核对解密头部 | 单独声明的兼容能力，独立测试 | 头部没有 AEAD 完整性，不可与现代 AEAD 给出相同证据等级 |
| Trojan | 业务 TLS 终止后，恒时验证已知认证摘要、CRLF、CMD 与地址结构 | 受控业务 TLS/明文观察位置内的已知凭据流；包括其 TCP 内承载的 UDP | 普通透传 TLS 只有 SNI/ALPN；有密码或服务器私钥仍不能被动解密 TLS 1.3/PFS |

如目标是“禁止所有不可识别代理”，可另选应用白名单或拒绝未知应用策略，但会同时影响普通未知业务与不透明 TLS。它是不同的访问策略，不能把拒绝量算成四种协议的识别成功，也不随协议复选框自动启用。

## 2. 已核实的现有架构与缺口

| 位置 | 当前行为 | 改造用途 |
| --- | --- | --- |
| `internal/policy/inspect.go` | TCP 首字节 `0x16` 进入 ClientHello，`0x04/0x05` 判 SOCKS，未知二进制可能仅捕获一个字节 | 改成多个候选共享有限前缀的增量检测，消除密文首字节碰撞 |
| `internal/policy/match.go` | 只允许 `http/socks`，编译并封存有效策略 | 公共协议注册表、声明范围、检测预算和证据阈值 |
| `internal/agent/runtime.go` | `handleTCP` 先检查，再拨目标与计量 Relay；另有 UDP `blocked` 旧实现 | 入口 gate、原始字节一次回放、公共 UDP 检测 |
| `internal/agent/udp.go` | peer 哈希 worker，在创建会话和转发前检查 | 每数据报检查、有限会话状态、拒绝缓存 |
| `internal/tunnel/tunnel.go`、`group_policy.go` | 出口检查入口给出的首部，校验实际业务前缀一致，再转发 | 独立出口检测、阶段 gate、原始字节验证 |
| `internal/tunnel/chain.go`、`mux.go` | 保留 RuleID 与前缀，按业务 stream 中继 | 每 stream 独立检测，覆盖链式/反向/mux |
| `internal/tunnel/datagram.go` | QUIC 解封装后、写 UDP origin 前检查 | 检测业务 payload，保留原生 UDP/QUIC 载波 |
| `internal/platform/resources.go`、`group_policy_compile.go`、`managed_services.go` | 规范化组参数、策略合并、能力检查、服务配置 | 全部路径编译同一检测计划和阻断原因 |
| `internal/agent/service_manager.go` | 本地证书/CA/精确监听许可、服务准备和代次更新 | 借鉴本地 profile 生命周期；不直接当作业务 TLS 终止器 |
| `web/src/components/GroupAdvanced.vue`、`web/src/core/groupAdvanced.ts`、OpenAPI | 仅 HTTP/SOCKS；严格枚举限制分布多处 | 新协议设置、真实覆盖预览、版本兼容 |

正常保存组配置时，`splitGroupPolicy` 已把高级协议列表同步为旧的 `app:*` 字段，Agent 配置编译会合并这些字段；因此不能把现有直连 UDP 说成完全没有执行高级协议设置。新引擎应直接使用统一编译计划，消除镜像字段与多份 detector 的一致性风险。

本轮运行一次临时 Go 复现程序，调用真实公共检测代码，得到：

```text
RFC1928 SOCKS5 UDP header blocked: false
Unrelated UDP starting with 0x05 blocked: true
Invalid SOCKS5 greeting (NMETHODS=0) classified as: socks
Invalid greeting denied by SOCKS policy: true
Detector enum shadowsocks accepted: false
Detector enum trojan accepted: false
Detector enum vmess accepted: false
Detector enum socks5 accepted: false
```

这些结果证明当前首部判断有漏检/误判与枚举缺口，不是新增协议功能的验收证据。实施阶段需要正式回归和独立真实客户端测试。

## 3. 公共检测引擎

建议新增 `internal/policy/detect`，协议注册、TCP/UDP 解析及证据模型在公共包内共享，供入口和托管出口调用。接口草案：

```go
type Detection struct {
    Protocol string
    Variant  string
    Status   Status // need_more, no_match, match, unavailable
    Evidence Evidence // structural, authenticated, legacy_auth, probable
    Need     int
    Reason   Reason
}

type Detector interface {
    Feed(prefix []byte, end bool, meta Metadata) Detection
}
```

整体结果另记录 `unknown/opaque`、封装层和实际检测位置。结构匹配、认证确认、旧版身份 token 与统计猜测不能合并为一个可信等级；SOCKS greeting 可以按声明的结构匹配规则禁用，SS/VMess AEAD 必须通过认证后才按协议禁用。

多个候选读取同一有限前缀；已知密钥认证证据优先于弱结构猜测。不得先用旧 `Inspect` 硬分类再追加 SS/VMess，因为其随机首字节可能正好是 `0x16/0x04/0x05`。不能按字节逐次执行全套昂贵解密；detector 返回明确的下一长度，达到后再尝试。

初始建议每流原始 TCP 检测缓冲上限维持 65,535 字节，绝对检测期限不超过现有 5 秒；每条规则最多 16 个待尝试凭据项、有限解密次数和全节点缓冲额度。具体上限通过真实客户端与性能测试修订。超过支持长度、时间或封装深度单独报告，不能伪装成协议匹配。动态分配按上限和实际需要进行，避免每连接预分配最大缓冲。

凭据解析、基础 KDF 与 detector plan 在配置准备时完成；每会话的 salt/nonce 派生按协议完成。SS2022 的 BLAKE3 等现有依赖未提供的能力需选择维护中的实现，锁定版本、许可证和漏洞检查，不自行设计密码学替代协议。

TLS 外层确认只证明是 TLS，不证明内层应用符合白名单。超时、畸形协议和资源预算不足分别处理；默认未知允许时，必须完整回放已经读取的字节，避免额外破坏未知业务。

## 4. TLS、WS 与分阶段检测

TFP 隧道解密只解除面板自己的 TLS/WS/HTTP 载波；里面的客户端业务 TLS 仍然加密。现有 `ReverseTLS`、service TLS profile 及 `direct-tls` 都不是客户端业务 TLS 解密功能。

增加独立的管理员业务入口配置，由本地 profile 指定业务域名证书、私钥、精确监听许可和上游 TLS 身份。客户端必须正常验证该服务身份；上游可显式重加密并验证目标证书。只适用于拥有服务身份和可部署解密位置的业务。面板只下发 profile 标签，不提供关闭证书验证的捷径。

业务层顺序为：外层 SNI/Host 检查与规则选择 → 明确配置的 TLS 终止 → 必要的 WS 适配 → 内层协议认证检查 → 原始业务或适配后的上游流。原始 `TLSRequired` 仍针对入站 wire 的 TLS 状态，不能因内层已解密就判为非 TLS。wire TLS、解密明文与 WS 检查视图的作用层由规则/服务配置显式绑定；入口检查原始入站，出口检查自己的实际观察层。入口终止后不能让出口凭未经验证的“原始为 TLS”标签视为本地 TLSRequired 已满足。

明文 WS 客户端通常等待 `101` 响应才发送内层首部，因此不能在目标拨号前一直等待 VMess/SS 首包。需要允许受支持外层握手完成，再在首个内层业务发送前 gate；握手字节按现有计量规则结算。若现有策略也明确禁用外层 HTTP，仍按该策略处理，不因其承载 VMess 自动放行。

WS 的 mask 还原和跨帧组装形成检查视图；允许时回放原始帧或由明确的适配器重编码。检查视图绝不能直接冒充原始 socket 前缀。需要处理控制帧、分片、二进制、early-data、有限重组与上游响应；若 early-data 位于 Upgrade 请求头，须在转发该请求前检查，不能只在 101 后阻断。WSS 先做业务 TLS 终止。gRPC/HTTP2、HTTP CONNECT、SIP003 插件等逐一声明支持范围，不能由现有 HTTP Path 检查推导为已经支持。

现有 v3 隧道协议只有一次静态 `inspectionFrame`，出口在 ready 前裁决，不足以处理响应后的内层协议。保留 v3 已有行为，新增显式协商的 staged-inspection 数据面能力；分别定义握手、内层 gate 和允许后的 relay 阶段。

出口按自己本地 detector/profile 重新检查实际业务字节；入口上报的协议名称只是诊断提示。继续绑定 RuleID、network、target、grant 与真实前缀/帧序列，不让新阶段绕过授权。密文 SS/VMess 在原始流透传模式保持逐字节一致；业务 TLS/WS 终止模式则按照明确适配后的数据契约独立验收。

## 5. SOCKS5 与 UDP

TCP 在完整 greeting 可判定时立即裁决，不伪装 SOCKS 服务端发送应答，也不等待只有服务端应答后才出现的请求命令。支持 no-auth、user/password 等完整方法列表；需要观察完整会话时必须采用双向状态机，并明确可先发送的握手范围。

UDP 按 RFC1928 校验两个零 RSV 字节、FRAG、ATYP、IPv4/IPv6/域名长度和端口。首部结构没有身份认证：独立结构匹配仅是有限证据。推荐增强覆盖通过受控 SOCKS 服务记录 TCP UDP ASSOCIATE、BND 端点、允许来源与生命周期；当前通用固定目标转发器没有这种状态，需新增服务协作，而非凭 peer 字符串假设关联成功。拒绝分片或有限重组行为须按对应检测/服务模式明确，不得丢弃所有普通 UDP 的所谓非零 FRAG。

各 UDP 路径调用同一引擎：直连入口、普通出口 QUIC DATAGRAM 解封装、UDP-over-TCP、链式与反向中继。保持每报边界、worker 分片、批量 I/O 和额度计量。

未知初报不能缓存为永久允许，否则后续代理报文可绕过。SS UDP 应按每报认证；已确认禁止可设置有限 TTL/容量的拒绝缓存，key 包含 RuleID、策略/profile 代次、peer、目标和候选，变更或撤销时失效。统计缓存拒绝与实际认证次数分开，缓存不能成为其他身份复用的授权证据。

无嗅探策略保持既有数据路径；启用检测后不新增逐包 SQL/WAL、无限 key 遍历或全局解析锁。TCP/UDP、计量、候选与服务代次保持一致。

## 6. 配置、能力、凭据和诊断

协议规范值建议为 `http`、`socks4`、`socks5`、`shadowsocks`、`trojan`、`vmess`。保留历史 `socks` 为 SOCKS4+5 联集；输入兼容 `app:`，输出规范化。顶层历史裸 `http` 的载波限制语义与高级字段裸 `http` 的应用限制语义保持区分。Shadowsocks 版本、cipher 和 VMess 版本由 profile/能力明确，不用一个总开关宣称任意变体覆盖。

保持 `GroupPolicyVersion=2` 的存量行为，新增独立检测版本和配置对象。当前 `activeAdvanced` 与 `Seal` 精确比较版本，直接改常量为 3 会使旧配置失活；如果确需 v3，必须实现 0/2/3 兼容与显式迁移。新字段要同步严格 JSON decoder、API、OpenAPI、前端解析和策略 hash。

能力按协议、版本、网络、封装和方式声明，例如 `inspect:socks5-tcp-v1`、`inspect:ss-aead2017-v1`、`inspect:vmess-aead-v1`、`business-tls-termination-v1`、`staged-inspection-v1`。仅实际通过相应互操作测试的节点声明能力。界面明确提供 Shadowsocks、Trojan、VMess、SOCKS5 四项，每项同时展示版本/载波范围、本地准备状态和缺失条件；保留 SOCKS4 与旧 SOCKS 联集兼容。

配置准备与逐流未知分开：严格要求覆盖时，缺少声明范围内的静态前提（节点能力、profile、业务解密位置）使规则编译/准备失败，路线不下发，预览展示具体原因。前提满足后，某条流使用未知密钥、认证不匹配或不可识别封装，仍可能得到 `unknown/opaque`；默认允许，只有独立配置未知拒绝才丢弃。显式观察/尽力检测模式可记录不可确认项，但界面不能显示“完整禁用已生效”。严格覆盖的范围只包含声明和实测的组合，不代表识别任意未知加密协议。

新增独立的 Agent 本地检测 profile，可借鉴 service profile 的私有文件、严格读取和准备/提交机制。密码、SS PSK、UUID、Trojan hash 与证书私钥均不进入面板 API、日志、ACK 或业务诊断；Trojan hash 是可用认证凭据，也不能公开。配置绑定许可的设备组/规则/目标或服务身份，限制文件大小、凭据项数量和使用范围。标签指向同名但内容不一致时，入口和出口各自使用本地实际凭据，预览只显示准备能力，不暴露密钥摘要。

profile 轮换创建本地不透明代次，重新准备 detector，撤销旧会话/拒绝缓存；失败保留合法当前代次并报告错误。各检测节点必须部署必要 profile，面板不能代替分发未获授权的服务密钥。

运行诊断增加协议、版本、证据类型、可见层、入口/出口位置、未知/不可见/预算原因及有限计数。只用固定维度，不上报原始 payload、认证首部、目标用户名或高基数 Host。多跳的拒绝观测不得加总冒充用户连接数。

## 7. 转发与计量约束

原始首部 gate 位于目标拨号/业务发送前。多阶段 gate 可以先转发必要的外层握手，但禁止的内层业务不能进入业务目标；因此不能对这种模式承诺整个连接零握手流量。

现有 `Relay` 在 `Read` 返回 `n>0` 时先发送再处理 error。阻断 gate 对已禁止缓冲必须返回 `0, ErrDenied`，不能返回 `n>0, ErrDenied` 泄漏数据。允许后的原始数据恰好回放一次，保持 EOF、half-close、deadline、计量和业务边界。在该计量位置尚未发送的拒绝缓冲不新增用量；已发送合法握手和入口到隧道的业务传输按现有规则结算。出口拒绝不自动回滚入口已经发送并计量的隧道字节，因此“禁止业务未抵达 origin”不等于全路径零扣费；如需后者，须另设计发送前预检或结算机制。

规则策略、目标/候选、授权或 profile 代次变化时关闭旧检测状态；仅商业租约续期沿用符合当前策略的连接。反向与 mux 的检测单元是业务 stream，而不是共享物理连接。

## 8. 实施阶段与交付门槛

| 阶段 | 主要交付 | 主要文件/模块 | 完成门槛 |
| --- | --- | --- | --- |
| P0 统一契约 | 协议注册表、证据模型、预算、未知语义、独立检测版本与能力 | contract、policy、resources、OpenAPI、GroupAdvanced | 新旧配置回读与三库兼容；旧节点明确拒绝未支持模式 |
| P1 SOCKS 修正 | 完整 SOCKS4/5 TCP 解析、标准 SOCKS UDP 首部、公共检测入口 | policy/detect、runtime、udp、tunnel/chain/datagram | 分片及首字节碰撞回归；实 SOCKS 客户端允许/拒绝/恢复；UDP结构证据准确呈现 |
| P2 认证检测 | 本地 profile；SS AEAD2017/2022、VMess AEAD；legacy 单独能力 | 新 profile loader、SS/VMess detector、编译与准备流程 | 每个支持 cipher/版本使用固定版本独立客户端验证 TCP/协议内UDP；错误凭据不冒充确认 |
| P3 受控业务入口 | 业务 TLS、Trojan、WS 多阶段检测、受控 SOCKS UDP association | TLS/WS适配器、服务配置、分阶段隧道协商 | 普通 HTTPS/WS 负例通过；已知 Trojan TCP/内部UDP阻断；BND/来源/生命周期正确 |
| P4 全链路验收 | 各路径真实流量、诊断、轮换/撤销、负载和发布说明 | integration、浏览器、Linux CI与独立性能环境 | 下述矩阵通过；实际支持组合和不可见场景写入交付说明后才发布能力 |

P2 首批建议 SS2017 AES-128/256-GCM、ChaCha20-Poly1305，SS2022 AES-128/256-GCM、ChaCha20-Poly1305，以及现代 VMess AEAD；SIP023 多用户单列子任务。每项须与锁定版本真实客户端协议细节核对后实施。SS legacy 默认诊断，VMess legacy 属于后续显式兼容项，不能把尚未验证的版本标为支持。

阶段依赖：P0 → P1 → P2；P3 依赖 P0/P1/P2 的引擎和证据模型；P4 随阶段增量执行，最终全链路验收。实施分工可继续由协议、运行时、配置/测试 3 个 Agent 按文件范围协作；主 Agent 负责接口一致性、合并、集成和验收。各阶段交付无需依赖生产部署。

## 9. 验收矩阵

| 类别 | 必须验证 |
| --- | --- |
| 实客户端 | 固定版本 SOCKS客户端、Shadowsocks-rust/另一独立实现、Trojan客户端、Xray/V2Ray；关策略成功、开策略阻断、解除后恢复；不是仅自制 encoder/decoder互相通过 |
| 负例 | 普通 TCP二进制、TLS/HTTPS、HTTP/WS、QUIC、随机 `0x04/05/16` 前缀；不把普通加密数据判为 SS/VMess/Trojan |
| 首部边界 | 逐字节分片、截断、错长度/ATYP/tag/UUID/key、时间窗、early-data、多帧、慢连接和预算耗尽；错误理由准确 |
| UDP | 标准 IPv4/IPv6/域名首部、association、FRAG、未知初报后协议变化、每报认证、缓存代次、来源/目标变化与容量上界 |
| 路径 | 直连入口、两个 Agent 普通出口、QUIC、UDP-over-TCP、链式、反向、mux；仅对宣布支持的封装组合验证并标明范围 |
| 独立出口 | 关闭入口检测后出口仍能用本地profile拒绝；假分类、假首部及回放不一致拒绝；出口实际字节优先 |
| 业务阻断 | 目标观察禁止业务未到达；多阶段允许外层握手但不泄漏内层禁止首部；不能只凭客户端超时证明成功 |
| 连续性/计量 | 允许流回放一次、EOF/half-close、租约续期、计费、热更新、账号停用、凭据轮换、错误Apply保留当前配置 |
| 控制面/UI | 新旧协议名、strict decoder、JSONC、预览/真实状态、协议/证据计数、profile缺失、旧Agent、SQLite/PostgreSQL/MySQL |
| 性能/资源 | 同硬件Linux跨机重复采样 throughput/PPS/CPU/P99；无策略吞吐/PPS下降≤5%、CPU增加≤5%作为待实测预算；启用密码学检测单列开销；有限FD/goroutine/内存、慢连接、24h稳定性 |

解析器补有界 fuzz/race 和独立参考向量，TLS终止补证书验证与轮换检查。仍使用 [UDP 性能验收手册](udp-performance-lab.md) 的真实计量与拓扑，不以 Windows 回环或 CI 功能检查替代性能达标。

## 10. 协议依据

- [SOCKS5 RFC1928](https://www.rfc-editor.org/rfc/rfc1928)：greeting、UDP ASSOCIATE、数据报格式与来源绑定。
- [Shadowsocks AEAD](https://github.com/shadowsocks/shadowsocks-org/blob/main/docs/doc/aead.md)：salt、subkey、TCP分块与UDP认证。
- [SS2022 SIP022](https://github.com/shadowsocks/shadowsocks-org/blob/main/docs/doc/sip022.md)、[多用户 SIP023](https://github.com/shadowsocks/shadowsocks-org/blob/main/docs/doc/sip023.md)：请求格式、时间戳、认证及identity链。
- [SS legacy stream](https://github.com/shadowsocks/shadowsocks-org/blob/main/docs/doc/stream.md)、[SIP003](https://github.com/shadowsocks/shadowsocks-org/blob/main/docs/doc/sip003.md)：非AEAD与插件边界。
- [Trojan 官方协议](https://github.com/trojan-gfw/trojan/blob/master/docs/protocol.md)：真实TLS、SHA224摘要和TCP内UDP framing。
- [VMess 官方协议](https://github.com/v2fly/v2fly-github-io/blob/master/docs/developer/protocols/vmess.md)、[AEAD参考实现](https://github.com/v2fly/v2ray-core/blob/master/proxy/vmess/aead/encrypt.go)：身份、长度与认证头；不能以CRC32替代GCM认证。

本轮完成源码核对、协议资料核对和现有检测缺口的最小复现。上述新增检测器、适配器与性能门槛尚待实施和验收。
