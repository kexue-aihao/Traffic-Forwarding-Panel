<script setup lang="ts">
import {
  computed,
  defineAsyncComponent,
  onMounted,
  onUnmounted,
  ref,
} from "vue";
import { api, errorText, ApiError } from "../core/api";
import ProbeHistory from "../components/ProbeHistory.vue";
import LocationFlag from "../components/LocationFlag.vue";
const ProbeActions = defineAsyncComponent(
  () => import("../components/ProbeActions.vue"),
);
import ProbeMeter from "../components/ProbeMeter.vue";
import { adminSite, state, notice } from "../core/state";
import Select from "../components/Select.vue";
import {
  displayTimeZoneLabel,
  formatDateTime,
  formatBytes,
} from "../core/format";
interface ProbeIP {
  address: string;
  source: string;
  family: string;
  observed_at: string;
}
interface Probe {
  node_id: string;
  sampled_at: string;
  online?: boolean;
  cpu_percent: number | null;
  memory_used: string | null;
  memory_total: string | null;
  disk_used: string | null;
  disk_total: string | null;
  upload_bps: number | null;
  download_bps: number | null;
  upload_total?: string | null;
  download_total?: string | null;
  load1: number | null;
  cpu_model: string | null;
  swap_used: string | null;
  swap_total: string | null;
  uptime_seconds: string | null;
  public_ips?: ProbeIP[];
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
  ipv4_location?: Probe["location"];
  ipv6_location?: Probe["location"];
}
const probes = ref<Probe[]>([]);
const names = ref<Record<string, string>>({});
interface Node {
  id: string;
  name: string;
  capabilities?: string[];
}
const nodes = ref<Node[]>([]);
const operation = ref<{ node: Node; mode: "shell" | "uninstall" }>();
const canManage = computed(() => adminSite && state.user?.role === "admin");
function act(p: Probe, mode: "shell" | "uninstall") {
  operation.value = {
    node: nodes.value.find((n) => n.id === p.node_id) || {
      id: p.node_id,
      name: nodeTitle(p),
    },
    mode,
  };
}
function removed() {
  operation.value = undefined;
  notice("设备已完成卸载");
  void load();
}
function percent(used: string | null, total: string | null) {
  return used !== null && total !== null && Number(total) > 0
    ? (Number(used) / Number(total)) * 100
    : null;
}
function uptime(value: string | null) {
  if (value === null) return "未知";
  const seconds = Number(value);
  return `${Math.floor(seconds / 86400)} 天 ${Math.floor((seconds % 86400) / 3600)} 小时`;
}
function status(p: Probe) {
  return p.online === false
    ? "离线"
    : now.value - Date.parse(p.sampled_at) > 30000
      ? "数据陈旧"
      : "在线";
}

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
function nodeShortID(p: Probe) {
  return p.node_id.length > 12 ? `${p.node_id.slice(0, 8)}…` : p.node_id;
}
function groupLabel(p: Probe) {
  return (p.group_ids || []).map((id) => groupNames.value[id] || id).join("、");
}
function ipFamily(ip: ProbeIP) {
  const family = ip.family?.toLowerCase();
  if (family === "ipv4" || family === "ipv6") return family;
  return ip.address.includes(":") ? "ipv6" : "ipv4";
}
function addressFor(p: Probe, family: "ipv4" | "ipv6") {
  let best: ProbeIP | undefined;
  for (const ip of p.public_ips || []) {
    if (ipFamily(ip) !== family || !ip.address) continue;
    if (!best || Date.parse(ip.observed_at) > Date.parse(best.observed_at)) {
      best = ip;
    }
  }
  return best?.address || "";
}
function locationFor(
  p: Probe,
  family: "ipv4" | "ipv6",
): Probe["location"] | undefined {
  // Once either split field is present, the control plane is providing the
  // address-family-aware response. Do not let the legacy fallback copy a v6
  // location into the v4 column when the v4 lookup is still pending.
  if (p.ipv4_location !== undefined || p.ipv6_location !== undefined) {
    return family === "ipv4" ? p.ipv4_location : p.ipv6_location;
  }
  if (family === "ipv4") return p.location;
  // Older control planes only returned one location. Keep that response
  // compatible when the machine has no separate IPv4 observation.
  return !addressFor(p, "ipv4") ? p.location : undefined;
}
function addressLabel(p: Probe, family: "ipv4" | "ipv6") {
  const address = addressFor(p, family);
  if (address) return address;
  if (p.public_ips === undefined) return canManage.value ? "未上报" : "已隐藏";
  return "未知";
}
const error = ref("");
const connected = ref(false);
const now = ref(Date.now());

let events: EventSource | undefined;
let ticker: ReturnType<typeof setInterval> | undefined;
let alive = true;
function accept(items: Probe[]) {
  probes.value = items;
}
let generation = 0;
const count = computed(() => probes.value.length);
async function load() {
  const request = ++generation;
  events?.close();
  connected.value = false;
  error.value = "";
  try {
    const scope = group.value
      ? `?group_id=${encodeURIComponent(group.value)}`
      : "";
    const [p, n, g] = await Promise.all([
      api<{ items: Probe[] }>("/probes" + scope),
      loadNodes(scope),
      loadGroups(),
    ]);
    if (!alive || request !== generation) return;
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
        if (!alive || request !== generation) return;
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
    if (alive && request === generation) error.value = errorText(e);
  }
}
// scope 带上 group_id 时，历史选择器只列这个组的机器，与上面的探针卡片一致。
async function loadNodes(scope: string) {
  const result: Node[] = [];
  for (let page = 1; ; page++) {
    const next = await api<{
      items: Node[];
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
          <span
            class="live-dot"
            :data-live="String(connected)"
            aria-hidden="true"
          />
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
      <p class="small muted">按设备组查看实时状态，历史趋势跟随同一范围。</p>
    </div>
    <p v-if="error" class="warning" role="status">{{ error }}</p>
    <p v-if="!probes.length" class="card empty">
      尚无授权节点采样。节点接入并上报后将在这里显示。
    </p>
    <div class="probe-list">
      <article
        v-for="p in probes"
        :key="p.node_id"
        class="card probe"
        :data-status="status(p)"
      >
        <div class="probe-head">
          <div class="probe-identity">
            <h2 class="probe-name">
              <span class="probe-name-chip" :title="p.node_id">
                <span>{{ nodeTitle(p) }}</span>
                <small>ID: {{ nodeShortID(p) }}</small>
              </span>
            </h2>
            <p
              v-if="p.location?.country_name || groupLabel(p)"
              class="small muted probe-place"
            >
              <template v-if="p.location?.country_name"
                >位置 {{ p.location.country_name
                }}<template v-if="p.location.city"
                  >·{{ p.location.city }}</template
                ><template v-if="groupLabel(p)"> · </template></template
              ><template v-if="groupLabel(p)"
                >设备组 {{ groupLabel(p) }}</template
              >
            </p>
          </div>
          <div class="probe-head-rate">
            <span
              ><b aria-hidden="true">↑</b
              ><span>{{ formatBytes(p.upload_bps, "/s") }}</span></span
            >
            <span
              ><b aria-hidden="true">↓</b
              ><span>{{ formatBytes(p.download_bps, "/s") }}</span></span
            >
          </div>
        </div>
        <div class="probe-row">
          <div class="probe-cell probe-state-cell">
            <span class="probe-label">状态</span>
            <span class="probe-state-value">
              <span
                class="probe-status-square"
                :data-live="String(status(p) === '在线')"
                aria-hidden="true"
              />
              <span class="sr-only">{{ status(p) }}</span>
            </span>
          </div>
          <div class="probe-cell probe-location-cell">
            <span class="probe-label">IPv4 地址</span>
            <LocationFlag
              :code="locationFor(p, 'ipv4')?.country_code"
              :name="locationFor(p, 'ipv4')?.country_name"
              :size="26"
            />
            <span class="probe-ip" :title="addressLabel(p, 'ipv4')">
              {{ addressLabel(p, "ipv4") }}
            </span>
          </div>
          <div class="probe-cell probe-location-cell">
            <span class="probe-label">IPv6 地址</span>
            <LocationFlag
              :code="locationFor(p, 'ipv6')?.country_code"
              :name="locationFor(p, 'ipv6')?.country_name"
              :size="26"
            />
            <span class="probe-ip" :title="addressLabel(p, 'ipv6')">
              {{ addressLabel(p, "ipv6") }}
            </span>
          </div>
          <div class="metrics probe-network probe-rate">
            <span class="probe-label">速率</span>
            <div class="probe-network-values">
              <span
                ><b aria-hidden="true">↑</b
                ><span>{{ formatBytes(p.upload_bps, "/s") }}</span></span
              >
              <span
                ><b aria-hidden="true">↓</b
                ><span>{{ formatBytes(p.download_bps, "/s") }}</span></span
              >
            </div>
          </div>
          <div class="probe-cell probe-uptime">
            <span class="probe-label">开机时长</span>
            <strong>{{ uptime(p.uptime_seconds) }}</strong>
          </div>
          <div class="metrics probe-network probe-traffic">
            <span class="probe-label">流量</span>
            <div class="probe-network-values">
              <span
                ><b aria-hidden="true">↑</b
                ><span>{{ formatBytes(p.upload_total ?? null) }}</span></span
              >
              <span
                ><b aria-hidden="true">↓</b
                ><span>{{ formatBytes(p.download_total ?? null) }}</span></span
              >
            </div>
          </div>
          <div class="probe-resources">
            <ProbeMeter
              label="CPU"
              :value="p.cpu_percent"
              :detail="p.cpu_model || '型号未知'"
            />
            <ProbeMeter
              label="内存"
              :value="percent(p.memory_used, p.memory_total)"
              :detail="`${formatBytes(p.memory_used)} / ${formatBytes(p.memory_total)}`"
            />
            <ProbeMeter
              label="磁盘"
              :value="percent(p.disk_used, p.disk_total)"
              :detail="`${formatBytes(p.disk_used)} / ${formatBytes(p.disk_total)}`"
            />
          </div>
          <div v-if="canManage" class="probe-actions">
            <button @click="act(p, 'shell')">WebSSH</button>
            <button class="danger" @click="act(p, 'uninstall')">
              卸载设备
            </button>
          </div>
        </div>
        <details class="probe-details">
          <summary>
            设备详情
            <span class="muted"
              >· 采样于 {{ formatDateTime(p.sampled_at) }}</span
            >
          </summary>
          <p class="small muted">采样于 {{ formatDateTime(p.sampled_at) }}</p>
          <dl class="metrics">
            <div>
              <dt>CPU 型号</dt>
              <dd>{{ p.cpu_model || "未知" }}</dd>
            </div>
            <div>
              <dt>负载</dt>
              <dd>{{ p.load1 ?? "未知" }}</dd>
            </div>
            <div>
              <dt>内存 已用 / 总量</dt>
              <dd>
                {{ formatBytes(p.memory_used) }} /
                {{ formatBytes(p.memory_total) }}
              </dd>
            </div>
            <div>
              <dt>磁盘 已用 / 总量</dt>
              <dd>
                {{ formatBytes(p.disk_used) }} / {{ formatBytes(p.disk_total) }}
              </dd>
            </div>
            <div>
              <dt>虚拟交换 已用 / 总量</dt>
              <dd>
                {{ formatBytes(p.swap_used) }} / {{ formatBytes(p.swap_total) }}
              </dd>
            </div>
          </dl>
          <p v-for="ip in p.public_ips || []" :key="ip.address" class="small">
            {{ ip.family }} · {{ ip.address }}<br /><span class="muted"
              >{{ ip.source }} · {{ formatDateTime(ip.observed_at) }}</span
            >
          </p>
          <p v-if="!p.public_ips?.length" class="small muted">
            公网地址不对普通账号展示。脚本取地址请用下方接口。
          </p>
        </details>
      </article>
    </div>
    <p class="small muted">
      累计流量来自机器的非回环网卡计数，重启或网卡重置后可能归零，包含其他程序和虚拟网卡流量，不代表转发计费流量。单位按
      1024 进位：1024 GB = 1 TB。
    </p>
    <ProbeHistory :nodes="nodes" />
    <ProbeActions
      v-if="operation"
      :node="operation.node"
      :mode="operation.mode"
      @close="operation = undefined"
      @removed="removed"
    />
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
