<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
interface Settings {
  version: number;
  name: string;
  announcement: string;
  registration: string;
  captcha: boolean;
  accent: string;
  payments_enabled: boolean;
  minimum_recharge_cents: string;
  maximum_recharge_cents: string;
  diagnostics_enabled: boolean;
  diagnostics_per_minute: number;
}
const allowed = computed(() => adminSite && state.user?.role === "admin");
const form = ref<Settings | null>(null),
  error = ref(""),
  busy = ref(false),
  invite = ref("");
async function load() {
  error.value = "";
  try {
    form.value = await api<Settings>("/site");
  } catch (e) {
    error.value = errorText(e);
  }
}
async function save() {
  busy.value = true;
  error.value = "";
  try {
    form.value = await api<Settings>("/site", "PUT", form.value);
    notice("站点设置已保存，刷新页面可查看品牌更新。");
  } catch (e) {
    error.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
async function createInvite() {
  busy.value = true;
  error.value = "";
  try {
    invite.value = (
      await api<{ code: string }>("/registration-invites", "POST", {})
    ).code;
  } catch (e) {
    error.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
onMounted(() => {
  if (allowed.value) void load();
});
</script>
<template>
  <section class="page-header">
    <div>
      <p class="eyebrow">SETTINGS</p>
      <h1>站点设置</h1>
    </div>
    <button v-if="allowed" :disabled="busy" @click="load">重新加载</button>
  </section>
  <p v-if="!allowed" class="card">仅管理员可以修改站点设置。</p>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <form v-if="allowed && form" class="card" @submit.prevent="save">
    <h2>品牌与公告</h2>
    <label>站点名称<input v-model="form.name" maxlength="100" required /></label
    ><label
      >公告<textarea v-model="form.announcement" maxlength="8000" rows="4" />
    </label>
    <label
      >默认配色<select v-model="form.accent" aria-label="默认配色">
        <option
          v-for="color in [
            'blue',
            'teal',
            'violet',
            'magenta',
            'amber',
            'graphite',
          ]"
          :key="color"
        >
          {{ color }}
        </option>
      </select></label
    >
    <h2>注册与登录</h2>
    <label
      >注册策略<select v-model="form.registration" aria-label="注册策略">
        <option value="closed">关闭注册</option>
        <option value="open">开放注册</option>
        <option value="invite">凭注册邀请码注册</option>
      </select></label
    >
    <label class="check"
      ><input
        v-model="form.captcha"
        type="checkbox"
      />登录和注册启用图形验证码</label
    >
    <p class="small muted">
      注册邀请码一次有效，7 天后过期；与返佣邀请码分别管理。
    </p>
    <button type="button" :disabled="busy" @click="createInvite">
      生成注册邀请码</button
    ><label v-if="invite">请保存邀请码<input :value="invite" readonly /></label>
    <h2>充值与诊断</h2>
    <label class="check"
      ><input
        v-model="form.payments_enabled"
        type="checkbox"
      />允许创建充值订单</label
    >
    <div class="form-grid">
      <label
        >最小充值（分）<input
          v-model="form.minimum_recharge_cents"
          inputmode="numeric"
          pattern="[0-9]+"
          required /></label
      ><label
        >最大充值（分）<input
          v-model="form.maximum_recharge_cents"
          inputmode="numeric"
          pattern="[0-9]+"
          required
      /></label>
    </div>
    <p class="small muted">
      关闭充值后，已有订单仍可核对和入账。商户密钥通过部署配置文件管理。
    </p>
    <label class="check"
      ><input
        v-model="form.diagnostics_enabled"
        type="checkbox"
      />允许授权用户诊断自己的规则</label
    ><label
      >每账号每分钟诊断次数<input
        v-model.number="form.diagnostics_per_minute"
        type="number"
        min="1"
        max="30"
        required
    /></label>
    <button class="primary" :disabled="busy">
      {{ busy ? "保存中…" : "保存站点设置" }}
    </button>
  </form>
</template>
