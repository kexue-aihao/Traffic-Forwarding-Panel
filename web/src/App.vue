<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import Select from "./components/Select.vue";
import { useRoute, useRouter } from "vue-router";
import Icon from "./components/Icon.vue";
import { api, errorText } from "./core/api";
import { state, adminSite } from "./core/state";
import type { User } from "./core/state";
const route = useRoute();
const router = useRouter();
// 探针页是独立窗口：没有侧栏与顶栏，内容铺满整屏。
const standalone = computed(() => route.meta.standalone === true);
const username = ref("");
const password = ref("");
const busy = ref(false);
const error = ref("");
const bootError = ref("");
const site = ref({
  name: "流量控制台",
  announcement: "",
  registration: "closed",
  captcha: false,
  accent: "blue",
});
const registering = ref(false),
  registerInvite = ref(""),
  captcha = ref<{ id: string; image: string } | null>(null),
  captchaAnswer = ref("");
async function loadCaptcha() {
  if (!site.value.captcha) return;
  try {
    captcha.value = await api("/auth/captcha");
    captchaAnswer.value = "";
  } catch (e) {
    error.value = errorText(e);
  }
}
async function loadSite() {
  site.value = await api("/site");
  document.title = site.value.name;
  try {
    if (!localStorage.getItem("panel-accent")) {
      accent.value = site.value.accent;
      document.documentElement.dataset.accent = site.value.accent;
    }
  } catch {}
  await loadCaptcha();
}
const menus = computed<
  { path: string; label: string; icon: string; blank?: boolean }[]
>(() => [
  { path: "/overview", label: "概览", icon: "activity" },
  { path: "/rules", label: "转发规则", icon: "arrow-right-left" },
  { path: "/exits", label: "出口管理", icon: "arrow-right-left" },
  { path: "/nodes", label: "服务器", icon: "server" },
  { path: "/probes", label: "实时探针", icon: "activity", blank: true },
  { path: "/diagnostics", label: "网络诊断", icon: "radar" },
  { path: "/commerce", label: "套餐与钱包", icon: "wallet" },
  { path: "/operations", label: "运营与任务", icon: "layers" },
  { path: "/account", label: "账号与 API", icon: "shield" },
  { path: "/api-docs", label: "API 列表", icon: "book-open" },
  ...(adminSite && state.user?.role === "admin"
    ? [
        { path: "/settings", label: "站点设置", icon: "settings" },
        { path: "/groups", label: "设备组", icon: "layers" },
        { path: "/identity-groups", label: "身份用户组", icon: "shield" },
        { path: "/users", label: "用户管理", icon: "users" },
        { path: "/audit", label: "操作审计", icon: "shield" },
      ]
    : []),
]);
const title = computed(
  () => menus.value.find((m) => m.path === route.path)?.label || "控制台",
);
// ── 导航指示条 ────────────────────────────────────────────────
// 用一个独立元素在导航项之间滑动，而不是每项各画一条。差别不只是省几个
// 节点：换页因此变成一次连续的运动，而不是两次独立的出现与消失。
// 位置实测自元素的 offsetTop —— 按固定行高算是更省事，但改行高或插分隔线
// 时会静默错位，而错位的那一像素很难被注意到。
const navEls = ref<Record<string, HTMLElement | null>>({});
const indicator = ref({ top: 0, visible: false, ready: false });
// :ref 挂在 RouterLink 上拿到的是**组件实例**而不是 DOM 元素，
// 直接 instanceof 判断会永远是 false，指示条就永远不落位。
// 这里两种形态都接住：是元素就用，是组件就取它的根节点。
function registerNav(path: string, el: unknown) {
  const node =
    el instanceof HTMLElement
      ? el
      : ((el as { $el?: unknown } | null)?.$el ?? null);
  navEls.value[path] = node instanceof HTMLElement ? node : null;
}
function updateIndicator() {
  const el = navEls.value[route.path];
  if (!el) {
    indicator.value = { ...indicator.value, visible: false };
    return;
  }
  indicator.value = {
    top: el.offsetTop + (el.offsetHeight - 16) / 2,
    visible: true,
    ready: indicator.value.ready,
  };
}
// 首帧先落位（不动画），下一帧才允许过渡 —— 否则首次进入页面时
// 指示条会从顶部「飞」到当前项，看起来像个 bug。
async function settleIndicator() {
  await nextTick();
  updateIndicator();
  requestAnimationFrame(() => {
    indicator.value = { ...indicator.value, ready: true };
  });
}
watch(() => route.path, () => void nextTick().then(updateIndicator));
watch(() => menus.value.length, () => void nextTick().then(updateIndicator));
// ── 卡片光标聚光 ──────────────────────────────────────────────
// 鼠标在卡面上移动时，一层极淡的径向高光跟着走 —— 它给静态的卡片一个
// 「有光从上面照下来」的物理感，是整个界面里最便宜也最有效的质感来源。
//
// 用事件委托挂在滚动容器上，而不是给每张卡片各绑一个监听：卡片是模板里的
// div，数量多且随页面重建。触屏没有 hover 语义，跟随一个不存在的光标
// 只是白耗算力，所以先看指针类型。
const finePointer = matchMedia("(pointer: fine)");
function trackSpotlight(event: MouseEvent) {
  if (!finePointer.matches) return;
  const node = (event.target as HTMLElement | null)?.closest<HTMLElement>(
    ".card, .surface",
  );
  if (!node) return;
  // 用 getBoundingClientRect 换算坐标，而不是读 offsetX/offsetY：
  // 后者相对**事件目标**，而鼠标经常落在卡片的子元素上 —— 那样光斑
  // 会在每个子元素边界上跳一下。
  const rect = node.getBoundingClientRect();
  node.style.setProperty("--spot-x", `${event.clientX - rect.left}px`);
  node.style.setProperty("--spot-y", `${event.clientY - rect.top}px`);
}
onMounted(() => {
  window.addEventListener("resize", updateIndicator);
  void settleIndicator();
  document
    .getElementById("view")
    ?.addEventListener("mousemove", trackSpotlight, { passive: true });
});
onBeforeUnmount(() => {
  window.removeEventListener("resize", updateIndicator);
  document
    .getElementById("view")
    ?.removeEventListener("mousemove", trackSpotlight);
});
const theme = ref(document.documentElement.dataset.theme || "dark");
const accent = ref(document.documentElement.dataset.accent || "blue");
const accents = [
  ["blue", "湛蓝"],
  ["teal", "青碧"],
  ["violet", "紫罗兰"],
  ["magenta", "品红"],
  ["amber", "琥珀"],
  ["graphite", "石墨"],
];
// 画布底色，与 static/theme.js 里的取值保持一致：两处都要同步
// theme-color，否则移动端地址栏会在切换主题后留着上一套颜色。
const canvas = { dark: "#07080b", light: "#eceff5" } as const;
function appearance() {
  document.documentElement.dataset.theme = theme.value;
  document.documentElement.dataset.accent = accent.value;
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute("content", canvas[theme.value as keyof typeof canvas] ?? canvas.dark);
  try {
    localStorage.setItem("panel-theme", theme.value);
    localStorage.setItem("panel-accent", accent.value);
  } catch {
    /* session preference remains usable */
  }
}
async function bootstrap() {
  bootError.value = "";
  state.ready = false;
  try {
    await loadSite();
    state.user = (await api<{ user: User }>("/auth/session")).user;
  } catch (e) {
    if (!(e instanceof Error && "status" in e && e.status === 401))
      bootError.value = errorText(e);
  } finally {
    state.ready = true;
  }
}
async function login() {
  busy.value = true;
  error.value = "";
  try {
    if (registering.value) {
      await api("/auth/register", "POST", {
        username: username.value,
        password: password.value,
        invite: registerInvite.value,
        captcha_id: captcha.value?.id || "",
        captcha_answer: captchaAnswer.value,
      });
      registering.value = false;
      password.value = "";
      state.notice = "注册成功，请登录。";
      await loadCaptcha();
      return;
    }
    state.user = (
      await api<{ user: User }>("/auth/login", "POST", {
        username: username.value,
        password: password.value,
        captcha_id: captcha.value?.id || "",
        captcha_answer: captchaAnswer.value,
      })
    ).user;
    password.value = "";
  } catch (e) {
    error.value = errorText(e);
    await loadCaptcha();
  } finally {
    busy.value = false;
  }
}
async function logout() {
  busy.value = true;
  try {
    await api("/auth/logout", "POST", {});
    state.user = null;
  } catch (e) {
    state.notice = errorText(e);
  } finally {
    busy.value = false;
  }
}
onMounted(async () => {
  await router.isReady();
  await bootstrap();
});
</script>
<template>
  <div class="shell">
    <!-- 环境光晕：磨砂能读得出来的前提。光斑必须落在侧栏与顶栏覆盖的
         区域上，否则那两处背后什么都没有，玻璃就只是「深色块」。 -->
    <div v-if="!standalone" class="ambient" aria-hidden="true">
      <div class="ambient-orb ambient-orb-a" />
      <div class="ambient-orb ambient-orb-b" />
      <div class="ambient-orb ambient-orb-c" />
      <div class="ambient-orb ambient-orb-d" />
    </div>
    <a v-if="!standalone" class="skip-link" href="#view">跳到主要内容</a>
    <div class="layout">
      <aside v-if="!standalone" class="sidebar glass">
        <a class="brand" href="#/overview">
          <span class="brand-mark brand-gradient">
            <Icon name="arrow-right-left" />
          </span>
          <span>
            <span class="brand-name">{{ site.name }}</span>
            <small>{{ adminSite ? "管理员工作空间" : "用户工作空间" }}</small>
          </span>
        </a>
        <p class="nav-caption">Workspace</p>
        <nav aria-label="主要导航">
          <span
            class="nav-indicator"
            :data-ready="String(indicator.ready)"
            :style="{
              transform: `translateY(${indicator.top}px)`,
              display: indicator.visible ? undefined : 'none',
            }"
            aria-hidden="true"
          />
          <template v-for="item in menus" :key="item.path">
            <!-- 独立窗口的页面不能在外壳里打开：这里新开一个标签页。 -->
            <a
              v-if="item.blank"
              :href="`#${item.path}`"
              target="_blank"
              rel="noopener"
              ><Icon :name="item.icon" />{{ item.label }}</a
            >
            <RouterLink
              v-else
              :ref="(el) => registerNav(item.path, el)"
              :to="item.path"
              :aria-current="route.path === item.path ? 'page' : undefined"
              ><Icon :name="item.icon" />{{ item.label }}</RouterLink
            >
          </template>
        </nav>
        <div class="sidebar-foot">
          <span class="status-dot" /> 私有部署 · 自主掌控
        </div>
      </aside>
      <div class="content">
        <header v-if="!standalone" class="topbar glass">
          <span>{{ title }}</span>
          <div class="toolbar">
            <label class="sr-only" for="accent">品牌配色</label
            ><Select id="accent" v-model="accent" @change="appearance">
              <option
                v-for="[value, label] in accents"
                :key="value"
                :value="value"
              >
                {{ label }}
              </option></select
            ><button
              :aria-label="theme === 'dark' ? '切换浅色主题' : '切换深色主题'"
              @click="
                theme = theme === 'dark' ? 'light' : 'dark';
                appearance();
              "
            >
              <Icon :name="theme === 'dark' ? 'sun' : 'moon'" /></button
            ><span v-if="state.user" class="username">{{
              state.user.username
            }}</span
            ><button
              v-if="state.user"
              :disabled="busy || state.modalOpen"
              @click="logout"
            >
              退出
            </button>
          </div>
        </header>
        <main id="view" :class="{ 'view-standalone': standalone }" tabindex="-1">
          <!-- 骨架屏按真实首页的比例摆：标题、说明、三张统计卡、一块内容卡。
               形状对上了，数据到达时是「填上」而不是「重排」。 -->
          <div v-if="!state.ready" class="page boot" aria-busy="true">
            <p class="sr-only" role="status">正在连接控制台</p>
            <div class="skeleton boot-head" />
            <div class="skeleton boot-sub" />
            <div class="stats">
              <div v-for="n in 3" :key="n" class="skeleton skeleton-stat" />
            </div>
            <div class="skeleton skeleton-block" />
          </div>
          <section v-else-if="bootError" class="card">
            <h1>暂时无法连接</h1>
            <p role="alert">{{ bootError }}</p>
            <button @click="bootstrap">重试</button>
          </section>
          <section v-else-if="!state.user" class="login card">
            <p class="eyebrow">WELCOME BACK</p>
            <h1>连接你的网络</h1>
            <p class="muted">登录后管理转发服务与实时资源。</p>
            <p v-if="site.announcement" class="site-announcement">
              {{ site.announcement }}
            </p>
            <form @submit.prevent="login">
              <label
                >用户名<input
                  v-model="username"
                  autocomplete="username"
                  required
                  maxlength="64" /></label
              ><label
                >密码<input
                  v-model="password"
                  type="password"
                  autocomplete="current-password"
                  required
              /></label>
              <label v-if="registering && site.registration === 'invite'"
                >注册邀请码<input v-model="registerInvite" required
              /></label>
              <div v-if="site.captcha">
                <img
                  v-if="captcha"
                  :src="captcha.image"
                  alt="六位数字验证码"
                  width="144"
                  height="40"
                /><button type="button" @click="loadCaptcha">
                  换一张验证码</button
                ><label
                  >验证码<input
                    v-model="captchaAnswer"
                    required
                    maxlength="6"
                    inputmode="numeric"
                    autocomplete="off"
                /></label>
              </div>
              <p v-if="error" role="alert" class="error">{{ error }}</p>
              <button
                class="primary"
                :disabled="busy"
                :data-busy="String(busy)"
                :aria-busy="busy"
              >
                {{ registering ? "创建账号" : "登录控制台" }}
              </button>
            </form>
            <button
              v-if="!adminSite && site.registration !== 'closed'"
              type="button"
              :disabled="busy"
              @click="
                registering = !registering;
                error = '';
                loadCaptcha();
              "
            >
              {{ registering ? "已有账号，返回登录" : "注册账号" }}
            </button>
          </section>
          <section
            v-else-if="adminSite && state.user.role !== 'admin'"
            class="card"
          >
            <h1>无管理权限</h1>
            <p>当前账号无法访问管理员后台。</p>
            <a href="/">返回用户工作空间</a>
          </section>
          <template v-else>
            <div v-if="site.announcement" class="page">
              <p class="card site-announcement">{{ site.announcement }}</p>
            </div>
            <RouterView v-slot="{ Component }">
              <Transition name="view" mode="out-in">
                <component :is="Component" :key="route.path" />
              </Transition>
            </RouterView>
          </template>
        </main>
      </div>
    </div>
    <Transition name="toast">
      <div v-if="state.notice" class="toast" role="status" aria-live="polite">
        {{ state.notice
        }}<button aria-label="关闭通知" @click="state.notice = ''">×</button>
      </div>
    </Transition>
  </div>
</template>
