# 实施契约 v1

时间用 UTC RFC3339；金额/累计字节使用十进制字符串，采样速率 `upload_bps/download_bps` 为 JSON number（单位 bytes/s），ID 为不透明字符串。接口前缀 `/api/v1`。成功对象直接返回 JSON；分页列表 `{items:[],total:0}`；错误一般为 `{code,error,request_id?}`，部分鉴权错误仍为 HTTP 文本。分页 query 为 `page` (1 起)、`page_size` (默认 20，最多 100)；未实现通用 `q` 搜索。

浏览器使用 HttpOnly Cookie；修改请求 `X-Requested-With: fetch` 并核对 Origin。同源 fetch。自动化采用独立 Bearer Token。登录/注册节点也限制请求大小和速率。除专门支付通知外严格拒绝未知 JSON 字段。

| 方法/路径 | 请求 | 返回/权限 |
|---|---|---|
| POST /auth/login | `{username,password}` | `{user: User}` + Cookie |
| GET /auth/session | 无 | `{user: User}`；未登录 401 |
| POST /auth/logout | `{}` | 204 |
| POST /auth/password | `{current_password,password}` | 204；撤销旧会话和 Token |
| GET /auth/tokens | 无 | 当前用户 Token 元数据，不返回明文/哈希 |
| POST /auth/tokens | `{name,expires_at}` | `{id,token,expires_at,scope}`；仅创建时返回明文，最长一年 |
| DELETE /auth/tokens/{id} | 无 | 204；只能撤销本人 Token |
| GET /users | 分页 | 管理员 User 列表 |
| POST /users | `{username,password,role}` | 管理员创建 |
| PUT /users/{id}/status | `{disabled}` | 管理员停用/启用，保留最后一个管理员 |
| GET /groups | 分页 | 授权 Group 列表 |
| POST /groups | `{name,user_ids,blocked_protocols,multiplier,port_min,port_max}` | 管理员 Group |
| PUT /groups/{id} | 同上加 version | 管理员乐观锁 |
| GET /nodes | 分页 | 授权 Node 列表 |
| POST /nodes/enrollment | `{name,group_ids}` | 管理员 `{token,expires_at}`，仅展示一次 |
| POST /nodes/{id}/rotate-token | `{}` | 管理员；返回新凭据，旧节点凭据失效 |
| GET /rules | 分页 | 管理员全局/用户自有 Rule；无 tunnel.token、chain各跳token、lease |
| POST /rules | `{name,node_id,group_id,network,transport,listen,target,enabled,tunnel?}` | Rule；管理员可指定 user_id |
| PUT /rules/{id} | 规则字段加 version | 乐观锁 |
| DELETE /rules/{id}?version=N | 无 | 204；待节点确认解绑才释放端口 |
| GET /probes | 无 | `{items: Probe[]}`；授权过滤，普通用户隐藏 public_ips |
| GET /probes/events | 无 | SSE `event: probes` + 同上 JSON，每 5 秒重校验身份/授权 |
| GET /probes/{node_id}/history | `resolution=minute\|hour&from=RFC3339&to=RFC3339` | 授权节点聚合历史；无权限与节点不存在均404 |
| GET /audit | 分页 | 管理员审计列表 |
| GET /health | 无 | `{status,database,version}`，不含 DSN |
| POST /agent/register | Registration | Registered |
| GET /agent/config | 节点 Bearer | Config；最长 24 小时并受有效期/配额限制 |
| POST /agent/ack | Ack | 204，防旧版本覆盖 |
| POST /agent/probe | Probe | 204，身份绑定 node_id |
| POST /agent/usage | UsageBatch | `{accepted: [id...]}`；持久化后确认 |
| POST /agent/leases/retire | `{lease_id,used_bytes}` | 204；最终用量必须已全部结算，重复相同退租安全 |

共享 Go 类型见 `internal/contract/types.go`。探针实时接口先实现 SSE；WebSocket 作为后续相同权限语义传输适配，不能混称隧道 WSS。

已挂载商业接口：`GET/POST /plans`（创建仅管理员）、`GET /wallet`、`GET /ledger`、`GET /orders`、`POST /orders`、`POST /purchases`、`GET /entitlement`、`GET /payment-channels`。plans/orders/ledger 使用统一分页，钱包/订单/账本/权益只能读当前用户。购买请求 `{plan_id,expected_version,idempotency_key}`；充值 `{channel,amount_cents,idempotency_key}`。先充值钱包，再余额购买，立即新周期/新有效期，旧事实不删除。

`POST /orders/{id}/reconcile` 主动核对本人订单。`payment_uncertain` HTTP409 表示创建结果待核实，应保留原幂等键并查询订单；同键改金额或渠道冲突，不新建外部付款。`unsupported` HTTP422 表示该协议没有查单能力，不代表已付或失败。金额/订单号/币种验证与回调共用唯一入账事务。`POST /payments/{channel}/notify` 为供应商验签通知；EPay 还接受 GET。不得用浏览器返回页当作到账凭据。

Token 目前固定 `owner-resources` 范围：即使创建者是管理员，Bearer 请求也按普通用户资源权限处理；不提供管理员自动化权限或任意自定义 scope。

**规则与隧道**

监听地址、协议、所属用户、节点和组在创建后不可变；修改使用 `version` 乐观锁，迁移监听需删除并等待节点 ACK 释放端口。端口按节点/网络/端口保守独占，暂不支持同机按 IP 细分复用。

设备组屏蔽值分层：`network:tcp|udp`、`transport:direct|tls|ws|wss|http`、`app:http|socks`。兼容历史裸值，其中组的 `http` 指承载；规则仅允许应用协议进一步收紧。未知应用默认允许；不会检查未解密 HTTPS 路径。组的承载拒绝同时作用于链式每一跳。

加密规则的 `tunnel` 为 `{endpoint,server_name,token,chain?}`。`chain` 最多两项，每项 `{transport,endpoint,server_name,token}`，加首出口共最多三出口；TLS/HTTP endpoint 使用 host:port，WS/WSS 使用对应 URL。服务端保存前检查重复地址与跳数，实际连接再检查出口稳定身份和白名单。列表与创建/修改返回都隐藏每跳 token；只在完整身份（承载/地址/证书名）不变时保留省略的旧 token。分配节点的配置接口才下发凭据。出口需配置证书、稳定唯一 ID 和下一跳白名单，见 [Agent 文档](../examples/agent-README.md)。

**历史探针**

历史返回 `{items:[],total}`，按 `sampled_at`（桶起点 UTC）升序，查询 `[from,to)`。默认 minute 与最近一小时；分钟最多查7天、小时最多查180天，超范围400。每项有 `resolution`、`samples`、`cpu_percent`、`memory_percent`、`disk_percent`、`load1`、`upload_bps`、`download_bps`；指标缺失为 null。每个指标按自己的有效样本数求平均，小时按分钟原始样本数加权，不平均“平均值”。没有采样的桶不返回，不补0；不存储公网地址。

聚合接受前一分钟迟到采样，因此分钟数据通常在桶结束后1–2分钟可查；当前小时逐步汇总。历史队列有8192桶上限，满时 Agent 上报返回503，持久化失败保留队列并记录日志；历史指标不是精确账单。正常重启可能丢失最近尚未完成的两分钟监控聚合，数据库故障期间重启还可能丢失待落库监控队列；精确计量独立使用 Agent WAL/确认补传。实时订阅仍每5秒发布最新采样。

共享文件由主智能体维护；各领域扩展先沟通再合入。当前为手工接口文档，完整 OpenAPI schema、异步批量任务等仍见 [待办状态](implementation-status.md)。
