<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import Modal from "../components/Modal.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
type Row = Record<string, unknown>;
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
const token = ref("");
const initial = ref("");
const options = ref<{ nodes: Row[]; groups: Row[]; users: Row[] }>({
  nodes: [],
  groups: [],
  users: [],
});
const form = ref({
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
  user_ids: [] as string[],
  group_ids: [] as string[],
  blocked_protocols: [] as string[],
  multiplier: "1",
  port_min: 10000,
  port_max: 60000,
});
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
    user_ids: (row?.user_ids as string[]) || [],
    group_ids: [],
    blocked_protocols: (row?.blocked_protocols as string[]) || [],
    multiplier: String(row?.multiplier || "1"),
    port_min: Number(row?.port_min || 10000),
    port_max: Number(row?.port_max || 60000),
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
    };
  if (resource === "nodes") return { name: f.name, group_ids: f.group_ids };
  return {
    name: f.name,
    node_id: f.node_id,
    group_id: f.group_id,
    network: f.network,
    transport: f.transport,
    listen: f.listen,
    target: f.target,
    enabled: f.enabled,
    ...(f.transport === "direct"
      ? {}
      : {
          tunnel: {
            endpoint: f.endpoint,
            server_name: f.server_name,
            ...(f.token ? { token: f.token } : {}),
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
      token.value = `${result.token}\n有效期至 ${result.expires_at}`;
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
function value(v: unknown) {
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
                  resource === 'rules' || (resource === 'groups' && canManage)
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
                {{ value(row[col]) }}
              </td>
              <td
                v-if="
                  resource === 'rules' || (resource === 'groups' && canManage)
                "
                data-label="操作"
              >
                <div class="toolbar">
                  <button @click="open(row)">编辑</button
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
              >入口服务器<select v-model="form.node_id" required>
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
              >设备组<select v-model="form.group_id" required>
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
            <div class="form-grid">
              <label
                >传输层<select v-model="form.network">
                  <option value="tcp">TCP</option>
                  <option value="udp">UDP</option>
                </select></label
              ><label
                >隧道<select v-model="form.transport">
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
            <label
              >监听地址<input
                v-model="form.listen"
                required
                placeholder=":10000" /></label
            ><label
              >目标地址<input
                v-model="form.target"
                required
                placeholder="127.0.0.1:8080" /></label
            ><template v-if="form.transport !== 'direct'"
              ><label>隧道端点<input v-model="form.endpoint" required /></label
              ><label>TLS 服务器名称<input v-model="form.server_name" /></label
              ><label
                >隧道凭据<input
                  v-model="form.token"
                  type="password"
                  autocomplete="new-password"
                  :required="!selected"
                  :placeholder="
                    selected ? '留空保留既有凭据' : ''
                  " /></label></template
            ><label class="check"
              ><input v-model="form.enabled" type="checkbox" />启用规则</label
            ></template
          ><template v-if="resource === 'groups'"
            ><fieldset>
              <legend>屏蔽协议</legend>
              <label
                v-for="p in [
                  'tcp',
                  'udp',
                  'direct',
                  'tls',
                  'ws',
                  'wss',
                  'http',
                ]"
                :key="p"
                class="check"
                ><input
                  v-model="form.blocked_protocols"
                  type="checkbox"
                  :value="p"
                />{{ p }}</label
              >
            </fieldset>
            <label
              >流量倍率<input
                v-model="form.multiplier"
                required
                pattern="[0-9]+(\.[0-9]+)?"
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
  </section>
</template>
