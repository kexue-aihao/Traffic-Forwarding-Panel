<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { api, errorText, ApiError } from "../core/api";
import ProbeHistory from "../components/ProbeHistory.vue";
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
  uptime_seconds: string | null;
  public_ips?: {
    address: string;
    source: string;
    family: string;
    observed_at: string;
  }[];
}
const probes = ref<Probe[]>([]);
const names = ref<Record<string, string>>({});
const nodes = ref<{ id: string; name: string }[]>([]);
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
function bytes(value: string | null) {
  if (value === null) return "未知";
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
    const [p, n] = await Promise.all([
      api<{ items: Probe[] }>("/probes"),
      loadNodes(),
    ]);
    if (!alive) return;
    accept(p.items);
    nodes.value = n;
    names.value = Object.fromEntries(n.map((x) => [x.id, x.name]));
    events?.close();
    events = new EventSource("/api/v1/probes/events", {
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
async function loadNodes() {
  const result: { id: string; name: string }[] = [];
  for (let page = 1; ; page++) {
    const next = await api<{
      items: { id: string; name: string }[];
      total: number;
    }>(`/nodes?page=${page}&page_size=100`);
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
          {{ count }} 个可见采样 ·
          {{ connected ? "实时连接已建立" : "实时连接未建立" }}
          · {{ displayTimeZoneLabel }}
        </p>
      </div>
      <button @click="load">重新连接</button>
    </div>
    <p v-if="error" class="warning" role="status">{{ error }}</p>
    <ProbeHistory :nodes="nodes" />
    <p v-if="!probes.length" class="card empty">
      尚无授权节点采样。节点接入并上报后将在这里显示。
    </p>
    <div class="probe-grid">
      <article v-for="p in probes" :key="p.node_id" class="card probe">
        <div class="section-heading">
          <h2>{{ names[p.node_id] || p.node_id }}</h2>
          <span class="badge">{{
            now - Date.parse(p.sampled_at) > 30000 ? "数据陈旧" : "近期采样"
          }}</span>
        </div>
        <p class="small muted">采样于 {{ formatDateTime(p.sampled_at) }}</p>
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
        </dl>
        <svg
          v-if="(histories[p.node_id]?.length || 0) > 1"
          class="chart"
          viewBox="0 0 300 76"
          role="img"
          :aria-label="`${names[p.node_id] || p.node_id} 本次会话上行趋势，自动缩放`"
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
        <p v-else class="small muted">公网地址未提供或当前账号无查看权限。</p>
      </article>
    </div>
  </section>
</template>
