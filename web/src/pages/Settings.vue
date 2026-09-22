<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import Select from "../components/Select.vue";
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
  // 金额单位是元（结算币种），不是分：界面上写「1.00」就是一元。
  currency: string;
  minimum_recharge: string;
  maximum_recharge: string;
  diagnostics_enabled: boolean;
  diagnostics_per_minute: number;
  // 探针页面的位置图标要查 IP 归属地，模板里用 {ip} 占位。留空即关闭，
  // 面板不会把机器地址发给任何第三方。
  geo_lookup_url: string;
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
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">SETTINGS</p>
        <h1>站点设置</h1>
      </div>
      <button v-if="allowed" :disabled="busy" @click="load">重新加载</button>
    </div>
    <p v-if="!allowed" class="card">仅管理员可以修改站点设置。</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form v-if="allowed && form" class="card" @submit.prevent="save">
      <h2>品牌与公告</h2>
      <label>站点名称<input v-model="form.name" maxlength="100" required /></label
      ><label
        >公告<textarea v-model="form.announcement" maxlength="8000" rows="4" />
      </label>
      <label
        >默认配色<Select v-model="form.accent" aria-label="默认配色">
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
        </Select></label
      >
      <h2>注册与登录</h2>
      <label
        >注册策略<Select v-model="form.registration" aria-label="注册策略">
          <option value="closed">关闭注册</option>
          <option value="open">开放注册</option>
          <option value="invite">凭注册邀请码注册</option>
        </Select></label
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
          >最小充值（元）<input
            v-model="form.minimum_recharge"
            inputmode="decimal"
            pattern="[0-9]+(\.[0-9]{1,2})?"
            required /></label
        ><label
          >最大充值（元）<input
            v-model="form.maximum_recharge"
            inputmode="decimal"
            pattern="[0-9]+(\.[0-9]{1,2})?"
            required
        /></label>
      </div>
      <p class="small muted">
        结算币种为人民币元，金额最多两位小数。加密货币通道按各通道配置的汇率折算
        成应付的 USDT，手续费也按通道设置。关闭充值后，已有订单仍可核对和入账；
        商户密钥通过部署配置文件管理。
      </p>
      <h2>探针位置</h2>
      <label
        >地区查询地址<input
          v-model="form.geo_lookup_url"
          placeholder="https://ipwho.is/{ip}"
      /></label>
      <p class="small muted">
        探针页面用它把机器 IP 换成位置图标。地址必须含
        <code>{ip}</code> 占位符；留空表示关闭，届时机器只显示中性图标。
        查询会带上服务器的公网地址，请按自己的隐私要求选择服务。
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
      <div class="form-actions">
        <button
          class="primary"
          :disabled="busy"
          :data-busy="String(busy)"
          :aria-busy="busy"
        >
          保存站点设置
        </button>
      </div>
    </form>
  </section>
</template>
