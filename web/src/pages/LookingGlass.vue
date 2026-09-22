<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import Select from "../components/Select.vue";
import { api, errorText } from "../core/api";
import { notice } from "../core/state";

/**
 * 网络诊断（LookingGlass）。
 *
 * 从**节点所在的位置**发 ping / tcping / mtr —— 排查「用户说连不上」时，
 * 先要回答的是「从入口机器看过去，目标到底是通的还是不通的」，而这从面板
 * 本机 ping 是看不出来的。
 *
 * 底层不经过节点运维的远程终端：那条通道是一个不受限的 shell，默认关闭、
 * 还要每次二次授权。这里只有三种方法、参数结构化、面板拼 argv 由 agent
 * 直接 exec，所以默认就能用。
 */
interface Node {
  id: string;
  name: string;
  capabilities?: string[];
}
interface Request {
  id: string;
  status: string;
  output?: string;
  error?: string;
}

const nodes = ref<Node[]>([]);
const nodeID = ref("");
const method = ref("ping");
const target = ref("");
const output = ref("");
const error = ref("");
const busy = ref(false);
const requestID = ref("");
const loading = ref(true);

const methods = [
  { value: "ping", label: "ping（连通性与延迟）" },
  { value: "tcping", label: "tcping（端口连通性）" },
  { value: "mtr", label: "mtr（逐跳路由）" },
];
const placeholder = computed(() =>
  method.value === "tcping" ? "10.20.0.11:27015" : "10.20.0.11 或 db.internal",
);
// 只有声明了能力的节点能被诊断 —— 老 Agent 收不到这类请求。这里先标出来，
// 免得操作方点了运行才被拒绝。
const capable = computed(
  () =>
    nodes.value.filter((n) => (n.capabilities || []).includes("looking-glass-v1"))
      .length,
);
const selected = computed(() =>
  nodes.value.find((n) => n.id === nodeID.value),
);
const runnable = computed(
  () =>
    !busy.value &&
    target.value.trim() !== "" &&
    !!selected.value?.capabilities?.includes("looking-glass-v1"),
);

let timer: ReturnType<typeof setTimeout> | undefined;
let alive = true;

async function load() {
  loading.value = true;
  error.value = "";
  try {
    // 接口一次最多给 100 条，机器多起来要分页取完：漏掉的那几台会直接从下拉里
    // 消失，操作方只会以为「这台机器选不了」。做法与探针页的历史选择器一致。
    const items: Node[] = [];
    for (let page = 1; ; page++) {
      const next = await api<{ items: Node[]; total: number }>(
        `/nodes?page=${page}&page_size=100`,
      );
      if (!alive) return;
      items.push(...next.items);
      if (!next.items.length || items.length >= next.total) break;
    }
    nodes.value = items;
    const first =
      items.find((n) => (n.capabilities || []).includes("looking-glass-v1")) ||
      items[0];
    nodeID.value = first?.id || "";
  } catch (e) {
    if (alive) error.value = errorText(e);
  } finally {
    if (alive) loading.value = false;
  }
}

// 结果是异步来的（面板排队 → 节点认领 → 执行 → 回传），所以这里轮询而不是
// 等一个长连接：一次诊断最多几秒，轮询足够，也不必为它维护一条常驻通道。
async function poll(id: string, deadline: number) {
  if (!alive) return;
  if (Date.now() > deadline) {
    error.value = "等待节点响应超时。请确认节点在线且已升级到支持该功能的版本。";
    busy.value = false;
    return;
  }
  try {
    const r = await api<Request>(`/looking-glass/${encodeURIComponent(id)}`);
    if (!alive) return;
    output.value = r.output || "";
    if (r.status === "done" || r.status === "failed") {
      if (r.error) error.value = r.error;
      busy.value = false;
      return;
    }
    if (r.status === "expired") {
      error.value = "节点在有效期内没有领取这条诊断，可能已离线。";
      busy.value = false;
      return;
    }
  } catch (e) {
    if (!alive) return;
    error.value = errorText(e);
    busy.value = false;
    return;
  }
  timer = setTimeout(() => void poll(id, deadline), 800);
}

async function run() {
  const node = selected.value;
  if (!node || !runnable.value) return;
  busy.value = true;
  error.value = "";
  requestID.value = "";
  output.value = "正在等待节点执行…\n";
  try {
    const created = await api<Request>(
      `/nodes/${encodeURIComponent(node.id)}/looking-glass`,
      "POST",
      { method: method.value, target: target.value.trim() },
    );
    requestID.value = created.id;
    void poll(created.id, Date.now() + 40000);
  } catch (e) {
    error.value = errorText(e);
    output.value = "";
    busy.value = false;
  }
}

function copy() {
  if (!output.value) return;
  void navigator.clipboard
    .writeText(output.value)
    .then(() => notice("诊断输出已复制。"))
    .catch(() => notice("复制失败，请手动选中。"));
}

onMounted(load);
onUnmounted(() => {
  alive = false;
  clearTimeout(timer);
});
</script>

<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">NETWORK DIAGNOSTICS</p>
        <h1>网络诊断</h1>
        <p class="muted">
          从节点所在的位置执行 ping、tcping 与 mtr，用于判断目标在入口机器看过去是否可达。
        </p>
      </div>
      <button :disabled="loading" @click="load">刷新节点</button>
    </div>

    <p v-if="error" class="warning" role="alert">{{ error }}</p>

    <section class="card">
      <p v-if="loading" class="empty">正在读取节点…</p>
      <p v-else-if="!nodes.length" class="empty">没有可诊断的节点。</p>
      <template v-else>
        <div class="form-grid">
          <label
            >节点<Select v-model="nodeID" aria-label="诊断节点">
              <option v-for="n in nodes" :key="n.id" :value="n.id">
                {{ n.name
                }}{{
                  (n.capabilities || []).includes("looking-glass-v1")
                    ? ""
                    : "（Agent 版本过低）"
                }}
              </option>
            </Select></label
          >
          <label
            >方式<Select v-model="method" aria-label="诊断方式">
              <option v-for="m in methods" :key="m.value" :value="m.value">
                {{ m.label }}
              </option>
            </Select></label
          >
        </div>
        <label
          >目标<input
            v-model="target"
            :placeholder="placeholder"
            aria-label="诊断目标"
            maxlength="300"
            @keyup.enter="run"
        /></label>
        <p class="muted small">
          {{
            method === "tcping"
              ? "填写 主机:端口，例如 10.20.0.11:27015。tcping 由 Agent 自己完成 TCP 握手，不依赖节点上装了哪个工具。"
              : "填写主机名或 IP；ping 与 mtr 调用节点上的系统命令，缺失时会明确提示。"
          }}
        </p>
        <div class="form-actions">
          <button class="primary" :disabled="!runnable" @click="run">
            {{ busy ? "执行中…" : "开始诊断" }}
          </button>
        </div>
      </template>
    </section>

    <section v-if="output || busy" class="card">
      <div class="section-heading">
        <h2>输出<span v-if="requestID" class="muted small"> · 诊断编号 {{ requestID }}</span></h2>
        <button :disabled="!output" @click="copy">复制</button>
      </div>
      <pre class="terminal-output" role="log" aria-label="诊断输出">{{
        output
      }}</pre>
      <p v-if="!capable && nodes.length" class="muted small">
        当前可见的节点都没有声明 looking-glass-v1，请先把 Agent 升级到支持该功能的版本。
      </p>
    </section>
  </section>
</template>
