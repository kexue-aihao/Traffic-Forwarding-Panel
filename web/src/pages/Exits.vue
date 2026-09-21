<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import Modal from "../components/Modal.vue";
import { api, errorText } from "../core/api";
import { adminSite, state, notice } from "../core/state";
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
  weight: number;
  enabled: boolean;
  version: number;
  online: boolean;
}
interface Choice {
  id: string;
  name: string;
}
const admin = computed(() => adminSite && state.user?.role === "admin"),
  items = ref<Exit[]>([]),
  groups = ref<Choice[]>([]),
  nodes = ref<Choice[]>([]),
  form = ref<Exit | null>(null),
  error = ref(""),
  busy = ref(false),
  page = ref(1),
  total = ref(0);
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
    await api(
      "/exits" + (form.value.id ? "/" + encodeURIComponent(form.value.id) : ""),
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
async function move(n: number) {
  page.value = n;
  await load();
}
onMounted(load);
</script>
<template>
  <section class="page-header">
    <div>
      <p class="eyebrow">ROUTING</p>
      <h1>出口管理</h1>
    </div>
    <div class="toolbar">
      <button @click="load">刷新</button
      ><button v-if="admin" class="primary" @click="open()">新增出口</button>
    </div>
  </section>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <section class="card">
    <p class="muted">
      规则可在授权出口组内按权重自动选择在线节点，也可指定出口。计费倍率为入口组
      × 出口组；在线状态依据节点心跳。
    </p>
    <p v-if="!items.length">暂无授权出口。</p>
    <div v-for="v in items" :key="v.id" class="task-row">
      <span
        >{{ v.name }} · {{ v.transport }} · 权重 {{ v.weight }} ·
        {{ v.enabled ? (v.online ? "在线" : "离线") : "已停用" }}</span
      ><button v-if="admin" @click="open(v)">编辑出口</button>
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
        >出口设备组<select v-model="form.group_id" aria-label="出口设备组" required>
          <option value="" disabled>选择设备组</option>
          <option v-for="g in groups" :key="g.id" :value="g.id">
            {{ g.name }}
          </option>
        </select></label
      ><label
        >出口服务器<select v-model="form.node_id" aria-label="出口服务器" required>
          <option value="" disabled>选择服务器</option>
          <option v-for="n in nodes" :key="n.id" :value="n.id">
            {{ n.name }}
          </option>
        </select></label
      >
      <label
        >出口承载<select v-model="form.transport" aria-label="出口承载">
          <option v-for="t in ['tls', 'ws', 'wss', 'http']" :key="t">
            {{ t }}
          </option>
        </select></label
      ><label>出口端点<input v-model="form.tunnel.endpoint" required /></label
      ><label>TLS 服务器名称<input v-model="form.tunnel.server_name" /></label
      ><label
        >出口凭据<input
          v-model="form.tunnel.token"
          type="password"
          autocomplete="new-password"
          :required="!form.id"
          placeholder="编辑时留空保留既有凭据" /></label
      ><label
        >选择权重<input
          v-model.number="form.weight"
          type="number"
          min="1"
          max="100"
          required /></label
      ><label class="check"
        ><input v-model="form.enabled" type="checkbox" />启用出口</label
      ><button class="primary" :disabled="busy">保存出口</button>
    </form></Modal
  >
</template>
