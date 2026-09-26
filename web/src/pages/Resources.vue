<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import Select from "../components/Select.vue";
import { useRoute, useRouter } from "vue-router";
import Modal from "../components/Modal.vue";
import NodeOperations from "../components/NodeOperations.vue";
import OnboardCommand from "../components/OnboardCommand.vue";
import OnboardPanel from "../components/OnboardPanel.vue";
import GroupAdvanced from "../components/GroupAdvanced.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";
import {
  groupTypes,
  isEntryGroup,
  isExitGroup,
  isPhysicalExitGroup,
} from "../core/groups";
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
  "identity-groups": "身份用户组",
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
  () =>
    !["identity-groups", "users", "audit"].includes(resource) ||
    canManage.value,
);
const editing = ref(false);
const busy = ref(false);
const formError = ref("");
const selected = ref<Row | null>(null);
const operationNode = ref<Row | null>(null);
const token = ref("");
const generatedPassword = ref("");
const generatedUsername = ref("");
const passwordCopied = ref(false);
const passwordResetTarget = ref<Row | null>(null);
const resetPasswordSecret = ref("");
const resetPasswordCopied = ref(false);
const groupTransports = [
  { value: "direct", label: "直接转发" },
  { value: "direct-tls", label: "TLS 直连目标" },
  { value: "tls", label: "TLS 隧道" },
  { value: "ws", label: "WebSocket 隧道" },
  { value: "wss", label: "WSS 隧道" },
  { value: "http", label: "HTTP 隧道" },
];
// 承载。direct 与 direct-tls 都没有出口，区别只在到目标的那一段加不加密 ——
// 前者把业务明文直接发出去，后者在同一条连接上先做 TLS 握手。
const transports = [
  { value: "direct", label: "direct（明文到目标）" },
  { value: "direct-tls", label: "direct-tls（TLS 到目标）" },
  { value: "tls", label: "tls" },
  { value: "ws", label: "ws" },
  { value: "wss", label: "wss" },
  { value: "http", label: "http" },
];
const onboardTarget = ref<Row | null>(null);
const joinKey = ref("");
const confirmRotate = ref(false);
const advancedTarget = ref<Row | null>(null);

/**
 * 账号的 API 凭据。
 *
 * 管理员在这里给某个 UID 发密钥、看它有没有被用过、需要时重置 —— 这就是
 * 「建完账号把钥匙交给用户」的完整闭环。明文只在创建与重置的那一次响应里
 * 出现，之后连管理员也取不回来，所以界面必须把那一次展示做得足够醒目。
 */
interface UserToken {
  id: string;
  name: string;
  prefix: string;
  scope: string;
  created_at: string;
  expires_at: string | null;
  permanent: boolean;
  last_used_at?: string | null;
}
const tokenUser = ref<Row | null>(null);
const userTokens = ref<UserToken[]>([]);
const issueTokenName = ref("");
const issuePermanent = ref(false);
const issueTokenDays = ref(30);
const issuedSecret = ref("");
const issuedFor = ref("");
const tokenBusy = ref(false);
async function openUserTokens(row: Row) {
  tokenUser.value = row;
  userTokens.value = [];
  issueTokenName.value = "";
  issuePermanent.value = false;
  issueTokenDays.value = 30;
  issuedSecret.value = "";
  issuedFor.value = "";
  formError.value = "";
  await loadUserTokens();
}
async function loadUserTokens() {
  if (!tokenUser.value) return;
  try {
    const result = await api<{ items: UserToken[] }>(
      `/users/${encodeURIComponent(String(tokenUser.value.id))}/tokens?page_size=100`,
    );
    userTokens.value = result.items;
  } catch (e) {
    formError.value = errorText(e);
  }
}
async function issueUserToken() {
  if (tokenBusy.value || !tokenUser.value) return;
  tokenBusy.value = true;
  formError.value = "";
  try {
    const result = await api<{ token: string }>(
      `/users/${encodeURIComponent(String(tokenUser.value.id))}/tokens`,
      "POST",
      {
        name: issueTokenName.value,
        ...(issuePermanent.value
          ? { permanent: true }
          : {
              expires_at: new Date(
                Date.now() + issueTokenDays.value * 86400000,
              ).toISOString(),
            }),
      },
    );
    issuedSecret.value = result.token;
    issuedFor.value = issueTokenName.value;
    issueTokenName.value = "";
    await loadUserTokens();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    tokenBusy.value = false;
  }
}
async function resetUserToken(token: UserToken) {
  if (tokenBusy.value || !tokenUser.value) return;
  tokenBusy.value = true;
  formError.value = "";
  try {
    const result = await api<{ token: string }>(
      `/users/${encodeURIComponent(String(tokenUser.value.id))}/tokens/${encodeURIComponent(token.id)}/reset`,
      "POST",
      {},
    );
    issuedSecret.value = result.token;
    issuedFor.value = token.name;
    await loadUserTokens();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    tokenBusy.value = false;
  }
}
async function revokeUserToken(token: UserToken) {
  if (tokenBusy.value || !tokenUser.value) return;
  tokenBusy.value = true;
  formError.value = "";
  try {
    await api(
      `/users/${encodeURIComponent(String(tokenUser.value.id))}/tokens/${encodeURIComponent(token.id)}`,
      "DELETE",
    );
    notice("API Token 已撤销，使用它的脚本会立刻失效。");
    await loadUserTokens();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    tokenBusy.value = false;
  }
}
// 刚建出来的那一行要高亮扫过一次：保存后列表会重新加载，新行出现在一堆
// 长得一样的行里，没有任何提示的话用户得自己找。
const highlighted = ref("");
let highlightTimer: ReturnType<typeof setTimeout> | undefined;
function highlight(id: unknown) {
  clearTimeout(highlightTimer);
  highlighted.value = String(id);
  highlightTimer = setTimeout(() => (highlighted.value = ""), 600);
}
onUnmounted(() => clearTimeout(highlightTimer));
// 令牌与过期时间分开存：接入命令要用裸令牌拼，过期时间要单独渲染。
const tokenExpiry = ref("");
// 一次性令牌那条命令。地址取浏览器当前的 origin：面板在反向代理后面时，
// 那正是设备应当访问到的公网地址。
const origin = location.origin;
const entryCommand = computed(
  () =>
    `bash <(curl -fLsS ${origin}/download/agent-install.sh) -t '${token.value}' -u '${origin}' -n '${tokenName.value}'`,
);
const entryManual = computed(
  () =>
    `TFP_ENROLLMENT_TOKEN='${token.value}' tfp-agent -panel '${origin}' -name '${tokenName.value}'`,
);
const tokenName = ref("");
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
  identityGroups: Row[];
}>({
  nodes: [],
  exits: [],
  groups: [],
  users: [],
  identityGroups: [],
});
const form = ref({
  group_type: "",
  direct_policy: "forbid",
  chain_group_ids: ["", ""],
  exit_group_id: "",
  exit_id: "auto",
  proxy_accept: "off",
  proxy_send: "off",
  trusted_cidrs: "",
  name: "",
  username: "",
  role: "user",
  identity_group_id: "",
  node_id: "",
  group_id: "",
  network: "tcp",
  transport: "direct",
  listen: "",
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
  identity_group_ids: [] as string[],
  group_ids: [] as string[],
  multiplier: "1",
  port_min: 10000,
  port_max: 60000,
  max_rules: 0,
});
const formIsEntry = computed(
  () =>
    form.value.group_type === "entry" ||
    (!!selected.value && !form.value.group_type),
);
const formForwards = computed(
  () =>
    form.value.group_type !== "monitor" &&
    (!!form.value.group_type || !!selected.value),
);
const entryGroups = computed(() => options.value.groups.filter(isEntryGroup));
// 下拉的 value 只能是字符串，而这一项要同时提交机器与设备组，所以把两个 id
// 拼起来（都是接口给的不透明字符串，中间用 :: 隔开）。
function entryKeyOf(nodeID: string, groupID: string) {
  return nodeID && groupID ? `${nodeID}::${groupID}` : "";
}
// 入口服务器与设备组是同一件事的两面：规则必须落在「某台机器 + 它所属的入口
// 组」这个组合上，后端也是按这个组合校验的（节点必须在该组里）。分成两个下拉
// 之后，运营方看到的是同一台机器的两个名字 —— 组里只有一台机器时更是纯重复。
// 合成一个：一台机器一条选项，名字取设备组名；只有一组里不止一台机器时才补上
// 机器名，否则两条选项读起来一模一样。
const entryOptions = computed(() => {
  const names = new Map(
    entryGroups.value.map((g) => [String(g.id), String(g.name)]),
  );
  const rows: {
    key: string;
    label: string;
    nodeID: string;
    groupID: string;
  }[] = [];
  for (const node of options.value.nodes) {
    const nodeID = String(node.id);
    for (const id of ((node.group_ids as string[]) || []).map(String)) {
      // 出口组和链式组不能当入口，它们不出现在这里。
      if (!names.has(id)) continue;
      rows.push({
        key: entryKeyOf(nodeID, id),
        label: String(node.name),
        nodeID,
        groupID: id,
      });
    }
  }
  const perGroup = new Map<string, number>();
  for (const row of rows)
    perGroup.set(row.groupID, (perGroup.get(row.groupID) || 0) + 1);
  for (const row of rows) {
    const group = names.get(row.groupID) || row.groupID;
    row.label =
      (perGroup.get(row.groupID) || 0) > 1
        ? `${group} · ${row.label}`
        : group;
  }
  // 编辑一条入口已经不在组里的旧规则时，列表里没有对应选项，下拉会是空的。
  // 补一条只读的，让运营方看到这条规则实际落在哪里。
  const current = entryKeyOf(form.value.node_id, form.value.group_id);
  if (selected.value && current && !rows.some((row) => row.key === current)) {
    const node = options.value.nodes.find(
      (n) => String(n.id) === form.value.node_id,
    );
    rows.push({
      key: current,
      label: [
        node ? String(node.name) : form.value.node_id,
        names.get(form.value.group_id) || form.value.group_id,
      ].join(" · "),
      nodeID: form.value.node_id,
      groupID: form.value.group_id,
    });
  }
  return rows;
});
const entryKey = computed({
  get: () => entryKeyOf(form.value.node_id, form.value.group_id),
  set: (value: string) => {
    const row = entryOptions.value.find((item) => item.key === value);
    form.value.node_id = row?.nodeID || "";
    form.value.group_id = row?.groupID || "";
  },
});
const exitGroups = computed(() => options.value.groups.filter(isExitGroup));
const chainChoices = computed(() =>
  options.value.groups.filter(
    (g) => isPhysicalExitGroup(g) && g.id !== selected.value?.id,
  ),
);
const selectedExitGroup = computed(() =>
  options.value.groups.find((g) => g.id === form.value.exit_group_id),
);
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
function openAdvanced(row: Row) {
  advancedTarget.value = row;
  formError.value = "";
}
async function advancedSaved(group: Row) {
  advancedTarget.value = null;
  await load();
  highlight(group.id);
}
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
  generatedPassword.value = "";
  generatedUsername.value = "";
  passwordCopied.value = false;
  tokenExpiry.value = "";
  tokenName.value = "";
  form.value = {
    group_type: String(row?.type || ""),
    direct_policy: String(row?.direct_policy || (row ? "allow" : "forbid")),
    chain_group_ids: [...((row?.chain_group_ids as string[]) || ["", ""])],
    exit_group_id: String(row?.exit_group_id || ""),
    exit_id: String(row?.exit_id || "auto"),
    proxy_accept: String((row?.proxy_protocol as Row)?.accept || "off"),
    proxy_send: String((row?.proxy_protocol as Row)?.send || "off"),
    trusted_cidrs: (
      ((row?.proxy_protocol as Row)?.trusted_cidrs as string[]) || []
    ).join(","),
    name: String(row?.name || ""),
    username: String(row?.username || ""),
    role: String(row?.role || "user"),
    identity_group_id: String(
      (resource === "identity-groups" ? row?.id : row?.identity_group_id) || "",
    ),
    node_id: String(row?.node_id || ""),
    group_id: String(row?.group_id || ""),
    network: String(row?.network || "tcp"),
    transport: String(row?.transport || "direct"),
    listen: String(row?.listen || ""),
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
    identity_group_ids: (row?.identity_group_ids as string[]) || [],
    group_ids: [],
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
    if (resource === "nodes")
      options.value.groups = (await choices("/groups")).filter(
        (g) => g.type !== "chain_exit",
      );
    if (resource === "groups") {
      [options.value.identityGroups, options.value.groups] = await Promise.all([
        choices("/identity-groups"),
        choices("/groups"),
      ]);
    }
    if (resource === "users")
      options.value.identityGroups = await choices("/identity-groups");
  } catch (e) {
    formError.value = errorText(e);
  }
}
function payload(): Row {
  const f = form.value;
  if (resource === "users")
    return selected.value
      ? { identity_group_id: f.identity_group_id }
      : {
          username: f.username,
          role: f.role,
          identity_group_id: f.identity_group_id,
        };
  if (resource === "identity-groups")
    return { id: f.identity_group_id, name: f.name };
  if (resource === "groups")
    return {
      name: f.name,
      type: f.group_type,
      direct_policy: formIsEntry.value ? f.direct_policy : "",
      chain_group_ids: f.group_type === "chain_exit" ? f.chain_group_ids : [],
      ...(selected.value?.advanced
        ? { advanced: selected.value.advanced }
        : {}),
      identity_group_ids: f.identity_group_ids,
      blocked_protocols: selected.value?.blocked_protocols || [],
      disabled_networks: selected.value?.disabled_networks || [],
      disabled_transports: selected.value?.disabled_transports || [],
      multiplier: f.multiplier,
      port_min: f.port_min,
      port_max: f.port_max,
      max_rules: f.max_rules,
    };
  if (resource === "nodes") return { name: f.name, group_ids: f.group_ids };
  return {
    exit_group_id: f.exit_group_id,
    exit_id: f.exit_group_id
      ? selectedExitGroup.value?.type === "chain_exit"
        ? "auto"
        : f.exit_id
      : "",
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
      : f.transport === "direct-tls"
        ? f.server_name
          ? { tunnel: { server_name: f.server_name } }
          : {}
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
        : resource === "users" && selected.value
          ? `/users/${encodeURIComponent(String(selected.value.id))}/identity-group`
          : `/${resource}${selected.value ? "/" + encodeURIComponent(String(selected.value.id)) : ""}`;
    const result = await api<{
      id?: string;
      token?: string;
      expires_at?: string;
      username?: string;
      initial_password?: string;
    }>(path, selected.value ? "PUT" : "POST", data);
    if (resource === "nodes") {
      token.value = String(result.token || "");
      tokenExpiry.value = String(result.expires_at || "");
      tokenName.value = String(form.value.name || "");
      initial.value = JSON.stringify(form.value);
    } else if (resource === "users" && !selected.value) {
      generatedPassword.value = String(result.initial_password || "");
      generatedUsername.value = String(result.username || form.value.username);
      passwordCopied.value = false;
      initial.value = JSON.stringify(form.value);
      await load();
      highlight(result.id);
    } else if (resource === "users") {
      editing.value = false;
      notice("用户的身份用户组已更新。");
      await load();
      highlight(selected.value?.id);
    } else {
      editing.value = false;
      notice(
        resource === "identity-groups"
          ? selected.value
            ? "身份用户组已更新，用户和设备组关联已同步。"
            : "身份用户组已创建，可在用户管理中分配。"
          : "已保存。转发规则需等待节点应用回执。",
      );
      await load();
      highlight(result.id);
    }
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}

async function copyGeneratedPassword() {
  try {
    await navigator.clipboard.writeText(generatedPassword.value);
    passwordCopied.value = true;
  } catch {
    notice("复制失败，请手动选中初始密码。");
  }
}

function closeEditing() {
  generatedPassword.value = "";
  generatedUsername.value = "";
  passwordCopied.value = false;
  editing.value = false;
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

function openPasswordReset(row: Row) {
  passwordResetTarget.value = row;
  resetPasswordSecret.value = "";
  resetPasswordCopied.value = false;
  formError.value = "";
}

async function resetUserPassword() {
  if (!passwordResetTarget.value || busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    const result = await api<{ password: string }>(
      `/users/${encodeURIComponent(String(passwordResetTarget.value.id))}/reset-password`,
      "POST",
      {},
    );
    resetPasswordSecret.value = result.password;
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}

async function copyResetPassword() {
  try {
    await navigator.clipboard.writeText(resetPasswordSecret.value);
    resetPasswordCopied.value = true;
  } catch {
    notice("复制失败，请手动选中新密码。");
  }
}

function closePasswordReset() {
  passwordResetTarget.value = null;
  resetPasswordSecret.value = "";
  resetPasswordCopied.value = false;
}
async function remove() {
  if (!deleting.value || busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    const deletingIdentityGroup = resource === "identity-groups";
    await api(
      deletingIdentityGroup
        ? `/identity-groups/${encodeURIComponent(String(deleting.value.id))}`
        : `/${resource}/${encodeURIComponent(String(deleting.value.id))}?version=${Number(deleting.value.version)}`,
      "DELETE",
    );
    deleting.value = null;
    notice(
      deletingIdentityGroup
        ? "身份用户组已删除。"
        : resource === "groups"
          ? "设备组已删除，设备归属和组授权已解除。"
          : "删除请求已提交，端口释放以节点确认解绑为准。",
    );
    if (rows.value.length === 1 && page.value > 1) {
      query(page.value - 1);
      return;
    }
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

// 设备组的固定接入密钥：点「接入设备」时才按组去取。它不进 GET /groups 的
// 列表响应 —— 那个接口普通用户也能调，而密钥只有管理员该看到。
async function onboardGroup(group: Row) {
  onboardTarget.value = group;
  joinKey.value = "";
  confirmRotate.value = false;
  formError.value = "";
  try {
    const result = await api<{ join_key: string }>(
      `/groups/${encodeURIComponent(String(group.id))}/join-key`,
    );
    joinKey.value = result.join_key;
  } catch (e) {
    formError.value = errorText(e);
  }
}
// 轮换不可逆：已经分发到各处的命令会立刻失效，所以要点两次。
async function rotateJoinKey() {
  const group = onboardTarget.value;
  if (!group || busy.value) return;
  if (!confirmRotate.value) {
    confirmRotate.value = true;
    return;
  }
  busy.value = true;
  formError.value = "";
  try {
    const result = await api<{ join_key: string }>(
      `/groups/${encodeURIComponent(String(group.id))}/join-key`,
      "POST",
      {},
    );
    joinKey.value = result.join_key;
    confirmRotate.value = false;
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
function value(v: unknown, column: string) {
  if (column === "type")
    return groupTypes.find((t) => t.value === v)?.label || "未分类（旧组）";
  if (
    ["blocked_protocols", "disabled_networks", "disabled_transports"].includes(
      column,
    )
  ) {
    const items = (v as string[] | null) || [];
    return items.length
      ? items
          .map((p) =>
            column === "disabled_transports"
              ? groupTransports.find((t) => t.value === p)?.label || p
              : p.replace(/^app:/, "").toUpperCase(),
          )
          .join("、")
      : "无";
  }
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
        ? [
            "name",
            "type",
            "blocked_protocols",
            "multiplier",
            "port_min",
            "port_max",
          ]
        : resource === "identity-groups"
          ? ["name", "id", "user_count", "device_group_count"]
          : resource === "users"
            ? [
                "username",
                "role",
                "identity_group_name",
                "identity_group_id",
                "disabled",
              ]
            : // 审计的列过去是从首行的对象键里取的，而 Go 的 JSON 编码会把 map 的键
              // 按字典序排 —— 于是表头冒出 action / id / user_id 这些原始英文键，
              // 顺序也随字段增删而变。这里写死，和时间一样只是展示口径。
              resource === "audit"
              ? ["created_at", "action", "target", "user_id"]
              : Object.keys(rows.value[0] || {}).slice(0, 6),
);
const labels: Record<string, string> = {
  name: "名称",
  type: "设备类型",
  transport: "转发方式",
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
  disabled_networks: "禁用网络协议",
  disabled_transports: "禁用转发方式",
  multiplier: "流量倍率",
  port_min: "起始端口",
  port_max: "结束端口",
  username: "用户名",
  role: "角色",
  identity_group_name: "身份用户组",
  identity_group_id: "身份用户组 ID",
  user_count: "用户数",
  device_group_count: "授权设备组数",
  disabled: "停用",
  created_at: "时间",
  action: "操作",
  user_id: "操作者",
  id: resource === "identity-groups" ? "身份用户组 ID" : "ID",
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
              : resource === "identity-groups"
                ? "新建身份用户组后，在用户管理中分配。同组用户共享已授权的设备组。"
                : resource === "users"
                  ? "通过身份用户组 ID 分配设备组访问权限，角色决定后台管理权限。"
                  : resource === "groups"
                    ? "将设备组授权给身份用户组，该身份组内的用户即可访问。"
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
                  (['groups', 'identity-groups', 'users', 'nodes'].includes(
                    resource,
                  ) &&
                    canManage)
                "
              >
                操作
              </th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="row in rows"
              :key="String(row.id)"
              :class="{ 'sweep-in': highlighted === String(row.id) }"
            >
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
                  (['groups', 'identity-groups', 'users', 'nodes'].includes(
                    resource,
                  ) &&
                    canManage)
                "
                data-label="操作"
              >
                <div class="toolbar">
                  <button
                    v-if="resource === 'groups' && row.type !== 'chain_exit'"
                    @click="onboardGroup(row)"
                  >
                    接入设备
                  </button>
                  <button
                    v-if="resource === 'groups'"
                    @click="openAdvanced(row)"
                  >
                    高级设置
                  </button>
                  <button v-if="resource === 'rules'" @click="diagnose(row)">
                    诊断</button
                  ><button
                    v-if="resource === 'rules'"
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
                    v-if="['rules', 'groups'].includes(resource)"
                    class="danger"
                    @click="
                      deleting = row;
                      formError = '';
                    "
                  >
                    删除
                  </button>
                  <button v-if="resource === 'users'" @click="open(row)">
                    身份组
                  </button>
                  <button
                    v-if="resource === 'users'"
                    @click="openPasswordReset(row)"
                  >
                    重置密码
                  </button>
                  <button
                    v-if="resource === 'users'"
                    @click="openUserTokens(row)"
                  >
                    API 凭据
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
                  <button
                    v-if="resource === 'identity-groups'"
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
      v-if="tokenUser"
      :title="`${tokenUser.username} 的 API 凭据`"
      :busy="tokenBusy"
      @close="tokenUser = null"
    >
      <p v-if="formError" class="error" role="alert">{{ formError }}</p>
      <template v-if="issuedSecret">
        <p class="warning">
          请立刻把这串密钥交给 {{ tokenUser.username }}。面板只存摘要，关闭之后
          再也无法查看 —— 弄丢了只能重置。
        </p>
        <label v-if="issuedFor"
          >{{ issuedFor
          }}<textarea
            aria-label="API Token 密钥"
            :value="issuedSecret"
            readonly
            rows="3"
          />
        </label>
        <div class="form-actions">
          <button type="button" class="primary" @click="issuedSecret = ''">
            已复制，继续管理
          </button>
        </div>
      </template>
      <template v-else>
        <p class="muted small">
          凭据只能访问持有人自己的资源与设备地址接口，不具有管理员权限。
          有效期可以是有限时长或永久；永久凭据泄露后风险一直存在，用完请撤销。
        </p>
        <div v-if="userTokens.length" class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>密钥前缀</th>
                <th>有效期</th>
                <th>最近使用</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="t in userTokens" :key="t.id">
                <td data-label="名称">{{ t.name }}</td>
                <td data-label="密钥前缀">
                  <code>{{ t.prefix || "—" }}</code>
                </td>
                <td data-label="有效期">
                  {{ t.permanent ? "永久有效" : formatDateTime(t.expires_at) }}
                </td>
                <td data-label="最近使用">
                  {{
                    t.last_used_at ? formatDateTime(t.last_used_at) : "尚未使用"
                  }}
                </td>
                <td data-label="操作">
                  <button :disabled="tokenBusy" @click="resetUserToken(t)">
                    重置</button
                  ><button
                    class="danger"
                    :disabled="tokenBusy"
                    @click="revokeUserToken(t)"
                  >
                    撤销
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="empty">这个账号还没有 API 凭据。</p>
        <form @submit.prevent="issueUserToken">
          <label
            >凭据名称<input
              v-model="issueTokenName"
              required
              maxlength="190"
              placeholder="例如：探针脚本" /></label
          ><label class="check"
            ><input
              v-model="issuePermanent"
              type="checkbox"
            />永久有效（不过期）</label
          ><label v-if="!issuePermanent"
            >有效天数<input
              v-model.number="issueTokenDays"
              type="number"
              min="1"
              max="364"
              required
          /></label>
          <div class="form-actions">
            <button
              class="primary"
              type="submit"
              :disabled="tokenBusy"
              :data-busy="String(tokenBusy)"
              :aria-busy="tokenBusy"
            >
              生成凭据
            </button>
          </div>
        </form>
      </template>
    </Modal>
    <Modal
      v-if="editing"
      :title="
        resource === 'nodes'
          ? '一次性节点接入凭据'
          : resource === 'users' && selected
            ? '修改身份用户组 · ' + selected.username
            : (selected ? '编辑' : '新增') + titles[resource]
      "
      :busy="busy"
      :dirty="dirty"
      @close="closeEditing"
      ><form @submit.prevent="save">
        <p v-if="formError" class="error" role="alert">{{ formError }}</p>
        <template v-if="token"
          ><p class="muted small">
            下面的接入命令只展示这一次，关闭后无法再次查看。不要发送给未授权人员。
          </p>
          <OnboardCommand
            :command="entryCommand"
            :manual="entryManual"
            :expires-at="tokenExpiry"
          />
          <div class="form-actions">
            <button type="button" @click="closeEditing">关闭</button>
          </div></template
        ><template v-else-if="generatedPassword"
          ><p class="warning">
            {{ generatedUsername }} 的初始密码只展示这一次，关闭后无法再次查看。
          </p>
          <label
            >初始密码<textarea
              aria-label="初始密码"
              :value="generatedPassword"
              readonly
              rows="2"
            />
          </label>
          <div class="form-actions">
            <button
              type="button"
              class="primary"
              @click="copyGeneratedPassword"
            >
              {{ passwordCopied ? "已复制" : "复制初始密码" }}
            </button>
            <button type="button" @click="closeEditing">关闭</button>
          </div></template
        ><template v-else
          ><label v-if="resource !== 'users'"
            >名称<input v-model="form.name" required maxlength="100"
          /></label>
          <template v-if="resource === 'identity-groups'">
            <label
              >身份用户组 ID<input
                v-model="form.identity_group_id"
                aria-label="身份用户组 ID"
                required
                maxlength="64"
                pattern="[A-Za-z0-9_\-]{1,64}"
                placeholder="例如 1001 或 vip_users"
                autocomplete="off"
                spellcheck="false"
            /></label>
            <p class="muted small">
              手动设置唯一 ID，支持 1–64 位字母、数字、下划线和短横线。
            </p>
            <p v-if="selected" class="muted small">
              修改 ID 后，已关联的用户和设备组会同步更新，原有访问权限保留。
            </p>
          </template>
          <template v-if="resource === 'users'"
            ><label
              >用户名<input
                v-model="form.username"
                autocomplete="off"
                required
                :disabled="!!selected"
                maxlength="64" /></label
            ><label
              >角色<Select
                v-model="form.role"
                aria-label="角色"
                :disabled="!!selected"
              >
                <option value="user">普通用户</option>
                <option value="admin">管理员</option>
              </Select></label
            ><label
              >身份用户组 ID<Select
                v-model="form.identity_group_id"
                aria-label="身份用户组 ID"
                required
              >
                <option value="" disabled>选择身份用户组</option>
                <option
                  v-for="identity in options.identityGroups"
                  :key="String(identity.id)"
                  :value="identity.id"
                >
                  {{ identity.name }} · {{ identity.id }}
                </option>
              </Select></label
            >
            <p v-if="!options.identityGroups.length" class="muted small">
              请先在侧栏“身份用户组”中新建身份组，再为用户分配。
            </p></template
          ><template v-if="resource === 'rules'"
            ><label
              >入口服务器<Select
                v-model="entryKey"
                aria-label="入口服务器"
                required
                :disabled="!!selected"
              >
                <option value="" disabled>选择服务器</option>
                <option
                  v-for="row in entryOptions"
                  :key="row.key"
                  :value="row.key"
                >
                  {{ row.label }}
                </option>
              </Select></label
            >
            <p v-if="!entryOptions.length" class="muted small">
              没有可选的入口服务器：入口是「一台机器 + 它所属的入口设备组」，
              你名下还没有这样的组合。机器要先加入你被授权的入口设备组（管理员在
              「设备组」页操作），规则才能落在它上面。
            </p>
            <label
              >出口选择<Select
                v-model="form.exit_group_id"
                aria-label="出口选择"
              >
                <option value="">直接转发或手工配置隧道</option>
                <option
                  v-for="g in exitGroups"
                  :key="String(g.id)"
                  :value="g.id"
                >
                  {{ g.name }} · 出口倍率 {{ g.multiplier }}
                </option>
              </Select></label
            >
            <label
              v-if="
                form.exit_group_id && selectedExitGroup?.type !== 'chain_exit'
              "
              >出口节点<Select v-model="form.exit_id" aria-label="出口节点">
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
              </Select></label
            >
            <p v-if="form.exit_group_id" class="small muted">
              {{
                selectedExitGroup?.type === "chain_exit"
                  ? "按配置顺序经过各出口组，每一跳自动选择授权且在线的节点。"
                  : "自动选择该组内授权且在线的出口。"
              }}流量按入口组倍率 × 出口组倍率结算。
            </p>
            <div class="form-grid">
              <label
                >传输层<Select v-model="form.network" :disabled="!!selected">
                  <option value="tcp">TCP</option>
                  <option value="udp">UDP</option>
                </Select></label
              ><label
                >转发方式<Select v-model="form.transport" aria-label="转发方式">
                  <option
                    v-for="t in transports"
                    :key="t.value"
                    :value="t.value"
                  >
                    {{ t.label }}
                  </option>
                </Select></label
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
                placeholder="留空则从设备组端口范围自动分配" /></label
            ><label
              >目标地址<input
                v-model="form.target"
                required
                placeholder="127.0.0.1:8080" /></label
            ><template
              v-if="form.transport === 'direct-tls' && !form.exit_group_id"
              ><label
                >TLS 校验名<input
                  v-model="form.server_name"
                  placeholder="留空则用目标地址的主机部分" /></label></template
            ><template
              v-else-if="form.transport !== 'direct' && !form.exit_group_id"
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
                  ><Select
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
                  </Select>
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
                  >接收<Select v-model="form.proxy_accept" aria-label="接收">
                    <option value="off">关闭</option>
                    <option value="v1">v1</option>
                    <option value="v2">v2</option>
                  </Select></label
                ><label
                  >发送<Select v-model="form.proxy_send" aria-label="发送">
                    <option value="off">关闭</option>
                    <option value="v1">v1</option>
                    <option value="v2">v2</option>
                  </Select></label
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
            ><label
              >设备类型<Select
                v-model="form.group_type"
                aria-label="设备类型"
                :required="!selected"
                :disabled="!!selected?.type"
              >
                <option value="" disabled>请选择设备类型</option>
                <option v-for="t in groupTypes" :key="t.value" :value="t.value">
                  {{ t.label }}
                </option>
              </Select></label
            >
            <p v-if="form.group_type" class="small muted">
              {{
                groupTypes.find((t) => t.value === form.group_type)?.description
              }}
              类型创建后不可修改。
            </p>
            <p v-else-if="selected" class="small muted">
              此设备组来自旧版，可继续使用。分类前请确认现有设备、规则和出口符合所选类型。
            </p>
            <fieldset v-if="form.group_type === 'chain_exit'">
              <legend>链式出口配置</legend>
              <p class="small muted">
                按经过的顺序选择 2–3 个出口设备组，不能重复。
              </p>
              <label v-for="(_, index) in form.chain_group_ids" :key="index"
                >第 {{ index + 1 }} 跳出口组
                <Select
                  v-model="form.chain_group_ids[index]"
                  :aria-label="`第 ${index + 1} 跳出口组`"
                  required
                >
                  <option value="" disabled>选择出口设备组</option>
                  <option
                    v-for="g in chainChoices"
                    :key="String(g.id)"
                    :value="g.id"
                    :disabled="
                      form.chain_group_ids.some(
                        (id, i) => i !== index && id === g.id,
                      )
                    "
                  >
                    {{ g.name }}
                  </option>
                </Select>
              </label>
              <div class="toolbar">
                <button
                  v-if="form.chain_group_ids.length < 3"
                  type="button"
                  @click="form.chain_group_ids.push('')"
                >
                  添加第 3 跳
                </button>
                <button
                  v-else
                  type="button"
                  @click="form.chain_group_ids.pop()"
                >
                  移除第 3 跳
                </button>
              </div>
              <p v-if="chainChoices.length < 2" class="small muted">
                请先创建至少两个出口设备组，再配置链式出口。
              </p>
            </fieldset>
            <template v-if="formIsEntry">
              <label
                >入口直出策略<Select
                  v-model="form.direct_policy"
                  aria-label="入口直出策略"
                >
                  <option value="forbid">禁止直接转发</option>
                  <option value="allow">可选直接转发</option>
                  <option value="force">强制直接转发</option>
                </Select></label
              >
            </template>
            <label v-if="formForwards"
              >流量倍率<input
                v-model="form.multiplier"
                required
                pattern="[0-9]+(\.[0-9]+)?"
            /></label>
            <label v-if="formIsEntry"
              >每用户规则上限（0 表示不限）<input
                v-model.number="form.max_rules"
                type="number"
                min="0"
                max="100000"
                required
            /></label>
            <div v-if="formIsEntry" class="form-grid">
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
            <fieldset class="identity-grants">
              <legend>授权身份用户组</legend>
              <p class="muted small">
                勾选身份用户组后，组内所有用户均获得此设备组的访问权限。
              </p>
              <p v-if="!options.identityGroups.length" class="muted small">
                请先在侧栏“身份用户组”中新建身份组。
              </p>
              <label
                v-for="identity in options.identityGroups"
                :key="String(identity.id)"
                class="check"
                ><input
                  v-model="form.identity_group_ids"
                  type="checkbox"
                  :value="identity.id"
                /><span>{{ identity.name }} · {{ identity.id }}</span></label
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
            <button
              class="primary"
              :disabled="busy"
              :data-busy="String(busy)"
              :aria-busy="busy"
            >
              保存
            </button>
          </div></template
        >
      </form></Modal
    ><GroupAdvanced
      v-if="advancedTarget"
      :group="advancedTarget"
      @close="advancedTarget = null"
      @saved="advancedSaved"
    /><Modal
      v-if="deleting"
      :title="
        resource === 'identity-groups'
          ? '删除身份用户组'
          : resource === 'groups'
            ? '删除设备组'
            : '删除转发规则'
      "
      :busy="busy"
      @close="deleting = null"
      ><p v-if="resource === 'identity-groups'">
        确认删除
        {{ deleting.name }}？仅未关联用户且未授权给设备组的身份组可以删除。
      </p>
      <p v-else-if="resource === 'groups'">
        确认删除 {{ deleting.name }}？删除后将解除该组的设备归属和用户授权，
        清理闲置出口，原接入命令将失效。设备和历史流量记录保留。
        若仍被转发规则或其他设备组引用，请先解除引用；已删除的规则须等待节点确认停止。
      </p>
      <p v-else>
        确认删除 {{ deleting.name }}？现有转发会在节点应用配置后停止。
      </p>
      <p v-if="formError" class="error" role="alert">{{ formError }}</p>
      <div class="form-actions">
        <button class="danger" :disabled="busy" @click="remove">
          确认删除
        </button>
      </div></Modal
    >
    <Modal
      v-if="passwordResetTarget"
      :title="`重置密码 · ${passwordResetTarget.username}`"
      :busy="busy"
      @close="closePasswordReset"
    >
      <p v-if="formError" class="error" role="alert">{{ formError }}</p>
      <template v-if="resetPasswordSecret">
        <p class="warning">
          新密码只展示这一次。该账号原有会话和 API Token 已全部撤销。
        </p>
        <label
          >新密码<textarea
            aria-label="重置后的新密码"
            :value="resetPasswordSecret"
            readonly
            rows="2"
          />
        </label>
        <div class="form-actions">
          <button type="button" class="primary" @click="copyResetPassword">
            {{ resetPasswordCopied ? "已复制" : "复制新密码" }}
          </button>
          <button type="button" @click="closePasswordReset">关闭</button>
        </div>
      </template>
      <template v-else>
        <p>确认重置 {{ passwordResetTarget.username }} 的登录密码？</p>
        <p class="muted small">
          系统会生成新密码，并立即撤销该账号的所有会话和 API Token。
        </p>
        <div class="form-actions">
          <button
            type="button"
            class="danger"
            :disabled="busy"
            @click="resetUserPassword"
          >
            确认重置
          </button>
        </div>
      </template>
    </Modal>
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
      <div class="form-actions">
        <button :disabled="busy" @click="changeStatus">确认修改状态</button>
      </div></Modal
    >
    <Modal
      v-if="onboardTarget"
      :title="`接入设备 · ${onboardTarget.name}`"
      :busy="busy"
      @close="
        onboardTarget = null;
        confirmRotate = false;
      "
      ><p v-if="formError" class="error" role="alert">{{ formError }}</p>
      <p v-else-if="!joinKey" class="empty">正在读取接入密钥…</p>
      <OnboardPanel v-else :access-key="joinKey" />
      <div v-if="joinKey" class="actions">
        <button
          type="button"
          class="danger"
          :disabled="busy"
          @click="rotateJoinKey"
        >
          {{
            confirmRotate ? "确认轮换，已分发的命令将立即失效" : "轮换接入密钥"
          }}
        </button>
        <button
          v-if="confirmRotate"
          type="button"
          :disabled="busy"
          @click="confirmRotate = false"
        >
          取消
        </button>
      </div>
      <div class="form-actions">
        <button type="button" @click="onboardTarget = null">关闭</button>
      </div></Modal
    >
  </section>
</template>
