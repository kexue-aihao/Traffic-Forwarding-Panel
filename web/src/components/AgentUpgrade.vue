<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import Modal from "./Modal.vue";
import { api, errorText } from "../core/api";

const props = defineProps<{
  node: {
    id: string;
    name: string;
    agent_version?: string;
    capabilities?: string[];
    upgrade_key_sha256?: string;
    os?: string;
    arch?: string;
  };
}>();
const emit = defineEmits<{ close: []; shell: []; upgraded: [] }>();
interface Release {
  version: string;
  url: string;
  sha256: string;
  signature: string;
  os: string;
  arch: string;
}
interface UpdateStatus {
  state: string;
  target_version: string;
  release?: Release;
}
interface Settings {
  enabled: boolean;
  available: boolean;
  reason: string;
  public_key_sha256?: string;
}
interface Operation {
  id: string;
  kind: string;
  status: string;
  upgrade?: Release;
  error?: string;
  expires_at?: string;
}
const update = ref<UpdateStatus>();
const settings = ref<Settings>();
const operations = ref<Operation[]>([]);
const requestedRelease = ref<Release>();
const password = ref("");
const error = ref("");
const refreshError = ref("");
const busy = ref(false);
const refreshing = ref(false);
const fresh = ref(false);
const loaded = ref(false);
const nodePath = `/nodes/${encodeURIComponent(props.node.id)}`;
const activeStates = ["pending", "running", "staged"];
function operationStatus(op: Operation) {
  if (
    ["pending", "running"].includes(op.status) &&
    op.expires_at &&
    Date.parse(op.expires_at) <= Date.now() &&
    !(op.kind === "uninstall" && op.status === "running")
  )
    return "expired";
  return op.status;
}
const operationLabels: Record<string, string> = {
  pending: "等待设备接收",
  running: "正在下载并校验升级文件",
  staged: "等待新版本启动和健康确认",
  succeeded: "升级完成",
  failed: "升级失败，可重试",
  rolled_back: "新版本启动失败，已恢复旧版本",
  cancelled: "升级已取消",
  expired: "升级任务已过期，可重试",
};
const updateLabels: Record<string, string> = {
  current: "已是面板对应版本",
  disabled: "已关闭，可手动升级",
  unavailable: "暂不可用",
  unsupported: "暂不支持此节点平台",
  requires_setup: "需要首次初始化",
  version_skipped: "当前版本较新或无法比较",
  waiting: "等待自动升级",
  ...operationLabels,
};
const release = computed(() => requestedRelease.value || update.value?.release);
const targetVersion = computed(
  () => release.value?.version || update.value?.target_version,
);
const latestUpgrade = computed(() =>
  operations.value.find((op) => op.kind === "upgrade"),
);
const active = computed(() =>
  operations.value.find((op) => activeStates.includes(operationStatus(op))),
);
const unsupported = computed(
  () =>
    update.value?.state === "unsupported" ||
    (!!props.node.os && props.node.os !== "linux") ||
    (!!props.node.arch && !["amd64", "arm64"].includes(props.node.arch)),
);
const needsSetup = computed(
  () =>
    update.value?.state === "requires_setup" ||
    !props.node.capabilities?.includes("upgrade-v1") ||
    (!!settings.value?.public_key_sha256 &&
      settings.value.public_key_sha256 !== props.node.upgrade_key_sha256),
);
const completed = computed(
  () =>
    latestUpgrade.value?.status === "succeeded" &&
    latestUpgrade.value.upgrade?.version === targetVersion.value,
);
function olderVersion(current: string, target: string) {
  const pattern =
    /^v?(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;
  const a = pattern.exec(current),
    b = pattern.exec(target);
  if (!a || !b || b[4]) return false;
  for (let i = 1; i <= 3; i++) {
    const x = BigInt(a[i]),
      y = BigInt(b[i]);
    if (x !== y) return x < y;
  }
  return !!a[4];
}
const behind = computed(() =>
  olderVersion(props.node.agent_version || "", targetVersion.value || ""),
);
const canUpgrade = computed(
  () =>
    fresh.value &&
    settings.value?.available &&
    release.value &&
    !unsupported.value &&
    !needsSetup.value &&
    behind.value &&
    !completed.value &&
    !active.value &&
    !["unavailable", "unsupported", "version_skipped"].includes(
      update.value?.state || "",
    ),
);
function operationKey() {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}
let key = operationKey();
let submittedID = "";
let alive = true;
let timer: ReturnType<typeof setInterval> | undefined;
const reported = new Set<string>();
function acceptOperations(items: Operation[]) {
  operations.value = items;
  const submitted = items.find((op) => op.id === submittedID);
  if (submitted && !activeStates.includes(operationStatus(submitted))) {
    key = operationKey();
    requestedRelease.value = undefined;
    submittedID = "";
  }
  const latest = latestUpgrade.value;
  if (latest?.status === "succeeded" && !reported.has(latest.id)) {
    reported.add(latest.id);
    emit("upgraded");
  }
}
async function refresh() {
  if (refreshing.value) return;
  refreshing.value = true;
  try {
    const [status, config, tasks] = await Promise.all([
      api<UpdateStatus>(nodePath + "/agent-update"),
      api<Settings>("/agent-update-settings"),
      api<{ items: Operation[] }>(nodePath + "/operations"),
    ]);
    if (!alive) return;
    update.value = status;
    settings.value = config;
    acceptOperations(tasks.items);
    refreshError.value = "";
    fresh.value = true;
    loaded.value = true;
  } catch (e) {
    if (alive) {
      fresh.value = false;
      refreshError.value = errorText(e);
    }
  } finally {
    if (alive) refreshing.value = false;
  }
}
async function upgrade() {
  if (!canUpgrade.value || !release.value || busy.value || refreshing.value)
    return;
  busy.value = true;
  error.value = "";
  // Preserve both the signed release and key when a response is lost. Retrying
  // must resolve the original request rather than restart the Agent twice.
  requestedRelease.value = { ...release.value };
  try {
    const access = await api<{ token: string }>(
      nodePath + "/operation-access",
      "POST",
      {
        password: password.value,
      },
    );
    if (!alive) return;
    const result = await api<Operation>(nodePath + "/upgrade", "POST", {
      access_token: access.token,
      idempotency_key: key,
      upgrade: requestedRelease.value,
    });
    if (!alive) return;
    submittedID = result.id;
    acceptOperations([
      result,
      ...operations.value.filter((op) => op.id !== result.id),
    ]);
    await refresh();
  } catch (e) {
    if (alive) {
      error.value = errorText(e);
      await refresh();
    }
  } finally {
    password.value = "";
    if (alive) busy.value = false;
  }
}
onMounted(() => {
  void refresh();
  timer = setInterval(() => {
    if (!busy.value) void refresh();
  }, 5000);
});
onUnmounted(() => {
  alive = false;
  clearInterval(timer);
  password.value = "";
});
</script>

<template>
  <Modal
    :title="`升级 Agent · ${node.name}`"
    :busy="busy"
    @close="emit('close')"
  >
    <p v-if="error || refreshError" class="error" role="alert">
      {{ error || refreshError }}
    </p>
    <p v-if="!loaded && !refreshError" class="muted" role="status">
      正在检查 Agent 升级状态…
    </p>
    <template v-if="loaded">
      <p>
        当前版本：{{ node.agent_version || "未知" }} · 目标版本：{{
          targetVersion || "暂无"
        }}
      </p>
      <p class="muted">
        自动升级：{{ updateLabels[update?.state || ""] || "状态未知" }}
      </p>
      <p v-if="!settings?.available" class="warning">
        {{
          settings?.reason ||
          "面板尚未准备升级文件或公开 HTTPS 地址，请先更新完整面板安装包并检查站点设置。"
        }}
      </p>
      <p v-else-if="unsupported" class="warning">
        当前仅支持 Linux amd64 / arm64 节点。
      </p>
      <template v-else-if="needsSetup">
        <p class="warning">
          此节点尚未初始化升级能力。请到「设备组 →
          接入设备」复制完整接入命令，在原机器上重新执行一次；此后升级面板即可自动升级
          Agent。
        </p>
        <p class="small muted">
          沿用原状态目录、证书和部署参数。旧版本升级前请停止服务并备份 Agent
          状态。
        </p>
        <div class="actions">
          <a href="#/groups" @click="emit('close')">前往设备组</a>
          <button
            v-if="node.capabilities?.includes('shell-v1')"
            @click="emit('shell')"
          >
            打开 WebSSH
          </button>
        </div>
      </template>
      <p v-else-if="!behind && !active && !completed" class="muted">
        {{
          node.agent_version === targetVersion
            ? "无需升级。"
            : "当前版本较新或无法比较，请核对节点版本；不会自动降级。"
        }}
      </p>
      <p v-if="active" class="muted" role="status">
        {{
          active.kind === "upgrade"
            ? operationLabels[active.status]
            : "设备有正在执行的运维任务，请等待任务结束。"
        }}
      </p>
      <template v-if="latestUpgrade && !active">
        <p role="status">
          {{
            operationLabels[operationStatus(latestUpgrade)] || "升级状态未知"
          }}
        </p>
        <p v-if="latestUpgrade.error" class="error">
          {{ latestUpgrade.error }}
        </p>
      </template>
      <form v-if="canUpgrade" @submit.prevent="upgrade">
        <p class="warning">
          升级会重启
          Agent，现有转发连接会短暂中断。新版本健康检查失败时自动恢复旧程序。
        </p>
        <label
          >管理员密码<input
            v-model="password"
            type="password"
            autocomplete="current-password"
            required
            :disabled="busy"
        /></label>
        <button class="primary" :disabled="busy || refreshing || !password">
          {{ busy ? "正在提交…" : `升级到 ${targetVersion}` }}
        </button>
      </form>
    </template>
    <button type="button" :disabled="refreshing || busy" @click="refresh">
      刷新升级状态
    </button>
  </Modal>
</template>
