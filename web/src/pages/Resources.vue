<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import Modal from "../components/Modal.vue";
import NodeOperations from "../components/NodeOperations.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";
type Row = Record<string, unknown>;
interface Hop {
  transport: string;
  endpoint: string;
  server_name: string;
  token: string;
}
const route = useRoute();
const router = useRouter();
const resource = String(route.params.resource);
const titles: Record<string, string> = {
  rules: "转发规则",
  nodes: "服务器",
  groups: "设备组",
  users: "用户管理",
  audit: "操作审计",
};
const rows = ref<Row[]>([]);
const total = ref(0);
const loading = ref(false);
const error = ref("");
const search = ref(String(route.query.q || ""));
const page = computed(() => Math.max(1, Number(route.query.page) || 1));
const canManage = computed(() => adminSite && state.user?.role === "admin");
const allowed = computed(
  () => !["users", "audit"].includes(resource) || canManage.value,
);
const editing = ref(false);
const busy = ref(false);
const formError = ref("");
const selected = ref<Row | null>(null);
const operationNode = ref<Row | null>(null);
const token = ref("");
const diagnosis = ref<{
  checks: { name: string; ok: boolean; detail: string }[];
} | null>(null);
async function diagnose(row: Row) {
  error.value = "";
  try {
    diagnosis.value = await api(
      `/rules/${encodeURIComponent(String(row.id))}/diagnose`,
    );
  } catch (e) {
    error.value = errorText(e);
  }
}
const networkBusy = ref(false),
  networkResult = ref<{
    id: string;
    status: string;
    checks: {
      stage: string;
      ok: boolean;
      milliseconds: number;
      detail: string;
    }[];
  } | null>(null);
let diagnosticTimer: ReturnType<typeof setTimeout> | undefined;
onUnmounted(() => clearTimeout(diagnosticTimer));
async function pollDiagnostic() {
  if (!networkResult.value) return;
  try {
    networkResult.value = await api(`/diagnostics/${networkResult.value.id}`);
    if (["pending", "running"].includes(networkResult.value!.status)) {
      diagnosticTimer = setTimeout(pollDiagnostic, 2000);
    } else {
      networkBusy.value = false;
    }
  } catch (e) {
    error.value = errorText(e);
    networkBusy.value = false;
  }
}
async function networkDiagnose(row: Row) {
  error.value = "";
  networkBusy.value = true;
  clearTimeout(diagnosticTimer);
  try {
    networkResult.value = await api(
      `/rules/${encodeURIComponent(String(row.id))}/network-diagnostic`,
      "POST",
      {},
    );
    void pollDiagnostic();
  } catch (e) {
    error.value = errorText(e);
    networkBusy.value = false;
  }
}
const initial = ref("");
const options = ref<{
  nodes: Row[];
  groups: Row[];
  users: Row[];
  exits: Row[];
}>({
  nodes: [],
  exits: [],
  groups: [],
  users: [],
});
const form = ref({
  exit_group_id: "",
  exit_id: "auto",
  proxy_accept: "off",
  proxy_send: "off",
  trusted_cidrs: "",
  name: "",
  username: "",
  password: "",
  role: "user",
  node_id: "",
  group_id: "",
  network: "tcp",
  transport: "direct",
  listen: ":10000",
  target: "",
  enabled: true,
  endpoint: "",
  server_name: "",
  token: "",
  chain: [] as Hop[],
  mux: false,
  reverse: "",
  backends: [] as { target: string; weight: number; disabled: boolean }[],
  shared: false,
  shared_parent: "",
  shared_name: "",
  user_ids: [] as string[],
  group_ids: [] as string[],
  blocked_protocols: [] as string[],
  multiplier: "1",
  port_min: 10000,
  port_max: 60000,
  max_rules: 0,
});
watch(
  () => form.value.exit_group_id,
  async (value) => {
    if (!value) return;
    try {
      options.value.exits = await choices("/exits");
    } catch (e) {
      formError.value = errorText(e);
    }
  },
);
const dirty = computed(() => JSON.stringify(form.value) !== initial.value);
async function load() {
  if (!allowed.value) return;
  loading.value = true;
  error.value = "";
  try {
    const result = await api<{ items: Row[]; total: number }>(
      `/${resource}?page=${page.value}&page_size=20&q=${encodeURIComponent(String(route.query.q || ""))}`,
    );
    rows.value = result.items;
    total.value = result.total;
  } catch (e) {
    if (!(e instanceof DOMException && e.name === "AbortError"))
      error.value = errorText(e);
  } finally {
    loading.value = false;
  }
}
async function choices(path: string) {
  const items: Row[] = [];
  let p = 1;
  while (true) {
    const batch = await api<{ items: Row[]; total: number }>(
      path + "?page_size=100&page=" + p,
    );
    items.push(...batch.items);
    if (!batch.items.length || items.length >= batch.total) return items;
    p++;
  }
}
async function open(row: Row | null = null) {
  selected.value = row;
  formError.value = "";
  token.value = "";
  form.value = {
    exit_group_id: String(row?.exit_group_id || ""),
    exit_id: String(row?.exit_id || "auto"),
    proxy_accept: String((row?.proxy_protocol as Row)?.accept || "off"),
    proxy_send: String((row?.proxy_protocol as Row)?.send || "off"),
    trusted_cidrs: (
      ((row?.proxy_protocol as Row)?.trusted_cidrs as string[]) || []
    ).join(","),
    name: String(row?.name || ""),
    username: "",
    password: "",
    role: "user",
    node_id: String(row?.node_id || ""),
    group_id: String(row?.group_id || ""),
    network: String(row?.network || "tcp"),
    transport: String(row?.transport || "direct"),
    listen: String(row?.listen || ":10000"),
    target: String(row?.target || ""),
    enabled: row?.enabled !== false,
    endpoint: String((row?.tunnel as Row | undefined)?.endpoint || ""),
    server_name: String((row?.tunnel as Row | undefined)?.server_name || ""),
    token: "",
    mux: !!(row?.tunnel as Row | undefined)?.mux,
    reverse: String((row?.tunnel as Row | undefined)?.reverse || ""),
    backends: (
      (row?.backends as {
        target: string;
        weight: number;
        disabled: boolean;
      }[]) || []
    ).map((b) => ({ ...b })),
    shared: !!row?.shared_tls,
    shared_parent: String(
      (row?.shared_tls as Row | undefined)?.parent_id || "",
    ),
    shared_name: String(
      (row?.shared_tls as Row | undefined)?.server_name || "",
    ),
    chain: (((row?.tunnel as Row | undefined)?.chain as Row[]) || []).map(
      (h) => ({
        transport: String(h.transport),
        endpoint: String(h.endpoint),
        server_name: String(h.server_name || ""),
        token: "",
      }),
    ),
    user_ids: (row?.user_ids as string[]) || [],
    group_ids: [],
    blocked_protocols: ((row?.blocked_protocols as string[]) || []).map((p) =>
      p.includes(":")
        ? p
        : ["tcp", "udp"].includes(p)
          ? "network:" + p
          : p === "socks"
            ? "app:socks"
            : "transport:" + p,
    ),
    multiplier: String(row?.multiplier || "1"),
    port_min: Number(row?.port_min || 10000),
    port_max: Number(row?.port_max || 60000),
    max_rules: Number(row?.max_rules || 0),
  };
  initial.value = JSON.stringify(form.value);
  editing.value = true;
  try {
    if (resource === "rules") {
      const [nodes, groups] = await Promise.all([
        choices("/nodes"),
        choices("/groups"),
      ]);
      options.value.nodes = nodes;
      options.value.groups = groups;
    }
    if (resource === "nodes") options.value.groups = await choices("/groups");
    if (resource === "groups") options.value.users = await choices("/users");
  } catch (e) {
    formError.value = errorText(e);
  }
}
function payload(): Row {
  const f = form.value;
  if (resource === "users")
    return { username: f.username, password: f.password, role: f.role };
  if (resource === "groups")
    return {
      name: f.name,
      user_ids: f.user_ids,
      blocked_protocols: f.blocked_protocols,
      multiplier: f.multiplier,
      port_min: f.port_min,
      port_max: f.port_max,
      max_rules: f.max_rules,
    };
  if (resource === "nodes") return { name: f.name, group_ids: f.group_ids };
  return {
    exit_group_id: f.exit_group_id,
    exit_id: f.exit_group_id ? f.exit_id : "",
    proxy_protocol:
      f.network === "tcp" &&
      (f.proxy_accept !== "off" || f.proxy_send !== "off")
        ? {
            accept: f.proxy_accept,
            send: f.proxy_send,
            trusted_cidrs: f.trusted_cidrs
              .split(",")
              .map((x) => x.trim())
              .filter(Boolean),
          }
        : null,
    name: f.name,
    node_id: f.node_id,
    group_id: f.group_id,
    network: f.network,
    transport: f.transport,
    listen: f.listen,
    target: f.target,
    enabled: f.enabled,
    backends: f.network === "tcp" ? f.backends : [],
    shared_tls:
      f.shared && f.network === "tcp"
        ? {
            parent_id: f.shared_parent,
            server_name: f.shared_name.toLowerCase(),
          }
        : null,
    ...(f.transport === "direct" || f.exit_group_id
      ? {}
      : {
          tunnel: {
            endpoint: f.endpoint,
            server_name: f.server_name,
            mux: f.mux,
            reverse: f.reverse,
            ...(f.token ? { token: f.token } : {}),
            chain: f.chain.map((h) => ({
              transport: h.transport,
              endpoint: h.endpoint,
              server_name: h.server_name,
              ...(h.token ? { token: h.token } : {}),
            })),
          },
        }),
  };
}
async function save() {
  if (busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    const data = payload();
    if (selected.value) data.version = selected.value.version;
    const path =
      resource === "nodes"
        ? "/nodes/enrollment"
        : `/${resource}${selected.value ? "/" + encodeURIComponent(String(selected.value.id)) : ""}`;
    const result = await api<{ token?: string; expires_at?: string }>(
      path,
      selected.value ? "PUT" : "POST",
      data,
    );
    if (resource === "nodes") {
      token.value = `${result.token}\n有效期至 ${formatDateTime(result.expires_at)}（${displayTimeZoneLabel}）`;
      initial.value = JSON.stringify(form.value);
    } else {
      editing.value = false;
      notice("已保存。转发规则需等待节点应用回执。");
      await load();
    }
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
const deleting = ref<Row | null>(null);
const statusTarget = ref<Row | null>(null);
async function changeStatus() {
  if (!statusTarget.value || busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    await api(
      `/users/${encodeURIComponent(String(statusTarget.value.id))}/status`,
      "PUT",
      { disabled: !statusTarget.value.disabled },
    );
    statusTarget.value = null;
    notice("账号状态已更新。停用账号的会话与 API Token 会被撤销。");
    await load();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
async function remove() {
  if (!deleting.value || busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    await api(
      `/rules/${encodeURIComponent(String(deleting.value.id))}?version=${Number(deleting.value.version)}`,
      "DELETE",
    );
    deleting.value = null;
    notice("删除请求已提交，端口释放以节点确认解绑为准。");
    await load();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
function query(next: number) {
  void router.replace({
    query: { q: search.value || undefined, page: String(next) },
  });
}
watch(() => route.query, load);
onMounted(load);
function value(v: unknown, column: string) {
  if (
    [
      "last_seen",
      "created_at",
      "expires_at",
      "sampled_at",
      "observed_at",
    ].includes(column)
  )
    return typeof v === "string" ? formatDateTime(v) : "未知";
  return v === null || v === undefined
    ? "—"
    : typeof v === "object"
      ? JSON.stringify(v)
      : String(v);
}
const columns = computed(() =>
  resource === "rules"
    ? ["name", "transport", "listen", "target", "enabled", "version"]
    : resource === "nodes"
      ? [
          "name",
          "agent_version",
          "last_seen",
          "desired_version",
          "applied_version",
          "apply_error",
        ]
      : resource === "groups"
        ? ["name", "blocked_protocols", "multiplier", "port_min", "port_max"]
        : resource === "users"
          ? ["username", "role", "disabled"]
          : Object.keys(rows.value[0] || {}).slice(0, 6),
);
const labels: Record<string, string> = {
  name: "名称",
  transport: "隧道",
  listen: "监听",
  target: "目标",
  enabled: "启用",
  version: "版本",
  agent_version: "Agent 版本",
  last_seen: "最后心跳",
  desired_version: "期望版本",
  applied_version: "应用版本",
  apply_error: "应用错误",
  blocked_protocols: "屏蔽协议",
  multiplier: "流量倍率",
  port_min: "起始端口",
  port_max: "结束端口",
  username: "用户名",
  role: "角色",
  disabled: "停用",
  created_at: "时间",
};
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">RESOURCE MANAGEMENT</p>
        <h1>{{ titles[resource] }}</h1>
        <p class="muted">
          {{
            resource === "nodes"
              ? "在线心跳与配置应用状态分别展示。"
              : "配置与权限由服务端统一校验。"
          }}
          <span v-if="resource === 'nodes' || resource === 'audit'"
            >时间使用{{ displayTimeZoneLabel }}。</span
          >
        </p>
      </div>
      <button
        v-if="
          allowed &&
          (resource === 'rules' || (canManage && resource !== 'audit'))
        "
        class="primary"
        @click="open()"
      >
        {{ resource === "nodes" ? "生成接入凭据" : "新增" }}
      </button>
    </div>
    <p v-if="!allowed" class="card">无权访问此资源。</p>
    <template v-else
      ><form class="searchbar" @submit.prevent="query(1)">
        <input
          v-model="search"
          aria-label="搜索资源"
          placeholder="搜索名称…"
        /><button>搜索</button><button type="button" @click="load">刷新</button>
      </form>
      <p v-if="error" role="alert" class="error">{{ error }}</p>
      <div class="card table-wrap" :aria-busy="loading">
        <p v-if="loading" class="empty">正在加载…</p>
        <p v-else-if="!rows.length" class="empty">暂无数据。</p>
        <table v-else>
          <thead>
            <tr>
              <th v-for="col in columns" :key="col">
                {{ labels[col] || col }}
              </th>
              <th
                v-if="
                  resource === 'rules' ||
                  (['groups', 'users', 'nodes'].includes(resource) && canManage)
                "
              >
                操作
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in rows" :key="String(row.id)">
              <td
                v-for="col in columns"
                :key="col"
                :data-label="labels[col] || col"
              >
                {{ value(row[col], col) }}
              </td>
              <td
                v-if="
                  resource === 'rules' ||
                  (['groups', 'users', 'nodes'].includes(resource) && canManage)
                "
                data-label="操作"
              >
                <div class="toolbar">
                  <button v-if="resource === 'rules'" @click="diagnose(row)">
                    诊断</button
                  ><button
                    :disabled="networkBusy"
                    @click="networkDiagnose(row)"
                  >
                    网络诊断
                  </button>
                  <button
                    v-if="resource === 'nodes'"
                    @click="operationNode = row"
                  >
                    节点运维
                  </button>
                  <button
                    v-if="!['users', 'nodes'].includes(resource)"
                    @click="open(row)"
                  >
                    编辑</button
                  ><button
                    v-if="resource === 'rules'"
                    class="danger"
                    @click="
                      deleting = row;
                      formError = '';
                    "
                  >
                    删除
                  </button>
                  <button
                    v-if="resource === 'users'"
                    :disabled="row.role === 'admin'"
                    @click="
                      statusTarget = row;
                      formError = '';
                    "
                  >
                    {{ row.disabled ? "启用" : "停用" }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="pagination">
        <span>共 {{ total }} 条 · 第 {{ page }} 页</span
        ><button :disabled="page <= 1 || loading" @click="query(page - 1)">
          上一页</button
        ><button
          :disabled="page * 20 >= total || loading"
          @click="query(page + 1)"
        >
          下一页
        </button>
      </div></template
    >
    <section v-if="diagnosis" class="card">
      <h2>规则状态诊断</h2>
      <p class="muted">检查控制面记录和租约状态。</p>
      <p v-for="check in diagnosis.checks" :key="check.name">
        {{ check.ok ? "通过" : "需处理" }} · {{ check.name }}：{{
          check.detail
        }}
      </p>
      <button @click="diagnosis = null">关闭诊断</button>
    </section>
    <section v-if="networkResult" class="card">
      <h2>网络诊断 / Looking Glass</h2>
      <p>
        {{
          networkResult.status === "completed"
            ? "检查完成"
            : networkResult.status === "expired"
              ? "诊断已超时"
              : "等待节点执行…"
        }}
      </p>
      <p v-for="(c, i) in networkResult.checks" :key="i">
        {{ c.ok ? "通过" : "失败" }} · {{ c.stage }} · {{ c.milliseconds }} ms
      </p>
      <p class="small muted">
        从入口 Agent 检查当前 TCP
        规则；隧道检查包含出口到目标的握手，不发送业务数据。
      </p>
    </section>
    <NodeOperations
      v-if="operationNode"
      :node="operationNode"
      @close="operationNode = null"
    />
    <Modal
      v-if="editing"
      :title="
        resource === 'nodes'
          ? '一次性节点接入凭据'
          : (selected ? '编辑' : '新增') + titles[resource]
      "
      :busy="busy"
      :dirty="dirty"
      @close="editing = false"
      ><form @submit.prevent="save">
        <p v-if="formError" class="error" role="alert">{{ formError }}</p>
        <template v-if="token"
          ><p>请立即保存凭据，关闭后不再展示。不要发送给未授权人员。</p>
          <textarea
            :value="token"
            readonly
            rows="5"
            aria-label="一次性接入凭据"
          /><button type="button" @click="editing = false">
            已保存，关闭
          </button></template
        ><template v-else
          ><label v-if="resource !== 'users'"
            >名称<input v-model="form.name" required maxlength="100" /></label
          ><template v-if="resource === 'users'"
            ><label
              >用户名<input
                v-model="form.username"
                autocomplete="off"
                required
                maxlength="64" /></label
            ><label
              >初始密码<input
                v-model="form.password"
                type="password"
                autocomplete="new-password"
                required
                minlength="12" /></label
            ><label
              >角色<select v-model="form.role">
                <option value="user">普通用户</option>
                <option value="admin">管理员</option>
              </select></label
            ></template
          ><template v-if="resource === 'rules'"
            ><label
              >入口服务器<select
                v-model="form.node_id" aria-label="入口服务器"
                required
                :disabled="!!selected"
              >
                <option value="" disabled>选择服务器</option>
                <option
                  v-for="n in options.nodes"
                  :key="String(n.id)"
                  :value="n.id"
                >
                  {{ n.name }}
                </option>
              </select></label
            ><label
              >设备组<select
                v-model="form.group_id" aria-label="设备组"
                required
                :disabled="!!selected"
              >
                <option value="" disabled>选择设备组</option>
                <option
                  v-for="g in options.groups"
                  :key="String(g.id)"
                  :value="g.id"
                >
                  {{ g.name }}
                </option>
              </select></label
            >
            <label
              >出口选择<select v-model="form.exit_group_id" aria-label="出口选择">
                <option value="">直接转发或手工配置隧道</option>
                <option
                  v-for="g in options.groups"
                  :key="String(g.id)"
                  :value="g.id"
                >
                  {{ g.name }} · 出口倍率 {{ g.multiplier }}
                </option>
              </select></label
            >
            <label v-if="form.exit_group_id"
              >出口节点<select v-model="form.exit_id" aria-label="出口节点">
                <option value="auto">按权重自动选择</option>
                <option
                  v-for="e in options.exits.filter(
                    (e) => e.group_id === form.exit_group_id,
                  )"
                  :key="String(e.id)"
                  :value="e.id"
                >
                  {{ e.name }} · {{ e.online ? "在线" : "离线" }}
                </option>
              </select></label
            >
            <p v-if="form.exit_group_id" class="small muted">
              自动选择该组内授权且在线的出口。流量按入口组倍率 ×
              出口组倍率结算。
            </p>
            <div class="form-grid">
              <label
                >传输层<select v-model="form.network" :disabled="!!selected">
                  <option value="tcp">TCP</option>
                  <option value="udp">UDP</option>
                </select></label
              ><label
                >隧道<select v-model="form.transport" aria-label="隧道">
                  <option
                    v-for="t in ['direct', 'tls', 'ws', 'wss', 'http']"
                    :key="t"
                  >
                    {{ t }}
                  </option>
                </select></label
              >
            </div>
            <p class="small muted">
              可用组合由节点能力校验。WS 与 HTTP 本身不加密。
            </p>
            <p v-if="selected" class="small muted">
              入口、设备组、传输层和监听地址创建后不可更改。如需调整，请删除并等待节点确认解绑后重建。
            </p>
            <label
              >监听地址<input
                v-model="form.listen"
                :disabled="!!selected"
                required
                placeholder=":10000" /></label
            ><label
              >目标地址<input
                v-model="form.target"
                required
                placeholder="127.0.0.1:8080" /></label
            ><template v-if="form.transport !== 'direct' && !form.exit_group_id"
              ><label>隧道端点<input v-model="form.endpoint" required /></label
              ><label>TLS 服务器名称<input v-model="form.server_name" /></label
              ><label
                >隧道凭据<input
                  v-model="form.token"
                  type="password"
                  autocomplete="new-password"
                  :required="!selected"
                  :placeholder="selected ? '留空保留既有凭据' : ''"
              /></label>
              <label class="check"
                ><input v-model="form.mux" type="checkbox" />启用 Mux
                连接复用</label
              >
              <label
                >反向出口标识<input
                  v-model="form.reverse"
                  maxlength="128"
                  placeholder="留空使用普通出口"
              /></label>
              <p v-if="form.reverse" class="muted small">
                出口主动连接隧道端点。反向路由不能添加后续出口。
              </p>
              <fieldset>
                <legend>后续出口（最多两跳）</legend>
                <p class="muted small">
                  入口 → 首出口<span
                    v-for="(_, index) in form.chain"
                    :key="index"
                  >
                    → 出口 {{ index + 2 }}</span
                  >
                  → 目标
                </p>
                <div
                  v-for="(hop, index) in form.chain"
                  :key="index"
                  class="card"
                >
                  <label :for="`hop-transport-${index}`"
                    >出口 {{ index + 2 }} 承载</label
                  ><select
                    :id="`hop-transport-${index}`"
                    v-model="hop.transport"
                  >
                    <option
                      v-for="t in ['tls', 'ws', 'wss', 'http']"
                      :key="t"
                      :value="t"
                    >
                      {{ t }}
                    </option>
                  </select>
                  <label
                    >出口 {{ index + 2 }} 端点<input
                      v-model="hop.endpoint"
                      required
                  /></label>
                  <label
                    >出口 {{ index + 2 }} TLS 服务器名称<input
                      v-model="hop.server_name"
                  /></label>
                  <label
                    >出口 {{ index + 2 }} 凭据<input
                      v-model="hop.token"
                      type="password"
                      autocomplete="new-password"
                      :required="!selected"
                      :placeholder="selected ? '地址和身份不变时留空保留' : ''"
                  /></label>
                  <button type="button" @click="form.chain.splice(index, 1)">
                    移除出口 {{ index + 2 }}
                  </button>
                </div>
                <button
                  type="button"
                  :disabled="form.chain.length >= 2 || !!form.reverse"
                  @click="
                    form.chain.push({
                      transport: 'tls',
                      endpoint: '',
                      server_name: '',
                      token: '',
                    })
                  "
                >
                  添加后续出口
                </button>
              </fieldset></template
            >
            <fieldset v-if="form.network === 'tcp'">
              <legend>Proxy Protocol</legend>
              <div class="form-grid">
                <label
                  >接收<select v-model="form.proxy_accept" aria-label="接收">
                    <option value="off">关闭</option>
                    <option value="v1">v1</option>
                    <option value="v2">v2</option>
                  </select></label
                ><label
                  >发送<select v-model="form.proxy_send" aria-label="发送">
                    <option value="off">关闭</option>
                    <option value="v1">v1</option>
                    <option value="v2">v2</option>
                  </select></label
                >
              </div>
              <label v-if="form.proxy_accept !== 'off'"
                >可信上游 CIDR<input
                  v-model="form.trusted_cidrs"
                  required
                  placeholder="192.0.2.0/24,2001:db8::/32"
              /></label>
              <p class="small muted">
                仅支持
                TCP。启用接收时，上游必须发送所选版本的头；发送需目标服务支持该版本。
              </p>
            </fieldset>
            <fieldset v-if="form.network === 'tcp'">
              <legend>多目标与故障转移</legend>
              <p class="small muted">
                配置后按权重分配新连接；连接失败剔除目标，健康检查成功后恢复。已有连接不能迁移。
              </p>
              <div
                v-for="(backend, index) in form.backends"
                :key="index"
                class="card"
              >
                <label
                  >后端 {{ index + 1 }} 地址<input
                    v-model="backend.target"
                    required
                    placeholder="127.0.0.1:8080"
                /></label>
                <label
                  >后端 {{ index + 1 }} 权重<input
                    v-model.number="backend.weight"
                    type="number"
                    min="1"
                    max="100"
                    required
                /></label>
                <label class="check"
                  ><input
                    v-model="backend.disabled"
                    type="checkbox"
                  />停用此后端</label
                >
                <button type="button" @click="form.backends.splice(index, 1)">
                  移除后端 {{ index + 1 }}
                </button>
              </div>
              <button
                type="button"
                :disabled="form.backends.length >= 16"
                @click="
                  form.backends.push({ target: '', weight: 1, disabled: false })
                "
              >
                添加后端
              </button>
            </fieldset>
            <fieldset v-if="form.network === 'tcp'">
              <legend>TLS 共享端口</legend>
              <label class="check"
                ><input
                  v-model="form.shared"
                  type="checkbox"
                  :disabled="!!selected && !!form.shared_parent"
                />按 SNI 路由</label
              >
              <template v-if="form.shared">
                <label
                  >匹配域名<input
                    v-model="form.shared_name"
                    required
                    placeholder="app.example.com"
                /></label>
                <label
                  >母规则 ID<input
                    v-model="form.shared_parent"
                    :disabled="!!selected"
                    placeholder="留空创建母规则"
                /></label>
                <p class="small muted">
                  子规则须与母规则属于同一账号、节点、设备组和监听地址。空 SNI
                  与未匹配域名会被拒绝；客户端直接验证回源服务证书。
                </p>
              </template>
            </fieldset>
            <label class="check"
              ><input v-model="form.enabled" type="checkbox" />启用规则</label
            ></template
          ><template v-if="resource === 'groups'"
            ><fieldset>
              <legend>屏蔽网络协议</legend>
              <label v-for="p in ['tcp', 'udp']" :key="p" class="check"
                ><input
                  v-model="form.blocked_protocols"
                  type="checkbox"
                  :value="'network:' + p"
                />{{ p.toUpperCase() }}</label
              >
            </fieldset>
            <fieldset>
              <legend>屏蔽隧道承载</legend>
              <label
                v-for="p in ['direct', 'tls', 'ws', 'wss', 'http']"
                :key="p"
                class="check"
                ><input
                  v-model="form.blocked_protocols"
                  type="checkbox"
                  :value="'transport:' + p"
                />{{ p === "direct" ? "直接转发" : p.toUpperCase() }}</label
              >
            </fieldset>
            <fieldset>
              <legend>屏蔽明文应用协议</legend>
              <label v-for="p in ['http', 'socks']" :key="p" class="check"
                ><input
                  v-model="form.blocked_protocols"
                  type="checkbox"
                  :value="'app:' + p"
                />{{ p.toUpperCase() }} 应用流量</label
              >
            </fieldset>
            <p class="small muted">
              网络和隧道限制控制可创建的规则；应用识别只检查可识别的明文流量。未知流量允许通过，密文内的协议和
              URL 路径无法识别。禁用 HTTP 隧道不等同于屏蔽 HTTP 应用。
            </p>
            <label
              >流量倍率<input
                v-model="form.multiplier"
                required
                pattern="[0-9]+(\.[0-9]+)?"
            /></label>
            <label
              >每用户规则上限（0 表示不限）<input
                v-model.number="form.max_rules"
                type="number"
                min="0"
                max="100000"
                required
            /></label>
            <div class="form-grid">
              <label
                >起始端口<input
                  v-model.number="form.port_min"
                  type="number"
                  min="1"
                  max="65535"
                  required /></label
              ><label
                >结束端口<input
                  v-model.number="form.port_max"
                  type="number"
                  :min="form.port_min"
                  max="65535"
                  required
              /></label>
            </div>
            <fieldset>
              <legend>授权用户</legend>
              <label
                v-for="u in options.users"
                :key="String(u.id)"
                class="check"
                ><input
                  v-model="form.user_ids"
                  type="checkbox"
                  :value="u.id"
                />{{ u.username }}</label
              >
            </fieldset></template
          >
          <fieldset v-if="resource === 'nodes'">
            <legend>关联设备组</legend>
            <label v-for="g in options.groups" :key="String(g.id)" class="check"
              ><input
                v-model="form.group_ids"
                type="checkbox"
                :value="g.id"
              />{{ g.name }}</label
            >
          </fieldset>
          <div class="form-actions">
            <button class="primary" :disabled="busy">
              {{ busy ? "正在提交…" : "保存" }}
            </button>
          </div></template
        >
      </form></Modal
    ><Modal
      v-if="deleting"
      title="删除转发规则"
      :busy="busy"
      @close="deleting = null"
      ><p>确认删除 {{ deleting.name }}？现有转发会在节点应用配置后停止。</p>
      <p v-if="formError" class="error" role="alert">{{ formError }}</p>
      <button class="danger" :disabled="busy" @click="remove">
        确认删除
      </button></Modal
    >
    <Modal
      v-if="statusTarget"
      :title="statusTarget.disabled ? '启用账号' : '停用账号'"
      :busy="busy"
      @close="statusTarget = null"
      ><p>
        {{ statusTarget.disabled ? "恢复" : "停用" }}
        {{ statusTarget.username }}？停用会撤销所有会话和 API
        Token，并更新节点转发权限。
      </p>
      <p v-if="formError" class="error" role="alert">{{ formError }}</p>
      <button :disabled="busy" @click="changeStatus">
        确认修改状态
      </button></Modal
    >
  </section>
</template>
