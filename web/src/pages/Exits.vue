<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import Select from "../components/Select.vue";
import Modal from "../components/Modal.vue";
import OnboardPanel from "../components/OnboardPanel.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
import { isPhysicalExitGroup } from "../core/groups";
interface Exit {
  id: string;
  name: string;
  group_id: string;
  node_id: string;
  transport: string;
  tunnel: {
    endpoint: string;
    server_name: string;
    token?: string;
    mux?: boolean;
    reverse?: string;
    chain?: unknown[];
  };
  udp?: {
    endpoint: string;
    server_name: string;
    token?: string;
    allow_tcp_fallback?: boolean;
  } | null;
  weight: number;
  enabled: boolean;
  version: number;
  online: boolean;
}
interface Choice {
  id: string;
  name: string;
  type?: string;
  owner_id?: string;
}
const admin = computed(() => adminSite && state.user?.role === "admin"),
  userExit = computed(() => !!state.user && !admin.value),
  items = ref<Exit[]>([]),
  groups = ref<Choice[]>([]),
  nodes = ref<Choice[]>([]),
  form = ref<Exit | null>(null),
  error = ref(""),
  busy = ref(false),
  page = ref(1),
  total = ref(0);
const groupDialog = ref(false),
  groupName = ref("我的出口设备组"),
  groupBusy = ref(false),
  groupError = ref(""),
  groupAccessKey = ref(""),
  groupID = ref("");
async function all(path: string) {
  const out: Choice[] = [];
  for (let p = 1; ; p++) {
    const batch = await api<{ items: Choice[]; total: number }>(
      `${path}?page_size=100&page=${p}`,
    );
    out.push(...batch.items);
    if (!batch.items.length || out.length >= batch.total) return out;
  }
}
async function load() {
  error.value = "";
  try {
    const v = await api<{ items: Exit[]; total: number }>(
      `/exits?page=${page.value}`,
    );
    items.value = v.items;
    total.value = v.total;
  } catch (e) {
    error.value = errorText(e);
  }
}
async function open(v?: Exit) {
  error.value = "";
  try {
    [groups.value, nodes.value] = await Promise.all([
      all("/groups"),
      all("/nodes"),
    ]);
    groups.value = groups.value.filter(
      (g) =>
        isPhysicalExitGroup(g) &&
        (admin.value || g.owner_id === state.user?.id),
    );
    form.value = v
      ? (JSON.parse(JSON.stringify(v)) as Exit)
      : {
          id: "",
          name: "",
          group_id: "",
          node_id: "",
          transport: "tls",
          tunnel: { endpoint: "", server_name: "", token: "" },
          weight: 1,
          enabled: true,
          version: 0,
          online: false,
        };
  } catch (e) {
    error.value = errorText(e);
  }
}
async function save() {
  if (!form.value) return;
  busy.value = true;
  error.value = "";
  try {
    const base = admin.value ? "/exits" : "/my-exits";
    await api(
      base + (form.value.id ? "/" + encodeURIComponent(form.value.id) : ""),
      form.value.id ? "PUT" : "POST",
      form.value,
    );
    form.value = null;
    notice("出口已保存。规则会在入口节点下次同步时重新选择出口。");
    await load();
  } catch (e) {
    error.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
async function createMyExitGroup() {
  if (groupBusy.value) return;
  groupBusy.value = true;
  groupError.value = "";
  try {
    const group = await api<{ id: string }>("/my-exit-groups", "POST", {
      name: groupName.value.trim(),
      type: "exit",
      blocked_protocols: [],
      disabled_networks: [],
      disabled_transports: [],
      chain_group_ids: [],
      multiplier: "1",
      port_min: 10000,
      port_max: 60000,
      max_rules: 0,
    });
    groupID.value = group.id;
    groupAccessKey.value = (
      await api<{ join_key: string }>(
        `/groups/${encodeURIComponent(group.id)}/join-key`,
      )
    ).join_key;
    groupDialog.value = true;
    await load();
  } catch (e) {
    groupError.value = errorText(e);
  } finally {
    groupBusy.value = false;
  }
}
async function move(n: number) {
  page.value = n;
  await load();
}
onMounted(load);
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">ROUTING</p>
        <h1>单端出口</h1>
      </div>
      <div class="toolbar">
        <button @click="load">刷新</button
        ><button v-if="admin || userExit" class="primary" @click="open()">
          {{ admin ? "新增出口" : "绑定我的出口设备" }}
        </button>
        <button v-if="userExit" @click="createMyExitGroup">
          新增我的出口设备组
        </button>
      </div>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <section class="card">
      <p class="muted">
        规则可在授权出口组内按权重自动选择在线节点，也可指定出口。计费倍率为入口组
        × 出口组；在线状态依据节点心跳。
      </p>
      <p v-if="userExit" class="muted small">
        普通用户的出口只归自己使用，不计入额外费用。先创建出口设备组并执行接入命令，再绑定已上线的出口设备。
      </p>
      <p v-if="!items.length">暂无授权出口。</p>
      <div v-for="v in items" :key="v.id" class="task-row">
        <span
          >{{ v.name }} · {{ v.transport }} · 权重 {{ v.weight }} ·
          {{ v.enabled ? (v.online ? "在线" : "离线") : "已停用"
          }}{{ v.udp ? " · 原生 UDP 已配置" : "" }}</span
        ><button @click="open(v)">编辑出口</button>
      </div>
      <div class="toolbar">
        <button :disabled="page <= 1" @click="move(page - 1)">上一页</button
        ><span>{{ page }} · 共 {{ total }} 个</span
        ><button :disabled="page * 20 >= total" @click="move(page + 1)">
          下一页
        </button>
      </div>
    </section>
    <Modal
      v-if="form"
      :title="form.id ? '编辑出口' : '新增出口'"
      :busy="busy"
      :dirty="true"
      @close="form = null"
      ><form @submit.prevent="save">
        <p v-if="error" role="alert" class="error">{{ error }}</p>
        <label
          >出口名称<input v-model="form.name" required maxlength="100" /></label
        ><label
          >出口设备组<Select
            v-model="form.group_id"
            aria-label="出口设备组"
            required
          >
            <option value="" disabled>选择设备组</option>
            <option v-for="g in groups" :key="g.id" :value="g.id">
              {{ g.name }}
            </option>
          </Select></label
        ><label
          >出口服务器<Select
            v-model="form.node_id"
            aria-label="出口服务器"
            required
          >
            <option value="" disabled>选择服务器</option>
            <option v-for="n in nodes" :key="n.id" :value="n.id">
              {{ n.name }}
            </option>
          </Select></label
        >
        <label
          >出口承载<Select v-model="form.transport" aria-label="出口承载">
            <option v-for="t in ['tls', 'ws', 'wss', 'http']" :key="t">
              {{ t }}
            </option>
          </Select></label
        ><label>出口端点<input v-model="form.tunnel.endpoint" required /></label
        ><label>TLS 服务器名称<input v-model="form.tunnel.server_name" /></label
        ><label
          >出口凭据<input
            v-model="form.tunnel.token"
            type="password"
            autocomplete="new-password"
            :required="!form.id"
            placeholder="编辑时留空保留既有凭据"
        /></label>
        <label v-if="admin" class="check"
          ><input
            type="checkbox"
            :checked="!!form.udp"
            @change="
              form.udp = ($event.target as HTMLInputElement).checked
                ? {
                    endpoint: '',
                    server_name: form.tunnel.server_name,
                    token: '',
                    allow_tcp_fallback: false,
                  }
                : null
            "
          />启用原生 UDP 出口</label
        >
        <template v-if="admin && form.udp">
          <label
            >UDP 出口端点<input
              v-model="form.udp.endpoint"
              required
              placeholder="exit.example.com:9443"
          /></label>
          <label
            >UDP TLS 校验名称<input v-model="form.udp.server_name"
          /></label>
          <label
            >UDP 出口凭据<input
              v-model="form.udp.token"
              type="password"
              autocomplete="new-password"
              placeholder="与出口监听凭据一致；编辑时留空保留"
          /></label>
          <label class="check"
            ><input
              v-model="form.udp.allow_tcp_fallback"
              type="checkbox"
            />允许不支持原生 UDP 的节点使用 TCP 隧道</label
          >
          <p class="muted small">
            入口与出口支持原生 UDP 时使用 QUIC DATAGRAM。设备组高级设置中的 UDP
            over TCP 可强制使用 TCP 隧道。
          </p>
        </template>
        <label
          >选择权重<input
            v-model.number="form.weight"
            type="number"
            min="1"
            max="100"
            required /></label
        ><label class="check"
          ><input v-model="form.enabled" type="checkbox" />启用出口</label
        >
        <div class="form-actions">
          <button class="primary" :disabled="busy">保存出口</button>
        </div>
      </form></Modal
    >
    <Modal
      v-if="groupDialog"
      title="接入我的出口设备"
      :busy="groupBusy"
      @close="groupDialog = false"
    >
      <p v-if="groupError" class="error" role="alert">{{ groupError }}</p>
      <p class="muted small">
        设备组
        {{
          groupID
        }}
        已创建。把下面的命令复制到你的出口机器上，注册成功后回到本页绑定出口设备。
      </p>
      <OnboardPanel :access-key="groupAccessKey" />
      <div class="form-actions">
        <button type="button" @click="groupDialog = false">完成</button>
      </div>
    </Modal>
  </section>
</template>
