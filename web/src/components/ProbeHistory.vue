<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, errorText } from "../core/api";
import {
  displayTimeZoneLabel,
  formatDateTime,
  formatDate,
  formatTime,
} from "../core/format";

interface HistorySample {
  sampled_at: string;
  resolution: "minute" | "hour";
  samples: number;
  cpu_percent: number | null;
  memory_percent: number | null;
  disk_percent: number | null;
  load1: number | null;
  upload_bps: number | null;
  download_bps: number | null;
}
const props = defineProps<{ nodes: { id: string; name: string }[] }>();
const route = useRoute();
const router = useRouter();
const ranges = [
  { id: "1h", label: "最近 1 小时", hours: 1, resolution: "minute" },
  { id: "24h", label: "最近 24 小时", hours: 24, resolution: "minute" },
  { id: "7d", label: "最近 7 天", hours: 168, resolution: "minute" },
  { id: "30d", label: "最近 30 天", hours: 720, resolution: "hour" },
  { id: "180d", label: "最近 180 天", hours: 4320, resolution: "hour" },
] as const;
const metrics = [
  { key: "cpu_percent", label: "CPU", unit: "%" },
  { key: "memory_percent", label: "内存", unit: "%" },
  { key: "disk_percent", label: "磁盘", unit: "%" },
  { key: "load1", label: "1 分钟负载", unit: "" },
  { key: "upload_bps", label: "上行", unit: "bytes/s" },
  { key: "download_bps", label: "下行", unit: "bytes/s" },
] as const;
const selectedNode = computed(
  () =>
    props.nodes.find((node) => node.id === route.query.node) || props.nodes[0],
);
const range = computed(
  () => ranges.find((item) => item.id === route.query.range) || ranges[1],
);
const metric = computed(
  () => metrics.find((item) => item.key === route.query.metric) || metrics[0],
);
const items = ref<HistorySample[]>([]);
const loading = ref(false);
const error = ref("");
const windowStart = ref(0);
const windowEnd = ref(0);
const selectedIndex = ref(0);
let generation = 0;
let alive = true;
const selected = computed(() => items.value[selectedIndex.value]);
const interval = computed(() =>
  range.value.resolution === "minute" ? 60000 : 3600000,
);
const valid = computed(() =>
  items.value.filter((item) => item[metric.value.key] !== null),
);
const ceiling = computed(() =>
  metric.value.unit === "%"
    ? Math.max(100, ...valid.value.map((item) => item[metric.value.key] || 0))
    : Math.max(1, ...valid.value.map((item) => item[metric.value.key] || 0)),
);
function x(item: HistorySample) {
  return (
    6 +
    (592 * (Date.parse(item.sampled_at) - windowStart.value)) /
      Math.max(windowEnd.value - windowStart.value, 1)
  );
}
function y(item: HistorySample) {
  return 160 - (146 * (item[metric.value.key] || 0)) / ceiling.value;
}
const segments = computed(() => {
  const lines: { points: string; count: number; x: number; y: number }[] = [];
  let lastTime = 0;
  let line: (typeof lines)[number] | undefined;
  for (const item of items.value) {
    const time = Date.parse(item.sampled_at);
    if (item[metric.value.key] === null) {
      line = undefined;
    } else {
      if (!line || time - lastTime > interval.value) {
        line = { points: "", count: 0, x: x(item), y: y(item) };
        lines.push(line);
      }
      line.points += `${x(item).toFixed(2)},${y(item).toFixed(2)} `;
      line.count++;
    }
    lastTime = time;
  }
  return lines;
});
const number = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 });
const compact = new Intl.NumberFormat(undefined, {
  notation: "compact",
  maximumFractionDigits: 1,
});
function value(item: HistorySample | undefined) {
  const amount = item?.[metric.value.key];
  return amount === undefined || amount === null
    ? "未知"
    : `${number.format(amount)}${metric.value.unit ? " " + metric.value.unit : ""}`;
}
function axisTime(timestamp: number) {
  return range.value.hours <= 24
    ? formatTime(timestamp)
    : formatDate(timestamp);
}
function select(key: string, event: Event) {
  void router.replace({
    query: { ...route.query, [key]: (event.target as HTMLSelectElement).value },
  });
}
async function load() {
  const request = ++generation;
  items.value = [];
  error.value = "";
  if (!selectedNode.value) {
    loading.value = false;
    return;
  }
  loading.value = true;
  windowEnd.value =
    (Math.floor(Date.now() / interval.value) + 1) * interval.value;
  windowStart.value = windowEnd.value - range.value.hours * 3600000;
  const query = new URLSearchParams({
    resolution: range.value.resolution,
    from: new Date(windowStart.value).toISOString(),
    to: new Date(windowEnd.value).toISOString(),
  });
  try {
    const result = await api<{ items: HistorySample[]; total: number }>(
      `/probes/${encodeURIComponent(selectedNode.value.id)}/history?${query}`,
    );
    if (!alive || request !== generation) return;
    items.value = result.items;
    selectedIndex.value = Math.max(0, result.items.length - 1);
  } catch (cause) {
    if (
      alive &&
      request === generation &&
      !(cause instanceof DOMException && cause.name === "AbortError")
    )
      error.value = errorText(cause);
  } finally {
    if (alive && request === generation) loading.value = false;
  }
}
// Query changes rotate the shared page request signal, including metric changes.
watch([() => selectedNode.value?.id, () => route.query], load, {
  immediate: true,
});
onUnmounted(() => {
  alive = false;
  generation++;
});
</script>

<template>
  <section
    class="card probe-history"
    aria-labelledby="probe-history-title"
    :aria-busy="loading"
  >
    <div class="section-heading">
      <h2 id="probe-history-title">历史趋势</h2>
      <button :disabled="loading || !selectedNode" @click="load">
        刷新历史
      </button>
    </div>
    <p class="small muted">
      分钟采样保留 7 天，小时汇总保留 180 天。最近 1–2
      分钟的采样稍后可见，当前小时逐步汇总；缺失数据留空，不代表零用量或计费流量。
    </p>
    <div v-if="nodes.length" class="history-controls">
      <label
        >历史节点<select
          :value="selectedNode?.id"
          @change="select('node', $event)"
        >
          <option v-for="node in nodes" :key="node.id" :value="node.id">
            {{ node.name }}
          </option>
        </select></label
      >
      <label
        >历史时间范围<select
          :value="range.id"
          @change="select('range', $event)"
        >
          <option v-for="item in ranges" :key="item.id" :value="item.id">
            {{ item.label }}
          </option>
        </select></label
      >
      <label
        >历史指标<select :value="metric.key" @change="select('metric', $event)">
          <option v-for="item in metrics" :key="item.key" :value="item.key">
            {{ item.label }}{{ item.unit ? ` (${item.unit})` : "" }}
          </option>
        </select></label
      >
    </div>
    <p v-if="loading" role="status">正在加载历史采样…</p>
    <p v-else-if="error" class="warning" role="alert">
      {{ error }} <button @click="load">重试历史查询</button>
    </p>
    <p v-else-if="!selectedNode" class="empty">暂无可查询历史的授权节点。</p>
    <template v-else>
      <p class="small muted history-window">
        {{ formatDateTime(windowStart) }} 至 {{ formatDateTime(windowEnd) }} ·
        {{ displayTimeZoneLabel }} ·
        {{ range.resolution === "minute" ? "分钟" : "小时" }}汇总
      </p>
      <p v-if="!items.length" class="empty" role="status">
        此时间范围暂无历史采样。
      </p>
      <template v-else>
        <p class="small muted" role="status">
          {{ items.length }} 个采样桶 · {{ valid.length }} 个{{
            metric.label
          }}有效值
        </p>
        <div v-if="valid.length" class="history-plot">
          <div class="history-y" aria-hidden="true">
            <span>{{ compact.format(ceiling) }}</span
            ><span>0</span>
          </div>
          <svg
            class="history-chart"
            viewBox="0 0 604 170"
            preserveAspectRatio="none"
            role="img"
            :aria-label="`${selectedNode.name} ${metric.label}历史趋势，缺测处断线；下方滑块可逐点查看`"
          >
            <line x1="6" y1="14" x2="598" y2="14" class="history-guide" />
            <line x1="6" y1="160" x2="598" y2="160" class="history-guide" />
            <g v-for="(segment, index) in segments" :key="index">
              <polyline
                v-if="segment.count > 1"
                :points="segment.points"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
                vector-effect="non-scaling-stroke"
              />
              <circle
                v-else
                :cx="segment.x"
                :cy="segment.y"
                r="3"
                fill="currentColor"
              />
            </g>
            <circle
              v-if="selected && selected[metric.key] !== null"
              :cx="x(selected)"
              :cy="y(selected)"
              r="4"
              fill="currentColor"
            />
          </svg>
          <div class="history-x" aria-hidden="true">
            <span>{{ axisTime(windowStart) }}</span
            ><span>{{ axisTime(windowEnd) }}</span>
          </div>
        </div>
        <p v-else class="empty">该指标在此时间范围没有有效值。</p>
        <label v-if="items.length > 1" class="history-scrubber"
          >查看历史采样<input
            v-model.number="selectedIndex"
            type="range"
            min="0"
            :max="items.length - 1"
            step="1"
            :aria-valuetext="
              selected
                ? `${formatDateTime(selected.sampled_at)}，${displayTimeZoneLabel}，${metric.label} ${value(selected)}`
                : ''
            "
        /></label>
        <p v-if="selected" class="history-selection" aria-live="polite">
          {{ formatDateTime(selected.sampled_at) }} · {{ metric.label }}
          <strong>{{ value(selected) }}</strong> · {{ selected.samples }} 次上报
        </p>
      </template>
    </template>
  </section>
</template>

<style scoped>
.history-controls {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr;
  gap: 16px;
}
.history-controls label {
  min-width: 0;
}
.history-controls select {
  width: 100%;
}
.history-chart {
  width: 100%;
  height: 180px;
  display: block;
  color: var(--accent);
}
.history-plot {
  display: grid;
  grid-template-columns: 48px minmax(0, 1fr);
}
.history-y,
.history-x {
  display: flex;
  justify-content: space-between;
  color: var(--muted);
  font-size: 12px;
}
.history-y {
  flex-direction: column;
  text-align: right;
  padding: 5px 6px 0 0;
}
.history-x {
  grid-column: 2;
  gap: 8px;
}
.history-guide {
  stroke: var(--line);
  stroke-dasharray: 4 4;
}
.history-scrubber {
  font-size: 12px;
  color: var(--muted);
}
.history-scrubber input {
  accent-color: var(--accent);
}
.history-selection {
  font-size: 12px;
}
.history-window {
  font-variant-numeric: tabular-nums;
}
@media (max-width: 768px) {
  .history-controls {
    grid-template-columns: 1fr;
    gap: 0;
  }
}
@media (forced-colors: active) {
  .history-chart {
    color: CanvasText;
  }
  .history-guide {
    stroke: GrayText;
  }
}
</style>
