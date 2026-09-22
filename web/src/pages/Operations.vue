<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import Select from "../components/Select.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
import { formatDateTime } from "../core/format";
const admin = computed(() => adminSite && state.user?.role === "admin");
interface Task {
  id: string;
  kind: string;
  status: string;
  result?: string;
  error?: string;
  created_at: string;
}
interface Hook {
  format?: string;
  id: string;
  url: string;
  events: string[];
  enabled: boolean;
  muted_until: string | null;
  version: number;
}
interface Event {
  id: string;
  kind: string;
  payload: string;
  created_at: string;
}
interface AlertPolicy {
  enabled: boolean;
  offline_seconds: number;
  recovery_seconds: number;
  expiry_hours: number;
  remaining_percent: number;
  version: number;
}
interface Delivery {
  event_id: string;
  subscription_id: string;
  status: string;
  attempts: number;
}
interface Commission {
  refunded_cents?: string;
  id: string;
  amount_cents: string;
  status: string;
  created_at: string;
}
interface Code {
  id: string;
  code_hint: string;
  amount_cents: string;
  plan_id: string;
  used: number;
  max_uses: number;
  enabled: boolean;
}
interface Refund {
  id: string;
  status: string;
  amount_cents: string;
  reason: string;
}
const tasks = ref<Task[]>([]),
  hooks = ref<Hook[]>([]),
  deliveries = ref<Delivery[]>([]),
  commissions = ref<Commission[]>([]),
  codes = ref<Code[]>([]),
  refunds = ref<Refund[]>([]);
const error = ref(""),
  busy = ref(false),
  loading = ref(false),
  secret = ref(""),
  secretLabel = ref("");
const redeem = ref(""),
  invitation = ref(""),
  hookURL = ref(""),
  importJSON = ref("");
const codeForm = ref({ amount_cents: "1000", plan_id: "", max_uses: 1 });
const rate = ref(0),
  refundOrder = ref(""),
  refundAmount = ref(""),
  refundReason = ref(""),
  evidence = ref(""),
  commissionID = ref(""),
  commissionReason = ref("");
const detail = ref<Task | null>(null);
const importMode = ref("create"),
  preview = ref<
    {
      index: number;
      action: string;
      error?: string;
      rule?: Record<string, unknown>;
    }[]
  >([]),
  previewSource = ref(""),
  hookFormat = ref("webhook"),
  purchaseID = ref(""),
  purchaseAmount = ref(""),
  purchaseReason = ref(""),
  funding = ref<
    { order_id: string; amount_cents: string; refunded_cents: string }[]
  >([]);
watch([importJSON, importMode], () => {
  preview.value = [];
  previewSource.value = "";
});
async function previewImport() {
  await run(async () => {
    const parsed = JSON.parse(importJSON.value);
    const rules = Array.isArray(parsed) ? parsed : parsed.items;
    if (!Array.isArray(rules) || !rules.length)
      throw Error("请输入规则数组或导出文件。");
    preview.value = (
      await api<{ items: typeof preview.value }>(
        "/tasks/rules/preview",
        "POST",
        { rules, mode: importMode.value },
      )
    ).items;
    previewSource.value = importJSON.value;
  });
}
async function lookupFunding() {
  await run(async () => {
    funding.value = (
      await api<{ items: typeof funding.value }>(
        `/purchases/${encodeURIComponent(purchaseID.value)}/funding`,
      )
    ).items;
  });
}
async function refundPurchase() {
  await run(async () => {
    const body = {
      amount_cents: purchaseAmount.value,
      reason: purchaseReason.value,
    };
    await api(
      `/purchases/${encodeURIComponent(purchaseID.value)}/refund`,
      "POST",
      {
        ...body,
        idempotency_key: await keyFor(
          "purchase-refund:" + purchaseID.value,
          body,
        ),
      },
    );
    notice(
      "套餐退款已入钱包，配额和相关佣金已同步调整。需要原路退款时，继续登记充值退款。",
    );
    funding.value = (
      await api<{ items: typeof funding.value }>(
        `/purchases/${encodeURIComponent(purchaseID.value)}/funding`,
      )
    ).items;
  });
}

const hookEvents = ref("*"),
  events = ref<Event[]>([]),
  policy = ref<AlertPolicy | null>(null);
const eventOptions = [
  { value: "*", label: "全部事件" },
  { value: "node.offline,node.recovered", label: "节点离线和恢复" },
  {
    value: "entitlement.expiring,entitlement.expired,entitlement.low_quota",
    label: "套餐到期和低配额",
  },
  {
    value: "wallet.recharge,refund.completed,refund.canceled",
    label: "充值和退款",
  },
];
const eventNames: Record<string, string> = {
  "node.offline": "节点离线",
  "node.recovered": "节点恢复",
  "entitlement.expiring": "套餐即将到期",
  "entitlement.expired": "套餐已到期",
  "entitlement.low_quota": "套餐剩余流量不足",
  "wallet.recharge": "充值到账",
  "wallet.purchase": "购买扣款",
  "entitlement.purchased": "套餐已购买",
  "entitlement.redeemed": "套餐已兑换",
  "wallet.redeem": "兑换到账",
  "wallet.addon": "叠加包扣款",
  "entitlement.addon": "流量已增加",
  "alerts.policy_updated": "告警设置已更新",
};
function eventSummary(e: Event) {
  try {
    const data = JSON.parse(e.payload).data;
    return (
      data.name ||
      (data.remaining_bytes
        ? `剩余 ${data.remaining_bytes} 字节`
        : data.amount_cents
          ? `${data.amount_cents} 分`
          : "")
    );
  } catch {
    return "";
  }
}
async function savePolicy() {
  await run(async () => {
    policy.value = await api<AlertPolicy>("/alert-policy", "PUT", policy.value);
    notice("告警设置已保存。");
  });
}
async function updateHook(h: Hook, values: Partial<Hook>) {
  await run(async () => {
    await api(`/webhooks/${encodeURIComponent(h.id)}`, "PUT", {
      events: h.events,
      enabled: h.enabled,
      muted_until: h.muted_until,
      version: h.version,
      ...values,
    });
    await load();
  });
}
function muteHook(h: Hook) {
  return updateHook(h, {
    muted_until: new Date(Date.now() + 3600000).toISOString(),
  });
}
function hookMuted(h: Hook) {
  return !!h.muted_until && Date.parse(h.muted_until) > Date.now();
}
function changeEvents(h: Hook, value: string) {
  return updateHook(h, { events: value.split(",") });
}
let active = true;
onUnmounted(() => {
  active = false;
  secret.value = "";
});
function status(value: string) {
  return (
    (
      {
        pending: "待执行",
        running: "执行中",
        completed: "已完成",
        completed_with_errors: "部分失败",
        canceled: "已取消",
        failed: "失败",
        paid: "已结算",
        reversed: "已回冲",
        pending_external: "等待外部退款",
        delivered: "已投递",
        suppressed: "已静默",
        access_revoked: "授权已撤销",
      } as Record<string, string>
    )[value] || value
  );
}
async function load() {
  if (loading.value) return;
  loading.value = true;
  error.value = "";
  const results = await Promise.allSettled([
    api<{ items: Task[] }>("/tasks").then((x) => {
      if (active) tasks.value = x.items || [];
    }),
    api<{ items: Hook[] }>("/webhooks").then((x) => {
      if (active) hooks.value = x.items || [];
    }),
    api<{ items: Delivery[] }>("/webhook-deliveries").then((x) => {
      if (active) deliveries.value = x.items || [];
    }),
    api<{ items: Event[] }>("/events").then((x) => {
      if (active) events.value = x.items || [];
    }),
    api<{ items: Commission[] }>("/commissions").then((x) => {
      if (active) commissions.value = x.items || [];
    }),
    ...(admin.value
      ? [
          api<AlertPolicy>("/alert-policy").then((x) => {
            if (active) policy.value = x;
          }),
          api<{ items: Code[] }>("/redeem-codes").then((x) => {
            if (active) codes.value = x.items || [];
          }),
          api<{ rate_bps: number }>("/commission-policy").then((x) => {
            if (active) rate.value = x.rate_bps;
          }),
        ]
      : []),
  ]);
  if (active) {
    error.value = results
      .filter((x): x is PromiseRejectedResult => x.status === "rejected")
      .map((x) => errorText(x.reason))
      .join("；");
    loading.value = false;
  }
}
async function run(fn: () => Promise<void>) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
  } catch (e) {
    if (active) error.value = errorText(e);
  } finally {
    if (active) busy.value = false;
  }
}
// A digest preserves the retry key across reloads without storing imported credentials.
async function keyFor(scope: string, payload: unknown) {
  const digest = Array.from(
    new Uint8Array(
      await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(JSON.stringify(payload)),
      ),
    ),
  )
    .map((x) => x.toString(16).padStart(2, "0"))
    .join("");
  const name = `tfp-operation:${state.user?.id}:${scope}:${digest}`;
  let key = sessionStorage.getItem(name);
  if (!key) {
    key = crypto.randomUUID();
    sessionStorage.setItem(name, key);
  }
  return key;
}
async function redeemCode() {
  await run(async () => {
    await api("/redeem", "POST", { code: redeem.value });
    redeem.value = "";
    notice("兑换成功，请在套餐与钱包页面查看权益。");
  });
}
async function createInvitation() {
  await run(async () => {
    const x = await api<{ code: string }>("/referrals", "POST", {});
    secret.value = x.code;
    secretLabel.value = "邀请码（仅显示本次）";
  });
}
async function bindInvitation() {
  await run(async () => {
    await api("/referrals/bind", "POST", { code: invitation.value });
    invitation.value = "";
    notice("邀请关系已绑定。");
  });
}
async function createHook() {
  await run(async () => {
    const x = await api<{ secret: string }>("/webhooks", "POST", {
      url: hookURL.value,
      format: hookFormat.value,
      events: hookEvents.value.split(","),
    });
    secret.value = x.secret;
    secretLabel.value = "通知签名密钥（仅显示本次）";
    hookURL.value = "";
    await load();
  });
}
async function removeHook(id: string) {
  await run(async () => {
    await api(`/webhooks/${encodeURIComponent(id)}`, "DELETE");
    await load();
  });
}
async function createCode() {
  await run(async () => {
    const x = await api<{ code: string }>(
      "/redeem-codes",
      "POST",
      codeForm.value,
    );
    secret.value = x.code;
    secretLabel.value = "兑换码（仅显示本次）";
    await load();
  });
}
async function revokeCode(id: string) {
  await run(async () => {
    await api(`/redeem-codes/${encodeURIComponent(id)}`, "DELETE");
    await load();
  });
}
async function saveRate() {
  await run(async () => {
    await api("/commission-policy", "PUT", { rate_bps: rate.value });
    notice("返佣比例已保存，仅影响后续购买。");
  });
}
async function resolveCommission(action: string) {
  await run(async () => {
    await api(
      `/commissions/${encodeURIComponent(commissionID.value)}/resolve`,
      "POST",
      { action, reason: commissionReason.value },
    );
    notice("佣金处理已记录。");
    await load();
  });
}
async function exportRules() {
  await run(async () => {
    await api("/tasks/rules/export", "POST", {
      idempotency_key: crypto.randomUUID(),
    });
    await load();
  });
}
async function importRules() {
  await run(async () => {
    const parsed: unknown = JSON.parse(importJSON.value);
    const rules = Array.isArray(parsed)
      ? parsed
      : parsed && typeof parsed === "object" && "items" in parsed
        ? parsed.items
        : null;
    if (!Array.isArray(rules) || !rules.length)
      throw Error("请输入规则数组或包含 items 的导出文件内容。");
    if (
      !preview.value.length ||
      previewSource.value !== importJSON.value ||
      preview.value.some((x) => x.error)
    )
      throw Error("请先预览并修正冲突。");
    const confirmed = rules.map((rule: Record<string, unknown>, i: number) => ({
      ...rule,
      version: preview.value[i]?.rule?.version || 0,
    }));
    const payload = { rules: confirmed, mode: importMode.value };
    await api("/tasks/rules/import", "POST", {
      ...payload,
      idempotency_key: await keyFor("import", payload),
    });
    importJSON.value = "";
    notice("导入任务已创建。");
    await load();
  });
}
async function viewTask(t: Task) {
  await run(async () => {
    detail.value = await api<Task>(`/tasks/${encodeURIComponent(t.id)}`);
  });
}
async function cancelTask(t: Task) {
  await run(async () => {
    await api(`/tasks/${encodeURIComponent(t.id)}/cancel`, "POST", {});
    await load();
  });
}
function download() {
  if (!detail.value?.result) return;
  const blob = new Blob([detail.value.result], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `${detail.value.kind}-${detail.value.id}.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
async function readRefunds() {
  refunds.value =
    (
      await api<{ items: Refund[] }>(
        `/orders/${encodeURIComponent(refundOrder.value)}/refunds`,
      )
    ).items || [];
}
async function requestRefund() {
  await run(async () => {
    const payload = {
      amount_cents: refundAmount.value,
      reason: refundReason.value,
    };
    await api(
      `/orders/${encodeURIComponent(refundOrder.value)}/refund`,
      "POST",
      {
        ...payload,
        idempotency_key: await keyFor("refund:" + refundOrder.value, payload),
      },
    );
    await readRefunds();
    notice("已预留钱包余额，需在外部退款完成后登记凭证。");
  });
}
async function resolveRefund(item: Refund, completed: boolean) {
  await run(async () => {
    if (!evidence.value.trim()) throw Error("请填写外部退款凭证或取消依据。");
    await api(`/refunds/${encodeURIComponent(item.id)}/resolve`, "POST", {
      completed,
      evidence: evidence.value,
    });
    await readRefunds();
  });
}
onMounted(load);
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">OPERATIONS</p>
        <h1>运营与任务</h1>
        <p class="muted">兑换、邀请、通知与批量规则操作。</p>
      </div>
      <button :disabled="loading || busy" @click="load">刷新列表</button>
    </div>
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <section v-if="secret" class="card">
      <h2>{{ secretLabel }}</h2>
      <p class="mono">{{ secret }}</p>
      <p class="muted">请妥善保存，关闭后无法再次读取。</p>
      <button @click="secret = ''">已保存，关闭</button>
    </section>
    <div class="probe-grid">
      <section class="card">
        <h2>兑换权益</h2>
        <form @submit.prevent="redeemCode">
          <label
            >兑换码<input
              v-model="redeem"
              required
              maxlength="128"
              autocomplete="off" /></label
          ><div class="form-actions">
            <button class="primary" :disabled="busy">兑换</button>
          </div>
        </form>
      </section>
      <section class="card">
        <h2>邀请</h2>
        <p class="muted">每个账号可生成一个邀请码；绑定需在首次购买前完成。</p>
        <button :disabled="busy" @click="createInvitation">生成邀请码</button>
        <form @submit.prevent="bindInvitation">
          <label
            >邀请人的邀请码<input
              v-model="invitation"
              required
              maxlength="128"
              autocomplete="off" /></label
          ><button :disabled="busy">绑定邀请人</button>
        </form>
        <p v-if="!commissions.length" class="muted">暂无佣金记录。</p>
        <p v-for="c in commissions" :key="c.id">
          {{ c.amount_cents }}（已回冲 {{ c.refunded_cents || "0" }}） 分 ·
          {{ status(c.status) }} ·
          {{ formatDateTime(c.created_at) }}
        </p>
      </section>
    </div>
    <section class="card">
      <h2>事件通知</h2>
      <p class="muted">
        通知发送到你提供的公网 HTTPS 地址。接收端须核验签名并按事件 ID
        去重；失败最多尝试 12 次。
      </p>
      <form @submit.prevent="createHook">
        <label
          >通知通道<Select v-model="hookFormat" aria-label="通知通道">
            <option value="webhook">签名 Webhook</option>
            <option value="feishu">飞书群机器人</option>
            <option value="discord">Discord Webhook</option>
          </Select></label
        >
        <label
          >接收地址<input
            v-model="hookURL"
            type="url"
            placeholder="https://hooks.example.com/events"
            required
            maxlength="2048" /></label
        ><label
          >订阅事件<Select v-model="hookEvents" aria-label="订阅事件">
            <option v-for="o in eventOptions" :key="o.value" :value="o.value">
              {{ o.label }}
            </option>
          </Select></label
        ><button :disabled="busy">添加通知地址</button>
      </form>
      <p v-if="!hooks.length" class="muted">暂无通知地址。</p>
      <div v-for="h in hooks" :key="h.id" class="card">
        <p>
          {{ h.url }} · {{ h.enabled ? "已启用" : "已停用"
          }}<span v-if="hookMuted(h)">
            · 静默至 {{ formatDateTime(h.muted_until!) }}</span
          >
        </p>
        <Select
          :aria-label="`通知事件 ${h.url}`"
          :value="h.events.join(',')"
          :disabled="busy"
          @change="changeEvents(h, $event)"
        >
          <option v-for="o in eventOptions" :key="o.value" :value="o.value">
            {{ o.label }}
          </option>
          <option
            v-if="!eventOptions.some((o) => o.value === h.events.join(','))"
            :value="h.events.join(',')"
          >
            自定义事件
          </option>
        </Select>
        <div class="actions">
          <button
            :disabled="busy"
            @click="updateHook(h, { enabled: !h.enabled })"
          >
            {{ h.enabled ? "停用通知" : "启用通知" }}</button
          ><button v-if="!hookMuted(h)" :disabled="busy" @click="muteHook(h)">
            静默一小时</button
          ><button
            v-else
            :disabled="busy"
            @click="updateHook(h, { muted_until: null })"
          >
            解除静默</button
          ><button :disabled="busy" @click="removeHook(h.id)">删除地址</button>
        </div>
      </div>
      <details>
        <summary>最近投递记录</summary>
        <p v-if="!deliveries.length">暂无投递。</p>
        <p v-for="d in deliveries" :key="d.event_id + d.subscription_id">
          {{ status(d.status) }} · 已尝试 {{ d.attempts }} 次 · {{ d.event_id }}
        </p>
      </details>
    </section>
    <section class="card">
      <h2>最近事件</h2>
      <p class="small muted">
        事件保留30天；静默或停用期间不向该地址补发通知。
      </p>
      <p v-if="!events.length" class="muted">暂无事件。</p>
      <p v-for="e in events" :key="e.id">
        {{ formatDateTime(e.created_at) }} ·
        {{ eventNames[e.kind] || e.kind }} · {{ eventSummary(e) }}
      </p>
    </section>
    <section class="card">
      <h2>规则导入与导出</h2>
      <p class="muted">
        导出最多 10,000 条规则，导入最多 1,000
        条。导出不包含隧道密钥；加密规则导入前需要补充密钥。取消保留已完成的规则。
      </p>
      <button :disabled="busy" @click="exportRules">创建导出任务</button>
      <details>
        <summary>导入规则</summary>
        <form @submit.prevent="previewImport">
          <label
            >导入方式<Select v-model="importMode" aria-label="导入方式">
              <option value="create">新增规则</option>
              <option value="update_by_port">
                按节点、协议和端口更新已有规则
              </option>
            </Select></label
          >
          <label
            >规则 JSON<textarea
              v-model="importJSON"
              required
              rows="7"
              maxlength="1000000"
              spellcheck="false"
            /></label
          ><button :disabled="busy">预览导入</button>
          <div v-if="preview.length">
            <p v-for="item in preview" :key="item.index">
              第 {{ item.index + 1 }} 条 ·
              {{
                item.error || (item.action === "update" ? "将更新" : "将新增")
              }}
            </p>
            <div class="form-actions">
              <button
                type="button"
                class="primary"
                :disabled="busy || preview.some((x) => x.error)"
                @click="importRules"
              >
                确认并创建导入任务
              </button>
            </div>
          </div>
        </form>
      </details>
      <p v-if="!tasks.length" class="muted">暂无任务。</p>
      <div v-for="t in tasks" :key="t.id" class="task-row">
        <span
          >{{ t.kind === "rules.import" ? "规则导入" : "规则导出" }} ·
          {{ status(t.status) }} · {{ formatDateTime(t.created_at) }}</span
        ><button :disabled="busy" @click="viewTask(t)">查看结果</button
        ><button
          v-if="['pending', 'running'].includes(t.status)"
          :disabled="busy"
          @click="cancelTask(t)"
        >
          取消任务
        </button>
      </div>
      <section v-if="detail">
        <h3>任务结果</h3>
        <p>{{ status(detail.status) }} {{ detail.error }}</p>
        <pre v-if="detail.result" class="operation-result">{{
          detail.result
        }}</pre>
        <button v-if="detail.result" @click="download">下载 JSON</button
        ><button @click="detail = null">收起</button>
      </section>
    </section>
    <template v-if="admin">
      <section v-if="policy" class="card">
        <h2>告警设置</h2>
        <form @submit.prevent="savePolicy">
          <label class="check"
            ><input
              v-model="policy.enabled"
              type="checkbox"
            />启用状态告警</label
          >
          <p class="muted small">
            持续失联后发送一次离线通知；恢复须经过确认时间且再次收到心跳。到期和低配额每个权益周期各通知一次。
          </p>
          <div class="form-grid">
            <label
              >离线宽限（秒）<input
                v-model.number="policy.offline_seconds"
                type="number"
                min="90"
                max="3600"
                required /></label
            ><label
              >恢复确认（秒）<input
                v-model.number="policy.recovery_seconds"
                type="number"
                min="5"
                max="600"
                required /></label
            ><label
              >到期提前（小时）<input
                v-model.number="policy.expiry_hours"
                type="number"
                min="1"
                max="720"
                required /></label
            ><label
              >剩余流量阈值（%）<input
                v-model.number="policy.remaining_percent"
                type="number"
                min="1"
                max="50"
                required
            /></label>
          </div>
          <button :disabled="busy">保存告警设置</button>
        </form>
      </section>
      <section class="card">
        <h2>发行兑换码</h2>
        <form @submit.prevent="createCode">
          <div class="form-grid">
            <label
              >充值金额（分）<input
                v-model="codeForm.amount_cents"
                required
                pattern="[0-9]+" /></label
            ><label
              >套餐 ID（可选）<input
                v-model="codeForm.plan_id"
                maxlength="64" /></label
            ><label
              >可兑换人数<input
                v-model.number="codeForm.max_uses"
                type="number"
                min="1"
                max="1000000"
                required
            /></label>
          </div>
          <button :disabled="busy">生成兑换码</button>
        </form>
        <p v-for="c in codes" :key="c.id">
          {{ c.code_hint }}… · 已用 {{ c.used }}/{{ c.max_uses }} ·
          {{ c.enabled ? "可兑换" : "已停用" }}
          <button v-if="c.enabled" :disabled="busy" @click="revokeCode(c.id)">
            停用
          </button>
        </p>
      </section>
      <section class="card">
        <h2>返佣管理</h2>
        <p class="muted">
          默认为 0。100 基点等于
          1%；佣金先记为待结算，审核后入钱包。已结算佣金回冲需要收款人的可用余额足够。
        </p>
        <form @submit.prevent="saveRate">
          <label
            >比例（基点）<input
              v-model.number="rate"
              type="number"
              min="0"
              max="10000"
              required /></label
          ><button :disabled="busy">保存比例</button>
        </form>
        <form @submit.prevent="resolveCommission('settle')">
          <label
            >佣金 ID<input
              v-model="commissionID"
              required
              maxlength="64" /></label
          ><label
            >审核或回冲原因<input
              v-model="commissionReason"
              required
              maxlength="500" /></label
          ><button :disabled="busy">审核结算</button
          ><button
            type="button"
            :disabled="busy || !commissionID || !commissionReason"
            @click="resolveCommission('reverse')"
          >
            回冲佣金
          </button>
        </form>
      </section>
      <section class="card">
        <h2>套餐退款与资金来源</h2>
        <p class="muted">
          退款退回钱包并按比例扣减可退配额、回冲佣金。已使用或仍分配给节点的额度不能退；历史购买缺少来源记录时不能自动退款。
        </p>
        <form @submit.prevent="refundPurchase">
          <label>购买记录 ID<input v-model="purchaseID" required /></label
          ><label
            >退回金额（分）<input
              v-model="purchaseAmount"
              required
              pattern="[0-9]+"
              inputmode="numeric" /></label
          ><label
            >套餐退款原因<input
              v-model="purchaseReason"
              required
              maxlength="500" /></label
          ><button :disabled="busy">退回钱包并调整佣金</button
          ><button
            type="button"
            :disabled="busy || !purchaseID"
            @click="lookupFunding"
          >
            查看资金来源
          </button>
        </form>
        <p v-for="(f, i) in funding" :key="i">
          {{ f.order_id || "非充值余额" }} · 分摊 {{ f.amount_cents }} 分 · 已退
          {{ f.refunded_cents }} 分
        </p>
        <h2>充值退款</h2>
        <p class="muted">
          先预留未花费的钱包余额，再通过商户渠道操作退款。只有拿到外部成功凭证后才能确认退款；取消会释放预留余额。
        </p>
        <form @submit.prevent="requestRefund">
          <label
            >订单 ID<input v-model="refundOrder" required maxlength="64"
          /></label>
          <div class="form-grid">
            <label
              >退款金额（分）<input
                v-model="refundAmount"
                required
                pattern="[1-9][0-9]*" /></label
            ><label
              >原因<input v-model="refundReason" required maxlength="500"
            /></label>
          </div>
          <button :disabled="busy">预留退款余额</button
          ><button
            type="button"
            :disabled="busy || !refundOrder"
            @click="run(readRefunds)"
          >
            查询退款记录
          </button>
        </form>
        <label
          >外部成功凭证或取消依据<input v-model="evidence" maxlength="500"
        /></label>
        <p v-for="r in refunds" :key="r.id">
          {{ r.amount_cents }} 分 · {{ status(r.status) }} · {{ r.reason }}
          <template v-if="r.status === 'pending_external'"
            ><button
              :disabled="busy || !evidence"
              @click="resolveRefund(r, true)"
            >
              确认外部退款成功</button
            ><button
              :disabled="busy || !evidence"
              @click="resolveRefund(r, false)"
            >
              取消并释放余额
            </button></template
          >
        </p>
      </section>
    </template>
  </section>
</template>
