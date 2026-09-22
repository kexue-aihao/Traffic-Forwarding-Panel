# 前端标准适配

M09 的原始来源为 `D:\Windows 10 Backup\桌面\WebUI-前端技术栈自述.md`，2026-09-20 保存 [逐字节副本](reference/webui-standard.md)。SHA256：`694F050FBD297A7BF8AA3719349AE2F738DE0F3CDAD5DBD7F59AEE3F553D0550`。

管理员与用户端共同遵循 Vue Composition API、预编译 `<script setup>` SFC、严格 TypeScript、Vite、Node ≥22.12（CI Node 24）。运行时仅 Vue、vue-router、自托管 Inter；手写 CSS 与组件、原生 fetch、reactive 状态和 SVG 图表；禁用组件框架、Tailwind、Pinia、axios 和图表依赖。

CSS 依次为 tokens、base、components；明暗与六套强调色相互独立，防闪烁脚本外置。固定视口外壳，主内容是唯一纵向滚动容器；320–1920 视口与 200% 缩放可操作。原生 dialog 管理键盘/焦点/脏表单，单一 motion 模块处理减弱动效、隐藏标签页和动画清理；高对比度/减少透明度可用。

视觉系统与姊妹面板 `E:\Telegram_session_Adblock`（`pnpm dev` 跑在 5273）的 `theme.css` 逐项对齐：表面五级 `bg-0..4`、文字四级 `ink/-muted/-subtle/-faint`、描边三档白 alpha、`--text-2xs`~`--text-3xl` 带配套行高、`--radius-lg/xl/2xl/3xl`、`--space-1..10`、`--duration-micro/state/layout/page` 与两条缓动、玻璃 `--glass-*`、环境光 `--orb-a..d`。**默认暗色**（画布 `#07080b`），`static/theme.js` 在 `<head>` 最前涂色并同步 `theme-color`；防闪烁另有一段内联 `<style>`，落在 CSP 的 `style-src 'unsafe-inline'` 内，不涉及脚本。

下拉框走 `components/Select.vue`：原生 `<select>` 的**弹出层由操作系统绘制**，选中高亮用的是系统强调色（Windows 上是一条亮蓝），`appearance: none` 只能换掉收起来时的箭头，碰不到弹层；`select option` 上的底色与文字色在多数平台被忽略。
组件因此把原生 `<select>` **留在原位当那个控件**（标签、键盘、表单语义、辅助技术与自动化全部照旧走它），只在 `mousedown` 上 `preventDefault` 按掉原生弹层，换成自己画的列表；列表标 `aria-hidden`，它对屏幕阅读器没有意义。
**不采用「按钮 + 隐藏 select」的双控件写法**：同一个标签会匹配到两个元素，辅助技术也会读出两个控件。收起状态的箭头仍走 `--select-chevron`；复选框与滑块用 `accent-color` 跟随品牌色。

每个页面必须以单个 `<section class="page">` 为根：`<Transition>` 只认单根组件，多根会让过渡失效并把后续导航一起带坏（`Exits.vue` 与 `Settings.vue` 原先就是多根，顺带也没有页面最大宽度与居中）。`.page` 负责最大宽度与居中，内边距挂在外壳的滚动容器上。

不使用 `filter: blur()` 之外，还有两处**留白**：`value-pulse` 与 `rule-x` 没有采用 —— 前者的适用场景是「偶尔变一次、变化值得被指出」的数字，而探针每几秒推一次，每个数字都脉冲只会变成持续抖动；后者是渐变分隔线，本项目的页面里没有需要它的位置。两者连同一直冗余的 `.tnum` 已从样式表删除，避免留下没人用的词汇。

**扁平优先于描边**（这是产品方的取向，与参考面板不同）：参考面板的输入框、卡片都画 border-[var(--color-line)]，这里一律不画，层级只用底色差表达 —— 与侧栏导航项同一套语言（默认一层底 / hover 提亮）。
控件底色因此必须比所在表面**再亮一档**（bg-3 而非 bg-2）：对话框的玻璃基色恰好就是 bg-2，用 bg-2 当控件底色的话，去掉描边后控件在对话框里会完全消失。
保留的线只有「分隔线」这一类：表格行、表单分组（legend 的 border-top）、动作区、侧栏与顶栏的外缘。

与参考面板的三处**故意偏离**，改动前请先读这段：

1. **环境光 orb 不用 `filter: blur()`。** 参考面板对四团 orb 施加 `blur(72px)` 并在漂移时 `scale()`；半视口大小的模糊元素每帧重新栅格化，标准 §7.6 明确禁止。这里用多控制点 `radial-gradient`（0%/30%/52%/70%）逼近高斯衰减，漂移只动 `translate3d`。观感一致，代价是零重栅格化。
2. **配色是六套而不是一套。** 参考面板只有品牌蓝 + violet。这里明暗是一个轴、品牌色相是另一个轴，配色只移动 `--color-accent*`、品牌渐变第二色标与第一团 orb；表面、灰阶、语义三色共享。新增一套配色要同步三处：`App.vue` 的 `accents`、`static/theme.js` 的数组、`tokens.css` 的 `[data-accent]` 色相块。
3. **不跟随系统明暗偏好。** 参考面板在无存储偏好时读 `prefers-color-scheme`；这里固定暗色优先，只有用户在顶栏显式切换才变浅色。要让回跟随系统，改 `static/theme.js` 一处即可。

另有一处是当前实现**优于**参考：≤900px 时侧栏转成横向导航条，内容区拿回整行宽度；参考面板在 390px 下仍占 220px 固定列，正文只剩窄窄一条。卡片光标聚光（`--spot-color`）按参考做法实现，用 `@property` 注册 `--spot-x/--spot-y/--spot-alpha`，前两个 `inherits: false` 以免鼠标每动一像素都让整棵子树失效。

Hash 路由，筛选/分页使用 router.replace；导航取消页面 GET，业务修改请求继续完成；统一 401 失效处理。每个页面必须呈现加载、空、错、无权限和陈旧状态，数据值作为文本渲染，禁止 v-html。

图标/字体和许可证全部随二进制发布，无 CDN。构建产物进入 internal/webui/assets，应用文件名固定 app.js/app.css，响应 no-store；Go embed `all:assets`，MIME 显式映射，生产 CSP 禁止内联脚本、eval/new Function。产物漂移检查包括未跟踪文件。

适配边界：源标准中的旧项目 legacy 渲染器、@ts-nocheck、Telegram 页面、预览账号、历史文件行数不是新产品功能，不复制。源 CI 对下划线文件遗漏的描述与 `all:assets` 不符，本项目以实际嵌入可访问性测试为准。Vue 标准运行时不带模板编译器。探针使用同源 SSE 起步，仍要求建立连接和每次推送都执行服务端权限过滤。

质量门禁由 A33–A36 定义。浏览器矩阵、离线资产、CSP、嵌入/MIME与真实API闭环应逐项记录结果；构建成功不能代替它们。
