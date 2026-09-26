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
// 面板列出的五种协议，以及各自需要哪些字段：字段少的那几种不该出现用不上的
// 输入框。协议名会进回调路径与适配器工厂，所以这里是固定的一份，不接受自定义。
interface PaymentChannelSettings {
  gateway: string;
  merchant_id: string;
  key: string;
  crypto_currency: string;
  signature_algorithm: string;
  epay_mode: string;
  method: string;
  notify_url: string;
  return_url: string;
  fee_percent: string;
  fee_fixed: string;
  rate: string;
  configured: boolean;
  key_set: boolean;
}
interface PaymentSettings {
  version: number;
  channels: Record<string, PaymentChannelSettings>;
}
interface PaymentProtocol {
  id: string;
  name: string;
  hint: string;
  note?: string;
  merchant?: boolean;
  crypto?: boolean;
  signature?: boolean;
  mode?: boolean;
}
type PaymentRow = PaymentProtocol & PaymentChannelSettings;
const paymentProtocols: PaymentProtocol[] = [
  { id: "epay", name: "易支付 EPay", merchant: true, mode: true, hint: "https://pay.example.com" },
  { id: "epusdt", name: "原版 EPUSDT", hint: "https://usdt.example.com", note: "这个协议没有查单能力：核对订单会返回未实现，到账只认回调。" },
  { id: "bepusdt", name: "BEpusdt", hint: "https://bepusdt.example.com" },
  { id: "tokenpay", name: "TokenPay", crypto: true, signature: true, hint: "https://tokenpay.example.com" },
  { id: "cryptomus", name: "Cryptomus", merchant: true, hint: "https://api.cryptomus.com" },
];
function blankChannel(): PaymentChannelSettings {
  return { gateway: "", merchant_id: "", key: "", crypto_currency: "", signature_algorithm: "", epay_mode: "", method: "", notify_url: "", return_url: "", fee_percent: "", fee_fixed: "", rate: "", configured: false, key_set: false };
}
const paymentRows = ref<PaymentRow[]>([]);
const paymentVersion = ref(0),
  paymentError = ref(""),
  paymentBusy = ref(false),
  paymentLoading = ref(false);
function fillPaymentRows(channels: Record<string, PaymentChannelSettings>) {
  paymentRows.value = paymentProtocols.map((protocol) => ({
    ...protocol,
    ...blankChannel(),
    ...(channels[protocol.id] || {}),
    id: protocol.id,
    name: protocol.name,
    // 密钥永远不回明文，输入框一律从空开始：留空就是沿用已保存的那一把。
    key: "",
  }));
}
async function loadPayments() {
  paymentLoading.value = true;
  paymentError.value = "";
  try {
    const result = await api<PaymentSettings>("/payment-settings");
    paymentVersion.value = result.version;
    fillPaymentRows(result.channels || {});
  } catch (e) {
    paymentError.value = errorText(e);
  } finally {
    paymentLoading.value = false;
  }
}
// 请求体只带协议认得的字段：多带一个 id 或 name 会被服务端的严格解码挡回来。
function paymentPayload(row: PaymentRow): PaymentChannelSettings {
  return {
    gateway: row.gateway,
    merchant_id: row.merchant_id,
    key: row.key,
    crypto_currency: row.crypto_currency,
    signature_algorithm: row.signature_algorithm,
    epay_mode: row.epay_mode,
    method: row.method,
    notify_url: row.notify_url,
    return_url: row.return_url,
    fee_percent: row.fee_percent,
    fee_fixed: row.fee_fixed,
    rate: row.rate,
    configured: row.configured,
    key_set: row.key_set,
  };
}
async function savePayments() {
  paymentBusy.value = true;
  paymentError.value = "";
  try {
    const channels: Record<string, PaymentChannelSettings> = {};
    // 网关留空的那几条服务端会当作「不再使用这条通道」，不必单独删。
    for (const row of paymentRows.value)
      channels[row.id] = paymentPayload(row);
    const result = await api<PaymentSettings>("/payment-settings", "PUT", {
      version: paymentVersion.value,
      channels,
    });
    paymentVersion.value = result.version;
    fillPaymentRows(result.channels || {});
    notice("支付通道已保存，立即生效。");
  } catch (e) {
    paymentError.value = errorText(e);
  } finally {
    paymentBusy.value = false;
  }
}
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
  if (!allowed.value) return;
  void load();
  void loadPayments();
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
        商户密钥在下面的支付通道里逐条填写。
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
    <section
      v-if="allowed && form"
      class="card"
      aria-labelledby="payment-channels-title"
    >
      <h2 id="payment-channels-title">支付通道</h2>
      <p class="small muted">
        每条通道按协议填网关、商户号与密钥。商户密钥只保存在服务端，界面永远不回
        明文：留空就是沿用已经存下来的那一把。清空网关等于不再使用这条通道。
      </p>
      <p v-if="paymentError" class="error" role="alert">{{ paymentError }}</p>
      <p v-if="paymentLoading" class="empty">正在读取支付通道…</p>
      <template v-else>
        <details v-for="row in paymentRows" :key="row.id" class="payment-channel">
          <summary>
            <strong>{{ row.name }}</strong>
            <span class="muted small">
              {{ row.configured ? (row.key_set ? "已配置" : "已配置 · 缺少密钥") : "未配置" }}
            </span>
          </summary>
          <p v-if="row.note" class="small muted">{{ row.note }}</p>
          <div class="form-grid">
            <label
              >网关地址<input
                v-model="row.gateway"
                type="url"
                :placeholder="row.hint"
            /></label>
            <label v-if="row.merchant"
              >商户号<input
                v-model="row.merchant_id"
                :placeholder="row.id === 'cryptomus' ? '商户 ID' : '商户号'"
            /></label>
            <label
              >商户密钥<input
                v-model="row.key"
                type="password"
                autocomplete="new-password"
                :placeholder="row.key_set ? '留空沿用已保存的密钥' : '商户后台的密钥'"
            /></label>
            <label v-if="row.crypto"
              >加密货币<input
                v-model="row.crypto_currency"
                placeholder="USDT"
            /></label>
            <label v-if="row.signature"
              >签名算法<Select
                v-model="row.signature_algorithm"
                :aria-label="`${row.name} 签名算法`"
              >
                <option value="">md5（默认）</option>
                <option value="hmac-sha256">hmac-sha256</option>
              </Select></label>
            <label v-if="row.mode"
              >接口模式<Select
                v-model="row.epay_mode"
                :aria-label="`${row.name} 接口模式`"
              >
                <option value="">submit（默认）</option>
                <option value="mapi">mapi</option>
              </Select></label>
            <label
              >手续费百分比<input
                v-model="row.fee_percent"
                inputmode="decimal"
                placeholder="0.00"
            /></label>
            <label
              >固定手续费（元）<input
                v-model="row.fee_fixed"
                inputmode="decimal"
                placeholder="0.00"
            /></label>
            <label v-if="row.crypto"
              >汇率（1 币折多少元）<input
                v-model="row.rate"
                inputmode="decimal"
                placeholder="7.20"
            /></label>
          </div>
          <details>
            <summary>回调地址（留空由面板按站点地址生成）</summary>
            <div class="form-grid">
              <label
                >异步通知地址<input
                  v-model="row.notify_url"
                  type="url"
                  :placeholder="`/api/v1/payments/${row.id}/notify`"
              /></label>
              <label
                >支付完成返回地址<input
                  v-model="row.return_url"
                  type="url"
                  placeholder="留空回到充值页"
              /></label>
            </div>
            <p class="small muted">
              网关后台要填同一个异步通知地址；地址必须是公网 HTTPS。
            </p>
          </details>
        </details>
        <div class="form-actions">
          <button
            class="primary"
            type="button"
            :disabled="paymentBusy"
            :data-busy="String(paymentBusy)"
            :aria-busy="paymentBusy"
            @click="savePayments"
          >
            保存支付通道
          </button>
        </div>
      </template>
    </section>
  </section>
</template>
