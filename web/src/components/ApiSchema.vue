<script setup lang="ts">
import { computed } from "vue";
import { resolveSchema, schemaConstraints, schemaType } from "../core/apiDocs";
import type { Schema } from "../core/apiDocs";
const props = withDefaults(
  defineProps<{
    schema?: Schema;
    definitions: Record<string, Schema>;
    depth?: number;
  }>(),
  { depth: 0 },
);
const resolved = computed(() => resolveSchema(props.schema, props.definitions));
const variants = computed(
  () =>
    resolved.value.anyOf || resolved.value.oneOf || resolved.value.allOf || [],
);
const fields = computed(() =>
  Object.entries(resolved.value.properties || {}).map(([name, schema]) => ({
    name,
    schema,
    resolved: resolveSchema(schema, props.definitions),
    required: resolved.value.required?.includes(name) || false,
  })),
);
function nested(schema: Schema) {
  const value = resolveSchema(schema, props.definitions);
  return !!(
    value.properties ||
    value.items ||
    value.anyOf ||
    value.oneOf ||
    value.allOf ||
    typeof value.additionalProperties === "object"
  );
}
</script>

<template>
  <div class="api-schema">
    <p v-if="resolved.description" class="small">{{ resolved.description }}</p>
    <p class="small muted">
      <code>{{ schemaType(schema, definitions) }}</code
      ><span v-if="schemaConstraints(resolved)">
        · {{ schemaConstraints(resolved) }}</span
      >
    </p>
    <p v-if="depth >= 8" class="small muted">更深层级请查看原始接口定义。</p>
    <template v-else>
      <div v-if="fields.length" class="api-fields">
        <div v-for="field in fields" :key="field.name" class="api-field">
          <div class="api-field-heading">
            <code>{{ field.name }}</code
            ><span :class="field.required ? 'api-required' : 'muted'">{{
              field.required ? "必填 / 必有" : "可选"
            }}</span
            ><code class="muted">{{
              schemaType(field.schema, definitions)
            }}</code>
          </div>
          <p v-if="field.resolved.description" class="small">
            {{ field.resolved.description }}
          </p>
          <p v-if="schemaConstraints(field.resolved)" class="small muted">
            {{ schemaConstraints(field.resolved) }}
          </p>
          <details v-if="nested(field.schema)">
            <summary>展开 {{ field.name }} 结构</summary>
            <ApiSchema
              :schema="field.schema"
              :definitions="definitions"
              :depth="depth + 1"
            />
          </details>
        </div>
      </div>
      <ApiSchema
        v-if="resolved.items"
        :schema="resolved.items"
        :definitions="definitions"
        :depth="depth + 1"
      />
      <div
        v-for="(variant, index) in variants"
        :key="index"
        class="api-variant"
      >
        <span class="small muted"
          >{{ resolved.allOf ? "同时满足" : "允许类型" }} {{ index + 1 }}</span
        >
        <ApiSchema
          :schema="variant"
          :definitions="definitions"
          :depth="depth + 1"
        />
      </div>
      <template v-if="resolved.additionalProperties">
        <p class="small muted">允许额外字段。</p>
        <ApiSchema
          v-if="typeof resolved.additionalProperties === 'object'"
          :schema="resolved.additionalProperties"
          :definitions="definitions"
          :depth="depth + 1"
        />
      </template>
    </template>
  </div>
</template>

<style scoped>
.api-schema {
  min-width: 0;
  overflow-wrap: anywhere;
}
.api-schema code {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}
.api-schema p {
  margin-bottom: 8px;
}
.api-fields {
  display: grid;
  gap: 0;
  border-top: 1px solid var(--color-line-faint);
}
.api-field {
  padding: 12px 0;
  border-bottom: 1px solid var(--color-line-faint);
}
.api-field-heading {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 6px 12px;
  margin-bottom: 6px;
  font-size: var(--text-xs);
}
.api-required {
  color: var(--color-accent);
}
.api-field details {
  padding: 4px 0 0 12px;
  border-left: 2px solid var(--color-line-faint);
}
.api-field summary {
  font-size: var(--text-xs);
  margin-bottom: 8px;
}
.api-variant {
  margin: 10px 0;
  padding-left: 12px;
  border-left: 2px solid var(--color-line-faint);
}
</style>
