<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import Icon from "./components/Icon.vue";
import { api, errorText } from "./core/api";
import { state, adminSite } from "./core/state";
import type { User } from "./core/state";
const route = useRoute();
const router = useRouter();
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
const menus = computed(() => [
  { path: "/overview", label: "概览", icon: "activity" },
  { path: "/rules", label: "转发规则", icon: "arrow-right-left" },
  { path: "/exits", label: "出口管理", icon: "arrow-right-left" },
  { path: "/nodes", label: "服务器", icon: "server" },
  { path: "/probes", label: "实时探针", icon: "activity" },
  { path: "/commerce", label: "套餐与钱包", icon: "wallet" },
  { path: "/operations", label: "运营与任务", icon: "layers" },
  { path: "/account", label: "账号与 API", icon: "shield" },
  ...(adminSite && state.user?.role === "admin"
    ? [
        { path: "/settings", label: "站点设置", icon: "settings" },
        { path: "/groups", label: "设备组", icon: "layers" },
        { path: "/users", label: "用户管理", icon: "users" },
        { path: "/audit", label: "操作审计", icon: "shield" },
      ]
    : []),
]);
const title = computed(
  () => menus.value.find((m) => m.path === route.path)?.label || "控制台",
);
const theme = ref(document.documentElement.dataset.theme || "light");
const accent = ref(document.documentElement.dataset.accent || "blue");
const accents = [
  ["blue", "湛蓝"],
  ["teal", "青碧"],
  ["violet", "紫罗兰"],
  ["magenta", "品红"],
  ["amber", "琥珀"],
  ["graphite", "石墨"],
];
function appearance() {
  document.documentElement.dataset.theme = theme.value;
  document.documentElement.dataset.accent = accent.value;
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
    <div class="ambient" aria-hidden="true" />
    <a class="skip-link" href="#view">跳到主要内容</a>
    <div class="layout">
      <aside class="sidebar">
        <a class="brand" href="#/overview"
          ><span class="brand-mark"><Icon name="arrow-right-left" /></span
          ><span
            >{{ site.name
            }}<small>{{
              adminSite ? "管理员工作空间" : "用户工作空间"
            }}</small></span
          ></a
        >
        <p class="nav-caption">WORKSPACE</p>
        <nav aria-label="主要导航">
          <RouterLink
            v-for="item in menus"
            :key="item.path"
            :to="item.path"
            :aria-current="route.path === item.path ? 'page' : undefined"
            ><Icon :name="item.icon" />{{ item.label }}</RouterLink
          >
        </nav>
        <div class="sidebar-foot">
          <span class="status-dot" /> 私有部署 · 自主掌控
        </div>
      </aside>
      <div class="content">
        <header class="topbar">
          <span>{{ title }}</span>
          <div class="toolbar">
            <label class="sr-only" for="accent">品牌配色</label
            ><select id="accent" v-model="accent" @change="appearance">
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
        <main id="view" tabindex="-1">
          <p v-if="!state.ready" class="empty">正在连接控制台…</p>
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
              <button class="primary" :disabled="busy">
                {{ busy ? "提交中…" : registering ? "创建账号" : "登录控制台" }}
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
          <template v-else
            ><p v-if="site.announcement" class="card site-announcement">
              {{ site.announcement }}
            </p>
            <RouterView :key="route.path"
          /></template>
        </main>
      </div>
    </div>
    <div v-if="state.notice" class="toast" role="status" aria-live="polite">
      {{ state.notice
      }}<button aria-label="关闭通知" @click="state.notice = ''">×</button>
    </div>
  </div>
</template>
