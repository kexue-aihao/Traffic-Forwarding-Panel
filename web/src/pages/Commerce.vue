<script setup lang="ts">
import { onMounted, ref } from "vue";
import Modal from "../components/Modal.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
interface Plan {
  id: string;
  name: string;
  price_cents: string;
  quota_bytes: string;
  months: number;
}
interface Order {
  id: string;
  channel: string;
  amount_cents: string;
  currency: string;
  status: string;
  payment_url?: string;
  created_at: string;
}
interface Channel {
  id: string;
  name: string;
  enabled: boolean;
  status: string;
  reason: string;
}
interface Entitlement {
  id: string;
  plan_id: string;
  version: number;
  expires_at: string;
  quota_bytes: string;
  used_bytes: string;
}
interface Ledger {
  id: string;
  amount_cents: string;
  balance_cents: string;
  kind: string;
  reference: string;
  created_at: string;
}
const plans = ref<Plan[]>([]);
const orders = ref<Order[]>([]);
const channels = ref<Channel[]>([]);
const ledger = ref<Ledger[]>([]);
const wallet = ref<{ balance_cents: string; currency: string } | null>(null);
const entitlement = ref<Entitlement | null>(null);
const error = ref("");
const busy = ref(false);
const loading = ref(false);
const selected = ref<Plan | null>(null);
const charging = ref(false);
const creating = ref(false);
const amount = ref("1000");
const channel = ref("");
const formError = ref("");
const key = ref("");
const planForm = ref({
  name: "",
  price_cents: "1000",
  quota_bytes: "10737418240",
  months: 1,
});
function money(cents: string) {
  const n = BigInt(cents);
  const neg = n < 0n;
  const a = neg ? -n : n;
  return `${neg ? "-" : ""}${a / 100n}.${String(a % 100n).padStart(2, "0")}`;
}
async function load() {
  loading.value = true;
  error.value = "";
  const results = await Promise.allSettled([
    api<{ items: Plan[] }>("/plans").then((x) => {
      plans.value = x.items;
    }),
    api<typeof wallet.value>("/wallet").then((x) => {
      wallet.value = x;
    }),
    api<{ items: Order[] }>("/orders").then((x) => {
      orders.value = x.items;
    }),
    api<{ items: Channel[] }>("/payment-channels").then((x) => {
      channels.value = x.items;
    }),
    api<Entitlement | null>("/entitlement").then((x) => {
      entitlement.value = x;
    }),
    api<{ items: Ledger[] }>("/ledger").then((x) => {
      ledger.value = x.items;
    }),
  ]);
  const failures = results.filter(
    (r): r is PromiseRejectedResult => r.status === "rejected",
  );
  if (failures.length)
    error.value = `部分商业功能暂不可用：${failures.map((f) => errorText(f.reason)).join("；")}`;
  loading.value = false;
}
function startCharge() {
  charging.value = true;
  key.value = crypto.randomUUID();
  formError.value = "";
}
function buy(plan: Plan) {
  selected.value = plan;
  key.value = crypto.randomUUID();
  formError.value = "";
}
async function submit() {
  if (busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    if (selected.value) {
      await api("/purchases", "POST", {
        plan_id: selected.value.id,
        expected_version: entitlement.value?.version || 0,
        idempotency_key: key.value,
      });
      selected.value = null;
      notice("购买已由服务端确认，请查看更新后的权益。");
    } else if (charging.value) {
      await api("/orders", "POST", {
        channel: channel.value,
        amount_cents: amount.value,
        idempotency_key: key.value,
      });
      charging.value = false;
      notice("充值订单已创建，到账以服务端支付核实结果为准。");
    } else {
      await api("/plans", "POST", planForm.value);
      creating.value = false;
      notice("套餐已创建。");
    }
    await load();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
function paymentURL(raw: string | undefined) {
  if (!raw) return "";
  try {
    const u = new URL(raw);
    return ["https:", "http:"].includes(u.protocol) ? u.href : "";
  } catch {
    return "";
  }
}
onMounted(load);
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">BILLING & PLANS</p>
        <h1>套餐与钱包</h1>
        <p class="muted">充值钱包后购买套餐，交易与权益分别记录。</p>
      </div>
      <div class="toolbar">
        <button @click="load" :disabled="loading">刷新状态</button
        ><button
          v-if="adminSite && state.user?.role === 'admin'"
          @click="
            creating = true;
            formError = '';
          "
        >
          新增套餐
        </button>
      </div>
    </div>
    <p v-if="error" class="warning" role="alert">{{ error }}</p>
    <div class="stats">
      <div class="card stat">
        <span class="muted">钱包余额</span
        ><strong
          >{{ wallet ? money(wallet.balance_cents) : "—" }}
          <small>{{ wallet?.currency }}</small></strong
        ><button
          :disabled="!channels.some((c) => c.enabled)"
          @click="startCharge()"
        >
          充值钱包
        </button>
      </div>
      <div class="card">
        <h2>当前权益</h2>
        <template v-if="entitlement"
          ><p>到期 {{ new Date(entitlement.expires_at).toLocaleString() }}</p>
          <p class="muted">
            已用 {{ entitlement.used_bytes }} /
            {{ entitlement.quota_bytes }} 字节
          </p></template
        >
        <p v-else class="muted">暂无可展示的套餐权益。</p>
      </div>
    </div>
    <h2>可选套餐</h2>
    <p v-if="!plans.length" class="muted">暂无可购买套餐。</p>
    <div class="probe-grid">
      <article v-for="plan in plans" :key="plan.id" class="card">
        <h3>{{ plan.name }}</h3>
        <p class="price">¥ {{ money(plan.price_cents) }}</p>
        <p class="muted">
          {{ plan.months }} 个月 · {{ plan.quota_bytes }} 字节
        </p>
        <button class="primary" :disabled="!wallet" @click="buy(plan)">
          余额购买
        </button>
      </article>
    </div>
    <details class="card">
      <summary>支付通道状态</summary>
      <p v-if="!channels.length" class="muted">暂未获取可用支付通道。</p>
      <p v-for="c in channels" :key="c.id">
        {{ c.name }} · {{ c.enabled ? "已启用" : "不可用" }}
        <span class="muted">{{ c.reason || c.status }}</span>
      </p>
    </details>
    <h2>充值订单</h2>
    <div class="card table-wrap">
      <p v-if="!orders.length" class="empty">暂无订单。</p>
      <table v-else>
        <thead>
          <tr>
            <th>订单</th>
            <th>金额</th>
            <th>状态</th>
            <th>创建时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="o in orders" :key="o.id">
            <td data-label="订单">{{ o.id }}</td>
            <td data-label="金额">
              {{ money(o.amount_cents) }} {{ o.currency }}
            </td>
            <td data-label="状态">{{ o.status }}</td>
            <td data-label="创建时间">
              {{ new Date(o.created_at).toLocaleString() }}
            </td>
            <td data-label="操作">
              <a
                v-if="paymentURL(o.payment_url) && o.status === 'pending'"
                :href="paymentURL(o.payment_url)"
                target="_blank"
                rel="noopener noreferrer"
                >前往支付 ↗</a
              ><span v-else>—</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <h2>钱包账本</h2>
    <div class="card table-wrap">
      <p v-if="!ledger.length" class="empty">暂无账本记录。</p>
      <table v-else>
        <thead>
          <tr>
            <th>类型</th>
            <th>变动金额</th>
            <th>余额</th>
            <th>时间</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="l in ledger" :key="l.id">
            <td data-label="类型">{{ l.kind }}</td>
            <td data-label="变动金额">{{ money(l.amount_cents) }}</td>
            <td data-label="余额">{{ money(l.balance_cents) }}</td>
            <td data-label="时间">
              {{ new Date(l.created_at).toLocaleString() }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <Modal
      v-if="selected || charging || creating"
      :title="selected ? '确认购买' : charging ? '充值钱包' : '新增套餐'"
      :busy="busy"
      :dirty="charging || creating"
      @close="
        selected = null;
        charging = false;
        creating = false;
      "
      ><form @submit.prevent="submit">
        <p v-if="formError" class="error" role="alert">{{ formError }}</p>
        <template v-if="selected"
          ><p>{{ selected.name }} · ¥ {{ money(selected.price_cents) }}</p>
          <p class="warning">
            购买将立即开始新周期和有效期，替换当前权益。原有使用记录保留。
          </p></template
        ><template v-else-if="charging"
          ><label
            >支付渠道<select v-model="channel" required>
              <option value="" disabled>选择渠道</option>
              <option
                v-for="c in channels.filter((x) => x.enabled)"
                :key="c.id"
                :value="c.id"
              >
                {{ c.name }}
              </option>
            </select></label
          ><label
            >充值金额（分）<input
              v-model="amount"
              inputmode="numeric"
              pattern="[1-9][0-9]*"
              required
          /></label>
          <p class="muted small">
            创建订单不会增加余额，完成支付后由服务端确认到账。
          </p></template
        ><template v-else
          ><label>名称<input v-model="planForm.name" required /></label
          ><label
            >价格（分）<input
              v-model="planForm.price_cents"
              inputmode="numeric"
              pattern="[0-9]+"
              required /></label
          ><label
            >流量配额（字节）<input
              v-model="planForm.quota_bytes"
              inputmode="numeric"
              pattern="[1-9][0-9]*"
              required /></label
          ><label
            >有效月数<input
              v-model.number="planForm.months"
              type="number"
              min="1"
              max="120"
              required /></label></template
        ><button class="primary" :disabled="busy">
          {{ busy ? "正在提交…" : "确认提交" }}
        </button>
      </form></Modal
    >
  </section>
</template>
