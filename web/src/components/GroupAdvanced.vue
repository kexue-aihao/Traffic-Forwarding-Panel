<script setup lang="ts">
import { computed, ref, watch } from "vue";
import Modal from "./Modal.vue";
import Select from "./Select.vue";
import { api, errorText } from "../core/api";
import { notice } from "../core/state";
import {
  advancedSections,
  applicationProtocols,
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
  rules: {
    rule_id: string;
    status: string;
    reason?: string;
    inspection?: {
      protocol: string;
      network: string;
      mode: string;
      status: string;
      reason?: string;
    }[];
  }[];
  notes: string[];
  inspection_profiles?: {
    node_id: string;
    name: string;
    profiles: {
      label: string;
      protocol: string;
      variants?: string[];
      networks?: string[];
      ready: boolean;
      reason?: string;
    }[];
  }[];
} | null>(null);
const previewError = ref("");
const error = ref("");
const settings = ref<Settings>(initialGroupAdvanced(props.group));
function ingressValue(key: string) {
  return (settings.value.shared_tls_ingress as Settings)?.[key];
}
function setIngress(key: string, value: unknown) {
  settings.value.shared_tls_ingress = {
    ...(settings.value.shared_tls_ingress as Settings),
    [key]: value,
  };
}
const ingressNodes = ref<Group[]>([]);
const ingressStatusError = ref("");
watch(
  () => !!ingressValue("enabled"),
  async (enabled) => {
    if (!enabled) return;
    try {
      const nodes: Group[] = [];
      for (let page = 1; ; page++) {
        const batch = await api<{ items: Group[]; total: number }>(
          `/nodes?page=${page}&page_size=100`,
        );
        nodes.push(...batch.items);
        if (!batch.items.length || nodes.length >= batch.total) break;
      }
      ingressNodes.value = nodes.filter((n) =>
        ((n.group_ids as string[]) || []).includes(String(props.group.id)),
      );
    } catch (e) {
      ingressStatusError.value = errorText(e);
    }
  },
  { immediate: true },
);
function ingressNodeState(node: Group) {
  if (
    !((node.capabilities as string[]) || []).includes("shared-tls-ingress-v1")
  )
    return "需要升级 Agent";
  if (node.apply_error) return "应用失败";
  if (
    !node.last_seen ||
    Date.now() - Date.parse(String(node.last_seen)) > 120000
  )
    return "节点离线";
  if (node.desired_version !== node.applied_version) return "等待节点确认";
  const status = ((node.tls_ingress_statuses as Group[]) || []).find(
    (s) => s.group_id === props.group.id,
  );
  if (!status) return "等待入口状态";
  if (status.state === "expired") return "配置已过期";
  return `监听中 · ${status.routes} 条路由 · ${status.connections} 个连接 · ${status.rejected} 次拒绝`;
}
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
  if (
    enabled &&
    !["http", "socks"].includes(value) &&
    !Object.hasOwn(settings.value, "inspection")
  ) {
    settings.value = {
      ...settings.value,
      inspection: {
        version: 1,
        profiles: [],
        mode: "strict",
        unknown: "allow",
      },
    };
    activated.value = true;
  }
}
function inspectionValue(key: string) {
  const v = settings.value.inspection as Settings | undefined;
  return String(
    v?.[key] || (key === "mode" ? "strict" : key === "unknown" ? "allow" : ""),
  );
}
function setInspection(key: string, value: unknown) {
  settings.value.inspection = {
    ...(settings.value.inspection as Settings),
    [key]: value,
  };
  if (key === "mode" && value === "observe")
    (settings.value.inspection as Settings).unknown = "allow";
}
function profileText() {
  const v = (settings.value.inspection as Settings)?.profiles;
  return Array.isArray(v) ? v.join("\n") : "";
}
function setProfiles(source: string) {
  setInspection(
    "profiles",
    source
      .split(/\r?\n/)
      .map((v) => v.trim())
      .filter(Boolean),
  );
}
function businessValue(key: string) {
  return (
    (settings.value.inspection as Settings)?.business as Settings | undefined
  )?.[key];
}
function setBusiness(key: string, value: unknown) {
  const current = (settings.value.inspection as Settings)?.business as
    | Settings
    | undefined;
  const next = { ...current, [key]: value };
  if (key === "websocket" && !value) next.websocket_early_data = false;
  if (!next.tls_profile && !next.upstream_tls_profile && !next.websocket) {
    const inspection = { ...(settings.value.inspection as Settings) };
    delete inspection.business;
    settings.value.inspection = inspection;
  } else setInspection("business", next);
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
        <div v-for="item in preview.rules" :key="item.rule_id">
          <p>{{ item.rule_id }} · {{ item.status }} {{ item.reason || "" }}</p>
          <p v-for="scope in item.inspection || []" :key="scope.protocol">
            {{ scope.protocol }} · {{ scope.network }} · {{ scope.mode }} ·
            {{ scope.status }} {{ scope.reason || "" }}
          </p>
        </div>
        <div
          v-for="node in preview.inspection_profiles || []"
          :key="node.node_id"
          aria-label="节点本地检测配置"
        >
          <p>{{ node.name || node.node_id }} · 本地检测配置</p>
          <p v-if="!node.profiles.length">
            未报告本地 profile；严格检测规则无法使用需要凭据的协议。
          </p>
          <p
            v-for="profile in node.profiles"
            :key="`${profile.label}/${profile.protocol}`"
          >
            {{ profile.label }} · {{ profile.protocol }} ·
            {{ (profile.variants || []).join(", ") }} ·
            {{ (profile.networks || []).join(", ") }} ·
            {{ profile.ready ? "就绪" : "未就绪" }} {{ profile.reason || "" }}
          </p>
        </div>
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

          <fieldset
            v-if="field.key === 'shared_tls_ingress'"
            class="choice-list"
          >
            <legend class="sr-only">设备组共享 TLS 入口</legend>
            <label class="check"
              ><input
                type="checkbox"
                :checked="!!ingressValue('enabled')"
                :disabled="busy"
                @change="
                  setIngress(
                    'enabled',
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />启用共享入口</label
            >
            <label
              >监听 IP<input
                :value="String(ingressValue('listen_ip') || '')"
                :disabled="busy"
                required
                placeholder="0.0.0.0"
                @input="
                  setIngress(
                    'listen_ip',
                    ($event.target as HTMLInputElement).value,
                  )
                "
            /></label>
            <label
              >共享 TCP 端口<input
                type="number"
                :value="Number(ingressValue('port'))"
                :disabled="busy"
                required
                min="1"
                max="65535"
                @input="
                  setIngress(
                    'port',
                    Number(($event.target as HTMLInputElement).value),
                  )
                "
            /></label>
            <p class="small muted">
              客户端必须发送每条规则的业务域名，目标证书须覆盖该域名。普通 TCP
              和原生 UDP 使用独立入口。
            </p>
            <p v-if="ingressStatusError" class="error">
              {{ ingressStatusError }}
            </p>
            <p
              v-for="node in ingressNodes"
              :key="String(node.id)"
              class="small muted"
            >
              {{ node.name }}：{{ ingressNodeState(node) }}
            </p>
          </fieldset>
          <label v-else-if="isBoolean(field.key)" class="check advanced-check">
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
            <option value="2">2 · 共享 TLS，独立端口仅管理员</option>
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
            <label
              v-for="choice in applicationProtocols"
              :key="choice.value"
              class="check"
            >
              <input
                type="checkbox"
                :checked="listValue(field.key).includes(choice.value)"
                :aria-label="choice.label"
                :disabled="busy"
                @change="
                  toggleProtocol(
                    choice.value,
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />
              <span
                >{{ choice.label }}
                <span class="muted small">· {{ choice.scope }}</span></span
              >
            </label>
          </fieldset>

          <fieldset v-else-if="field.key === 'inspection'" class="choice-list">
            <legend class="sr-only">协议检测范围</legend>
            <label
              >检测方式
              <Select
                :model-value="inspectionValue('mode')"
                aria-label="协议检测方式"
                :disabled="busy"
                @update:model-value="setInspection('mode', $event)"
              >
                <option value="strict">strict · 按声明范围阻断</option>
                <option value="observe">observe · 只观测</option>
              </Select>
            </label>
            <label
              >未知应用处理
              <Select
                :model-value="inspectionValue('unknown')"
                aria-label="未知应用处理"
                :disabled="busy || inspectionValue('mode') === 'observe'"
                @update:model-value="setInspection('unknown', $event)"
              >
                <option value="allow">allow · 允许未知应用</option>
                <option value="deny">deny · 拒绝未知应用</option>
              </Select>
            </label>
            <p v-if="inspectionValue('unknown') === 'deny'" class="muted small">
              会拒绝普通未知业务和不可见内层；拒绝未知不等于识别了代理协议。
            </p>
            <label
              >本地凭据 profile 标签（每行一个）
              <textarea
                :value="profileText()"
                aria-label="本地检测 profile 标签"
                :disabled="busy"
                rows="3"
                placeholder="local-ss\nlocal-vmess"
                @input="
                  setProfiles(($event.target as HTMLTextAreaElement).value)
                "
              />
            </label>
            <label
              >受控业务 TLS profile 标签
              <input
                :value="String(businessValue('tls_profile') || '')"
                aria-label="业务 TLS profile 标签"
                :disabled="busy"
                placeholder="owned-service"
                @input="
                  setBusiness(
                    'tls_profile',
                    ($event.target as HTMLInputElement).value.trim(),
                  )
                "
              />
            </label>
            <label
              >业务上游 TLS profile 标签
              <input
                :value="String(businessValue('upstream_tls_profile') || '')"
                aria-label="业务上游 TLS profile 标签"
                :disabled="busy"
                placeholder="trusted-origin"
                @input="
                  setBusiness(
                    'upstream_tls_profile',
                    ($event.target as HTMLInputElement).value.trim(),
                  )
                "
              />
            </label>
            <label class="check"
              ><input
                type="checkbox"
                :checked="Boolean(businessValue('websocket'))"
                :disabled="busy"
                @change="
                  setBusiness(
                    'websocket',
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />受控 WebSocket 内层检测</label
            >
            <label class="check"
              ><input
                type="checkbox"
                :checked="Boolean(businessValue('websocket_early_data'))"
                :disabled="busy || !businessValue('websocket')"
                @change="
                  setBusiness(
                    'websocket_early_data',
                    ($event.target as HTMLInputElement).checked,
                  )
                "
              />受控 Xray WebSocket early-data</label
            >
            <p v-if="businessValue('websocket_early_data')" class="muted small">
              仅支持 early-data 中完整的认证首部；截断首部在 Upgrade
              前拒绝。默认不把普通子协议名称当作业务数据。
            </p>
            <p class="muted small">
              仅用于拥有服务身份的入口。透传 TLS 的 SNI/ALPN 无法证明内层是
              Trojan、VMess 或 Shadowsocks。
            </p>
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
