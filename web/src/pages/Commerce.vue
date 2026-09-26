<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import Select from "../components/Select.vue";
import { useRoute, useRouter } from "vue-router";
import Modal from "../components/Modal.vue";
import { api, ApiError, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";
interface ResourceLimits {
  max_rules: number;
  max_connections_per_node: number;
  max_ips_per_node: number;
  bytes_per_second_per_node: string;
}
function emptyLimits(): ResourceLimits {
  return {
    max_rules: 0,
    max_connections_per_node: 0,
    max_ips_per_node: 0,
    bytes_per_second_per_node: "0",
  };
}
function limitsText(limits?: ResourceLimits) {
  const l = limits || emptyLimits();
  return `规则 ${l.max_rules || "不限"} · 每节点连接 ${l.max_connections_per_node || "不限"} · 每节点活跃 IP ${l.max_ips_per_node || "不限"} · 每节点合计带宽 ${l.bytes_per_second_per_node && l.bytes_per_second_per_node !== "0" ? l.bytes_per_second_per_node + " B/s" : "不限"}`;
}
interface Plan {
  limits?: ResourceLimits;
  id: string;
  name: string;
  price_cents: string;
  quota_bytes: string;
  months: number;
  active: boolean;
  version: number;
  kind: "period" | "addon";
}
interface Order {
  id: string;
  channel: string;
  // amount_cents 是钱包到账金额，payable_cents 是实际付给通道的金额，
  // 差额就是通道手续费。fee_cents 与 payable_crypto 由服务端算好。
  amount_cents: string;
  payable_cents?: string;
  fee_cents?: string;
  payable_crypto?: string;
  crypto_currency?: string;
  rate?: string;
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
  // 手续费与汇率都由服务端配置，前端只负责把结果算给用户看。
  fee_percent?: string;
  fee_fixed?: string;
  crypto_currency?: string;
  rate?: string;
}
interface Entitlement {
  limits?: ResourceLimits;
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
interface AutoRenew {
  enabled: boolean;
  plan_id?: string;
  last_error?: string;
}
const plans = ref<Plan[]>([]);
const orders = ref<Order[]>([]);
const channels = ref<Channel[]>([]);
const ledger = ref<Ledger[]>([]);
const wallet = ref<{ balance_cents: string; currency: string } | null>(null);
const entitlement = ref<Entitlement | null>(null);
const autoRenew = ref<AutoRenew>({ enabled: false });
const error = ref("");
const renewBusy = ref(false);
const renewError = ref("");
const renewPlan = ref("");
const busy = ref(false);
const loading = ref(false);
const selected = ref<Plan | null>(null);
const charging = ref(false);
const creating = ref(false);
const editingPlan = ref<Plan | null>(null);
// 充值金额以元为单位填写（最多两位小数），与站点设置一致。
const amount = ref("100");
const channel = ref("");
const formError = ref("");
const key = ref("");
// 未确认的支付意图跨刷新保留：金额以元记账，重试时必须原样重发，
// 否则幂等键会把两次不同的请求判成冲突。
const chargeDraft = ref<{
  channel: string;
  amount: string;
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
    "amount" in saved &&
    typeof saved.amount === "string" &&
    "idempotency_key" in saved &&
    typeof saved.idempotency_key === "string"
  )
    chargeDraft.value = {
      channel: saved.channel,
      amount: saved.amount,
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
  recharge: "充值到账",
  redeem: "兑换到账",
  commission: "佣金结算",
  refund_reserve: "退款预留",
  refund_release: "取消退款返还",
  purchase: "套餐购买",
  addon: "流量叠加包",
  commission_reversal: "佣金回冲",
  adjustment: "人工调整",
  refund: "退款",
};
const channelNames: Record<string, string> = {
  epay: "易支付 EPay",
  epusdt: "EPUSDT（原版）",
  bepusdt: "BEpusdt（兼容版）",
  tokenpay: "TokenPay",
  cryptomus: "Cryptomus",
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
        partially_refunded: "部分退款",
        paid_late: "晚到已到账",
      } as Record<string, string>
    )[order.status] || "状态待核实"
  );
}
function businessError(error: unknown) {
  if (error instanceof ApiError && error.code === "not_implemented")
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
  limits: emptyLimits(),
  name: "",
  price_cents: "1000",
  quota_bytes: "10737418240",
  months: 1,
  kind: "period" as "period" | "addon",
});
function money(cents: string) {
  const n = BigInt(cents);
  const neg = n < 0n;
  const a = neg ? -n : n;
  return `${neg ? "-" : ""}${a / 100n}.${String(a % 100n).padStart(2, "0")}`;
}

const selectedChannel = computed(
  () => channels.value.find((c) => c.id === channel.value) || null,
);

/**
 * 充值报价：钱包到账多少、手续费多少、实付多少、折合多少 USDT。
 *
 * 这里算的只是一份**预估**，真正下单和入账以服务端的同一套规则为准；界面提前
 * 把数字摆出来，是为了让人在点「支付」之前就知道自己要付多少。手续费按万分之
 * 一向下取整到分，与后端一致。
 */
const chargeQuote = computed(() => {
  const yuan = amount.value.trim();
  if (!/^[0-9]+(\.[0-9]{1,2})?$/.test(yuan)) return null;
  const cents = Math.round(Number(yuan) * 100);
  if (!Number.isFinite(cents) || cents < 1) return null;
  const c = selectedChannel.value;
  const percent = Number(c?.fee_percent || "0");
  const fixed = Math.round(Number(c?.fee_fixed || "0") * 100);
  const fee = Math.floor((cents * Math.round(percent * 100)) / 10000) + fixed;
  const payable = cents + fee;
  const rate = Number(c?.rate || "0");
  const crypto =
    c?.crypto_currency && rate > 0
      ? `${(Math.ceil((payable / 100 / rate) * 100) / 100).toFixed(2)} ${c.crypto_currency}`
      : "";
  return {
    amount: money(String(cents)),
    fee: money(String(fee)),
    payable: money(String(payable)),
    crypto,
    currency: c?.crypto_currency || "",
  };
});
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
    api<AutoRenew>("/auto-renew").then((x) => {
      if (version !== loadVersion) return;
      autoRenew.value = x;
      renewPlan.value = x.plan_id || entitlement.value?.plan_id || "";
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
async function saveAutoRenew(enabled: boolean) {
  if (renewBusy.value) return;
  const plan = renewPlan.value;
  renewError.value = "";
  if (enabled && !plan) {
    renewError.value = "请选择自动续费套餐。";
    return;
  }
  renewBusy.value = true;
  try {
    autoRenew.value = await api<AutoRenew>("/auto-renew", "POST", {
      enabled,
      plan_id: enabled ? plan : "",
    });
    notice(
      enabled ? "自动续费已开启，到期后使用钱包余额购买。" : "自动续费已关闭。",
    );
  } catch (e) {
    renewError.value = businessError(e);
  } finally {
    renewBusy.value = false;
  }
}
async function toggleAutoRenew(event: Event) {
  const target = event.target;
  if (target instanceof HTMLInputElement) {
    await saveAutoRenew(target.checked);
    target.checked = autoRenew.value.enabled;
  }
}
async function setPlanActive(plan: Plan) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await api(`/plans/${encodeURIComponent(plan.id)}`, "PATCH", {
      active: !plan.active,
    });
    await load();
  } catch (e) {
    error.value = businessError(e);
  } finally {
    busy.value = false;
  }
}
async function closeOrder(order: Order) {
  if (busy.value) return;
  busy.value = true;
  try {
    await api(`/orders/${encodeURIComponent(order.id)}/close`, "POST", {});
    await load();
  } catch (e) {
    orderError.value[order.id] = businessError(e);
  } finally {
    busy.value = false;
  }
}
function startCharge() {
  charging.value = true;
  if (chargeDraft.value) {
    channel.value = chargeDraft.value.channel;
    amount.value = chargeDraft.value.amount;
  }
  key.value = chargeDraft.value?.idempotency_key || crypto.randomUUID();
  formError.value = "";
}
function editPlan(plan: Plan) {
  editingPlan.value = plan;
  creating.value = true;
  formError.value = "";
  planForm.value = {
    name: plan.name,
    price_cents: plan.price_cents,
    quota_bytes: plan.quota_bytes,
    months: plan.months || 1,
    kind: plan.kind,
    limits: { ...(plan.limits || emptyLimits()) },
  };
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
      await api(
        selected.value.kind === "addon" ? "/addon-purchases" : "/purchases",
        "POST",
        {
          plan_id: selected.value.id,
          expected_plan_version: selected.value.version,
          expected_version: entitlement.value?.version || 0,
          idempotency_key: key.value,
        },
      );
      selected.value = null;
      notice("购买已由服务端确认，请查看更新后的权益。");
    } else if (charging.value) {
      chargeDraft.value ??= {
        channel: channel.value,
        amount: amount.value.trim(),
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
      const payload = {
        ...planForm.value,
        limits:
          planForm.value.kind === "addon"
            ? emptyLimits()
            : planForm.value.limits,
        months: planForm.value.kind === "addon" ? 0 : planForm.value.months,
        ...(editingPlan.value ? { version: editingPlan.value.version } : {}),
      };
      await api(
        editingPlan.value
          ? `/plans/${encodeURIComponent(editingPlan.value.id)}`
          : "/plans",
        editingPlan.value ? "PUT" : "POST",
        payload,
      );
      editingPlan.value = null;
      creating.value = false;
      notice("套餐已保存。");
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
      ["paid", "paid_late"].includes(result.status)
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
            editingPlan = null;
            formError = '';
          "
        >
          新增套餐
        </button>
      </div>
    </div>
    <p class="small muted">时间使用{{ displayTimeZoneLabel }}。</p>
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
          ><p>到期 {{ formatDateTime(entitlement.expires_at) }}</p>
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
      <div class="card">
        <h2>自动续费</h2>
        <p class="muted">
          到期后使用钱包余额购买所选套餐，并从成功时刻重置周期；余额不足时每小时重试。
        </p>
        <label for="renew-plan">自动续费套餐</label
        ><Select
          id="renew-plan"
          v-model="renewPlan"
          :disabled="renewBusy || loading || autoRenew.enabled"
        >
          <option value="">请选择套餐</option>
          <option
            v-for="p in plans.filter((p) => p.active && p.kind !== 'addon')"
            :key="p.id"
            :value="p.id"
          >
            {{ p.name }}
          </option>
        </Select>
        <p v-if="renewError" role="alert" class="error">{{ renewError }}</p>
        <p v-if="autoRenew.last_error" class="warning">
          {{
            autoRenew.last_error === "insufficient_funds"
              ? "余额不足，等待下次重试。"
              : "上次续费未完成，请检查套餐或联系管理员。"
          }}
        </p>
        <label class="toggle-row">
          <input
            type="checkbox"
            :checked="autoRenew.enabled"
            :disabled="loading || busy || renewBusy"
            @change="toggleAutoRenew"
          />
          <span>{{ autoRenew.enabled ? "已开启" : "已关闭" }}</span>
        </label>
      </div>
    </div>
    <p v-if="entitlement" class="muted small">
      当前权益限制：{{ limitsText(entitlement.limits) }}
    </p>
    <h2>可选套餐</h2>
    <p v-if="!plans.length" class="muted">暂无可购买套餐。</p>
    <div class="probe-grid">
      <article v-for="plan in plans" :key="plan.id" class="card">
        <h3>{{ plan.name }}</h3>
        <p class="price">¥ {{ money(plan.price_cents) }}</p>
        <p class="muted">
          {{ plan.kind === "addon" ? "流量叠加包" : `${plan.months} 个月` }} ·
          {{ plan.quota_bytes }} 字节
        </p>
        <button
          class="primary"
          :disabled="
            !plan.active ||
            !wallet ||
            !financialReady ||
            loading ||
            (plan.kind === 'addon' && !entitlement)
          "
          @click="buy(plan)"
        >
          {{
            !plan.active
              ? "暂停售"
              : plan.kind === "addon"
                ? "购买叠加包"
                : "余额购买"
          }}
        </button>
        <p v-if="plan.kind !== 'addon'" class="small muted">
          {{ limitsText(plan.limits) }}
        </p>
        <button
          v-if="adminSite && state.user?.role === 'admin'"
          :disabled="busy"
          @click="editPlan(plan)"
        >
          编辑套餐
        </button>
        <button
          v-if="adminSite && state.user?.role === 'admin'"
          :disabled="busy"
          @click="setPlanActive(plan)"
        >
          {{ plan.active ? "停售" : "重新上架" }}
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
              <template v-if="o.fee_cents && o.fee_cents !== '0'">
                <br /><span class="small muted"
                  >实付 {{ money(o.payable_cents || o.amount_cents) }} · 手续费
                  {{ money(o.fee_cents) }}</span
                >
              </template>
              <template v-if="o.payable_crypto">
                <br /><span class="small muted"
                  >≈ {{ o.payable_crypto }} {{ o.crypto_currency }}</span
                >
              </template>
            </td>
            <td data-label="状态">{{ orderState(o) }}</td>
            <td data-label="创建时间">
              {{ formatDateTime(o.created_at) }}
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
                v-if="['pending', 'closed', 'expired'].includes(o.status)"
                :disabled="!!reconciling || loading"
                @click="reconcile(o)"
              >
                {{ reconciling === o.id ? "正在查单…" : "核实支付状态" }}
              </button>
              <button
                v-if="o.status === 'pending'"
                :disabled="busy"
                @click="closeOrder(o)"
              >
                关闭订单
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
              {{ formatDateTime(l.created_at) }}
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
      :title="
        selected
          ? '确认购买'
          : charging
            ? '充值钱包'
            : editingPlan
              ? '编辑套餐'
              : '新增套餐'
      "
      :busy="busy"
      :dirty="charging || creating"
      @close="
        selected = null;
        charging = false;
        creating = false;
        editingPlan = null;
      "
      ><form @submit.prevent="submit">
        <p v-if="formError" class="error" role="alert">{{ formError }}</p>
        <template v-if="selected"
          ><p>{{ selected.name }} · ¥ {{ money(selected.price_cents) }}</p>
          <p class="warning">
            {{
              selected.kind === "addon"
                ? "叠加包增加当前周期配额，到期时间保持不变。"
                : "购买将立即开始新周期和有效期，替换当前权益。原有使用记录保留。"
            }}
          </p></template
        ><template v-else-if="charging"
          ><label
            >支付渠道<Select
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
                {{ channelName(c.id)
                }}<template
                  v-if="c.fee_percent && c.fee_percent !== '0.00' || (c.fee_fixed && c.fee_fixed !== '0.00')"
                  >（手续费 {{ c.fee_percent }}% + ¥{{ c.fee_fixed }}）</template
                >
              </option>
            </Select></label
          ><label
            >充值金额（元）<input
              v-model="amount"
              :disabled="!!chargeDraft"
              inputmode="decimal"
              pattern="[0-9]+(\.[0-9]{1,2})?"
              required
          /></label>
          <dl v-if="chargeQuote" class="metrics">
            <div>
              <dt>钱包到账</dt>
              <dd>¥ {{ chargeQuote.amount }}</dd>
            </div>
            <div>
              <dt>通道手续费</dt>
              <dd>
                ¥ {{ chargeQuote.fee }}
                <span
                  v-if="
                    (selectedChannel?.fee_percent &&
                      selectedChannel.fee_percent !== '0.00') ||
                    (selectedChannel?.fee_fixed &&
                      selectedChannel.fee_fixed !== '0.00')
                  "
                  class="small muted"
                  >（{{ selectedChannel?.fee_percent || "0.00" }}% + ¥{{
                    selectedChannel?.fee_fixed || "0.00"
                  }}）</span
                >
              </dd>
            </div>
            <div>
              <dt>实付</dt>
              <dd>¥ {{ chargeQuote.payable }}</dd>
            </div>
            <div v-if="chargeQuote.crypto">
              <dt>折合应付</dt>
              <dd>
                ≈ {{ chargeQuote.crypto }}
                <span class="small muted">（汇率 {{ selectedChannel?.rate }}）</span>
              </dd>
            </div>
          </dl>
          <p class="muted small">
            创建订单不会增加余额，完成支付后由服务端确认到账。到账金额与实付金额分开
            记账：手续费只增加实付，不影响钱包到账。加密货币金额是按通道汇率折算的
            预估值，最终以支付页面显示的金额为准。
          </p></template
        ><template v-else
          ><label
            >套餐类型<Select v-model="planForm.kind" :disabled="!!editingPlan">
              <option value="period">周期套餐</option>
              <option value="addon">流量叠加包</option>
            </Select></label
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
              required
          /></label>
          <fieldset v-if="planForm.kind === 'period'">
            <legend>套餐限制</legend>
            <p class="small muted">
              0 表示不限。规则数包含停用规则，按账号跨节点统计；连接、活跃 IP
              和上下行合计带宽由同一账号在每个节点的所有规则共享。编辑仅影响后续购买或兑换。
            </p>
            <label
              >账号规则总数<input
                v-model.number="planForm.limits.max_rules"
                type="number"
                min="0"
                max="100000"
                required
            /></label>
            <label
              >每节点最大连接数<input
                v-model.number="planForm.limits.max_connections_per_node"
                type="number"
                min="0"
                max="1000000"
                required
            /></label>
            <label
              >每节点活跃 IP 数<input
                v-model.number="planForm.limits.max_ips_per_node"
                type="number"
                min="0"
                max="1000000"
                required
            /></label>
            <label
              >每节点上下行合计（B/s）<input
                v-model="planForm.limits.bytes_per_second_per_node"
                inputmode="numeric"
                pattern="[0-9]+"
                required
            /></label>
          </fieldset>
          <label v-if="planForm.kind === 'period'"
            >有效月数<input
              v-model.number="planForm.months"
              type="number"
              min="1"
              max="120"
              required /></label></template
        ><div class="form-actions">
          <button
            class="primary"
            :disabled="busy"
            :data-busy="String(busy)"
            :aria-busy="busy"
          >
            确认提交
          </button>
        </div>
      </form></Modal
    >
  </section>
</template>
