<script setup lang="ts">
import { computed, ref, watch } from "vue";
import Modal from "./Modal.vue";
import Select from "./Select.vue";
import { api, errorText } from "../core/api";
import { notice } from "../core/state";
import {
  advancedSections,
  formatGroupAdvanced,
  initialGroupAdvanced,
  parseGroupAdvanced,
} from "../core/groupAdvanced";

type Group = Record<string, unknown>;
type Settings = Record<string, unknown>;
const props = defineProps<{
  group: Group;
  groups: Group[];
  groupsError?: string;
}>();
const emit = defineEmits<{ close: []; saved: [group: Group] }>();
const busy = ref(false);
const activated = ref(
  !props.group.advanced ||
    Number((props.group.advanced as Settings).policy_version) === 2,
);
const preview = ref<{
  rules: { rule_id: string; status: string; reason?: string }[];
  notes: string[];
} | null>(null);
const previewError = ref("");
const error = ref("");
const settings = ref<Settings>(initialGroupAdvanced(props.group));
const initial = JSON.stringify(settings.value);
const initialActivation = activated.value;
const rawText = ref(formatGroupAdvanced(settings.value));
const rawBaseline = ref(rawText.value);
const tlsDraft = ref(
  Object.hasOwn(settings.value, "tls")
    ? JSON.stringify(settings.value.tls, null, 2)
    : "{}",
);
const newField = ref("");
const listDrafts = ref<Record<string, string>>(initialListDrafts());
const dirty = computed(
  () =>
    activated.value !== initialActivation ||
    JSON.stringify(settings.value) !== initial ||
    rawText.value !== rawBaseline.value ||
    (Object.hasOwn(settings.value, "tls") &&
      tlsDraft.value !== JSON.stringify(settings.value.tls, null, 2)),
);
const availableSections = computed(() =>
  advancedSections
    .map((section) => ({
      ...section,
      fields: section.fields.filter(
        (field) => !Object.hasOwn(settings.value, field.key),
      ),
    }))
    .filter((section) => section.fields.length),
);
const configuredSections = computed(() =>
  advancedSections
    .map((section) => ({
      ...section,
      fields: section.fields.filter((field) =>
        Object.hasOwn(settings.value, field.key),
      ),
    }))
    .filter((section) => section.fields.length),
);
const configuredKeys = new Set(
  advancedSections.flatMap((section) =>
    section.fields.map((field) => field.key),
  ),
);
configuredKeys.add("policy_version");
const unknownKeys = computed(() =>
  Object.keys(settings.value).filter((key) => !configuredKeys.has(key)),
);

watch(
  [settings, tlsDraft],
  () => {
    if (rawText.value !== rawBaseline.value) return;
    try {
      syncRaw();
    } catch {
      // Keep incomplete number/TLS input editable; save reports validation errors.
    }
  },
  { deep: true },
);

function syncRaw() {
  rawText.value = formatGroupAdvanced(currentSettings());
  rawBaseline.value = rawText.value;
}

function addField() {
  const field = advancedSections
    .flatMap((section) => section.fields)
    .find((candidate) => candidate.key === newField.value);
  if (!field) return;
  settings.value = {
    ...settings.value,
    [field.key]: JSON.parse(JSON.stringify(field.value)),
  };
  if (field.key === "tls") tlsDraft.value = "{}";
  if (isStringList(field.key)) listDrafts.value[field.key] = "";
  newField.value = "";
  error.value = "";
}

function removeField(key: string) {
  const next = { ...settings.value };
  delete next[key];
  delete listDrafts.value[key];
  settings.value = next;
  error.value = "";
}

function isBoolean(key: string) {
  return ["tls_reject_empty_sni", "disable_udp", "udp_over_tcp"].includes(key);
}
function isNumber(key: string) {
  return ["max_fail", "fail_timout_sec"].includes(key);
}
function isStringList(key: string) {
  return ["allowed_host", "blocked_host", "blocked_path"].includes(key);
}
function listValue(key: string) {
  const value = settings.value[key];
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}
function setListText(key: string, source: string) {
  listDrafts.value[key] = source;
  settings.value[key] = source
    .split(/\r?\n/)
    .map((item) => item.trim())
    .filter(Boolean);
  error.value = "";
}
function initialListDrafts() {
  return Object.fromEntries(
    ["allowed_host", "blocked_host", "blocked_path"].map((key) => [
      key,
      listValue(key).join("\n"),
    ]),
  );
}
function toggleListValue(key: string, value: string, enabled: boolean) {
  const values = listValue(key).filter((item) => item !== value);
  if (enabled) values.push(value);
  settings.value[key] = values;
  error.value = "";
}
function toggleProtocol(value: string, enabled: boolean) {
  toggleListValue("blocked_protocol", value, enabled);
}
function toggleGroup(key: string, value: string, enabled: boolean) {
  toggleListValue(key, value, enabled);
}
function groupChoices(key: string) {
  const choices = props.groups
    .filter(
      (candidate) =>
        candidate.id !== props.group.id &&
        (!activated.value ||
          key !== "reverse_group" ||
          ["", "entry"].includes(String(candidate.type || ""))),
    )
    .map((candidate) => ({
      id: String(candidate.id),
      name: String(candidate.name),
    }));
  for (const id of listValue(key)) {
    if (!choices.some((candidate) => candidate.id === id))
      choices.push({ id, name: `已配置设备组 · ${id}` });
  }
  return choices;
}

function applyRaw() {
  error.value = "";
  try {
    const parsed = parseGroupAdvanced(rawText.value);
    const tlsText = Object.hasOwn(parsed, "tls")
      ? JSON.stringify(parsed.tls, null, 2)
      : "{}";
    settings.value = currentSettings(parsed, tlsText);
    listDrafts.value = initialListDrafts();
    tlsDraft.value = tlsText;
    syncRaw();
  } catch (e) {
    error.value = errorText(e);
  }
}

function formatRaw() {
  error.value = "";
  try {
    rawText.value = formatGroupAdvanced(parseGroupAdvanced(rawText.value));
  } catch (e) {
    error.value = errorText(e);
  }
}

function currentSettings(
  source = settings.value,
  tlsSource = tlsDraft.value,
): Settings {
  const next = JSON.parse(JSON.stringify(source)) as Settings;
  next.policy_version = activated.value ? 2 : 0;
  for (const key of ["max_fail", "fail_timout_sec"]) {
    if (!Object.hasOwn(next, key)) continue;
    const value = next[key];
    const max = key === "max_fail" ? 1000 : 86400;
    if (
      typeof value !== "number" ||
      !Number.isInteger(value) ||
      value < 0 ||
      value > max
    )
      throw new Error(`${key} 必须是 0 到 ${max} 之间的整数。`);
  }
  if (Object.hasOwn(next, "tls")) {
    let tls: unknown;
    try {
      tls = JSON.parse(tlsSource);
    } catch {
      throw new Error("tls 配置 JSON 格式有误。");
    }
    if (!tls || typeof tls !== "object" || Array.isArray(tls))
      throw new Error("tls 配置必须是 JSON 对象。");
    next.tls = tls;
  }
  return parseGroupAdvanced(JSON.stringify(next));
}

async function loadPreview() {
  previewError.value = "";
  try {
    preview.value = await api(
      `/groups/${encodeURIComponent(String(props.group.id))}/advanced-preview`,
      "POST",
      { advanced: currentSettings() },
    );
  } catch (e) {
    previewError.value = errorText(e);
  }
}
async function save() {
  if (busy.value) return;
  error.value = "";
  if (rawText.value !== rawBaseline.value) {
    try {
      parseGroupAdvanced(rawText.value);
      error.value = "请先应用 JSONC 编辑器中的更改。";
    } catch (e) {
      error.value = errorText(e);
    }
    return;
  }
  let advanced: Settings;
  try {
    advanced = currentSettings();
  } catch (e) {
    error.value = errorText(e);
    return;
  }
  busy.value = true;
  try {
    const result = await api<Group>(
      `/groups/${encodeURIComponent(String(props.group.id))}`,
      "PUT",
      {
        ...props.group,
        advanced,
        version: props.group.version,
      },
    );
    notice("设备组高级设置已保存。");
    emit("saved", result);
  } catch (e) {
    error.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <Modal
    class="group-advanced"
    title="设备组高级设置"
    :busy="busy"
    :dirty="dirty"
    @close="emit('close')"
  >
    <p class="advanced-group muted small">{{ group.name }}</p>
    <form @submit.prevent="save">
      <label class="check advanced-check"
        ><input
          v-model="activated"
          type="checkbox"
          :disabled="busy"
          aria-label="启用完整高级策略"
        />启用完整高级策略</label
      >
      <p class="muted small">
        {{
          activated
            ? "保存后下发完整策略；节点能力不足的规则将停止并显示原因。"
            : "保留旧版行为。尚未激活的 Host、Path、地址和反向配置不会自动执行。"
        }}
      </p>
      <p class="muted small">
        TCP 检查可见 Host/SNI 和明文 HTTP 路径；HTTPS 路径不可见。直连 UDP
        保持原生传输。反向服务须先配置托管 hub 和节点证书。
      </p>
      <button type="button" :disabled="busy" @click="loadPreview">
        预览策略影响
      </button>
      <p v-if="previewError" class="error">{{ previewError }}</p>
      <div v-if="preview" class="muted small" aria-label="策略影响预览">
        <p v-if="!preview.rules.length">无适用规则</p>
        <p v-for="item in preview.rules" :key="item.rule_id">
          {{ item.rule_id }} · {{ item.status }} {{ item.reason || "" }}
        </p>
        <p v-for="note in preview.notes" :key="note">{{ note }}</p>
      </div>
      <div class="editor-heading">
        <label for="group-advanced-add">额外设置参数</label>
        <span class="muted small">按需配置</span>
      </div>
      <div class="add-field">
        <Select
          id="group-advanced-add"
          v-model="newField"
          aria-label="添加额外参数"
          :disabled="busy || !availableSections.length"
        >
          <option value="">选择要添加的参数</option>
          <optgroup
            v-for="section in availableSections"
            :key="section.title"
            :label="section.title"
          >
            <option
              v-for="field in section.fields"
              :key="field.key"
              :value="field.key"
            >
              {{ field.label }} · {{ field.key }}
            </option>
          </optgroup>
        </Select>
        <button type="button" :disabled="busy || !newField" @click="addField">
          添加参数
        </button>
      </div>

      <p
        v-if="!configuredSections.length && !unknownKeys.length"
        class="empty advanced-empty"
      >
        尚未添加额外参数。
      </p>

      <section
        v-for="section in configuredSections"
        :key="section.title"
        class="advanced-section"
      >
        <h3>{{ section.title }}</h3>
        <div
          v-for="field in section.fields"
          :key="field.key"
          class="advanced-field"
        >
          <div class="field-heading">
            <div>
              <label :for="`advanced-${field.key}`">{{ field.label }}</label>
              <code>{{ field.key }}</code>
            </div>
            <button
              type="button"
              class="quiet"
              :aria-label="`移除${field.label}`"
              :disabled="busy"
              @click="removeField(field.key)"
            >
              移除
            </button>
          </div>
          <p class="muted small field-description">{{ field.description }}</p>

          <label v-if="isBoolean(field.key)" class="check advanced-check">
            <input
              :id="`advanced-${field.key}`"
              v-model="settings[field.key]"
              type="checkbox"
              :disabled="busy"
            />
            <span>启用</span>
          </label>

          <input
            v-else-if="isNumber(field.key)"
            :id="`advanced-${field.key}`"
            v-model.number="settings[field.key]"
            type="number"
            min="0"
            :max="field.key === 'max_fail' ? 1000 : 86400"
            step="1"
            required
            :disabled="busy"
          />

          <Select
            v-else-if="field.key === 'tls_inbound_policy'"
            :id="`advanced-${field.key}`"
            :model-value="String(settings[field.key])"
            :aria-label="field.label"
            :disabled="busy"
            @update:model-value="settings[field.key] = Number($event)"
          >
            <option value="0">0 · 宽松模式</option>
            <option value="1">1 · 仅允许 TLS 入站规则</option>
            <option value="2">2 · TLS 入站规则使用管理员独立端口</option>
          </Select>

          <Select
            v-else-if="field.key === 'protocol'"
            :id="`advanced-${field.key}`"
            :model-value="String(settings[field.key])"
            :aria-label="field.label"
            :disabled="busy"
            @update:model-value="settings[field.key] = $event"
          >
            <option value="tls">tls</option>
            <option value="tls_simple">tls_simple</option>
            <option value="ws">ws</option>
            <option value="http">http</option>
          </Select>

          <fieldset
            v-else-if="field.key === 'blocked_protocol'"
            class="choice-list"
          >
            <legend class="sr-only">应用协议屏蔽</legend>
            <label class="check">
              <input
                type="checkbox"
                :checked="listValue(field.key).includes('http')"
                :disabled="busy"
                @change="
                  toggleProtocol(
                    'http',
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />
              <span>HTTP</span>
            </label>
            <label class="check">
              <input
                type="checkbox"
                :checked="listValue(field.key).includes('socks')"
                :disabled="busy"
                @change="
                  toggleProtocol(
                    'socks',
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />
              <span>SOCKS</span>
            </label>
          </fieldset>

          <fieldset
            v-else-if="
              field.key === 'ipv6_group' || field.key === 'reverse_group'
            "
            class="group-choices"
          >
            <legend class="sr-only">{{ field.label }}</legend>
            <p v-if="groupsError" class="error" role="alert">
              无法读取设备组列表：{{ groupsError }}
            </p>
            <label
              v-for="choice in groupChoices(field.key)"
              :key="String(choice.id)"
              class="check"
            >
              <input
                type="checkbox"
                :checked="listValue(field.key).includes(String(choice.id))"
                :disabled="busy"
                @change="
                  toggleGroup(
                    field.key,
                    String(choice.id),
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />
              <span>{{ choice.name }}</span>
            </label>
            <p v-if="!groupChoices(field.key).length" class="muted small">
              没有可选的其他设备组。
            </p>
          </fieldset>

          <label v-else-if="isStringList(field.key)" class="list-input">
            <span class="muted small">每行一个值</span>
            <textarea
              :id="`advanced-${field.key}`"
              :value="listDrafts[field.key]"
              rows="3"
              :disabled="busy"
              @input="
                setListText(
                  field.key,
                  ($event.target as HTMLTextAreaElement).value,
                )
              "
            />
          </label>

          <label v-else-if="field.key === 'tls'" class="list-input">
            <span class="muted small">JSON 对象</span>
            <textarea
              id="advanced-tls"
              v-model="tlsDraft"
              rows="5"
              spellcheck="false"
              :disabled="busy"
              @input="error = ''"
            />
          </label>
        </div>
      </section>

      <p v-if="unknownKeys.length" class="muted small unknown-fields">
        兼容参数：{{ unknownKeys.join("、") }}。可在 JSONC 编辑器中查看和修改。
      </p>

      <details class="advanced-raw">
        <summary>JSONC 兼容编辑器</summary>
        <p class="muted small">
          支持 // 和 /* */ 注释；编辑后点击“应用 JSONC”更新表单，再保存设置。
        </p>
        <textarea
          v-model="rawText"
          class="advanced-editor"
          rows="14"
          spellcheck="false"
          autocapitalize="off"
          autocomplete="off"
          aria-label="设备组高级设置 JSON"
          :aria-invalid="!!error"
          :disabled="busy"
          @input="error = ''"
        />
        <div class="raw-actions">
          <button type="button" :disabled="busy" @click="formatRaw">
            格式化
          </button>
          <button type="button" :disabled="busy" @click="applyRaw">
            应用 JSONC
          </button>
        </div>
      </details>

      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <details class="advanced-help">
        <summary>参数说明</summary>
        <p class="muted small">
          空列表表示不配置该列表。白名单与其他入站屏蔽选项冲突；禁用 UDP 与 UDP
          over TCP 不能同时启用。
        </p>
        <div class="advanced-docs">
          <section v-for="section in advancedSections" :key="section.title">
            <h3>{{ section.title }}</h3>
            <dl>
              <template v-for="field in section.fields" :key="field.key">
                <dt>
                  <code>{{ field.key }}</code>
                </dt>
                <dd>{{ field.description }}</dd>
              </template>
            </dl>
          </section>
        </div>
      </details>

      <div class="form-actions">
        <button type="submit" class="primary" :disabled="busy">
          {{ busy ? "保存中…" : "保存高级设置" }}
        </button>
      </div>
    </form>
  </Modal>
</template>

<style scoped>
.group-advanced {
  width: min(860px, calc(100vw - 28px));
}
.advanced-group {
  margin: 0 0 var(--space-5);
  overflow-wrap: anywhere;
}
.editor-heading,
.field-heading,
.add-field,
.raw-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
.editor-heading label,
.field-heading label {
  margin: 0;
  font-weight: 600;
}
.add-field {
  justify-content: flex-start;
  margin-top: var(--space-2);
}
.add-field .select {
  flex: 1;
  max-width: 520px;
}
.advanced-empty {
  margin-block: var(--space-5);
}
.advanced-section {
  margin-top: var(--space-5);
}
.advanced-section h3 {
  margin: 0 0 var(--space-2);
  color: var(--color-ink-subtle);
  font-size: var(--text-xs);
  font-weight: 600;
}
.advanced-field {
  min-width: 0;
  padding: var(--space-3) 0;
  border-bottom: 1px solid var(--color-line-faint);
}
.field-heading > div {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
  flex-wrap: wrap;
}
.field-heading code {
  color: var(--color-ink-subtle);
  font-size: var(--text-2xs);
  overflow-wrap: anywhere;
}
.field-heading > button {
  flex: 0 0 auto;
  white-space: nowrap;
}
.field-description {
  margin: var(--space-1) 0 var(--space-2);
}
.advanced-check {
  margin-top: var(--space-2);
}
.choice-list,
.group-choices {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-5);
  border: 0;
  margin: 0;
  padding: 0;
}
.list-input {
  display: grid;
  gap: var(--space-1);
}
.list-input textarea {
  min-height: 72px;
  font-family: var(--font-mono);
  font-size: var(--text-sm);
}
.unknown-fields {
  margin-top: var(--space-4);
  overflow-wrap: anywhere;
}
.advanced-raw,
.advanced-help {
  margin-top: var(--space-4);
  font-size: var(--text-sm);
}
.advanced-raw summary,
.advanced-help summary {
  width: fit-content;
  cursor: pointer;
  font-weight: 600;
}
.advanced-editor {
  display: block;
  width: 100%;
  min-height: 220px;
  padding: var(--space-3);
  border: 1px solid var(--color-line-faint);
  background: var(--color-bg-1);
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  line-height: 1.65;
  tab-size: 2;
  resize: vertical;
}
.raw-actions {
  justify-content: flex-start;
  margin-top: var(--space-2);
}
.advanced-docs {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-5) var(--space-6);
  margin-top: var(--space-4);
}
.advanced-docs section {
  min-width: 0;
}
.advanced-docs h3 {
  margin: 0 0 var(--space-3);
  font-size: var(--text-sm);
}
.advanced-docs dl {
  margin: 0;
}
.advanced-docs dt {
  overflow-wrap: anywhere;
}
.advanced-docs dd {
  margin: var(--space-1) 0 var(--space-3);
  color: var(--color-ink-muted);
}
@media (max-width: 600px) {
  .add-field {
    align-items: stretch;
    flex-direction: column;
  }
  .add-field .select {
    max-width: none;
  }
  .advanced-docs {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
