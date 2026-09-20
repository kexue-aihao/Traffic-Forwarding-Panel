# Web 管理面板 · 前端技术栈自述

> 对象：本仓库 Web 管理面板（`web/` 源码 + `internal/webui/` 托管层）
> 基线：v1.12.0（`master`，提交 `72fdc0c`）
> 整理日期：2026-09-20

---

## 目录

1. [一句话概括](#1-一句话概括)
2. [技术选型总览](#2-技术选型总览)
3. [构建链路与产物](#3-构建链路与产物)
4. [应用架构](#4-应用架构)
5. [路由与状态管理](#5-路由与状态管理)
6. [请求层](#6-请求层)
7. [样式与设计系统](#7-样式与设计系统)
8. [静态资源策略](#8-静态资源策略)
9. [与 Go 后端的集成及安全约束](#9-与-go-后端的集成及安全约束)
10. [测试与质量门禁](#10-测试与质量门禁)
11. [约束速查表](#11-约束速查表)
12. [附录 A：前端文件清单](#附录-a前端文件清单)
13. [附录 B：版本与依赖清单](#附录-b版本与依赖清单)

---

## 1. 一句话概括

**Vue 3 + TypeScript + Vite 的轻依赖 SPA；样式与组件全部手写（token 化 CSS + 原生 Web API）；构建产物提交进仓库并 `go:embed` 进 Go 二进制，在严格 CSP 与完全离线自托管的前提下运行。**

运行时依赖只有三个：`vue`、`vue-router`、`@fontsource-variable/inter`。

---

## 2. 技术选型总览

### 2.1 核心框架

| 领域 | 选型 | 版本 | 说明 |
| --- | --- | --- | --- |
| UI 框架 | Vue | 3.5.43 | 全部为 Composition API + `<script setup>` 单文件组件 |
| 路由 | vue-router | 5.3.1 | `createWebHashHistory()`，hash 路由 |
| 语言 | TypeScript | 5.9.3 | `strict` 全开，另有 `noUnusedLocals` / `noUnusedParameters` / `noImplicitOverride` / `noFallthroughCasesInSwitch` / `verbatimModuleSyntax` / `isolatedModules` |
| 类型检查 | vue-tsc | 3.3.11 | 连同 `tsc -p tsconfig.node.json` 覆盖构建配置 |
| 构建 | Vite | 8.3.0 | Rolldown 内核（锁文件中为 `@rolldown/binding-*`） |
| 构建插件 | @vitejs/plugin-vue | 6.0.9 | —— |
| 字体 | @fontsource-variable/inter | 5.3.0 | 自托管可变字体，无 CDN |
| Node | —— | >= 22.12 | `engines` 约束；CI 使用 Node 24 |

### 2.2 明确「没有用」的东西

| 类别 | 状态 | 替代方案 |
| --- | --- | --- |
| UI 组件库 | 无（Element Plus / Ant Design / Naive 等都没有） | 手写 SFC + 手写 CSS |
| CSS 框架 | 无（无 Tailwind / UnoCSS） | 原生 CSS 自定义属性 + 三层样式表 |
| 状态管理库 | 无（无 Pinia / Vuex） | `reactive()` 单例 + `markRaw` |
| 图表库 | 无 | 手写 SVG（`createElementNS`） |
| HTTP 客户端 | 无（无 axios） | 原生 `fetch` 封装 |
| 图标库运行时 | 无 | 本地 Lucide SVG sprite + `<use>` |
| 运行时模板编译 | 禁用 | 模板全部预编译为 SFC |
| ESLint / Prettier | 仓库内无配置 | 类型检查 + 浏览器回归套件兜底 |

> 说明：`web/src/legacy/legacy.ts` 与 `web/src/core/dom.ts` 顶部带有 `eslint-disable` 注释，但工具链中没有 ESLint，这些注释不构成实际门禁。

### 2.3 用到的原生 Web API

`<dialog>` 原生模态 · `AbortController` / `AbortSignal.any` · Web Animations API（`element.animate`）· `ResizeObserver` · `IntersectionObserver` · `matchMedia` · `Intl.NumberFormat` / `Intl.DateTimeFormat` · `localStorage` · `URLSearchParams` · DOM 快照克隆 · `inert`

---

## 3. 构建链路与产物

### 3.1 源码到二进制的链路

```
web/src/**            ──┐
web/index.html          ├─► vite build ─► internal/webui/assets/ ─► git 提交 ─► go:embed ─► Go 二进制
web/static/**（拷贝）  ─┘
```

产物**提交进版本库**，因此 `go build`、Dockerfile 与 CI 都不需要 Node 工具链；代价是改前端必须重新构建二进制或镜像才能生效。

### 3.2 `web/vite.config.ts` 关键配置

| 配置 | 值 | 原因 |
| --- | --- | --- |
| `root` | `web/` | —— |
| `base` | `/assets/` | 与 Go 服务注册的静态路由一致 |
| `publicDir` | `false` | 静态文件由自定义插件发布，URL 保持根绝对、不被改写成打包器 import |
| `build.outDir` | `../internal/webui/assets` | 直接写入嵌入目录 |
| `build.emptyOutDir` | `true` | 输出目录在项目根之外，默认不会清空，会累积陈旧 bundle |
| `build.cssCodeSplit` | `false` | 产出单一 `app.css`，Go 测试钉住了该文件名 |
| `build.assetsInlineLimit` | `0` | 禁止 data: URL —— CSP 的 `font-src` 回落到 `'self'`，内联字体会被拦 |
| `build.sourcemap` | `false` | 产物入库，不需要 |
| `build.modulePreload.polyfill` | `false` | 它是 Vite 默认唯一的行内 `<script>`，被 CSP 禁止 |
| `define` | 关闭 Options API、prod devtools、hydration mismatch 详情 | 减体积 |

自定义插件两个：

- **`publishStatic`** —— 在 `closeBundle` 阶段把 `web/static/` 下的手写文件（防闪烁脚本、图标 sprite、favicon 及其许可证）逐字节拷进产物目录。
- **`themeScript`** —— 通过 `transformIndexHtml`（`order: 'post'`）把 `/assets/theme.js` 以**经典脚本**注入 `<head>` 首位，保证它在首次绘制和模块 bundle 之前执行，同时避免 Vite 把 `/assets/theme.js` 当成资源 import。

固定输出名：`app.js`、`app.css`、`chunks/[name]-[hash].js`、`fonts/[name][extname]`。不做哈希是因为响应头是 `Cache-Control: no-store`，哈希没有收益，而 Go 测试钉住了 `app.js` / `app.css`。

### 3.3 npm 脚本

| 脚本 | 命令 | 用途 |
| --- | --- | --- |
| `build` | `vite build` | 写 `../internal/webui/assets`（入库） |
| `watch` | `vite build --watch` | 配合本地预览迭代 |
| `build:scratch` | `vite build --outDir ../.gocache/web-build` | 输出到被忽略目录，不污染入库产物 |
| `typecheck` | `vue-tsc --noEmit && tsc --noEmit -p tsconfig.node.json` | 类型检查 |

### 3.4 产物清单与体积

| 文件 | 大小 | 来源 |
| --- | --- | --- |
| `app.js` | ~133 KB | 打包后的 JS（含 Vue 运行时） |
| `app.css` | ~65 KB | 单一合并样式表 |
| `index.html` | ~0.8 KB | SPA 外壳 |
| `theme.js` | ~1.8 KB | 手写，防主题闪烁 |
| `icons.svg` | ~4.5 KB | Lucide 精选 sprite |
| `favicon.svg` | ~1.1 KB | 手写 |
| `inter-var-latin.woff2` | ~47 KB | 自托管可变字体 |
| `inter-LICENSE.txt` / `lucide-LICENSE.txt` | —— | 随产物分发，满足许可要求 |

### 3.5 字节稳定性

`.gitattributes` 把 `internal/webui/assets/**` 与 `web/static/**` 标记为 `-text`（禁止行尾转换），`web/package-lock.json` 同理。否则在 Windows 上用 `core.autocrlf` 检出会产出 CRLF，使 CI 的漂移检查（`git diff --exit-code`）全行报错。

---

## 4. 应用架构

### 4.1 目录职责

```
web/
├── index.html              入口 HTML：lang="zh-CN"、media 分域的 theme-color、favicon
├── env.d.ts                vite/client 引用 + *.vue 模块声明（给纯 tsc 与编辑器兜底）
├── vite.config.ts          构建配置 + 两个自定义插件
├── src/
│   ├── main.ts             7 行：createApp(App).use(router).mount('#app')
│   ├── App.vue             外壳（91 行）
│   ├── router.ts           路由表、导航守卫、过渡与请求轮换
│   ├── components/         Icon.vue（Lucide sprite 包装，10 行）
│   ├── core/               运行时原语，每个能力只有一份实现
│   ├── pages/              LoginView.vue、LegacyRoute.vue
│   ├── shell/              SideBar / TopBar / 装饰层 / 指示器 / 重挂载
│   ├── legacy/legacy.ts    未迁移的 5 个页面（命令式，885 行，@ts-nocheck）
│   └── styles/             tokens → base → components
└── static/                 不参与打包、逐字节拷贝的文件
```

### 4.2 `App.vue` 外壳

- 环境光晕层 `.ambient`：四个大 orb（蓝、蓝紫、紫、青），`aria-hidden`，为毛玻璃层提供有色彩倾向的移动背景。
- `skip-link`（跳到主要内容）→ `.layout` → `SideBar` + `.content`（`TopBar` + `<main id="view" tabindex="-1">`）。
- `#view` 内按状态三选一：启动骨架屏 → 启动错误 + 重试 → `LoginView` 或 `RouterView`。
- `RouterView` 用 `:key="route.path + ':' + pageReloadToken"` —— 换 key 即重挂载，这就是 legacy 渲染器「刷新本页」的语义。
- 挂载时拉 `/api/session` 决定登录态；`ResizeObserver` 监听导航容器，驱动指示器重算位置。
- 独立的 `#modal-root` 与 `#toast-region`（`role="status"`、`aria-live="polite"`）。

### 4.3 `core/` —— 单一实现原则

| 模块 | 职责 |
| --- | --- |
| `api.ts` | fetch 封装、页面级请求轮换、401 处理 |
| `dialog.ts` | 原生 `<dialog>` 模态：脏检查、保存中拦截、退场动画、焦点还原、`activeModalRef` |
| `dom.ts` | hyperscript 式 `el()`、`icon()`、`button()`、`cell()`、`table()`、`pagination()`、`busy()` 等命令式 DOM 原语 |
| `format.ts` | 数字/紧凑数字/UTC 时间格式化（**冻结**，测试钉住渲染文本） |
| `motion.ts` | 唯一的动画所有者：`play` / `reveal` / `clear` / `reduced`，负责清掉自己设的 `will-change` |
| `params.ts` | hash 路由适配层 + `hashURL()` + 每个路由的 query 记忆 |
| `stores.ts` | `reactive()` 单例面板状态 |
| `theme.ts` | 明暗主题与六套配色的读写、`localStorage` 持久化 |
| `toast.ts` | toast 与 API `warning` 信封的中文化呈现 |
| `types/api.ts` | API 响应类型 |

设计意图写在注释里：**每个能力只有一份实现**，Vue 页面与尚未迁移的命令式页面共用同一套规则。

### 4.4 渐进式迁移架构（本项目最特殊的一点）

- 路由表里 5 个业务页（`dashboard` / `rules` / `builtin` / `audit` / `settings`）目前全部指向 `LegacyRoute.vue`，由 `legacy/legacy.ts` 的命令式渲染器接管。
- `LegacyRoute.vue` 是适配器：它负责 `<section class="page" tabindex="-1">` 外壳、加载骨架、入场动画、装饰层订阅，然后把 section 交给渲染器填充。
- `legacy.ts` 是「迁移前实现的逐字移植」，因此 `@ts-nocheck`，并在文件头声明：**过渡期由浏览器套件而非编译器保证其行为**。每迁完一页就从文件里删掉一页。
- `shell/decorate.ts` 为命令式 markup 做渐进增强：分段控件的滑动滑块、重复卡片的错峰入场；没有它一切功能照常，只是静止。

---

## 5. 路由与状态管理

### 5.1 路由

- `createWebHashHistory()`，路由为 `#/dashboard`、`#/rules`、`#/builtin`、`#/audit`、`#/settings`，`/` 重定向到 `/dashboard`。
- `beforeEach` 三件事：① 有未决模态框时拒绝导航并回滚 hash；② 路径变化时 `rotatePageRequests()` 取消上一页的 GET；③ 对即将离场的页面做**无 id、inert 的快照克隆**，用于交叉淡出。快速连点导航会取消上一个效果，任意时刻最多只有一个快照存在，页面内容不会被复制两份。
- `afterEach`：写 `state.route` / `state.hash`、记住该路由的 query、设置 `document.title`（页面名 · 广告拦截管理面板）、播放退场淡出、滚动复位、`nextTick` 后再同步导航指示器（直接读 `aria-current` 会读到上一帧）。

### 5.2 页面状态放在 hash query

筛选、分页、时间范围等页面状态一律写进 hash query，并通过 `router.replace` 更新 —— **不进历史栈**，避免污染后退按钮。`router.ts` 通过 `setRouteAdapter()` 把 `route` / `query` / `replaceQuery` / `reload` 注入 `core/params.ts`，这样 legacy 渲染器不必 import router（否则会形成循环依赖）。

`routeQueries` 记录每个导航项上次离开时的 query，`routeHref()` 据此生成链接，所以回到某个页面会回到上次的筛选条件。

### 5.3 状态

没有状态库。`core/stores.ts` 导出一个 `reactive()` 的单例 `state`：

```ts
{
  authenticated, username, route, hash,
  chats, builtinCatalog, builtinCatalogLoaded,
}
```

`builtinCatalog` 用 `markRaw()` 排除深响应（只整体读取、不增量变更）。注释明确了理由：面板状态同时被 Vue 外壳和未迁移的渲染器使用，所以保持单一响应式对象，而不是引入 store 库。

---

## 6. 请求层

`core/api.ts`：

- `fetch` + `credentials: 'same-origin'` + `X-Requested-With: fetch`（后端 CSRF 校验要求的自定义头）。
- **请求归属规则**：GET 继承当前页面的 `AbortController` 信号，随页面切换而取消；**mutation 永不因导航取消**。需要随页面消亡的 mutation（如内置库文本测试）可显式取 `pageSignal()`。多信号用 `AbortSignal.any()` 合并。
- 204 返回 `null`；非 2xx 时把后端的 `error` / `code` 包装成 Error 抛出，无 body 时给出中文兜底文案。
- 401 且本地认为已登录时：置 `state.authenticated = false`（外壳立刻切到登录页，不留半渲染页面）、强制关闭模态框、toast 提示「登录已过期」。
- 网络层失败统一转成「网络连接失败，请检查连接后重试。」

页面重载：`shell/pageReload.ts` 的 `pageReloadToken` 自增，触发 `RouterView` 的 key 变化从而重挂载组件。

---

## 7. 样式与设计系统

### 7.1 三层结构

```
styles/app.css       4 行，只有三个 @import
  ├── tokens.css     462 行  设计令牌（变量定义 + 别名块）
  ├── base.css       245 行  重置与基础排版
  └── components.css 950 行  结构 + 视觉表现
```

打包后合并为单一 `app.css`。

### 7.2 令牌体系（`tokens.css`）

| 组 | 内容 |
| --- | --- |
| 表面 | `--color-bg-0` ~ `--color-bg-4` 五级亮度阶梯（画布刻意远低于纯白，给卡片和玻璃层留出立足点） |
| 交互态 | `--color-hover` / `--color-active`，中性 alpha 叠加（不用品牌色着色） |
| 描边 | `--color-line-faint` / `--color-line` / `--color-line-strong` 三级 |
| 文字 | `--color-ink` / `-muted` / `-subtle` / `-faint` 四级灰阶 |
| 品牌 | `--color-accent`、`-bright`、`-deep`、`-soft`、`--color-on-accent`，全部用 `oklch()` 表达 |
| 语义 | `--color-success` / `--color-warn` / `--color-danger` 及各自 `-soft`，**独立于品牌色** |
| 环境光 | `--orb-a` ~ `--orb-d`、`--ambient-opacity`、`--orb-blur` |
| 玻璃 | `--glass-base` / `-alpha` / `--glass-deep-*` / `--glass-shadow` / `--spot-color` |
| 字体 | `--font-sans`（Inter var → 系统栈 → PingFang / 冬青黑 / 微软雅黑）、`--font-mono` |
| 字号 | `--text-2xs` ~ `--text-3xl` 全部带配套 `--line-height` |
| 其他 | `--tracking-tight`、`--radius-lg/xl/2xl`、`--space-*`（4/8/12/16/20/24/32/40） |

令牌的数值刻意与姊妹面板 `Telegram_session_Adblock` 的 `theme.css` 对齐，让两个控制台看起来是同一个产品。

### 7.3 六套配色（品牌色轴）

湛蓝（默认）、青碧、紫罗兰、品红、琥珀、石墨。

- 明暗是一个轴，品牌色相是**另一个**轴：配色只移动 `--color-accent*`、品牌渐变的两个色标和四个 orb，石墨画布、ink 灰阶、语义三色与所有非颜色量都共享。
- 通过 `<html data-accent="…">` 生效；六套值在 `tokens.css` 中**写两份** —— 一份给 `html[data-accent=…]`，一份给设置页的 `.palette-chip[data-palette=…]` 色板按钮，保证色板用自己推荐的配色自绘。
- 两条硬规则：① 没有配色占用绿或红色相（成功恒绿、危险恒红），暖色与品红都与它们拉开足够距离，主按钮不会被读成破坏性操作；② 亮度与彩度锁定在默认配色的量级上，明暗两套主题的对比度画像一致。石墨是唯一例外：近中性色在深色主题下偏亮，因此它是唯一翻转 `--color-on-accent` 的配色。
- 新增一套配色需要同步改三处：`core/theme.ts` 的 `ACCENTS`、`static/theme.js` 的数组、`tokens.css` 的令牌块（防闪烁脚本是经典脚本，无法 import 列表）。

### 7.4 用到的现代 CSS

`oklch()`（令牌的主要色彩空间）· `color-mix()` · `:has()` · `100dvh` · `text-wrap` · `:focus-visible` · `@supports` · 三档圆角与间距令牌 · 表格数字（tabular numerals）

### 7.5 响应式与无障碍

断点：`360px` · `640px`（标签堆到图标下方、表格转为带标题的记录卡）· `900px`（侧边栏变为水平导航条）· `1200px`（仪表盘趋势/失败列折叠）· `1700px`（宽屏增强）。

无障碍媒体查询：`prefers-reduced-motion`（关闭动画，`motion.ts` 里也会立即结束在跑的动画）、`prefers-reduced-transparency` / `prefers-contrast: more` / `forced-colors: active`（三者并列，一起降级玻璃与半透明效果）。

结构层面：跳转链接、`<main tabindex="-1">` 承接焦点、真实 `<dialog>` / `<details>` / `<a>` 元素、`role="switch"` + 中文可访问名、`aria-current="page"` 标记当前导航、`aria-live` 区域播报、移动端输入框 16px 以避免自动缩放。

### 7.6 布局与性能契约

- **外壳是固定视口列**：文档本身永不滚动，`#view` 是唯一滚动容器。`.shell` 必须是 `height: 100dvh; display: flex; flex-direction: column` 的全高 flex 列 —— 这是 `.layout` 的 `flex: 1` 能生效、`#view` 能拿到有界高度的前提。链条中任何一层退回普通块级元素，`#view` 的 `overflow-y: auto` 就不会触发，折叠线以下的内容会被 `body { overflow: hidden }` 裁掉。动效套件为此专门断言「外壳适配视口且超长页面可达」。
- **`backdrop-filter` 只用在真有内容从底下滚过的表面**：顶栏、侧栏、对话框、toast。内容卡片是不透明的，只用一根细分隔线加一像素内高光，**不投影** —— 给几十张卡片加模糊会白烧帧率，而且在平滑渐变上看起来一模一样。
- **环境光 orb 用 `radial-gradient` 绘制，禁止 `filter: blur()`**（半视口大小的模糊元素每帧都会重新栅格化），漂移只动 `translate3d`、**从不 scale**，周期 72–96 秒。
- 动效统一走 `core/motion.ts`：动画不会比它的元素活得更久，每个动画都会清掉自己设的 `will-change`；`prefers-reduced-motion` 变化或标签页隐藏时，所有动画立即清理。回归套件会在每次导航后断言 `will-change` 已被清掉。

---

## 8. 静态资源策略

### 8.1 图标

- 来源 `lucide-static@1.47.0`，人工精选后固化为 `web/static/icons.svg` 的 SVG sprite（文件头标注版本，`lucide-LICENSE.txt` 随产物分发）。
- 引用方式统一为根绝对路径 `<use href="/assets/icons.svg#name">`，**故意不让打包器内联** —— 内联会丢掉共享缓存，也会丢掉旁边的许可证文件。
- Vue 侧由 `components/Icon.vue`（10 行）包装；命令式侧由 `core/dom.ts` 的 `icon()` 包装。二者输出完全相同的 SVG 结构。

### 8.2 字体

`@fontsource-variable/inter` 提供 woff2，拷进 `assets/fonts/` 后由 Go 以 `font/woff2` 提供。**没有远程字体、没有 CDN**，CSP 的 `font-src` 回落到 `'self'`。中文回落到系统字体栈（PingFang SC / 冬青黑 / 微软雅黑）。

### 8.3 图表

仪表盘趋势图是手写 SVG（`legacy.ts` 中用 `createElementNS` 构建），无任何图表依赖；坐标轴数字用 `Intl.NumberFormat` 的 `notation: 'compact'`。

### 8.4 防主题闪烁（FOUC）

`web/static/theme.js` 是一段经典 IIFE，被注入 `<head>` **最前面**，在样式表和模块 bundle 之前执行：读 `localStorage` 的 `panel-theme` / `panel-accent`，立刻写 `<html data-theme>` 与 `data-accent`，并同步两个 `theme-color` meta（`index.html` 里那一对是 media 分域的，覆盖脚本执行前的系统偏好；脚本随后用存储偏好覆盖系统偏好）。所有 `localStorage` 读取都包在 try/catch 里，存储不可用时退回当前会话值。

---

## 9. 与 Go 后端的集成及安全约束

### 9.1 托管方式（`internal/webui/`）

- `//go:embed all:assets` —— `all:` 前缀保证点号/下划线开头的文件也进二进制（打包器可能产出这类共享 chunk，漏掉只会在生产环境暴露，而磁盘预览看不到）。
- 标准库 `http.ServeMux`：`GET /` 返回 SPA 外壳；`GET /assets/` 走 `http.FileServer`（保留 range 请求、目录重定向、404 行为）外加**显式的 Content-Type 表** —— Go 内建表在 Windows 上会被注册表覆盖，而 ES module 受严格 MIME 检查约束，一个以 `text/plain` 提供的脚本会让面板白屏。
- `/api/*` 的未知路径统一返回 JSON 404（每种方法一条模式），不让 API 客户端吃到 SPA 的 `index.html`。
- 请求体上限 1 MiB；`DisallowUnknownFields` 严格解析。

### 9.2 安全响应头（`middleware.go`）

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';
                         img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: no-referrer
Cache-Control: no-store
```

`Cache-Control: no-store` 覆盖所有响应（含 SPA 外壳与静态资源），保证面板升级后浏览器或代理不会拿到陈旧的 `index.html` / `app.js`。

认证：会话 cookie（可选 `WEBUI_SESSION_SECRET` 签名）+ 滑动续期；写操作额外要求 `X-Requested-With: fetch` 头（与 `SameSite=Lax` 构成纵深防御）。

### 9.3 CSP 对前端写法的硬性约束

| 约束 | 前端对应做法 |
| --- | --- |
| `script-src 'self'`，禁内联脚本 | 模板必须预编译（`.vue` SFC）；`modulePreload` polyfill 关闭；防闪烁脚本走外部文件 |
| 禁 `eval` / `new Function` | 运行时模板编译不可用；CI 用 `grep` 断言产物中数量为 0 |
| 无 CDN | 图标、字体、样式全部本地 |
| `img-src 'self' data:` | sprite 不内联为 data: URL |
| 所有 API 值都是文本 | **禁用 `v-html`**，每个值都作为文本节点插入（CI 检查 `web/src` 中无 `v-html`） |

---

## 10. 测试与质量门禁

### 10.1 类型检查

```bash
npm run typecheck   # vue-tsc --noEmit && tsc --noEmit -p tsconfig.node.json
```

### 10.2 CI（`.github/workflows/docker-publish.yml`）

在 Go 的 `test -race` / `vet` / `gofmt` 之后追加一个 Node 作业：

```bash
cd web
npm ci
npm run typecheck
npm run build
cd ..
git diff --exit-code -- internal/webui/assets                  # 产物与源码必须一致
test -z "$(git status --porcelain -- internal/webui/assets)"   # git diff 看不到新文件，这里补上
test -z "$(ls internal/webui/assets | grep -E '^[_.]')"        # go:embed 会丢掉这类文件
test "$(grep -cE 'new Function|[^.]eval\(' internal/webui/assets/app.js)" = "0"
```

随后 Trivy 扫源码依赖与文件系统（`skip-dirs: web/node_modules`，前端工具链不进镜像也不进二进制）。

Go 侧的 `internal/webui/handlers_test.go` 还钉住了 `/assets/app.css` 与 `/assets/app.js` 的 200 状态码与 `text/javascript` 类型。

### 10.3 浏览器回归（Python Playwright 1.63.0）

| 套件 | 文件 | 浏览器 | 覆盖 |
| --- | --- | --- | --- |
| 业务回归 | `scripts/test_webui.py` | Chromium | 趋势周期与键盘选日、图表/表格切换、轴缩放、零命中；规则搜索/筛选/URL 持久化/分页/开关回滚/正则校验/增删改/导出；未保存确认与模态框焦点循环；内置库分类、版本、组合条件、总开关与逐项开关、保存失败回滚、文本测试（含禁用检测项、Unicode 上限、失败与编辑后的过期响应）；审计筛选、分页、长文本按文本渲染、剪贴板反馈；失败请求与重试、导航后的迟到响应；1440/768/390/320 布局、明暗主题、移动端对话框；账号校验、改密、会话过期、登出；运行设置开关与校验；设置页配色选择器（切换后重绘、刷新后保留、与明暗轴互不干扰、每个色板用自己推荐的配色自绘） |
| 动效与视觉 | `scripts/test_webui_motion.py` | Chromium + Firefox + WebKit | 1920/1440/768/390/320 视口、动画打断与清理、`will-change` 回收、reduced-motion、forced-colors、主题与配色持久化、200% CSS 缩放、模态框焦点还原、会话过期；**不放宽生产 CSP** |

两个套件都只接受 localhost，并在改数据前检查 `X-WebUI-Preview` 响应头；截图、`<browser>-motion.webm` 录像与 `report.json` 写入 `.gocache/`。

可选无障碍检查：安装 `axe-core@4.13.0` 到被忽略的目录，用 `--axe` 参数启用 WCAG A/AA 扫描（官方文档明确它只是人工检查的补充，不构成完整合规证明）。

### 10.4 本地预览

```powershell
# 终端 1：前端 watch 构建
cd web; npm run watch
# 终端 2：带 build tag 的真实 handler 预览服务，资产从磁盘读
$env:WEBUI_PREVIEW_ADDR = '127.0.0.1:8765'
go test -tags webuipreview -run '^TestWebUIPreview$' -v ./internal/webui -timeout 0
```

登录 `admin` / `preview-only`。预览使用真实面板 handler + 一次性内存 store，不连 Telegram 与 PostgreSQL；`webuipreview` tag 与 `_test.go` 后缀保证它不进生产构建。

---

## 11. 约束速查表

### 写前端代码时必须遵守

| 必须 | 禁止 |
| --- | --- |
| 用 `.vue` SFC，模板预编译 | 运行时模板编译 |
| API 值一律作为文本节点渲染 | `v-html` |
| 动画走 `core/motion.ts` | 裸 `setTimeout` 帧循环、`filter: blur()` 做 orb、动画遗留 `will-change` |
| 页面状态写进 hash query，用 `router.replace` | 把筛选状态推进历史栈 |
| GET 随页面取消、mutation 不取消 | 让 mutation 随导航中断 |
| 新增配色同步三处（`theme.ts` / `theme.js` / `tokens.css`） | 只改一处 |
| 产物入库并保持 LF | 手改 `internal/webui/assets/` 下的文件 |
| 图标走本地 sprite、字体自托管 | 任何 CDN、远程字体 |

### 变更影响面速查

| 改动 | 需要做什么 |
| --- | --- |
| 改前端任何源码 | `cd web && npm run build`，把 `internal/webui/assets` 的 diff 一起提交 |
| 加一个图标 | 更新 `web/static/icons.svg`（连带版本注释与许可证），`Icon.vue` / `icon()` 直接按 id 使用 |
| 加一套配色 | `core/theme.ts` 的 `ACCENTS` + `static/theme.js` 数组 + `tokens.css` 两份令牌块 |
| 改输出文件名 | 同步 `internal/webui/handlers_test.go` 的断言与 `vite.config.ts` |
| 改后端 CSP | 先确认不破坏「预编译模板 + 无内联脚本 + 无 eval」这三条前端前提 |

---

## 附录 A：前端文件清单

| 路径 | 行数 | 说明 |
| --- | --- | --- |
| `web/index.html` | —— | 入口 HTML |
| `web/vite.config.ts` | —— | 构建配置与两个自定义插件 |
| `web/src/main.ts` | 7 | 应用引导 |
| `web/src/App.vue` | 91 | 外壳、启动流程、登录分流 |
| `web/src/router.ts` | 124 | 路由表、守卫、过渡、请求轮换 |
| `web/src/components/Icon.vue` | 10 | Lucide sprite 包装 |
| `web/src/pages/LoginView.vue` | 55 | 登录表单 |
| `web/src/pages/LegacyRoute.vue` | 40 | 命令式页面适配器 |
| `web/src/shell/SideBar.vue` | 50 | 侧边导航 |
| `web/src/shell/TopBar.vue` | 49 | 顶栏（主题切换、登出） |
| `web/src/shell/decorate.ts` | 114 | 渐进增强装饰层 |
| `web/src/shell/navIndicator.ts` | 27 | 导航指示条定位与滑动 |
| `web/src/shell/pageReload.ts` | 12 | 页面重挂载 token |
| `web/src/core/api.ts` | 65 | 请求层 |
| `web/src/core/dialog.ts` | 95 | 原生 `<dialog>` 模态 |
| `web/src/core/dom.ts` | 146 | 命令式 DOM 原语 |
| `web/src/core/format.ts` | 42 | 数字与 UTC 时间格式化 |
| `web/src/core/motion.ts` | 64 | 动画所有者 |
| `web/src/core/params.ts` | 59 | hash 路由适配层 |
| `web/src/core/stores.ts` | 33 | 面板状态单例 |
| `web/src/core/theme.ts` | 77 | 主题与配色 |
| `web/src/core/toast.ts` | 22 | toast 与警示呈现 |
| `web/src/legacy/legacy.ts` | 885 | 未迁移的 5 个命令式页面 |
| `web/src/styles/tokens.css` | 462 | 设计令牌 + 六套配色 |
| `web/src/styles/base.css` | 245 | 重置与基础排版 |
| `web/src/styles/components.css` | 950 | 组件结构 + 视觉表现 |
| `web/src/styles/app.css` | 4 | 样式入口（三个 `@import`） |
| `web/static/theme.js` | —— | 防闪烁经典脚本 |
| `web/static/icons.svg` | —— | Lucide 1.47.0 精选 sprite |
| `web/static/favicon.svg` | —— | favicon |

源码合计约 **3,700 行**（其中 885 行是待迁移的 legacy 实现）。

## 附录 B：版本与依赖清单

### 运行时依赖

| 包 | 版本 |
| --- | --- |
| `vue` | 3.5.43 |
| `vue-router` | 5.3.1 |
| `@fontsource-variable/inter` | 5.3.0 |

### 开发依赖

| 包 | 版本 |
| --- | --- |
| `vite` | 8.3.0 |
| `@vitejs/plugin-vue` | 6.0.9 |
| `typescript` | 5.9.3 |
| `vue-tsc` | 3.3.11 |
| `@types/node` | 24.13.6 |

### 工具与外部资产

| 项 | 版本 |
| --- | --- |
| Node.js | `engines >= 22.12`；CI 用 24 |
| Playwright（Python） | 1.63.0 |
| axe-core（可选） | 4.13.0 |
| Lucide 图标 | lucide-static 1.47.0 |
| Inter 字体 | `@fontsource-variable/inter` 5.3.0 |
| Go 服务端 | 标准库 `net/http` + `embed` |

### 相关文档

- `docs/webui-design.md` —— 视觉与动效系统、令牌表、布局契约、验证记录
- `docs/webui-development.md` —— 构建、本地预览、浏览器回归、无障碍检查的操作步骤
- `README.md` 第 6 节 —— 面板的部署、环境变量与访问控制
