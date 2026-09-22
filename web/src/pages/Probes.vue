<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { api, errorText, ApiError } from "../core/api";
import ProbeHistory from "../components/ProbeHistory.vue";
import LocationFlag from "../components/LocationFlag.vue";
import Select from "../components/Select.vue";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";
interface Probe {
  node_id: string;
  sampled_at: string;
  cpu_percent: number | null;
  memory_used: string | null;
  memory_total: string | null;
  disk_used: string | null;
  disk_total: string | null;
  upload_bps: number | null;
  download_bps: number | null;
  load1: number | null;
  cpu_model: string | null;
  swap_used: string | null;
  swap_total: string | null;
  uptime_seconds: string | null;
  public_ips?: {
    address: string;
    source: string;
    family: string;
    observed_at: string;
  }[];
  // 以下字段由控制面补齐：节点名、所属设备组和位置图标。
  node_name?: string;
  group_ids?: string[];
  location?: {
    country_code: string;
    country_name?: string;
    region?: string;
    city?: string;
    source?: string;
  };
}
const probes = ref<Probe[]>([]);
const names = ref<Record<string, string>>({});
const nodes = ref<{ id: string; name: string }[]>([]);
const groups = ref<{ id: string; name: string }[]>([]);
// 探针页面是按设备组看的：选了组就只看这个组的机器，没选就合并显示
// 当前账号有权查看的全部设备。
const group = ref("");
const groupNames = computed(() =>
  Object.fromEntries(groups.value.map((g) => [g.id, g.name])),
);
function nodeTitle(p: Probe) {
  return p.node_name || names.value[p.node_id] || p.node_id;
}
function groupLabel(p: Probe) {
  return (p.group_ids || [])
    .map((id) => groupNames.value[id] || id)
    .join("、");
}
const error = ref("");
const connected = ref(false);
const now = ref(Date.now());
const histories = ref<Record<string, number[]>>({});
let events: EventSource | undefined;
let ticker: ReturnType<typeof setInterval> | undefined;
let alive = true;
function accept(items: Probe[]) {
  probes.value = items;
  const allowed = new Set(items.map((p) => p.node_id));
  for (const key of Object.keys(histories.value))
    if (!allowed.has(key)) delete histories.value[key];
  for (const p of items) {
    if (p.upload_bps !== null) {
      const h = histories.value[p.node_id] || [];
      histories.value[p.node_id] = [...h, p.upload_bps].slice(-30);
    }
  }
}
function bytes(value: string | null | undefined) {
  // 缺失（undefined）与未知（null）都要当成「没有这个数」：老版本 Agent 不
  // 上报交换分区，字段就是缺的，不该让整张卡片渲染不出来。
  if (value === null || value === undefined) return "未知";
  const n = BigInt(value);
  return n >= 1073741824n
    ? `${Number(n / 1048576n) / 1024} GiB`
    : `${Number(n / 1024n)} KiB`;
}
function rate(value: number | null) {
  return value === null ? "未知" : `${(value / 1024).toFixed(1)} KiB/s`;
}
function points(id: string) {
  const h = histories.value[id] || [];
  const max = Math.max(...h, 1);
  return h
    .map(
      (v, i) =>
        `${(i * 300) / Math.max(h.length - 1, 1)},${68 - (v / max) * 60}`,
    )
    .join(" ");
}
const count = computed(() => probes.value.length);
async function load() {
  error.value = "";
  try {
    const scope = group.value ? `?group_id=${encodeURIComponent(group.value)}` : "";
    const [p, n, g] = await Promise.all([
      api<{ items: Probe[] }>("/probes" + scope),
      loadNodes(scope),
      loadGroups(),
    ]);
    if (!alive) return;
    accept(p.items);
    nodes.value = n;
    groups.value = g;
    names.value = Object.fromEntries(n.map((x) => [x.id, x.name]));
    events?.close();
    events = new EventSource("/api/v1/probes/events" + scope, {
      withCredentials: true,
    });
    events.onopen = () => {
      connected.value = true;
      error.value = "";
    };
    events.addEventListener("probes", (event: MessageEvent) => {
      try {
        accept((JSON.parse(event.data) as { items: Probe[] }).items);
        connected.value = true;
        error.value = "";
      } catch {
        error.value = "探针数据格式异常";
      }
    });
    events.onerror = () => {
      connected.value = false;
      error.value = "实时连接已中断，正在重连；原采样时间保留。";
      void api("/auth/session").catch((e) => {
        if (e instanceof ApiError && e.status === 401) events?.close();
      });
    };
  } catch (e) {
    error.value = errorText(e);
  }
}
// scope 带上 group_id 时，历史选择器只列这个组的机器，与上面的探针卡片一致。
async function loadNodes(scope: string) {
  const result: { id: string; name: string }[] = [];
  for (let page = 1; ; page++) {
    const next = await api<{
      items: { id: string; name: string }[];
      total: number;
    }>(`/nodes${scope ? scope + "&" : "?"}page=${page}&page_size=100`);
    result.push(...next.items);
    if (!alive || !next.items.length || result.length >= next.total)
      return result;
  }
}
/** 设备组列表就是探针页面的分组选择器：能选到的组，账号都有权查看。 */
async function loadGroups() {
  const result: { id: string; name: string }[] = [];
  for (let page = 1; ; page++) {
    const next = await api<{
      items: { id: string; name: string }[];
      total: number;
    }>(`/groups?page=${page}&page_size=100`);
    result.push(...next.items);
    if (!alive || !next.items.length || result.length >= next.total)
      return result;
  }
}
onMounted(() => {
  void load();
  ticker = setInterval(() => {
    now.value = Date.now();
  }, 5000);
});
onUnmounted(() => {
  alive = false;
  events?.close();
  clearInterval(ticker);
});
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">LIVE TELEMETRY</p>
        <h1>服务器探针</h1>
        <p class="muted">
          <span class="live-dot" :data-live="String(connected)" aria-hidden="true" />
          {{ count }} 个可见采样 ·
          {{ connected ? "实时连接已建立" : "实时连接未建立" }}
          · {{ displayTimeZoneLabel }}
        </p>
      </div>
      <button @click="load">重新连接</button>
    </div>
    <div class="card probe-scope">
      <label
        >设备组<Select
          :model-value="group"
          aria-label="设备组"
          @change="
            (value: string) => {
              group = value;
              void load();
            }
          "
        >
          <option value="">全部可见设备</option>
          <option v-for="g in groups" :key="g.id" :value="g.id">
            {{ g.name }}
          </option>
        </Select></label
      >
      <p class="small muted">
        探针是付费能力：只有拥有有效套餐、且设备属于所选设备组的账号才能查看，
        页面里也只有该组的机器。上方历史查询跟随同一个范围。
      </p>
    </div>
    <p v-if="error" class="warning" role="status">{{ error }}</p>
    <ProbeHistory :nodes="nodes" />
    <p v-if="!probes.length" class="card empty">
      尚无授权节点采样。节点接入并上报后将在这里显示。
    </p>
    <div class="probe-grid">
      <article v-for="p in probes" :key="p.node_id" class="card probe">
        <div class="section-heading">
          <h2 class="probe-name">
            <LocationFlag
              :code="p.location?.country_code"
              :name="p.location?.country_name"
            />{{ nodeTitle(p) }}
          </h2>
          <span class="badge">{{
            now - Date.parse(p.sampled_at) > 30000 ? "数据陈旧" : "近期采样"
          }}</span>
        </div>
        <p class="small muted">采样于 {{ formatDateTime(p.sampled_at) }}</p>
        <p
          v-if="p.location?.country_name || groupLabel(p)"
          class="small muted probe-place"
        >
          <template v-if="p.location?.country_name"
            >位置 {{ p.location.country_name
            }}<template v-if="p.location.city">·{{ p.location.city }}</template
            ><template v-if="groupLabel(p)"> · </template></template
          ><template v-if="groupLabel(p)">设备组 {{ groupLabel(p) }}</template>
        </p>
        <dl class="metrics">
          <div>
            <dt>上行</dt>
            <dd>{{ rate(p.upload_bps) }}</dd>
          </div>
          <div>
            <dt>下行</dt>
            <dd>{{ rate(p.download_bps) }}</dd>
          </div>
          <div>
            <dt>CPU 型号</dt>
            <dd>{{ p.cpu_model || "未知" }}</dd>
          </div>
          <div>
            <dt>CPU</dt>
            <dd>
              {{
                p.cpu_percent === null ? "未知" : p.cpu_percent.toFixed(1) + "%"
              }}
            </dd>
          </div>
          <div>
            <dt>负载</dt>
            <dd>{{ p.load1 ?? "未知" }}</dd>
          </div>
          <div>
            <dt>内存 已用 / 总量</dt>
            <dd>{{ bytes(p.memory_used) }} / {{ bytes(p.memory_total) }}</dd>
          </div>
          <div>
            <dt>磁盘 已用 / 总量</dt>
            <dd>{{ bytes(p.disk_used) }} / {{ bytes(p.disk_total) }}</dd>
          </div>
          <div>
            <dt>虚拟交换 已用 / 总量</dt>
            <dd>{{ bytes(p.swap_used) }} / {{ bytes(p.swap_total) }}</dd>
          </div>
        </dl>
        <svg
          v-if="(histories[p.node_id]?.length || 0) > 1"
          class="chart"
          viewBox="0 0 300 76"
          role="img"
          :aria-label="`${nodeTitle(p)} 本次会话上行趋势，自动缩放`"
        >
          <polyline
            :points="points(p.node_id)"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
          />
        </svg>
        <p class="small muted">趋势仅保留本次页面会话，不代表计费流量。</p>
        <div v-if="p.public_ips?.length">
          <p v-for="ip in p.public_ips" :key="ip.address" class="small">
            {{ ip.family }} · {{ ip.address }}<br /><span class="muted"
              >{{ ip.source }} · {{ formatDateTime(ip.observed_at) }}</span
            >
          </p>
        </div>
        <p v-else class="small muted">
          公网地址不对普通账号展示。脚本取地址请用下方接口。
        </p>
      </article>
    </div>
    <section class="card probe-api">
      <h2>设备地址接口</h2>
      <p class="small muted">
        给脚本用：带上管理员发给你的 API Token，就能取到本页面所选设备组当前的
        机器地址。同一组只有一台机器时用单台接口，多台时用列表接口 —— 机器被替换
        或换 IP 之后，返回的地址会跟着变，调用方不需要改代码。
      </p>
      <dl class="metrics">
        <div>
          <dt>单台设备</dt>
          <dd><code>GET /online/device/ip</code></dd>
        </div>
        <div>
          <dt>多台设备</dt>
          <dd><code>GET /online/device/ip/list</code></dd>
        </div>
        <div>
          <dt>鉴权</dt>
          <dd><code>Authorization: Bearer &lt;API Token&gt;</code></dd>
        </div>
        <div>
          <dt>限定设备组</dt>
          <dd><code>?group_id=&lt;设备组 ID&gt;</code></dd>
        </div>
      </dl>
      <p class="small muted">
        多台机器时单台接口返回 409 并提示改用列表；接口只返回当前账号有权查看的
        设备，与这个页面看到的是同一份数据。
      </p>
    </section>
  </section>
</template>
