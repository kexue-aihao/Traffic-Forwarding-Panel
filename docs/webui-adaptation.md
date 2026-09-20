# 前端标准适配

M09 的原始来源为 `D:\Windows 10 Backup\桌面\WebUI-前端技术栈自述.md`，2026-09-20 保存 [逐字节副本](reference/webui-standard.md)。SHA256：`694F050FBD297A7BF8AA3719349AE2F738DE0F3CDAD5DBD7F59AEE3F553D0550`。

管理员与用户端共同遵循 Vue Composition API、预编译 `<script setup>` SFC、严格 TypeScript、Vite、Node ≥22.12（CI Node 24）。运行时仅 Vue、vue-router、自托管 Inter；手写 CSS 与组件、原生 fetch、reactive 状态和 SVG 图表；禁用组件框架、Tailwind、Pinia、axios 和图表依赖。

CSS 依次为 tokens、base、components；明暗与六套强调色相互独立，防闪烁脚本外置。固定视口外壳，主内容是唯一纵向滚动容器；320–1920 视口与 200% 缩放可操作。原生 dialog 管理键盘/焦点/脏表单，单一 motion 模块处理减弱动效、隐藏标签页和动画清理；高对比度/减少透明度可用。

Hash 路由，筛选/分页使用 router.replace；导航取消页面 GET，业务修改请求继续完成；统一 401 失效处理。每个页面必须呈现加载、空、错、无权限和陈旧状态，数据值作为文本渲染，禁止 v-html。

图标/字体和许可证全部随二进制发布，无 CDN。构建产物进入 internal/webui/assets，应用文件名固定 app.js/app.css，响应 no-store；Go embed `all:assets`，MIME 显式映射，生产 CSP 禁止内联脚本、eval/new Function。产物漂移检查包括未跟踪文件。

适配边界：源标准中的旧项目 legacy 渲染器、@ts-nocheck、Telegram 页面、预览账号、历史文件行数不是新产品功能，不复制。源 CI 对下划线文件遗漏的描述与 `all:assets` 不符，本项目以实际嵌入可访问性测试为准。Vue 标准运行时不带模板编译器。探针使用同源 SSE 起步，仍要求建立连接和每次推送都执行服务端权限过滤。

质量门禁由 A33–A36 定义。浏览器矩阵、离线资产、CSP、嵌入/MIME与真实API闭环应逐项记录结果；构建成功不能代替它们。
