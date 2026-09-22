<script setup lang="ts">
import { computed, ref } from "vue";
import Modal from "./Modal.vue";
import { api, errorText } from "../core/api";
import { notice } from "../core/state";
import {
  advancedSections,
  formatGroupAdvanced,
  initialGroupAdvanced,
  parseGroupAdvanced,
} from "../core/groupAdvanced";

type Group = Record<string, unknown>;
const props = defineProps<{ group: Group }>();
const emit = defineEmits<{ close: []; saved: [group: Group] }>();
const busy = ref(false);
const error = ref("");
const initial = formatGroupAdvanced(initialGroupAdvanced(props.group));
const text = ref(initial);
const dirty = computed(() => text.value !== initial);

function format() {
  error.value = "";
  try {
    text.value = formatGroupAdvanced(parseGroupAdvanced(text.value));
  } catch (e) {
    error.value = errorText(e);
  }
}

async function save() {
  if (busy.value) return;
  error.value = "";
  let advanced: Record<string, unknown>;
  try {
    advanced = parseGroupAdvanced(text.value);
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
      <div class="editor-heading">
        <label for="group-advanced-json">额外设置参数</label>
        <span class="muted small">JSONC</span>
      </div>
      <p id="group-advanced-hint" class="muted small editor-hint">
        按 Nyanpass 参数格式填写，支持 // 和 /* */ 注释；只需保留要配置的参数。
      </p>
      <textarea
        id="group-advanced-json"
        v-model="text"
        class="advanced-editor"
        rows="18"
        spellcheck="false"
        autocapitalize="off"
        autocomplete="off"
        aria-label="设备组高级设置 JSON"
        aria-describedby="group-advanced-hint"
        :aria-invalid="!!error"
        :disabled="busy"
        @input="error = ''"
      />
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <details class="advanced-help">
        <summary>参数说明</summary>
        <p class="muted small">
          以下按 Nyanpass 参考配置说明字段含义，空数组表示不配置该列表。
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
        <button type="button" :disabled="busy" @click="format">格式化</button>
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
.editor-heading {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
.editor-heading label {
  margin: 0;
  font-weight: 600;
}
.editor-hint {
  margin: var(--space-2) 0 var(--space-3);
}
.advanced-editor {
  display: block;
  height: clamp(240px, 43dvh, 440px);
  padding: var(--space-4);
  border: 1px solid var(--color-line-faint);
  background: var(--color-bg-1);
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  line-height: 1.75;
  tab-size: 2;
  resize: vertical;
}
.advanced-help {
  margin-top: var(--space-4);
  font-size: var(--text-sm);
}
.advanced-help summary {
  width: fit-content;
  cursor: pointer;
  font-weight: 600;
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
  .advanced-docs {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
