<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import Modal from "./Modal.vue";
import Select from "./Select.vue";
import { api, errorText } from "../core/api";
import { formatDateTime } from "../core/format";
import { notice, state } from "../core/state";
import {
  exportPortForwardJSONL,
  parsePortForwardJSONL,
} from "../core/ruleFormats";

const props = defineProps<{ mode: "import" | "export" }>();
const emit = defineEmits<{ close: []; changed: [] }>();
interface Task {
  id: string;
  kind: string;
  status: string;
  result?: string;
  error?: string;
  created_at: string;
}
interface PreviewItem {
  index: number;
  action: string;
  error?: string;
  rule?: Record<string, unknown>;
}
const importing = computed(() => props.mode === "import");
const busy = ref(false);
const error = ref("");
const tasks = ref<Task[]>([]);
const detail = ref<Task | null>(null);
const importJSON = ref("");
const importMode = ref("create");
const importFormat = ref("panel");
const importTarget = ref("");
const importTargets = ref<
  { key: string; nodeID: string; groupID: string; label: string }[]
>([]);
const importTargetsLoaded = ref(false);
const exportFormat = ref("panel");
const preview = ref<PreviewItem[]>([]);
const previewSource = ref("");
let active = true;
onUnmounted(() => {
  active = false;
});

watch([importJSON, importMode, importFormat, importTarget], () => {
  preview.value = [];
  previewSource.value = "";
});
watch(importFormat, (format) => {
  if (format !== "port-jsonl" || importTargetsLoaded.value) return;
  void run(async () => {
    const [groups, nodes] = await Promise.all([
      choices("/groups"),
      choices("/nodes"),
    ]);
    if (!active) return;
    const entryGroups = new Map(
      groups
        .filter((group) => !group.type || group.type === "entry")
        .map((group) => [String(group.id), String(group.name)]),
    );
    importTargets.value = nodes.flatMap((node) =>
      ((node.group_ids as string[]) || [])
        .filter((groupID) => entryGroups.has(groupID))
        .map((groupID) => ({
          key: `${String(node.id)}::${groupID}`,
          nodeID: String(node.id),
          groupID,
          label: `${entryGroups.get(groupID)} · ${String(node.name)}`,
        })),
    );
    importTargetsLoaded.value = true;
  });
});

async function choices(path: string) {
  const items: Record<string, unknown>[] = [];
  for (let page = 1; ; page++) {
    const batch = await api<{
      items: Record<string, unknown>[];
      total: number;
    }>(`${path}?page_size=100&page=${page}`);
    items.push(...batch.items);
    if (!batch.items.length || items.length >= batch.total) return items;
  }
}

async function run(fn: () => Promise<void>) {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await fn();
  } catch (e) {
    if (active) error.value = errorText(e);
  } finally {
    if (active) busy.value = false;
  }
}

async function loadTasks() {
  const result = await api<{ items: Task[] }>("/tasks");
  if (!active) return;
  tasks.value = (result.items || []).filter(
    (task) => task.kind === `rules.${props.mode}`,
  );
  if (detail.value) {
    const updated = await api<Task>(
      `/tasks/${encodeURIComponent(detail.value.id)}`,
    );
    if (!active) return;
    detail.value = updated;
  }
  if (importing.value) emit("changed");
}

function importRulesFromInput(): Record<string, unknown>[] {
  if (importFormat.value === "port-jsonl") {
    const target = importTargets.value.find(
      (item) => item.key === importTarget.value,
    );
    return parsePortForwardJSONL(
      importJSON.value,
      target?.nodeID || "",
      target?.groupID || "",
    );
  }
  const parsed: unknown = JSON.parse(importJSON.value);
  const rules = Array.isArray(parsed)
    ? parsed
    : parsed && typeof parsed === "object" && "items" in parsed
      ? (parsed as { items?: unknown }).items
      : null;
  if (!Array.isArray(rules) || !rules.length)
    throw Error("请输入规则数组或包含 items 的导出文件内容。");
  if (
    rules.some(
      (rule) => !rule || typeof rule !== "object" || Array.isArray(rule),
    )
  ) {
    throw Error("规则列表中的每一项都必须是对象。");
  }
  return rules as Record<string, unknown>[];
}

async function previewImport() {
  await run(async () => {
    const rules = importRulesFromInput();
    const result = await api<{ items: PreviewItem[] }>(
      "/tasks/rules/preview",
      "POST",
      { rules, mode: importMode.value },
    );
    if (!active) return;
    preview.value = result.items;
    previewSource.value = importJSON.value;
  });
}

// Keep the existing retry key without storing imported credentials.
async function importKey(payload: unknown) {
  const digest = Array.from(
    new Uint8Array(
      await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(JSON.stringify(payload)),
      ),
    ),
  )
    .map((x) => x.toString(16).padStart(2, "0"))
    .join("");
  const name = `tfp-operation:${state.user?.id}:import:${digest}`;
  let key = sessionStorage.getItem(name);
  if (!key) {
    key = crypto.randomUUID();
    sessionStorage.setItem(name, key);
  }
  return key;
}

async function importRules() {
  await run(async () => {
    const rules = importRulesFromInput();
    if (
      !preview.value.length ||
      previewSource.value !== importJSON.value ||
      preview.value.some((x) => x.error)
    )
      throw Error("请先预览并修正冲突。");
    const confirmed = rules.map((rule, i) => ({
      ...rule,
      version: preview.value[i]?.rule?.version || 0,
    }));
    const payload = { rules: confirmed, mode: importMode.value };
    await api("/tasks/rules/import", "POST", {
      ...payload,
      idempotency_key: await importKey(payload),
    });
    if (!active) return;
    importJSON.value = "";
    notice("导入任务已创建。");
    await loadTasks();
  });
}

async function exportRules() {
  await run(async () => {
    await api("/tasks/rules/export", "POST", {
      idempotency_key: crypto.randomUUID(),
    });
    if (!active) return;
    notice("导出任务已创建。");
    await loadTasks();
  });
}

async function viewTask(task: Task) {
  await run(async () => {
    const result = await api<Task>(`/tasks/${encodeURIComponent(task.id)}`);
    if (!active) return;
    detail.value = result;
    if (importing.value) emit("changed");
  });
}

async function cancelTask(task: Task) {
  await run(async () => {
    await api(`/tasks/${encodeURIComponent(task.id)}/cancel`, "POST", {});
    await loadTasks();
  });
}

function status(value: string) {
  return (
    (
      {
        pending: "待执行",
        running: "执行中",
        completed: "已完成",
        completed_with_errors: "部分失败",
        canceled: "已取消",
        failed: "失败",
      } as Record<string, string>
    )[value] || value
  );
}

function download() {
  if (!detail.value?.result) return;
  let content = detail.value.result;
  let filename = `${detail.value.kind}-${detail.value.id}.json`;
  let type = "application/json";
  if (!importing.value && exportFormat.value === "port-jsonl") {
    try {
      content = exportPortForwardJSONL(content);
    } catch (e) {
      error.value = errorText(e);
      return;
    }
    filename = `rules-${detail.value.id}.jsonl`;
    type = "application/x-ndjson";
  }
  const blob = new Blob([content], { type });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

onMounted(() => void run(loadTasks));
</script>

<template>
  <Modal
    :title="importing ? '导入转发规则' : '导出转发规则'"
    :busy="busy"
    @close="emit('close')"
  >
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <p class="muted small">
      {{
        importing
          ? "导入最多 1,000 条规则。加密规则导入前需要补充隧道密钥；取消保留已完成的规则。"
          : "导出最多 10,000 条规则，不包含隧道密钥。"
      }}
    </p>
    <form v-if="importing" @submit.prevent="previewImport">
      <div class="form-grid">
        <label>
          规则数据格式<Select
            v-model="importFormat"
            aria-label="规则数据格式"
            :disabled="busy"
          >
            <option value="panel">面板规则 JSON</option>
            <option value="port-jsonl">端口转发 JSON Lines</option>
          </Select>
        </label>
        <label>
          导入方式<Select
            v-model="importMode"
            aria-label="导入方式"
            :disabled="busy"
          >
            <option value="create">新增规则</option>
            <option value="update_by_port">
              按节点、协议和端口更新已有规则
            </option>
          </Select>
        </label>
      </div>
      <label v-if="importFormat === 'port-jsonl'">
        导入目标<Select
          v-model="importTarget"
          aria-label="导入目标"
          :disabled="busy"
        >
          <option value="">选择入口节点和设备组</option>
          <option
            v-for="target in importTargets"
            :key="target.key"
            :value="target.key"
          >
            {{ target.label }}
          </option>
        </Select>
      </label>
      <p v-if="importFormat === 'port-jsonl'" class="small muted">
        每行一条 TCP 规则；多个 dest 会导入为等权后端，proxy_protocol 3
        会向目标发送 PROXY v2。
      </p>
      <label>
        规则 JSON<textarea
          v-model="importJSON"
          required
          rows="7"
          maxlength="1000000"
          spellcheck="false"
          :disabled="busy"
        />
      </label>
      <div v-if="preview.length">
        <p v-for="item in preview" :key="item.index">
          第 {{ item.index + 1 }} 条 ·
          {{ item.error || (item.action === "update" ? "将更新" : "将新增") }}
        </p>
      </div>
      <div class="form-actions">
        <button
          type="submit"
          :disabled="
            busy || (importFormat === 'port-jsonl' && !importTargetsLoaded)
          "
        >
          预览导入
        </button>
        <button
          v-if="preview.length"
          type="button"
          class="primary"
          :disabled="busy || preview.some((item) => item.error)"
          @click="importRules"
        >
          确认并创建导入任务
        </button>
      </div>
    </form>
    <form v-else @submit.prevent="exportRules">
      <label>
        导出格式<Select
          v-model="exportFormat"
          aria-label="导出格式"
          :disabled="busy"
        >
          <option value="panel">面板规则 JSON</option>
          <option value="port-jsonl">端口转发 JSON Lines</option>
        </Select>
      </label>
      <div class="form-actions">
        <button class="primary" :disabled="busy">创建导出任务</button>
      </div>
    </form>
    <section>
      <h3>{{ importing ? "导入任务" : "导出任务" }}</h3>
      <button type="button" :disabled="busy" @click="run(loadTasks)">
        刷新任务
      </button>
      <p v-if="!tasks.length" class="muted">暂无任务。</p>
      <div v-for="task in tasks" :key="task.id" class="task-row">
        <span
          >{{ status(task.status) }} ·
          {{ formatDateTime(task.created_at) }}</span
        >
        <button type="button" :disabled="busy" @click="viewTask(task)">
          查看结果
        </button>
        <button
          v-if="['pending', 'running'].includes(task.status)"
          type="button"
          :disabled="busy"
          @click="cancelTask(task)"
        >
          取消任务
        </button>
      </div>
      <section v-if="detail">
        <h3>任务结果</h3>
        <p>{{ status(detail.status) }} {{ detail.error }}</p>
        <pre v-if="detail.result" class="operation-result">{{
          detail.result
        }}</pre>
        <div class="actions">
          <button v-if="detail.result" type="button" @click="download">
            下载
            {{
              !importing && exportFormat === "port-jsonl"
                ? "JSON Lines"
                : "JSON"
            }}
          </button>
          <button type="button" @click="detail = null">收起</button>
        </div>
      </section>
    </section>
    <div class="form-actions">
      <button type="button" :disabled="busy" @click="emit('close')">
        关闭
      </button>
    </div>
  </Modal>
</template>
