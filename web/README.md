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

构建输出 `internal/webui/assets` 必须随源代码提交，不手改产物。Vite 保留 `/assets/inter.woff2` 绝对 URL 的提示是预期行为；字体和主题脚本在构建结束后发布。Go 托管层须支持 `/`、`/admin`、`/assets/*`，提供显式 MIME 和 `go:embed all:assets`。主题脚本是外部经典脚本，不放宽 CSP。

接口均请求 `/api/v1`；Cookie 同源；写请求携带 `X-Requested-With: fetch`。页面 GET 切换取消，认证 GET 不被路由启动取消，mutation 不自动重试或导航取消。SSE 订阅在页面退出时关闭，断网重连并显示陈旧采样；401 清理会话。

当前能力边界：探针趋势仅本次页面会话；节点/组/用户选择器自动分批读取，资源列表有分页。商业列表当前显示接口默认页，未实现分页和支付平台配置编辑。无改密、账号恢复、API Token、自定义多角色页面；不展示未接通功能为已完成。通道状态由服务端返回，支付返回 URL 不能视为到账。

字体许可证与 Lucide 许可证随本地产物发布。`lucide-static` 仅作为构建时依赖，输出选定 sprite，不加载图标运行时。普通用户的实际权限与探针字段过滤以服务端为准，隐藏菜单不作为权限边界。
