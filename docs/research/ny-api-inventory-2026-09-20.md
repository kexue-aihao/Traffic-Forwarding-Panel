**ny 面板接口实测清单 · 2026-09-20**

目标：https://alpha.argoa.org；页面显示版本：20260728。以下为参考产品的接口观察记录，不是新产品已经实现的 API 契约。

采集方式：浏览器 HTTP 请求/响应与 WebSocket 帧结构。管理员会话；实际执行登录、退出、页面查询及只读 GET，请求载荷仅保留字段与类型。未保存任何业务表单。

记录 67 次 HTTP 响应，去重为 31 个“方法 + 路径”，另有 1 个 WebSocket 路径、5 条接收帧。重复响应来自页面查询及局部复核，并非 67 个接口。

| 方法 | 路径 | HTTP 状态 | 业务 code | 查询参数名 |
|---|---|---|---|---|
| GET | `/api/v1/admin/aff/log` | 200 | 0 | page, size |
| GET | `/api/v1/admin/devicegroup` | 200 | 0 | — |
| GET | `/api/v1/admin/devicegroup/folder` | 200 | 0 | — |
| GET | `/api/v1/admin/kv/device-offline-notify-config` | 404 | 404 | — |
| GET | `/api/v1/admin/kv/invite_config` | 404 | 404 | — |
| GET | `/api/v1/admin/kv/payment_info` | 200 | 0 | — |
| GET | `/api/v1/admin/kv/site_notice` | 200 | 0 | — |
| GET | `/api/v1/admin/kv/telegram-bot-config` | 200 | 0 | — |
| GET | `/api/v1/admin/shop/order` | 200 | 0 | page, size |
| GET | `/api/v1/admin/shop/plan` | 200 | 0 | — |
| GET | `/api/v1/admin/shop/redeem` | 200 | 0 | page, size |
| GET | `/api/v1/admin/statistic` | 200 | 0 | top_users |
| GET | `/api/v1/admin/user` | 200 | 0 | page, size |
| GET | `/api/v1/admin/usergroup` | 200 | 0 | — |
| GET | `/api/v1/guest/kv/site_info` | 200 | 0 | — |
| GET | `/api/v1/system/info` | 200 | 0 | — |
| GET | `/api/v1/system/info/queue` | 200 | 0 | — |
| GET | `/api/v1/system/node/status` | 200 | 0 | — |
| GET | `/api/v1/user/aff/config` | 200 | 0 | — |
| GET | `/api/v1/user/devicegroup` | 200 | 0 | — |
| GET | `/api/v1/user/forward` | 200 | 0 | page, size |
| GET | `/api/v1/user/forward/folder` | 200 | 0 | — |
| GET | `/api/v1/user/info` | 200 | 0 | — |
| GET | `/api/v1/user/kv/site_notice` | 200 | 未记录 | — |
| GET | `/api/v1/user/notification/settings` | 200 | 0 | — |
| GET | `/api/v1/user/shop/order` | 200 | 0 | page, size |
| GET | `/api/v1/user/shop/payment_info` | 200 | 0 | — |
| GET | `/api/v1/user/shop/plan` | 200 | 0 | — |
| GET | `/api/v1/user/statistic` | 200 | 0 | — |
| POST | `/api/v1/auth/login` | 200 | 未记录 | — |
| POST | `/api/v1/auth/logout` | 200 | 0 | — |

WebSocket：`wss://alpha.argoa.org/api/v1/system/node/status_ws`，查询参数名 `token`，凭据值未保留。新旧探针均观察到推送帧。该接口是探针状态订阅，不是转发隧道协议的证据。

**返回格式及使用限制**

- 常见返回封装为 `{ code, data, msg }`；本轮分页响应为包含 `code`、`count`、`data` 的对象，不能把总数字段假定为 `total`。业务成功通常为 `code = 0`。
- `Authorization` 请求头实际存在；前端把登录返回值原样放入该请求头，不能擅自假设它是 `Bearer` 格式。
- 部分 KV 和设备组 `config` 是 JSON 编码的字符串，需要二次解析。
- 余额、价格、倍率等字段观察到字符串表示；新产品应明确精度、币种和舍入规则。
- `invite_config` 与 `device-offline-notify-config` 两项 KV 的 GET 返回 404。单次结果不能证明对应模块不存在，也不能据此断言后端故障。
- 普通用户的权限隔离没有实测；不能将管理员访问 `/user/*` 的结果当作普通用户的权限证据。
- 空数组没有提供元素结构；字段存在不代表必填。采集器有数组采样与深度限制，不能直接作为完整 OpenAPI 定义。

完整字段路径与类型见 [脱敏结构证据](ny-api-evidence-2026-09-20.json)。

**主要对象与字段含义**

| 对象 | 实测字段示例 | 对新产品的意义 |
|---|---|---|
| 用户 | admin、banned、group_id、plan_id、expire、max_rules、speed_limit、ip_limit、connection_limit、traffic_enable、traffic_used、balance、auto_renew | 身份、授权、套餐权益与计费状态需保持一致 |
| 设备组 | id、type、ratio、enable_for_gid、connect_host、port_range、config、traffic_used；管理员响应另含 token | 分开建模组、机器和权限；用户列表与管理员列表使用不同字段集 |
| 套餐 | type、price、multiple、hide、group_id、traffic、max_rules、speed_limit、ip_limit、connection_limit | 套餐快照、变更推送与现有用户权益需有明确规则 |
| 订单 | type、uid、amount、status、order_no、open_time、paid_time、message | 精确金额与支付状态机；时间单位、时区和状态迁移还需验证 |
| 兑换码 | code、count、plan_id、discount_ratio | 核销次数、折扣与套餐绑定；并发兑换未验证 |
| 通知订阅 | uid、msg_type、channel、mode、list | 按事件与通道配置接收策略 |
| 探针 | 组下 servers 数组，含 handle、online、ip4/ip6、system_state、system_info、last_seen、last_pull、weight | 机器会话与监控数据独立于设备组配置 |

金额和比例的字符串表示、流量的大数表示、配置中的 JSON 字符串均需在新产品 API 中明确契约。这里的观察不直接决定新产品的数据表结构。

**前端发现、尚未执行验证的接口**

下表来自本次站点提供的公开脚本。它们没有计入上方 31 个实测 HTTP 接口；表单、调用代码或参数名的存在，不代表后端功能、权限或副作用已经验证。`{id}` 等为路径占位符，不是真实资源 ID。

| 方法 | 路径或接口族 | 用途 / 已知请求体 |
|---|---|---|
| POST | `/api/v1/auth/register` | 注册；存在用户名、密码、验证码及邀请代码相关前端逻辑 |
| POST | `/api/v1/user/renew` | 续费当前套餐 |
| POST | `/api/v1/user/update_column` | `{ column, value }`；需要验证后端允许修改的字段白名单 |
| POST | `/api/v1/user/reset_password` | 改密；会话失效行为待验证 |
| PUT | `/api/v1/admin/user` | `{ username }` 创建用户 |
| POST | `/api/v1/admin/user/{uid}` | 更新用户字段 |
| DELETE | `/api/v1/admin/user` | `{ ids }` 删除用户 |
| GET / DELETE | `/api/v1/admin/user/delete_unused`、`/delete_unused_rules` | 第二个完整路径为 `/api/v1/admin/user/delete_unused_rules`；预览/执行清理 |
| PUT / DELETE | `/api/v1/admin/devicegroup` | 创建设备组 / `{ ids }` 删除；创建可带 `folder_id` |
| POST | `/api/v1/admin/devicegroup/{gid}` | 更新设备组配置 |
| POST | `/api/v1/admin/devicegroup/{gid}/reset_token` | 重置节点接入凭据 |
| POST | `/api/v1/admin/devicegroup/reset_traffic` | `{ ids }` 重置设备组计数 |
| PUT / POST / DELETE | `/api/v1/user/devicegroup` 及 `/{gid}` | 用户自带出口的创建/更新/删除；还存在对应重置 token/流量调用 |
| PUT / DELETE | `/api/v1/user/forward` | 新建规则 / `{ ids }` 删除规则；创建可带 `folder_id` |
| POST | `/api/v1/user/forward/{id}` | 更新规则配置或状态 |
| POST | `/api/v1/user/forward/batch_create`、`/batch_update`、`/batch_change` | 后两项同样位于 `/api/v1/user/forward/` 下；批量规则处理 |
| POST | `/api/v1/user/forward/search_rules` | 规则搜索；虽是 POST，前端将其用于查询 |
| POST | `/api/v1/user/forward/reset_traffic` | `{ ids }` 重置规则计数 |
| POST | `/api/v1/user/forward/{id}/diagnose` | 发起规则诊断，可能产生节点网络流量 |
| GET / PUT / POST / DELETE | `/api/v1/admin/user/{uid}/forward...` | 管理员代管指定用户的规则接口族；具体后缀与用户规则调用对应 |
| PUT / DELETE / POST | `/api/v1/user/forward/folder` 及 `/bind` | 分组维护；绑定体为 `{ folder_id, item_ids }`；设备组存在管理员对应分组调用 |
| POST | `/api/v1/user/devicegroup/looking_glass` | `{ handle, method, target }`，本轮未触发 Ping |
| PUT | `/api/v1/admin/kv/{key}` | `{ value }`，value 是字符串形式配置 |
| PUT / POST / DELETE | `/api/v1/admin/shop/plan` 及 `/{id}` | 套餐创建/更新/删除 |
| POST | `/api/v1/admin/shop/plan/{id}/push` | 将选择的套餐属性推送给持有者 |
| POST | `/api/v1/user/shop/deposit` | `{ amount, gateway_name }` 发起充值 |
| GET | `/api/v1/user/shop/get_deposit/{id}` | 获取充值支付信息，实际路径参数业务含义需用测试订单确认 |
| POST | `/api/v1/user/shop/purchase` | `{ plan_id }` 使用余额购买套餐 |
| GET / POST | `/api/v1/user/shop/redeem?code=...` | 查询 / 使用兑换码；实际代码不写入日志或文档 |
| POST | `/api/v1/admin/shop/redeem/import` | 批量导入兑换码 |
| POST | `/api/v1/admin/shop/order/accounting` | 人工记账；金额、用户等字段由记账表单构造 |
| POST | `/api/v1/admin/shop/order/{id}/manual_callback` | 补单并触发后续事务 |
| POST | `/api/v1/user/aff/deposit` | `{ amount }` 佣金转余额 |
| PUT | `/api/v1/user/notification/settings` | 保存通知订阅 |
| POST | `/api/v1/user/telegram/bind` | 绑定，`unbind=1` 表示解绑；本轮未调用 |
| PUT | `/api/v1/system/node/weight/{gid}/{handle}` | `{ value }` 调整节点权重 |
| POST | `/api/v1/system/node/update` | `{ clientScript, target }` 请求节点更新 |
| POST | `/api/v1/system/node/kick/{target}` | 探针中的清理离线服务器调用；实际语义待服务端验证 |
| POST | `/api/v1/system/node/terminal/{handle}?type=...` | 请求远程终端会话；本轮未调用 |

上述路径用于功能研究，暂不生成兼容 SDK，也不把未验证接口发布为可用契约。新产品的 API 应按自己的权限、状态机、幂等与错误格式重新定义，再用实现和测试生成 OpenAPI 文档。
