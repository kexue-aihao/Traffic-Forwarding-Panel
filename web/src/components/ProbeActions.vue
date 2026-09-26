<script setup lang="ts">
import { nextTick, onMounted, onUnmounted, ref } from "vue";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import Modal from "./Modal.vue";
import { api, errorText } from "../core/api";
const props = defineProps<{
  node: { id: string; name: string; capabilities?: string[] };
  mode: "shell" | "uninstall";
}>();
const emit = defineEmits<{ close: []; removed: [] }>();
interface Operation {
  id: string;
  kind: string;
  status: string;
  error?: string;
}
const password = ref("");
const error = ref("");
const busy = ref(false);
const connected = ref(false);
const op = ref<Operation>();
const terminalHost = ref<HTMLDivElement>();
const labels: Record<string, string> = {
  pending: "等待设备接收",
  running: "设备正在执行，等待结果",
  succeeded: "卸载已完成",
  failed: "执行失败，可检查设备后重试",
  expired: "设备未及时接收，任务已过期",
  cancelled: "已取消",
};
const supported = props.node.capabilities?.includes(props.mode + "-v1");
const path = `/nodes/${encodeURIComponent(props.node.id)}`;
let key = crypto.randomUUID();
let socket: WebSocket | undefined;
let terminal: Terminal | undefined;
let fit: FitAddon | undefined;
let resize: ResizeObserver | undefined;
let timer: ReturnType<typeof setInterval> | undefined;
let alive = true;
let wasConnected = false;
function send(message: object) {
  if (socket?.readyState === WebSocket.OPEN)
    socket.send(JSON.stringify(message));
}
async function refresh() {
  try {
    const result = await api<{ items: Operation[] }>(path + "/operations");
    if (!alive) return;
    if (op.value)
      op.value =
        result.items.find((item) => item.id === op.value?.id) || op.value;
    else if (props.mode === "uninstall")
      op.value = result.items.find(
        (item) =>
          item.kind === "uninstall" &&
          ["pending", "running"].includes(item.status),
      );
    if (op.value && !["pending", "running"].includes(op.value.status))
      key = crypto.randomUUID();
    if (props.mode === "uninstall" && op.value?.status === "succeeded")
      emit("removed");
  } catch (e) {
    if (alive) error.value = errorText(e);
  }
}
async function start() {
  busy.value = true;
  error.value = "";
  try {
    // WebSSH 不再要管理员密码：面板会话本身已经是管理员，再要一次密码只是把
    // 门槛挪个地方。不带密码换来的授权只够开终端，卸载仍然要密码。
    const access = await api<{ token: string }>(
      path + "/operation-access",
      "POST",
      props.mode === "shell"
        ? { scope: "shell" }
        : { password: password.value },
    );
    if (!alive) return;
    const result = await api<Operation>(path + "/" + props.mode, "POST", {
      access_token: access.token,
      idempotency_key: key,
    });
    if (!alive) return;
    op.value = result;
    if (!["pending", "running"].includes(result.status)) {
      key = crypto.randomUUID();
      throw new Error("该任务已结束，请重新操作。");
    }
    if (props.mode === "shell") await connect(result.id);
  } catch (e) {
    if (alive) error.value = errorText(e);
  } finally {
    password.value = "";
    busy.value = false;
  }
}
async function connect(id: string) {
  await nextTick();
  if (!terminalHost.value || !alive) return;
  terminal?.dispose();
  resize?.disconnect();
  terminal = new Terminal({
    cursorBlink: true,
    fontSize: 14,
    scrollback: 3000,
    theme: { background: "#101820", foreground: "#e6edf3" },
  });
  fit = new FitAddon();
  terminal.loadAddon(fit);
  terminal.open(terminalHost.value);
  fit.fit();
  terminal.writeln("正在连接设备…");
  const url = new URL(
    `/api/v1/node-operations/${encodeURIComponent(id)}/terminal`,
    location.href,
  );
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  socket = new WebSocket(url);
  socket.onopen = () => {
    wasConnected = true;
    connected.value = true;
    fit?.fit();
    send({ type: "resize", cols: terminal?.cols, rows: terminal?.rows });
    terminal?.focus();
  };
  socket.onmessage = (event) => {
    try {
      const msg = JSON.parse(event.data) as { type: string; data: string };
      if (msg.type === "output")
        terminal?.write(
          Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0)),
        );
    } catch {
      error.value = "终端数据异常";
      socket?.close();
    }
  };
  socket.onerror = () => {
    error.value = "终端连接失败，请检查设备状态和授权。";
  };
  socket.onclose = () => {
    connected.value = false;
    terminal?.writeln("\r\n[连接已关闭]");
    if (alive) void disconnect();
  };
  terminal.onData((data) => {
    // Bound pasted input as well as keystrokes to the wire limit in UTF-8 bytes.
    let chunk = "",
      size = 0;
    for (const char of data) {
      const bytes = new TextEncoder().encode(char).length;
      if (size + bytes > 4096) {
        send({ type: "input", data: chunk });
        chunk = "";
        size = 0;
      }
      chunk += char;
      size += bytes;
    }
    if (chunk) send({ type: "input", data: chunk });
  });
  terminal.onResize(({ cols, rows }) => send({ type: "resize", cols, rows }));
  resize = new ResizeObserver(() => fit?.fit());
  resize.observe(terminalHost.value);
}
async function disconnect() {
  socket?.close();
  const current = op.value;
  if (!current || !["pending", "running"].includes(current.status)) return;
  try {
    await api(
      `/node-operations/${encodeURIComponent(current.id)}/cancel`,
      "POST",
      {},
    );
    current.status = "cancelled";
    key = crypto.randomUUID();
    await refresh();
  } catch (e) {
    if (alive) error.value = errorText(e);
  }
}
onMounted(() => {
  void refresh();
  timer = setInterval(() => void refresh(), 3000);
});
onUnmounted(() => {
  alive = false;
  clearInterval(timer);
  resize?.disconnect();
  socket?.close();
  terminal?.dispose();
  password.value = "";
  if (props.mode === "shell" && (wasConnected || op.value)) void disconnect();
});
</script>
<template>
  <Modal
    :title="`${mode === 'shell' ? 'WebSSH' : '卸载设备'} · ${node.name}`"
    :busy="busy"
    @close="emit('close')"
  >
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="mode === 'shell'" class="muted">
      通过 Agent 打开交互终端，以 Agent 服务账号运行，支持 Ctrl+C
      和交互命令。会话最长 10 分钟。
    </p>
    <p v-else class="warning">
      将停止并卸载此机器上的 Agent
      和配套出口服务，移除本机接入凭据。面板保留历史流量和审计记录。请先移除相关转发规则；卸载开始后不能撤销。
    </p>
    <p v-if="!supported" class="warning">
      此 Agent
      尚不支持该功能。请从设备组复制最新安装命令，在机器上重新执行以更新
      Agent、启用终端和远程卸载，然后刷新页面。远程卸载仅支持默认路径的
      root/systemd 安装。
    </p>
    <form
      v-else-if="
        !op ||
        (!['pending', 'running'].includes(op.status) &&
          (mode === 'shell' || op.status !== 'succeeded'))
      "
      @submit.prevent="start"
    >
      <label
        v-if="mode !== 'shell'"
        >管理员密码<input
          v-model="password"
          type="password"
          autocomplete="current-password"
          required
      /></label>
      <button
        :class="mode === 'shell' ? 'primary' : 'danger'"
        :disabled="busy || connected"
      >
        {{ mode === "shell" ? "连接" : "确认卸载此设备" }}
      </button>
    </form>
    <p v-if="op && mode === 'uninstall'" role="status">
      {{ labels[op.status] || op.status }}
    </p>
    <p
      v-if="op?.status === 'running' && mode === 'uninstall'"
      class="small muted"
    >
      设备回传成功后才会从列表移除。网络中断时会重试，关闭此窗口不影响卸载。
    </p>
    <button
      v-if="op?.status === 'pending' && mode === 'uninstall'"
      :disabled="busy"
      @click="disconnect"
    >
      取消等待
    </button>
    <div
      v-if="op && mode === 'shell'"
      ref="terminalHost"
      class="web-terminal"
      aria-label="交互终端"
    />
    <button v-if="connected" @click="disconnect">断开连接</button>
  </Modal>
</template>
