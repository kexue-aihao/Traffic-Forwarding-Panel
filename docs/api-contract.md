# 实施契约 v1

时间用 UTC RFC3339；金额/累计字节使用十进制字符串，采样速率 `upload_bps/download_bps` 为 JSON number（单位 bytes/s），ID 为不透明字符串。接口前缀 `/api/v1`。成功对象直接返回 JSON；分页列表 `{items:[],total:0}`；错误一般为 `{code,error,request_id?}`，部分鉴权错误仍为 HTTP 文本。分页 query 为 `page` (1 起)、`page_size` (默认 20，最多 100)；未实现通用 `q` 搜索。

浏览器使用 HttpOnly Cookie；修改请求 `X-Requested-With: fetch` 并核对 Origin。同源 fetch。自动化采用独立 Bearer Token。登录/注册节点也限制请求大小和速率。除专门支付通知外严格拒绝未知 JSON 字段。

| 方法/路径 | 请求 | 返回/权限 |
|---|---|---|
| POST /auth/login | `{username,password}` | `{user: User}` + Cookie |
| GET /auth/session | 无 | `{user: User}`；未登录 401 |
| POST /auth/logout | `{}` | 204 |
| POST /auth/password | `{current_password,password}` | 204；撤销旧会话和 Token |
| GET /auth/tokens | 分页 | 本人 Token 元数据（`id,name,prefix,scope,created_at,expires_at,permanent,last_used_at`），不返回明文/哈希 |
| POST /auth/tokens | `{name,expires_at}` 或 `{name,permanent:true}` | `{id,token,name,prefix,expires_at,permanent,scope}`；仅创建时返回明文；有限期最长一年，`permanent` 需显式声明 |
| DELETE /auth/tokens/{id} | 无 | 204；只能撤销本人 Token |
| GET /users/{id}/tokens | 分页 | 管理员读取某账号的 Token 元数据；与本人视图同形，不返回明文/哈希 |
| POST /users/{id}/tokens | 同 `POST /auth/tokens` | 管理员为账号签发；明文只在这一条响应里出现，交由用户保存 |
| POST /users/{id}/tokens/{token_id}/reset | `{}` | 管理员就地换取新明文（同一 id/名称/有效期），旧密钥立即失效；明文同样只出现一次 |
| DELETE /users/{id}/tokens/{token_id} | 无 | 204；管理员撤销某账号的 Token |
| GET /users | 分页 | 管理员 User 列表 |
| POST /users | `{username,password,role}` | 管理员创建 |
| PUT /users/{id}/status | `{disabled}` | 管理员停用/启用，保留最后一个管理员 |
| GET /groups | 分页 | 授权 Group 列表 |
| POST /groups | `{name,user_ids,blocked_protocols,multiplier,port_min,port_max}` | 管理员 Group |
| PUT /groups/{id} | 同上加 version | 管理员乐观锁 |
| GET /groups/{id}/join-key | —— | 管理员 `{group_id,join_key}`，设备组的固定接入密钥；可随时再次读取 |
| POST /groups/{id}/join-key | `{}` | 管理员 `{group_id,join_key}`，轮换后已分发出去的接入命令立即失效 |
| GET /nodes | 分页 | 授权 Node 列表 |
| POST /nodes/enrollment | `{name,group_ids}` | 管理员 `{token,expires_at}`，仅展示一次 |
| POST /nodes/{id}/rotate-token | `{}` | 管理员；返回新凭据，旧节点凭据失效 |
| POST /nodes/{id}/looking-glass | `{method,target}` | 管理员（Cookie 会话）；在节点上跑 ping / tcping / mtr，返回 `{id,status}`，结果用下面那条轮询 |
| GET /looking-glass/{id} | —— | 创建者或管理员；`{status,output,error}`，两分钟内没被节点领走即 `expired` |
| GET /rules | 分页 | 管理员全局/用户自有 Rule；无 tunnel.token、chain各跳token、lease |
| POST /rules | `{name,node_id,group_id,network,transport,listen,target,enabled,tunnel?}` | Rule；管理员可指定 user_id |

> 规则的 `listen` 留空时，面板会从**设备组允许的端口范围**里随机分配一个尚未预留的端口（随机起点 + 环形扫描，避免总挑到同一个或撞上端口预留）；显式给出则原样使用。
| PUT /rules/{id} | 规则字段加 version | 乐观锁 |
| DELETE /rules/{id}?version=N | 无 | 204；待节点确认解绑才释放端口 |
| GET /probes | 可选 `group_id` | `{items: Probe[]}`；付费能力，需有效权益；只含授权设备组的机器，`group_id` 非本人所属组时 403。补 `node_name/group_ids/location`；普通用户隐藏 public_ips 但保留位置图标 |
| GET /probes/events | 可选 `group_id` | SSE `event: probes` + 同上 JSON，每 5 秒重校验身份/授权 |
| GET /online/device/ip | 可选 `group_id` | 探针页面预留的脚本接口：当前可见范围内**唯一**那台设备的 `{device:{...}}`；多台返回 409 并提示改用列表，无设备返回 404 |
| GET /online/device/ip/list | 可选 `group_id` | `{items:[DeviceIP],total}`；一组多台时用它。机器被替换 `node_id` 变、只换 IP 时 `address/observed_at` 变，调用方无需改代码 |
| GET /probes/{node_id}/history | `resolution=minute\|hour&from=RFC3339&to=RFC3339` | 授权节点聚合历史；无权限与节点不存在均404 |
| GET /audit | 分页 | 管理员审计列表 |
| GET /health | 无 | `{status,database,version}`，不含 DSN |
| POST /agent/register | Registration | Registered |
| GET /agent/config | 节点 Bearer | Config；最长 24 小时并受有效期/配额限制 |
| POST /agent/ack | Ack | 204，防旧版本覆盖 |
| POST /agent/probe | Probe | 204，身份绑定 node_id |
| POST /agent/usage | UsageBatch | `{accepted: [id...]}`；持久化后确认 |
| POST /agent/leases/retire | `{lease_id,used_bytes}` | 204；最终用量必须已全部结算，重复相同退租安全 |
| POST /nodes/{id}/operation-access | `{password}` | 管理员 Cookie 二次验证；返回 15 分钟节点运维 token |
| POST /nodes/{id}/terminal | `{access_token,idempotency_key}` | 创建审计终端任务；节点需声明 `terminal-v1` |
| POST /nodes/{id}/upgrade | `{access_token,idempotency_key,upgrade}` | 创建签名升级任务；Agent 校验 HTTPS、SHA-256、Ed25519 和平台 |
| GET /nodes/{id}/operations | 无 | 最近节点运维任务；仅管理员 Cookie |
| POST /node-operations/{id}/cancel | 无 | 取消 pending/running 任务 |
| GET /node-operations/{id}/terminal | WebSocket | 浏览器终端，重新验证 Cookie、授权和任务状态 |
| GET /node-operations/{id}/commands | 无 | 终端命令审计；不保存输出 |
| POST /agent/control | 无 | Agent Bearer 领取运维任务 |
| POST /agent/control/result | `{id,claim,status,error}` | 回执 `staged|succeeded|failed|rolled_back` |
| GET /agent/control/{id}/terminal | WebSocket | Agent 侧终端通道；需 claim |

共享 Go 类型见 `internal/contract/types.go`。探针实时接口先实现 SSE；WebSocket 作为后续相同权限语义传输适配，不能混称隧道 WSS。

`DeviceIP`：`{node_id,node_name,group_id,group_name,address,family,source,observed_at,online,last_seen?,location?}`。`address` 取该节点最近一次观测到的对外地址，**优先 IPv4**（客户拿它去连服务）；`online` 按最近心跳判断；`location` 是 `{country_code,country_name?,region?,city?,source}`，来自站点设置里的地区查询服务，查不到就没有这个字段 —— 客户端要能接受缺失。接口带 `Authorization: Bearer <API Token>`，也接受同源 Cookie 会话；`/online/device/ip` 与 `/online/device/ip/list` 两个不带 `/api/v1` 前缀的路径是为客户脚本保留的稳定入口，与带前缀的同名接口等价。

已挂载商业接口：`GET/POST /plans`（创建仅管理员）、`GET /wallet`、`GET /ledger`、`GET /orders`、`POST /orders`、`POST /purchases`、`GET /entitlement`、`GET /payment-channels`。plans/orders/ledger 使用统一分页，钱包/订单/账本/权益只能读当前用户。购买请求 `{plan_id,expected_version,idempotency_key}`；充值 `{channel,amount,idempotency_key}`，金额**以元计**（`amount_cents` 仍兼容旧客户端，两者只能给一个）。先充值钱包，再余额购买，立即新周期/新有效期，旧事实不删除。

通道级手续费与汇率：`payments.json` 的每条通道可设 `fee_percent`（百分数，最多两位小数）与 `fee_fixed`（元），手续费**加在充值金额之上** —— 钱包到账仍是用户填写的金额，实付是 `到账 + 手续费`，两者分别记在订单的 `amount_cents` 与 `payable_cents/fee_cents` 上，回调按实付核对、按到账入账。`rate` 是「1 单位加密货币折多少人民币」，`crypto_currency` 指定币种，用于给用户折算应付的 USDT（订单里是 `payable_crypto`）：Cryptomus/BEpusdt 下单时只收人民币金额由网关换算，TokenPay 以 `BaseCurrency` 计价，所以**网关侧要配同一个汇率**，报价与实收才会一致。`GET /payment-channels` 会返回 `fee_percent/fee_fixed/crypto_currency/rate` 供前端报价。

`POST /orders/{id}/reconcile` 主动核对本人订单。`payment_uncertain` HTTP409 表示创建结果待核实，应保留原幂等键并查询订单；同键改金额或渠道冲突，不新建外部付款。`unsupported` HTTP422 表示该协议没有查单能力，不代表已付或失败。金额/订单号/币种验证与回调共用唯一入账事务。`POST /payments/{channel}/notify` 为供应商验签通知；EPay 还接受 GET。不得用浏览器返回页当作到账凭据。

服务模式同时运行后台核对：每轮最多20单，查单每次15秒超时，重试按30秒起步至1小时退避且持久保存。没有查单能力的协议不调度；未知或查单失败保留pending，未伪装成已付/失败。恢复旧备份后的缺失调度记录会有界补齐。服务退出取消worker，数据库关闭前等待退出；本机管理命令不启动worker。

Token 目前固定 `owner-resources` 范围：即使创建者是管理员，Bearer 请求也按普通用户资源权限处理；不提供管理员自动化权限或任意自定义 scope。

Token 明文只在**创建或重置**的那一次响应里出现（库里只有 SHA-256 摘要），列表只给 `prefix`（明文前 8 位）用于辨认是哪一把。丢失或泄露的唯一补救是重置（同一行换新密钥、旧密钥立刻失效）或撤销重发。有效期二选一：`expires_at`（一年以内）或 `permanent:true`；两者都不给或都给一律 400，避免"忘了填就发了一把不过期的钥匙"。永久凭据在库里写 `expires_at=0`。`last_used_at` 按分钟节流更新，供运营方判断哪把凭据还在被使用。管理员在后台创建账号后即可代发凭据交给用户（`POST /users/{id}/tokens`），账号列表页直接展示前缀、有效期与最近使用，可就地重置或撤销。

**规则与隧道**

监听地址、协议、所属用户、节点和组在创建后不可变；修改使用 `version` 乐观锁，迁移监听需删除并等待节点 ACK 释放端口。端口按节点/网络/端口保守独占，暂不支持同机按 IP 细分复用。

设备组屏蔽值分层：`network:tcp|udp`、`transport:direct|tls|ws|wss|http`、`app:http|socks`。兼容历史裸值，其中组的 `http` 指承载；规则仅允许应用协议进一步收紧。未知应用默认允许；不会检查未解密 HTTPS 路径。组的承载拒绝同时作用于链式每一跳。

加密规则的 `tunnel` 为 `{endpoint,server_name,token,chain?,mux?,reverse?}`。`chain` 最多两项，每项 `{transport,endpoint,server_name,token}`，加首出口共最多三出口；`mux=true` 复用 TLS 载波，`reverse` 使用出口主动建立的认证载波。服务端保存前检查重复地址与跳数，实际连接再检查出口稳定身份和白名单。列表与创建/修改返回都隐藏每跳 token；只在完整身份（承载/地址/证书名）不变时保留省略的旧 token。分配节点的配置接口才下发凭据。出口需配置证书、稳定唯一 ID 和下一跳白名单，见 [Agent 文档](../examples/agent-README.md)。

故障转移规则可附 `backends:[{target,weight,disabled}]`（TCP、最多16个、权重1–100）。Agent 使用加权调度；连接失败剔除，10秒健康检查恢复，只影响新连接。TLS 共享端口使用 `shared_tls:{parent_id,server_name}`；母子规则必须同账号/组/节点/监听地址，SNI 必须精确小写 DNS 名称，空/未匹配 SNI 关闭连接，客户端验证原始回源 TLS。

**历史探针**

历史返回 `{items:[],total}`，按 `sampled_at`（桶起点 UTC）升序，查询 `[from,to)`。默认 minute 与最近一小时；分钟最多查7天、小时最多查180天，超范围400。每项有 `resolution`、`samples`、`cpu_percent`、`memory_percent`、`disk_percent`、`load1`、`upload_bps`、`download_bps`；指标缺失为 null。每个指标按自己的有效样本数求平均，小时按分钟原始样本数加权，不平均“平均值”。没有采样的桶不返回，不补0；不存储公网地址。

聚合接受前一分钟迟到采样并按精确时间去重，因此分钟数据通常在桶结束后1–2分钟可查；当前小时逐步汇总。历史队列有8192桶上限，每节点每分钟最多120个不同时间采样，满时Agent上报返回503，持久化失败保留队列并记录日志；历史指标不是精确账单。正常重启可能丢失最近尚未完成的两分钟监控聚合，数据库故障期间重启还可能丢失待落库监控队列；精确计量独立使用Agent WAL/确认补传。实时订阅仍每5秒发布最新采样。

共享文件由主智能体维护；各领域扩展先沟通再合入。机器可读契约见 [OpenAPI](../internal/openapi/openapi.json)，接口服务为 `/api/v1/openapi.json`；最终验收边界见 [实施状态](implementation-status.md)。


**商业生命周期与运营（2026-09-21）**

`Plan` 增加 `active`、`version` 和 `kind=period|addon`。周期套餐 `months=1..120`；叠加包 `months=0`，价格上限100,000,000分。`POST /purchases` 可带 `expected_plan_version`；页面始终提交所见版本，报价变化返回409。旧API省略版本仍按成交时价格执行。周期套餐的升降级采用相同购买操作，立即替换周期且不折算旧套餐剩余时间。历史购买保留当时套餐名称、价格、配额、月数及版本；本轮前的旧购买无事后伪造快照。

| 方法/路径 | 请求 | 结果与权限 |
|---|---|---|
| PUT /plans/{id} | Plan，必须包含原 version，kind不可变 | 管理员，版本冲突409，编辑不修改已售权益 |
| PATCH /plans/{id} | `{active}` | 管理员停售/上架，版本递增 |
| GET /purchases | 无 | 本人最近100次周期购买快照 `{items}` |
| POST /addon-purchases | `{plan_id,expected_version,expected_plan_version,idempotency_key}` | 本人活跃权益；只加配额、不改周期；同键重放返回原结果 |
| GET/POST /auto-renew | POST `{enabled,plan_id}` | 本人设置；返回 enabled/plan_id，GET另含 last_error |
| POST /orders/{id}/close | `{}` | 关闭本人待支付订单；仍核对晚到支付，不冒充渠道关单 |
| POST /orders/{id}/refund | `{amount_cents,idempotency_key,reason}` | 管理员预留未花费余额，返回 Refund，状态 pending_external |
| GET /orders/{id}/refunds | 无 | 本人或管理员 `{items}`；本人不见内部凭证/操作者 |
| POST /refunds/{id}/resolve | `{completed,evidence}` | 管理员记录外部退款成功，或取消并释放预留余额 |
| GET/POST /redeem-codes | POST `{amount_cents,plan_id,max_uses,expires_at?}` | 管理员；金额/周期套餐至少一项，明文仅生成时返回；GET最近100条元数据 |
| DELETE /redeem-codes/{id} | 无 | 管理员停用；历史核销保留 |
| POST /redeem | `{code}` | 本人核销，同码同用户重试返回原结果；金额为字符串 |
| POST /referrals | `{}` | 本人生成唯一邀请码，明文仅本次显示 |
| POST /referrals/bind | `{code}` | 本人首次购买前永久绑定；拒绝自身和环 |
| GET /commissions | 无 | 本人最近100笔 `{items}`，初始 pending |
| GET/PUT /commission-policy | PUT `{rate_bps}` | 管理员；0..10000基点，默认0，100基点=1%，不足1分舍去 |
| POST /commissions/{id}/resolve | `{action:"settle"|"reverse",reason}` | 管理员审核结算/回冲，独立调整记录；已入账回冲余额不足则拒绝 |
| GET/POST /webhooks | POST `{url,events,format?}` | 本人最多8个订阅；返回签名 secret 仅本次 |
| DELETE /webhooks/{id} | 无 | 本人删除并撤销待投递；已在网络中的请求可能完成 |
| GET /events?limit=50 | 无 | 本人事件，最多100；payload为签名使用的原始JSON字符串 |
| GET /webhook-deliveries | 无 | 本人最近100条投递状态、尝试次数；不暴露签名密钥 |

自动续费默认关闭，在当前权益到期后按指定在售周期套餐执行，成功时间是新周期起点。余额不足等失败持久记录，1小时后再试。关闭/改配置、购买与兑换共用用户事务锁；停用账号不自动扣款。套餐编辑可能改变之后自动续费价格。

退款预留是账本 `refund_reserve` 扣减可用余额，不表示外部资金已经退还。`completed=true` 必须填写操作者核实的外部凭证才标记完成，订单按累计金额显示 `partially_refunded|refunded`；取消记 `refund_release`。旧有效支付回调在退款后重放不会再加余额。购买资金来源及退款/返佣分摊见下文；未提供自动商户退款、拒付或余额欠款处理。佣金不对流量叠加包计提。

Webhook 支持 `*` 或 `wallet.recharge|purchase|addon|redeem|commission|commission_reversal|refund_reserve|refund_release`（每个独立名称均加 `wallet.`）、`entitlement.purchased|redeemed|addon`、`refund.completed|canceled`。仅公网HTTPS，禁止认证URL、query、重定向和私网/保留地址；每次解析全部地址并校验，直接连接校验过的IP，正常TLS证书校验。事件JSON为 `{id,type,data}`，金额字符串。请求头 `X-TFP-Event-ID`、`X-TFP-Event`、`X-TFP-Signature: sha256=<hex>`；签名为用secret对完整UTF-8请求体计算HMAC-SHA256。接收方按事件ID去重；至少一次投递，不能用请求次数重复入账。

网络超时10秒，恢复认领5分钟，失败退避30秒至1小时，最多12次。事件/投递记录保留30天，财务账本独立保留。SQL outbox 与业务同事务，外部投递在提交后执行。

**规则任务与状态诊断**

Group 增加 `max_rules`，0不限，正数是该组每用户的规则上限；创建和导入在同一组锁内检查。套餐另有账号规则总数及每节点共享连接/IP/速率限制，详见下文。`GET /rules/{id}/diagnose` 返回 `{rule_id,node_id,desired_version,applied_version,checks,generated_at}`，只检查控制面记录，普通用户不返回整个Node对象或原始Agent错误。

| 方法/路径 | 请求 | 结果 |
|---|---|---|
| POST /tasks/rules/import | `{rules,idempotency_key,mode?}` | 202 Task；1..1000条，最多1MiB |
| POST /tasks/rules/export | `{rule_ids?,idempotency_key}` | 202 Task；指定最多500个ID，省略导出最多10000条 |
| GET /tasks | 无 | 本人最近100条元数据 `{items}` |
| GET /tasks/{id} | 无 | 本人或管理员 Task；普通身份不能读取原管理员任务 |
| POST /tasks/{id}/cancel | `{}` | 202；请求取消，保留已完成导入项 |

Task `{id,kind,status,result?,error?,created_at,updated_at}`；result是JSON字符串。导出结果 `{version:1,items:Rule[]}` 隐藏租约和所有隧道凭据。导入结果 `{items:[{index,rule?|error?}]}`，每条成功/失败与检查点持久化；重启从检查点恢复。任务状态 `pending|running|completed|completed_with_errors|canceled|failed`。每用户最多10个未完成任务；单次运行4分钟超时，失联认领5分钟后恢复，认领令牌防旧工作进程继续写入。完成后清除导入载荷，结果7天后清理，任务幂等记录保留；同键不同请求409。导入支持 `create`（默认）和 `update_by_port`，页面使用服务器预览后提交；更新需携带预览所得版本，陈旧版本拒绝。

Cyber已按用户指示从支付渠道列表及本轮验收中排除。

### 机器可读契约与套餐限制

`GET /api/v1/openapi.json` 返回OpenAPI 3.1，源文件为 `internal/openapi/openapi.json`；执行 `go generate ./internal/openapi` 再生成，CI检查路由覆盖和生成漂移。

套餐创建/编辑可带 `limits`：`max_rules`（0..100000）、`max_connections_per_node`、`max_ips_per_node`（0..1000000）、`bytes_per_second_per_node`（0..1000000000000的十进制字符串，B/s）。0表示无商业限制；叠加包只能填写全0。购买/兑换保存权益限制快照，权益和Agent租约返回 `limits`。规则总数按账号跨节点统计（包括停用）；其余限制按账号在单个节点所有规则合计。IP按活动TCP连接/UDP会话来源地址去重，IPv4映射地址归一化。TCP等待带宽令牌，UDP超速丢包；突发量/计数释放时间见实施状态。套餐降级按ID顺序保留前N条可下发规则，不删除原规则。

Agent必须声明 `resource-limits-v1` 才会收到非零限制的规则；`POST /agent/ack` 可附带 `capabilities` 更新本机能力（最多64项、每项64字符）。能力变化会生成新配置版本，必须再次拉取与ACK；它只影响节点自己的配置下发，不能提升用户或管理权限。

### 状态告警与通知控制

`GET /alert-policy` 供登录用户读取阈值，`PUT /alert-policy` 仅管理员Cookie会话可写，字段为 `enabled`、`offline_seconds`（90..3600）、`recovery_seconds`（5..600）、`expiry_hours`（1..720）、`remaining_percent`（1..50）、`version`；旧版本返回409。默认值依次为true/90/15/72/10/0，首次保存变为版本1。

新增事件：`node.offline`、`node.recovered`、`entitlement.expiring`、`entitlement.expired`、`entitlement.low_quota`、`alerts.policy_updated`。节点变更按持久状态去重，恢复需持续等待并收到后续心跳；启动后的离线宽限避免把面板重启直接误报为节点故障。权益三类提醒每周期各一次。事件金额/流量为十进制字符串；节点事件只含ID、名称和观察时间。查询及投递按当前授权过滤；普通资源Token不能继承管理员全局节点范围。

`PUT /webhooks/{id}` 接收 `{events,enabled,muted_until,version}`，`muted_until` 为UTC RFC3339或null，最多未来30天。订阅列表新增 `muted_until` 和 `version`（从1起）。修改不改变地址/签名密钥；停用、静默或移除某类事件会终止相应待投递任务，解除后不补发历史。投递状态新增 `suppressed` 和 `access_revoked`，在途请求可能已发出。所有者不匹配404，版本冲突409。系统状态告警每10秒分批扫描，超过1000节点/1000当前权益时分页轮转。


### 站点、出口与网络诊断

| 方法/路径 | 请求 | 结果与权限 |
|---|---|---|
| GET /site | 无 | 公开 SiteSettings；默认关闭注册；金额与充值区间以元返回 |
| PUT /site | SiteSettings，原 version | 管理员版本化保存；名称/公告为纯文本 |
| GET /auth/captcha | 无 | 一次性图片验证码；3分钟有效，失败尝试也消费 |
| POST /auth/register | `{username,password,invite?,captcha_id?,captcha_answer?}` | 普通账号注册，受开放/邀请模式及验证码控制 |
| POST /registration-invites | `{}` | 管理员创建一次性注册邀请，7天有效 |
| GET/POST /exits | POST Exit | 登录用户仅见授权组的脱敏元数据；创建仅管理员 |
| PUT /exits/{id} | Exit，原 version | 管理员更新受管出口 |
| POST /tasks/rules/preview | `{rules,mode?}` | 模拟同一导入事务并全部回滚；逐项 action/rule/error |
| POST /rules/{id}/network-diagnostic | `{}` | 当前规则所有者/管理员创建有限 TCP 连通性诊断 |
| GET /diagnostics/{id} | 无 | 任务所有者/管理员查询脱敏结果 |
| POST /agent/diagnostics | 无 | 节点身份认领当前有效配置的诊断任务 |
| POST /agent/diagnostics/result | 任务 id/claim/checks | 节点身份回执，复核认领令牌 |
| GET /purchases/{id}/funding | 无 | 购买所有者/管理员读取来源分摊 |
| POST /purchases/{id}/refund | `{amount_cents,idempotency_key,reason}` | 管理员购买退款，恢复原来源余额、移除可退配额并回冲佣金 |

SiteSettings 包含 `version,name,announcement,registration,captcha,accent,payments_enabled,currency,minimum_recharge,maximum_recharge,diagnostics_enabled,diagnostics_per_minute,geo_lookup_url`。**对外金额单位是元**（`"1.00"`，最多两位小数的十进制字符串），结算币种 `currency` 固定为 `CNY`；账本内部仍按整数分记账，换算只在边界发生。旧文档里的 `minimum_recharge_cents/maximum_recharge_cents` 仍能读回并自动换算成元，保存时不再写出。注册为 `closed|open|invite`；诊断每分钟1..30次。充值限制按**到账金额**控制新订单，既有回调/查单继续。`geo_lookup_url` 是探针位置图标的地区查询模板，必须 HTTPS 且含 `{ip}`，留空即关闭。开启验证码后，登录与注册都提交 `captcha_id,captcha_answer`。

Rule 新增 `exit_group_id`、`exit_id`（具体ID或 `auto`），服务端生成 `selected_exit_id,billing_multiplier,exit_unavailable`。受管模式不公开底层 tunnel；授权组过滤、节点心跳、入口/出口协议策略和防同节点选路均在控制面校验。`auto` 使用加权稳定选择，路由改变重新下发并撤销旧租约。

Rule 的 `proxy_protocol:{accept,send,trusted_cidrs}` 支持 `off|v1|v2`；接收需至少一个可信 CIDR。仅 TCP，接收不能与共享 TLS 配置组合。Agent 需声明 `proxy-protocol-v1`；头部不计业务流量，合法原始来源地址参与 IP 限制。

Agent 诊断需 `diagnostics-v1`，只探测该节点当前有授权、已启用且租约有效的 TCP 规则目标，不接受任意地址；执行最多8秒、排队2分钟过期、每节点最多100个待处理任务。重新派发前检查规则版本、账号及组策略。该功能不提供 UDP 数据探测、traceroute 或通用 Looking Glass。

购买退款按累计比例计算配额和佣金回冲，不支持扣回已使用/预留配额；已结算佣金余额不足则整笔回滚。旧购买无来源分摊时不能自动退款。外部支付退款仍由管理员凭实际商户退款证据确认。`Commission.refunded_cents` 返回已回冲金额字符串，手工结算/回冲只处理剩余部分。

通知 `format` 为 `webhook`（默认）、`feishu` 或 `discord`。飞书仅允许 `https://open.feishu.cn/open-apis/bot/v2/hook/…`，Discord 仅允许 `https://discord.com/api/webhooks/…`；保留原 HTTPS/DNS/重定向限制、outbox 重试和权限复核。聊天消息只含事件类型及编号，Discord 禁用 mentions，飞书检查响应业务码；HMAC 对实际投递体签名。事件新增 `wallet.purchase_refund`。
