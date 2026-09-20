<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import Modal from "../components/Modal.vue";
import { api, ApiError, errorText } from "../core/api";
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
const chargeDraft = ref<{
  channel: string;
  amount_cents: string;
  idempotency_key: string;
} | null>(null);
const draftStorageKey = "tfp-charge-draft:" + state.user?.id;
try {
  const saved = JSON.parse(
    sessionStorage.getItem(draftStorageKey) || "null",
  ) as unknown;
  if (
    saved &&
    typeof saved === "object" &&
    "channel" in saved &&
    typeof saved.channel === "string" &&
    "amount_cents" in saved &&
    typeof saved.amount_cents === "string" &&
    "idempotency_key" in saved &&
    typeof saved.idempotency_key === "string"
  )
    chargeDraft.value = {
      channel: saved.channel,
      amount_cents: saved.amount_cents,
      idempotency_key: saved.idempotency_key,
    };
} catch {
  /* unavailable session storage leaves the current in-memory retry intact */
}
function persistDraft() {
  try {
    if (chargeDraft.value)
      sessionStorage.setItem(
        draftStorageKey,
        JSON.stringify(chargeDraft.value),
      );
    else sessionStorage.removeItem(draftStorageKey);
  } catch {
    /* storage may be unavailable */
  }
}
const reconciling = ref<string | null>(null);
const orderError = ref<Record<string, string>>({});
const financialReady = ref(false);
const route = useRoute();
const router = useRouter();
const totals = ref({ plans: 0, orders: 0, ledger: 0 });
const pages = computed(() => ({
  plans: Math.max(1, Number(route.query.plans_page) || 1),
  orders: Math.max(1, Number(route.query.orders_page) || 1),
  ledger: Math.max(1, Number(route.query.ledger_page) || 1),
}));
function turnPage(kind: "plans" | "orders" | "ledger", page: number) {
  void router.replace({
    query: { ...route.query, [kind + "_page"]: String(page) },
  });
}
let loadVersion = 0;
const ledgerKinds: Record<string, string> = {
  payment: "充值到账",
  purchase: "套餐购买",
  adjustment: "人工调整",
  refund: "退款",
};
const channelNames: Record<string, string> = {
  epay: "易支付 EPay",
  epusdt: "EPUSDT（原版）",
  bepusdt: "BEpusdt（兼容版）",
  tokenpay: "TokenPay",
  cryptomus: "Cryptomus",
  cyber: "Cyber",
};
function channelName(id: string) {
  return channelNames[id] || "其他支付渠道";
}
function channelState(channel: Channel) {
  return (
    (
      {
        unconfigured: "尚未配置商户信息",
        configured_unverified: "已配置，尚未完成真实支付验证",
        blocked: "暂不可用，等待确认服务商与接口版本",
        not_implemented: "暂未接入",
        ready: "可用",
      } as Record<string, string>
    )[channel.status] || (channel.enabled ? "已启用" : "暂不可用")
  );
}
function orderState(order: Order) {
  if (order.status === "pending")
    return paymentURL(order.payment_url)
      ? "待支付"
      : "核实中（创建结果待确认）";
  return (
    (
      {
        paid: "已到账",
        closed: "已关闭",
        failed: "处理失败",
        refunded: "已退款",
      } as Record<string, string>
    )[order.status] || "状态待核实"
  );
}
function businessError(error: unknown) {
  if (error instanceof ApiError && error.code === "unsupported")
    return "该支付渠道不支持主动查单，请联系管理员核实，勿重复付款。";
  const message = errorText(error);
  return (
    (
      {
        "Insufficient available balance": "可用余额不足，请核对钱包余额。",
        "Request could not be completed":
          "暂时无法完成处理，请刷新状态或联系管理员核实。",
      } as Record<string, string>
    )[message] || message
  );
}
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
  const version = ++loadVersion;
  loading.value = true;
  financialReady.value = false;
  wallet.value = null;
  entitlement.value = null;
  error.value = "";
  const results = await Promise.allSettled([
    api<{ items: Plan[]; total: number }>(
      `/plans?page=${pages.value.plans}&page_size=20`,
    ).then((x) => {
      if (version !== loadVersion) return;
      plans.value = x.items;
      totals.value.plans = x.total;
    }),
    api<typeof wallet.value>("/wallet").then((x) => {
      if (version !== loadVersion) return;
      wallet.value = x;
    }),
    api<{ items: Order[]; total: number }>(
      `/orders?page=${pages.value.orders}&page_size=20`,
    ).then((x) => {
      if (version !== loadVersion) return;
      orders.value = x.items;
      totals.value.orders = x.total;
    }),
    api<{ items: Channel[] }>("/payment-channels").then((x) => {
      if (version !== loadVersion) return;
      channels.value = x.items;
    }),
    api<Entitlement | null>("/entitlement").then((x) => {
      if (version !== loadVersion) return;
      entitlement.value = x;
    }),
    api<{ items: Ledger[]; total: number }>(
      `/ledger?page=${pages.value.ledger}&page_size=20`,
    ).then((x) => {
      if (version !== loadVersion) return;
      ledger.value = x.items;
      totals.value.ledger = x.total;
    }),
  ]);
  if (version !== loadVersion) return;
  financialReady.value =
    results[1]?.status === "fulfilled" && results[4]?.status === "fulfilled";
  const failures = results.filter(
    (r): r is PromiseRejectedResult => r.status === "rejected",
  );
  if (failures.length)
    error.value = `部分商业功能暂不可用：${failures.map((f) => businessError(f.reason)).join("；")}`;
  loading.value = false;
}
function startCharge() {
  charging.value = true;
  if (chargeDraft.value) {
    channel.value = chargeDraft.value.channel;
    amount.value = chargeDraft.value.amount_cents;
  }
  key.value = chargeDraft.value?.idempotency_key || crypto.randomUUID();
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
      chargeDraft.value ??= {
        channel: channel.value,
        amount_cents: amount.value,
        idempotency_key: key.value,
      };
      persistDraft();
      const order = await api<Order>("/orders", "POST", chargeDraft.value);
      charging.value = false;
      chargeDraft.value = null;
      persistDraft();
      notice(
        order.status === "pending" && !paymentURL(order.payment_url)
          ? "订单创建结果正在核实，请查单或联系管理员，勿重复付款。"
          : "充值订单已创建，到账以服务端支付核实结果为准。",
      );
    } else {
      await api("/plans", "POST", planForm.value);
      creating.value = false;
      notice("套餐已创建。");
    }
    await load();
  } catch (e) {
    formError.value = charging.value
      ? "订单创建结果待核实。请先查看订单；再次提交仅核实同一次请求，不会创建新的充值意图。 " +
        businessError(e)
      : businessError(e);
    if (state.user) await load();
  } finally {
    busy.value = false;
  }
}
async function reconcile(order: Order) {
  if (reconciling.value) return;
  reconciling.value = order.id;
  delete orderError.value[order.id];
  try {
    const result = await api<Order>(
      `/orders/${encodeURIComponent(order.id)}/reconcile`,
      "POST",
      {},
    );
    notice(
      result.status === "paid"
        ? "支付已由服务端确认，正在更新钱包。"
        : "尚未确认到账，请勿重复付款。",
    );
  } catch (e) {
    orderError.value[order.id] = businessError(e);
  } finally {
    if (state.user) await load();
    reconciling.value = null;
  }
}
function refreshVisible() {
  if (!document.hidden && state.user && !busy.value && !reconciling.value)
    void load();
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
onMounted(() => {
  void load();
  window.addEventListener("focus", refreshVisible);
});
onUnmounted(() => {
  loadVersion++;
  window.removeEventListener("focus", refreshVisible);
});
watch(
  () => route.query,
  () => {
    void load();
  },
);
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
        <p v-else class="muted">
          {{
            financialReady
              ? "当前没有套餐权益。"
              : "权益状态正在刷新或暂不可用。"
          }}
        </p>
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
        <button
          class="primary"
          :disabled="!wallet || !financialReady || loading"
          @click="buy(plan)"
        >
          余额购买
        </button>
      </article>
    </div>
    <div class="pagination" aria-label="套餐分页">
      <span>共 {{ totals.plans }} 个套餐 · 第 {{ pages.plans }} 页</span
      ><button
        :disabled="loading || pages.plans <= 1"
        @click="turnPage('plans', pages.plans - 1)"
      >
        上一页套餐</button
      ><button
        :disabled="loading || pages.plans * 20 >= totals.plans"
        @click="turnPage('plans', pages.plans + 1)"
      >
        下一页套餐
      </button>
    </div>
    <details class="card">
      <summary>支付通道状态</summary>
      <p v-if="!channels.length" class="muted">暂未获取可用支付通道。</p>
      <p v-for="c in channels" :key="c.id">
        {{ channelName(c.id) }} · {{ c.enabled ? "已启用" : "不可用" }}
        <span class="muted">{{ channelState(c) }}</span>
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
            <td data-label="订单">
              {{ o.id }}<br /><span class="small muted">{{
                channelName(o.channel)
              }}</span>
            </td>
            <td data-label="金额">
              {{ money(o.amount_cents) }} {{ o.currency }}
            </td>
            <td data-label="状态">{{ orderState(o) }}</td>
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
              ><span v-else-if="o.status === 'pending'" class="small muted"
                >请核实订单，勿重复付款。</span
              >
              <button
                v-if="o.status === 'pending'"
                :disabled="!!reconciling || loading"
                @click="reconcile(o)"
              >
                {{ reconciling === o.id ? "正在查单…" : "核实支付状态" }}
              </button>
              <p v-if="orderError[o.id]" role="alert" class="error small">
                {{ orderError[o.id] }}
              </p>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div class="pagination" aria-label="订单分页">
      <span>共 {{ totals.orders }} 个订单 · 第 {{ pages.orders }} 页</span
      ><button
        :disabled="loading || pages.orders <= 1"
        @click="turnPage('orders', pages.orders - 1)"
      >
        上一页订单</button
      ><button
        :disabled="loading || pages.orders * 20 >= totals.orders"
        @click="turnPage('orders', pages.orders + 1)"
      >
        下一页订单
      </button>
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
            <td data-label="类型">{{ ledgerKinds[l.kind] || "账务变动" }}</td>
            <td data-label="变动金额">{{ money(l.amount_cents) }}</td>
            <td data-label="余额">{{ money(l.balance_cents) }}</td>
            <td data-label="时间">
              {{ new Date(l.created_at).toLocaleString() }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div class="pagination" aria-label="账本分页">
      <span>共 {{ totals.ledger }} 条记录 · 第 {{ pages.ledger }} 页</span
      ><button
        :disabled="loading || pages.ledger <= 1"
        @click="turnPage('ledger', pages.ledger - 1)"
      >
        上一页账本</button
      ><button
        :disabled="loading || pages.ledger * 20 >= totals.ledger"
        @click="turnPage('ledger', pages.ledger + 1)"
      >
        下一页账本
      </button>
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
            >支付渠道<select
              v-model="channel"
              required
              :disabled="!!chargeDraft"
            >
              <option value="" disabled>选择渠道</option>
              <option
                v-for="c in channels.filter((x) => x.enabled)"
                :key="c.id"
                :value="c.id"
              >
                {{ channelName(c.id) }}
              </option>
            </select></label
          ><label
            >充值金额（分）<input
              v-model="amount"
              :disabled="!!chargeDraft"
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
