<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from "vue";
import Select from "./Select.vue";
import { useRoute, useRouter } from "vue-router";
import { api, errorText } from "../core/api";
import {
  displayTimeZoneLabel,
  formatDateTime,
  formatDate,
  formatTime,
} from "../core/format";

// 面板只画这几列。接口仍然返回 load1（一分钟负载），但上下行合并成一张图之后
// 不再需要一个单独的指标位，所以这里连类型都不留。
interface HistorySample {
  sampled_at: string;
  resolution: "minute" | "hour";
  samples: number;
  cpu_percent: number | null;
  memory_percent: number | null;
  disk_percent: number | null;
  upload_bps: number | null;
  download_bps: number | null;
}
type SeriesKey =
  | "cpu_percent"
  | "memory_percent"
  | "disk_percent"
  | "upload_bps"
  | "download_bps";
interface Series {
  key: SeriesKey;
  label: string;
}
// 一个指标可以有多条曲线：上行与下行画在同一张图上，共用一条纵轴。
interface Metric {
  id: string;
  label: string;
  unit: "" | "%" | "MB/s";
  series: Series[];
}
// label 由探针页面算好：管理员看到的是机器当前的公网 IPv4，用户视角下是设备组名
// （同组多台时带序号）。这里只在它缺失时退回机器名。
const props = defineProps<{
  nodes: { id: string; name: string; label?: string }[];
}>();
function nodeLabel(node: { name: string; label?: string }) {
  return node.label || node.name;
}
const route = useRoute();
const router = useRouter();
const ranges = [
  { id: "1h", label: "最近 1 小时", hours: 1, resolution: "minute" },
  { id: "24h", label: "最近 24 小时", hours: 24, resolution: "minute" },
  { id: "7d", label: "最近 7 天", hours: 168, resolution: "minute" },
  { id: "30d", label: "最近 30 天", hours: 720, resolution: "hour" },
  { id: "180d", label: "最近 180 天", hours: 4320, resolution: "hour" },
] as const;
const metrics: Metric[] = [
  {
    id: "cpu_percent",
    label: "CPU",
    unit: "%",
    series: [{ key: "cpu_percent", label: "CPU" }],
  },
  {
    id: "memory_percent",
    label: "内存",
    unit: "%",
    series: [{ key: "memory_percent", label: "内存" }],
  },
  {
    id: "disk_percent",
    label: "磁盘",
    unit: "%",
    series: [{ key: "disk_percent", label: "磁盘" }],
  },
  {
    id: "traffic",
    label: "上行/下行",
    unit: "MB/s",
    series: [
      { key: "upload_bps", label: "上行" },
      { key: "download_bps", label: "下行" },
    ],
  },
];
// 上下行合并之前，链接里留下的是 upload_bps / download_bps。那些链接已经发给
// 运维过，改指标不该让它们变成空白页，所以这两个值都落到合并后的曲线上；其余
// 认不出的值照旧退回第一个指标。
const mergedInto = new Set(["upload_bps", "download_bps"]);
const metric = computed(() => {
  const wanted = String(route.query.metric ?? "");
  const id = mergedInto.has(wanted) ? "traffic" : wanted;
  return metrics.find((item) => item.id === id) || metrics[0];
});
const selectedNode = computed(
  () =>
    props.nodes.find((node) => node.id === route.query.node) || props.nodes[0],
);
const range = computed(
  () => ranges.find((item) => item.id === route.query.range) || ranges[1],
);
const raw = ref<HistorySample[]>([]);
// 5 分钟一个点只合并这里列出的指标：列在里面的才会被画出来，也就只有它们的
// 缺测该让整个点留空。
const sampleKeys: SeriesKey[] = [
  "cpu_percent",
  "memory_percent",
  "disk_percent",
  "upload_bps",
  "download_bps",
];
// 波形图五分钟一个点。一分钟一个点时 24 小时窗口有 1440 个点，图上挤成一团
// 噪声，7 天窗口更是上万个。组内只要缺一分钟，这个点就整点留空 —— 断线是图上
// 「这里没测到」的唯一信号，取平均会把它抹平成一条平滑的线。
const plotStep = 5 * 60 * 1000;
function plotBucket(at: number) {
  return Math.floor(at / plotStep) * plotStep;
}
function mergeGroup(group: HistorySample[]): HistorySample {
  const point: HistorySample = { ...group[0] };
  point.samples = group.reduce((sum, item) => sum + item.samples, 0);
  for (const key of sampleKeys) {
    if (group.some((item) => item[key] === null)) {
      point[key] = null;
      continue;
    }
    point[key] =
      group.reduce((sum, item) => sum + (item[key] ?? 0), 0) / group.length;
  }
  return point;
}
const items = computed(() => {
  if (range.value.resolution !== "minute" || raw.value.length === 0)
    return raw.value;
  const groups = new Map<number, HistorySample[]>();
  for (const item of raw.value) {
    const key = plotBucket(Date.parse(item.sampled_at));
    const group = groups.get(key);
    if (group) group.push(item);
    else groups.set(key, [item]);
  }
  return [...groups.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([, group]) => mergeGroup(group));
});
const loading = ref(false);
const error = ref("");
const windowStart = ref(0);
const windowEnd = ref(0);
const selectedIndex = ref(0);
let generation = 0;
let alive = true;
const selected = computed(() => items.value[selectedIndex.value]);
// 「这里断线了」的判定间隔：跟着绘图粒度走，缺一个点就断开。
const interval = computed(() =>
  range.value.resolution === "minute" ? plotStep : 3600000,
);
// 窗口右端的落格粒度：分钟档按整分、小时档按整点 —— 也就是服务端自己用的那条
// 上界（历史接口的 to 最远只到「下一个整分 / 整点」）。这里不能跟着绘图粒度
// 走：向上取整到下一个 5 分钟整点会越过上界，接口返回 400
// history range outside retention，整块图变成一行报错。
const windowStep = computed(() =>
  range.value.resolution === "minute" ? 60000 : 3600000,
);
const plotted = computed(() =>
  metric.value.series.map((series, index) => ({
    ...series,
    index,
    segments: segmentsFor(series.key),
  })),
);
const valid = computed(() =>
  items.value.filter((item) =>
    metric.value.series.some((series) => item[series.key] !== null),
  ),
);
// 速率的纵轴下限给 1 KB/s：全零或极小的流量不该把刻度压成 0，而按 1 MB/s 起步
// 又会让几百 KB/s 的机器整条线贴在底部。
const ceiling = computed(() => {
  let top = metric.value.unit === "MB/s" ? 1024 : 1;
  for (const item of items.value)
    for (const series of metric.value.series)
      top = Math.max(top, item[series.key] || 0);
  return metric.value.unit === "%" ? Math.max(100, top) : top;
});
function x(item: HistorySample) {
  return (
    6 +
    (592 * (Date.parse(item.sampled_at) - windowStart.value)) /
      Math.max(windowEnd.value - windowStart.value, 1)
  );
}
function y(item: HistorySample, key: SeriesKey) {
  return 160 - (146 * (item[key] || 0)) / ceiling.value;
}
function segmentsFor(key: SeriesKey) {
  const lines: { points: string; count: number; x: number; y: number }[] = [];
  let lastTime = 0;
  let line: (typeof lines)[number] | undefined;
  for (const item of items.value) {
    const time = Date.parse(item.sampled_at);
    if (item[key] === null) {
      line = undefined;
    } else {
      if (!line || time - lastTime > interval.value) {
        line = { points: "", count: 0, x: x(item), y: y(item, key) };
        lines.push(line);
      }
      line.points += `${x(item).toFixed(2)},${y(item, key).toFixed(2)} `;
      line.count++;
    }
    lastTime = time;
  }
  return lines;
}
const number = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 });
const compact = new Intl.NumberFormat(undefined, {
  notation: "compact",
  maximumFractionDigits: 1,
});
// 速率一律按 MB/s 显示。两条曲线共用一条纵轴，自适应换单位会让它们看起来不在
// 同一量纲上；MB 与面板其余位置的 formatBytes 一样按 1024 进位，同一个数在
// 面板各处读出来是一致的。不加千位分隔符，也是为了与 formatBytes 的读法一致。
const rates = new Intl.NumberFormat(undefined, {
  maximumFractionDigits: 3,
  useGrouping: false,
});
function formatRate(bytesPerSecond: number) {
  return `${rates.format(bytesPerSecond / (1024 * 1024))} MB/s`;
}
function amount(item: HistorySample, key: SeriesKey) {
  const value = item[key];
  if (value === null) return "未知";
  return metric.value.unit === "MB/s"
    ? formatRate(value)
    : `${number.format(value)}${metric.value.unit ? " " + metric.value.unit : ""}`;
}
// 多曲线指标把每条曲线都读出来：读数行与滑块提示共用这一段。
function value(item: HistorySample | undefined) {
  if (!item) return "未知";
  return metric.value.series
    .map((series) =>
      metric.value.series.length > 1
        ? `${series.label} ${amount(item, series.key)}`
        : amount(item, series.key),
    )
    .join(" · ");
}
function axisTime(timestamp: number) {
  return range.value.hours <= 24
    ? formatTime(timestamp)
    : formatDate(timestamp);
}
function select(key: string, value: string) {
  void router.replace({ query: { ...route.query, [key]: value } });
}
async function load() {
  const request = ++generation;
  raw.value = [];
  error.value = "";
  if (!selectedNode.value) {
    loading.value = false;
    return;
  }
  loading.value = true;
  windowEnd.value =
    (Math.floor(Date.now() / windowStep.value) + 1) * windowStep.value;
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
    raw.value = result.items;
    selectedIndex.value = Math.max(0, items.value.length - 1);
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
      分钟采样保留 7 天，小时汇总保留 180 天。波形图每 5
      分钟一个点，5 分钟内只要缺一次采样这个点就留空；缺失不代表零用量或计费流量。速率按
      MB/s 显示，上行与下行画在同一张图上。
    </p>
    <div v-if="nodes.length" class="history-controls">
      <label
        >历史节点<Select
          :value="selectedNode?.id"
          @change="select('node', $event)"
        >
          <option v-for="node in nodes" :key="node.id" :value="node.id">
            {{ nodeLabel(node) }}
          </option>
        </Select></label
      >
      <label
        >历史时间范围<Select
          :value="range.id"
          @change="select('range', $event)"
        >
          <option v-for="item in ranges" :key="item.id" :value="item.id">
            {{ item.label }}
          </option>
        </Select></label
      >
      <label
        >历史指标<Select :value="metric.id" @change="select('metric', $event)">
          <option v-for="item in metrics" :key="item.id" :value="item.id">
            {{ item.label }}{{ item.unit ? ` (${item.unit})` : "" }}
          </option>
        </Select></label
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
            <span>{{
              metric.unit === "MB/s"
                ? formatRate(ceiling)
                : compact.format(ceiling)
            }}</span
            ><span>0</span>
          </div>
          <svg
            class="history-chart"
            viewBox="0 0 604 170"
            preserveAspectRatio="none"
            role="img"
            :aria-label="`${nodeLabel(selectedNode)} ${metric.label}历史趋势，缺测处断线；下方滑块可逐点查看`"
          >
            <line x1="6" y1="14" x2="598" y2="14" class="history-guide" />
            <line x1="6" y1="160" x2="598" y2="160" class="history-guide" />
            <g
              v-for="series in plotted"
              :key="series.key"
              :class="`history-series-${series.index}`"
            >
              <template v-for="(segment, index) in series.segments" :key="index">
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
              </template>
            </g>
            <template v-for="series in plotted" :key="`point-${series.key}`">
              <circle
                v-if="selected && selected[series.key] !== null"
                :class="`history-series-${series.index}`"
                :cx="x(selected)"
                :cy="y(selected, series.key)"
                r="4"
                fill="currentColor"
              />
            </template>
          </svg>
          <div class="history-x" aria-hidden="true">
            <span>{{ axisTime(windowStart) }}</span
            ><span>{{ axisTime(windowEnd) }}</span>
          </div>
        </div>
        <p v-else class="empty">该指标在此时间范围没有有效值。</p>
        <p v-if="metric.series.length > 1" class="history-legend">
          <span
            v-for="series in plotted"
            :key="series.key"
            :class="`history-series-${series.index}`"
            ><i aria-hidden="true"></i>{{ series.label }}</span
          >
        </p>
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
  color: var(--color-accent);
}
/* 上行用品牌色，下行用绿色：任何主题下都分得开。下行再加一条虚线，颜色之外
   还有一条独立线索，高对比模式下两条线也还能区分。 */
.history-series-0 {
  color: var(--color-accent);
}
.history-series-1 {
  color: var(--color-success);
}
.history-series-1 polyline {
  stroke-dasharray: 6 3;
}
.history-plot {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
}
.history-y,
.history-x {
  display: flex;
  justify-content: space-between;
  color: var(--color-ink-muted);
  font-size: 12px;
}
.history-y {
  min-width: 48px;
  flex-direction: column;
  text-align: right;
  padding: 5px 6px 0 0;
}
.history-x {
  grid-column: 2;
  gap: 8px;
}
.history-guide {
  stroke: var(--color-line);
  stroke-dasharray: 4 4;
}
.history-legend {
  display: flex;
  gap: 16px;
  font-size: 12px;
  color: var(--color-ink-muted);
}
.history-legend span {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.history-legend i {
  width: 14px;
  height: 2px;
  background: currentColor;
}
.history-legend .history-series-1 i {
  background: repeating-linear-gradient(
    to right,
    currentColor 0 6px,
    transparent 6px 9px
  );
}
.history-scrubber {
  font-size: 12px;
  color: var(--color-ink-muted);
}
.history-scrubber input {
  accent-color: var(--color-accent);
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
