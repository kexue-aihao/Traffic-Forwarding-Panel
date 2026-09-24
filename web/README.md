# 前端开发

Vue SFC + TypeScript strict + Vite；运行时依赖仅 Vue、vue-router、Inter。入口 `/` 与 `/admin` 使用同一构建、独立工作空间外壳和服务端会话身份；hash 路由不要求服务端补齐业务页面路径。

```sh
cd web
npm ci
npm run typecheck
npm run build
npm test
```

Node >=22.12，推荐 Node 24。浏览器回归需已安装 Playwright Chromium/Firefox/WebKit：`npx playwright install`。测试启动仅监听 localhost 的独立契约 fixture，使用生产 CSP，不连接真实支付、Agent 或数据库，因此不是后端端到端验收。覆盖登录、五档视口、脏对话框、API文本安全、指标未知状态、超出 JS 安全整数范围的金额、套餐确认、主题和会话失效；截图写 `.gocache/`。

构建输出 `internal/webui/assets` 必须随源代码提交，不手改产物。Vite 保留 `/assets/inter.woff2` 绝对 URL 的提示是预期行为；字体、主题脚本、favicon 与图标 sprite 在构建结束后发布。Go 托管层须支持 `/`、`/admin`、`/assets/*`，提供显式 MIME 和 `go:embed all:assets`。主题脚本是外部经典脚本，不放宽 CSP；防闪烁用的内联 `<style>` 由 `style-src 'unsafe-inline'` 覆盖，不涉及脚本。

设计令牌与姊妹面板 `Telegram_session_Adblock` 的 `theme.css` 对齐：表面五级、文字四级、描边三档、`--text-2xs`~`--text-3xl` 带配套行高、`--duration-*` 与三条缓动。明暗是一个轴、品牌色相是另一个轴，配色只移动品牌色与第一团环境光，表面与灰阶共享。默认暗色（画布 `#07080b`），`web/static/theme.js` 在 `<head>` 最前把它涂上。新增一套配色要同步三处：`App.vue` 的 `accents`、`static/theme.js` 的数组、`tokens.css` 的 `[data-accent]` 色相块。加一个图标要改 `vite.config.ts` 里的名字数组。

接口均请求 `/api/v1`；Cookie 同源；写请求携带 `X-Requested-With: fetch`。页面 GET 切换取消，认证 GET 不被路由启动取消，mutation 不自动重试或导航取消。SSE 订阅在页面退出时关闭，断网重连并显示陈旧采样；401 清理会话。

侧栏“API 列表”对管理员和普通用户开放，直接读取 `/api/v1/openapi.json`；列表支持关键词、方法、分类、调用身份筛选，并展示认证方式、参数、请求体、响应结构和原始定义。页面只提供查询便利，服务端接口鉴权仍按 OpenAPI 的 `x-role` 和 security 定义执行。

`npm run test:live` 编译真正 Go 面板并创建随机 SQLite 测试数据库和一次性管理员，在 `127.0.0.1:18081` 运行三浏览器联调。需要 Go，端口需空闲。测试账号/数据仅在 `.local/` 隔离数据库；服务在结束时停止。覆盖两身份登录、用户和组授权、接入凭据、模拟 HTTP Agent 注册及探针字段权限、零余额/购买余额不足、Token 隔离和撤销、改密与停用会话失效。Agent 上报是明确标记的模拟客户端，不作为真实转发或实付验收。

套餐、订单、账本分别使用 `plans_page`、`orders_page`、`ledger_page` hash query，并用 `router.replace` 更新，每页 20 条。支付创建结果不确定时保留相同幂等键、金额与渠道；同一标签页按账号保存未确认意图，避免导航或刷新丢失重试标识。无支付链接的 pending 订单显示“核实中”，提供服务端主动查单；不以返回 URL 判断到账。查单不受支持时明确提示，不显示付款成功。渠道状态统一中文，不展示商户密钥；商户配置由部署端管理。

`test:live` 生成 16 张真实后端界面截图：`.gocache/screens/{admin|user}-{overview|commerce|account|probes}-{desktop|mobile}.png`，分别为 1440×1000、320×800。截图使用隔离测试账号，探针节点明确名为 simulated，未知指标保持未知。桌面和移动端已人工抽查字体、长值换行、对比度、主滚动区及页面可达性；不将截图视为真实节点性能或真实支付验证。

探针卡片保留本次页面的实时短趋势；历史区域调用持久化API，提供CPU/内存/磁盘/负载/上下行六指标，最近1小时、24小时、7天、30天、180天。分钟保留7天、小时保留180天；当前小时包含已落库分钟，最近1–2分钟稍后可见。缺测断线、未知不补0，图表可通过键盘逐点读取；节点/范围/指标用hash query及`router.replace`保存。可查看当前授权但离线的节点。

历史fixture回归覆盖空值/零值/缺桶、取消GET与迟到响应、失败重试、节点分页、URL刷新恢复。live脚本明确向一次性SQLite库写聚合夹具，验证真实Go API、权限过滤和图表；Agent聚合/持久化正确性由Go测试验证。新增截图为`.gocache/screens/history-{desktop,mobile,mobile-chart}.png`。

当前能力边界：节点/组/用户选择器和API Token自动分批读取，资源列表与商业列表有分页。支付平台配置编辑、账号自助恢复、自定义多角色和链式拓扑表单尚未提供。通道状态由服务端返回，实付接入需要独立渠道验证。

设备组列表支持管理员删除操作，确认弹窗说明设备解绑、授权解除和接入命令失效。正在被规则或其他设备组引用时显示服务端的具体阻止原因；节点尚未确认已删除规则停止时，须等待确认后重试。成功后刷新列表，删除当前页最后一项时返回上一页。设备、租约和流量历史保留。

设备组的额外设置放在列表操作中的“高级设置”，基础表单不再堆放协议、TLS、UDP 等控件。独立 JSONC 编辑器提供带注释的参考模板、格式化和可折叠参数说明，适配桌面及窄屏。`test:live` 覆盖注释解析、错误输入、白名单冲突、嵌套 TLS 对象、零值和修改基础信息后的高级设置回读；界面截图为 `.gocache/screens/group-advanced-{desktop,mobile}.png`，展开说明截图为 `group-advanced-help-{desktop,mobile}.png`。组级参数的执行边界见 [API 契约](../docs/api-contract.md)。

字体许可证与 Lucide 许可证随本地产物发布。`lucide-static` 仅作为构建时依赖，输出选定 sprite，不加载图标运行时。普通用户的实际权限与探针字段过滤以服务端为准，隐藏菜单不作为权限边界。

管理员侧栏“身份用户组”支持新建、编辑和删除身份组，并显示唯一 ID、用户数和授权设备组数。新建时手动填写 ID（1–64 位字母、数字、下划线或短横线）；编辑可修改 ID 和名称，关联用户和设备组在同一事务内更新，原访问权限保留。用户管理在创建账号和修改身份组时选择身份用户组 ID；设备组通过 `identity_group_ids` 授权，同组用户共享设备组访问权限，账号角色及个人规则、套餐和余额仍各自独立。被用户或设备组引用的身份组须先解除关联再删除。升级时将旧逐用户授权迁移为各用户的独立身份组，保留原访问范围；管理员可随后合并分组。`test:live` 覆盖手动创建、编辑 ID、重复 ID 拒绝、分配、会话内换组、删除限制和解除引用后删除，截图为 `.gocache/screens/identity-groups-{desktop,mobile}.png`。

所有产品日期由`core/format.ts`固定使用`Asia/Shanghai`并注明“上海时间 UTC+8”，不跟随浏览器所在时区。Token有效天数为每24小时，仍向API发送UTC ISO时间。fixture强制纽约时区验证冬夏、UTC跨日、历史日期/时间轴和Token过期请求；未使用本地日期输入暗中换算时区。
