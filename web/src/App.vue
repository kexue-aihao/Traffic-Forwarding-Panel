<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute } from "vue-router";
import Icon from "./components/Icon.vue";
import { api, errorText } from "./core/api";
import { state, adminSite } from "./core/state";
import type { User } from "./core/state";
const route = useRoute();
const username = ref("");
const password = ref("");
const busy = ref(false);
const error = ref("");
const bootError = ref("");
const menus = computed(() => [
  { path: "/overview", label: "概览", icon: "activity" },
  { path: "/rules", label: "转发规则", icon: "arrow-right-left" },
  { path: "/nodes", label: "服务器", icon: "server" },
  { path: "/probes", label: "实时探针", icon: "activity" },
  { path: "/commerce", label: "套餐与钱包", icon: "wallet" },
  ...(adminSite && state.user?.role === "admin"
    ? [
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
    state.user = (
      await api<{ user: User }>("/auth/login", "POST", {
        username: username.value,
        password: password.value,
      })
    ).user;
    password.value = "";
  } catch (e) {
    error.value = errorText(e);
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
onMounted(bootstrap);
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
            >流量控制台<small>{{
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
              <p v-if="error" role="alert" class="error">{{ error }}</p>
              <button class="primary" :disabled="busy">
                {{ busy ? "正在登录…" : "登录控制台" }}
              </button>
            </form>
          </section>
          <section
            v-else-if="adminSite && state.user.role !== 'admin'"
            class="card"
          >
            <h1>无管理权限</h1>
            <p>当前账号无法访问管理员后台。</p>
            <a href="/">返回用户工作空间</a>
          </section>
          <RouterView v-else :key="route.path" />
        </main>
      </div>
    </div>
    <div v-if="state.notice" class="toast" role="status" aria-live="polite">
      {{ state.notice
      }}<button aria-label="关闭通知" @click="state.notice = ''">×</button>
    </div>
  </div>
</template>
