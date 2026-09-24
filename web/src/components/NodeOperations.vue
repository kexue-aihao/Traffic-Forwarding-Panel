<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import Modal from "./Modal.vue";
import { api, errorText } from "../core/api";
import { formatDateTime } from "../core/format";
const props = defineProps<{node: Record<string, unknown>}>();
const emit = defineEmits<{close: []}>();
interface Operation {id: string; kind: string; status: string; created_at: string; expires_at: string; error?: string}
const operations = ref<Operation[]>([]);
const error = ref("");
const busy = ref(false);
const password = ref("");
const access = ref("");
const accessExpiry = ref(0);
const authorized = computed(() => !!access.value);
const capabilities = computed(() => (props.node.capabilities as string[]) || []);
const nodePath = `/nodes/${encodeURIComponent(String(props.node.id))}`;
const release = ref({url: "", sha256: "", signature: "", version: "", os: String(props.node.os || "linux"), arch: String(props.node.arch || "amd64")});
const output = ref("");
const command = ref("");
const connected = ref(false);
const executing = ref(false);
const terminalID = ref("");
const audit = ref<{command: string; created_at: string}[]>([]);
let socket: WebSocket | undefined;
let timer: ReturnType<typeof setInterval> | undefined;
let alive = true;
let terminalKey = crypto.randomUUID();
let upgradeKey = crypto.randomUUID();
let upgradeIntent = "";
const labels: Record<string, string> = {pending: "等待节点", running: "执行中", staged: "等待新版本确认", succeeded: "已完成", failed: "失败", rolled_back: "已恢复旧版本", cancelled: "已取消", expired: "已过期"};
async function refresh() {
  try {
    const result = await api<{items: Operation[]}>(nodePath + "/operations");
    if (alive) operations.value = result.items;
    if (Date.now() >= accessExpiry.value) access.value = "";
  } catch (e) { if (alive) error.value = errorText(e); }
}
async function authorize() {
  busy.value = true; error.value = "";
  try {
    const result = await api<{token: string; expires_at: string}>(nodePath + "/operation-access", "POST", {password: password.value});
    access.value = result.token; accessExpiry.value = Date.parse(result.expires_at);
  } catch (e) { error.value = errorText(e); }
  finally {password.value = ""; busy.value = false;}
}
function append(text: string) { output.value = (output.value + text).slice(-131072); }
async function terminal() {
  busy.value = true; error.value = "";
  try {
    const op = await api<Operation>(nodePath + "/terminal", "POST", {access_token: access.value, idempotency_key: terminalKey});
    if (!["pending", "running"].includes(op.status)) { terminalKey = crypto.randomUUID(); throw new Error("该终端任务已结束，请重新创建。"); }
    terminalID.value = op.id;
    output.value = "正在连接节点…\n";
    const url = new URL(`/api/v1/node-operations/${encodeURIComponent(op.id)}/terminal`, location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    socket = new WebSocket(url);
    socket.onopen = () => {connected.value = true;};
    socket.onmessage = event => {
      try {
        const msg = JSON.parse(event.data) as {type: string; data?: string; code?: number};
        if (msg.type === "output") append(msg.data || "");
        if (msg.type === "exit") {executing.value = false; append(`\n[命令${msg.code ? "失败或达到执行限制" : "完成"}]\n`);}
      } catch { error.value = "终端响应格式异常"; socket?.close(); }
    };
    socket.onerror = () => {error.value = "终端连接失败，请检查节点状态和授权有效期。";};
    socket.onclose = () => { connected.value = false; executing.value = false; terminalID.value = ""; terminalKey = crypto.randomUUID(); append("\n[终端已断开]\n"); if (alive) void refresh(); };
    await refresh();
  } catch (e) { error.value = errorText(e); }
  finally {busy.value = false;}
}
function sendCommand() {
  if (!socket || socket.readyState !== WebSocket.OPEN || !command.value.trim() || executing.value) return;
  append(`\n$ ${command.value}\n`);
  socket.send(JSON.stringify({type: "command", command: command.value}));
  command.value = ""; executing.value = true;
}
async function cancel(op: Operation) {
  busy.value = true; error.value = "";
  try {await api(`/node-operations/${encodeURIComponent(op.id)}/cancel`, "POST", {}); if (op.id === terminalID.value) socket?.close(); await refresh();}
  catch (e) {error.value = errorText(e);}
  finally {busy.value = false;}
}
async function upgrade() {
  busy.value = true; error.value = "";
  const intent = JSON.stringify(release.value);
  if (intent !== upgradeIntent) {upgradeKey = crypto.randomUUID(); upgradeIntent = intent;}
  try {await api(nodePath + "/upgrade", "POST", {access_token: access.value, idempotency_key: upgradeKey, upgrade: release.value}); await refresh();}
  catch (e) {error.value = errorText(e);}
  finally {busy.value = false;}
}
async function commands(op: Operation) {
  try { audit.value = (await api<{items: typeof audit.value}>(`/node-operations/${encodeURIComponent(op.id)}/commands`)).items; }
  catch (e) {error.value = errorText(e);}
}
onMounted(() => {void refresh(); timer = setInterval(() => {void refresh();}, 5000);});
onUnmounted(() => {alive = false; clearInterval(timer); socket?.close(); access.value = "";});
</script>
<template>
  <Modal :title="`节点运维 · ${node.name}`" :busy="busy" @close="emit('close')">
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form v-if="!authorized" @submit.prevent="authorize">
      <p class="muted">验证管理员密码后，可在此节点执行运维操作。授权有效期 15 分钟。</p>
      <label>管理员密码<input v-model="password" type="password" autocomplete="current-password" required /></label>
      <button class="primary" :disabled="busy">验证运维权限</button>
    </form>
    <p v-else class="muted">运维授权至 {{ formatDateTime(new Date(accessExpiry).toISOString()) }}</p>
    <section class="card">
      <h2>远程命令终端</h2>
      <p class="muted small">以节点服务账号执行，每条命令独立运行，最长 60 秒、最多输出 1 MiB。命令会保留审计，请避免在命令中填写密钥。会话最长 10 分钟；交互式全屏程序暂不支持。</p>
      <p v-if="!capabilities.includes('terminal-v1')" class="muted">节点尚未启用远程终端。</p>
      <button :disabled="!authorized || busy || !!terminalID || !capabilities.includes('terminal-v1')" @click="terminal">创建终端</button>
      <button v-if="terminalID" @click="socket?.close()">断开终端</button>
      <pre v-if="output" class="terminal-output" role="log" aria-label="终端输出">{{ output }}</pre>
      <form v-if="terminalID" @submit.prevent="sendCommand">
        <label>命令<textarea v-model="command" rows="2" maxlength="4096" :disabled="!connected || executing" /></label>
        <button :disabled="!connected || executing || !command.trim()">{{ executing ? '执行中…' : '执行命令' }}</button>
      </form>
    </section>
    <section class="card">
      <h2>节点升级</h2>
      <p class="muted small">升级会中断现有连接。新进程未在 60 秒内完成配置同步时恢复旧版本。</p>
      <p v-if="!capabilities.includes('upgrade-v1')" class="muted">节点尚未配置升级签名公钥。</p>
      <form @submit.prevent="upgrade">
        <label>发布版本<input v-model="release.version" required maxlength="64" /></label>
        <label>下载地址<input v-model="release.url" type="url" required placeholder="https://releases.example.com/agent" /></label>
        <label>SHA256<input v-model="release.sha256" required pattern="[a-fA-F0-9]{64}" /></label>
        <label>发布签名<textarea v-model="release.signature" rows="2" required /></label>
        <p class="small muted">目标平台：{{ release.os }} / {{ release.arch }}</p>
        <button class="primary" :disabled="!authorized || busy || !capabilities.includes('upgrade-v1')">校验并升级节点</button>
      </form>
    </section>
    <section class="card">
      <h2>最近运维任务</h2>
      <p v-if="!operations.length" class="muted">暂无任务。</p>
      <article v-for="op in operations" :key="op.id" class="operation-item">
        <p>{{ ({ terminal: '终端命令', shell: 'WebSSH', uninstall: '卸载设备', upgrade: '升级' })[op.kind] || op.kind }} · {{ labels[op.status] || op.status }} · {{ formatDateTime(op.created_at) }}</p>
        <p v-if="op.error" class="error">{{ op.error }}</p>
        <button v-if="['pending', 'running'].includes(op.status) && !(op.kind === 'uninstall' && op.status === 'running')" :disabled="busy" @click="cancel(op)">取消任务</button>
        <button v-if="op.kind === 'terminal'" @click="commands(op)">查看命令审计</button>
      </article>
      <pre v-for="(entry, index) in audit" :key="index" class="terminal-output">{{ formatDateTime(entry.created_at) }}
{{ entry.command }}</pre>
    </section>
  </Modal>
</template>
