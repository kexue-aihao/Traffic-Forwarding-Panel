# 实施契约 v1

时间用 UTC RFC3339；金额/字节使用十进制字符串，ID 为不透明字符串。接口前缀 `/api/v1`。成功对象直接返回 JSON；列表 `{items:[],total:0}`；错误 `{code,error,request_id}`。列表 query 为 `page` (1 起)、`page_size` (默认 20，最多 100)、`q`。

浏览器使用 HttpOnly Cookie；修改请求 `X-Requested-With: fetch` 并核对 Origin。同源 fetch。自动化采用独立 Bearer Token。登录/注册节点也限制请求大小和速率。除专门支付通知外严格拒绝未知 JSON 字段。

| 方法/路径 | 请求 | 返回/权限 |
|---|---|---|
| POST /auth/login | `{username,password}` | `{user: User}` + Cookie |
| GET /auth/session | 无 | `{user: User}`；未登录 401 |
| POST /auth/logout | `{}` | 204 |
| GET /users | 分页 | 管理员 User 列表 |
| POST /users | `{username,password,role}` | 管理员创建 |
| GET /groups | 分页 | 授权 Group 列表 |
| POST /groups | `{name,user_ids,blocked_protocols,multiplier,port_min,port_max}` | 管理员 Group |
| PUT /groups/{id} | 同上加 version | 管理员乐观锁 |
| GET /nodes | 分页 | 授权 Node 列表 |
| POST /nodes/enrollment | `{name,group_ids}` | 管理员 `{token,expires_at}`，仅展示一次 |
| GET /rules | 分页 | 管理员全局/用户自有 Rule；无 tunnel.token/lease |
| POST /rules | `{name,node_id,group_id,network,transport,listen,target,enabled,tunnel?}` | Rule；管理员可指定 user_id |
| PUT /rules/{id} | 规则字段加 version | 乐观锁 |
| DELETE /rules/{id}?version=N | 无 | 204；待节点确认解绑才释放端口 |
| GET /probes | 无 | `{items: Probe[]}`；授权过滤，普通用户隐藏 public_ips |
| GET /probes/events | 无 | SSE `event: probes` + 同上 JSON，每 5 秒重校验身份/授权 |
| GET /audit | 分页 | 管理员审计列表 |
| GET /health | 无 | `{status,database,version}`，不含 DSN |
| POST /agent/register | Registration | Registered |
| GET /agent/config | 节点 Bearer | Config；最长 24 小时并受有效期/配额限制 |
| POST /agent/ack | Ack | 204，防旧版本覆盖 |
| POST /agent/probe | Probe | 204，身份绑定 node_id |
| POST /agent/usage | UsageBatch | `{accepted: [id...]}`；持久化后确认 |

共享 Go 类型见 `internal/contract/types.go`。探针实时接口先实现 SSE；WebSocket 作为后续相同权限语义传输适配，不能混称隧道 WSS。

商业接口由 D 实现，挂载前经主智能体同步：`GET/POST /plans`、`GET /wallet`、`GET /ledger`、`GET /orders`、`POST /orders`、`POST /purchases`、`GET /entitlement`、`GET /payment-channels`。购买请求 `{plan_id,expected_version,idempotency_key}`；充值 `{channel,amount_cents,idempotency_key}`。先充值钱包，再余额购买，立即新周期/新有效期，旧事实不删除。

共享文件由主智能体维护；各领域扩展应先沟通再合入。以上为实现契约，不是所有端点均已完成的声明。
