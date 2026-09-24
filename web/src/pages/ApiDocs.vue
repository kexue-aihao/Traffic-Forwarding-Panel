<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import Select from "../components/Select.vue";
import ApiSchema from "../components/ApiSchema.vue";
import { api, errorText } from "../core/api";
import { notice } from "../core/state";
import {
  authentication,
  entries,
  methods,
  resolveSchema,
  roles,
  schemaConstraints,
  schemaType,
} from "../core/apiDocs";
import type { APIDocument, APIEntry } from "../core/apiDocs";

const route = useRoute(),
  router = useRouter();
const origin = location.origin;
const document = ref<APIDocument | null>(null),
  loading = ref(true),
  error = ref("");
let alive = true,
  request = 0;
const all = computed(() => (document.value ? entries(document.value) : []));
const definitions = computed(() => document.value?.components?.schemas || {});
const categories = computed(() =>
  [...new Set(all.value.map((item) => item.category))].sort((a, b) =>
    a.localeCompare(b, "zh-CN"),
  ),
);
const methodOptions = computed(() =>
  methods.filter((method) => all.value.some((item) => item.method === method)),
);
function query(name: string) {
  return typeof route.query[name] === "string"
    ? (route.query[name] as string)
    : "";
}
function update(values: Record<string, string | undefined>) {
  void router.replace({ query: { ...route.query, ...values } });
}
function filter(name: string) {
  return computed({
    get: () => query(name),
    set: (value: string) =>
      update({
        [name]: value || undefined,
        page: undefined,
        operation: undefined,
      }),
  });
}
const search = filter("q"),
  method = filter("method"),
  category = filter("category"),
  role = filter("role");
const selected = computed(() => query("operation"));
const filtered = computed(() => {
  const terms = search.value
    .trim()
    .toLocaleLowerCase()
    .split(/\s+/)
    .filter(Boolean);
  return all.value.filter((item) => {
    if (method.value && item.method !== method.value) return false;
    if (category.value && item.category !== category.value) return false;
    if (role.value && item.role !== role.value) return false;
    const text = [
      item.path,
      item.method,
      item.id,
      item.name,
      item.category,
      item.summary,
      item.description,
      roles[item.role],
      ...(item["x-aliases"] || []),
    ]
      .join(" ")
      .toLocaleLowerCase();
    return terms.every((term) => text.includes(term));
  });
});
const pageSize = 20;
const pages = computed(() =>
  Math.max(1, Math.ceil(filtered.value.length / pageSize)),
);
const currentPage = computed(() => {
  const index = filtered.value.findIndex((item) => item.id === selected.value);
  if (index >= 0) return Math.floor(index / pageSize) + 1;
  return Math.min(
    pages.value,
    Math.max(1, Math.trunc(Number(query("page"))) || 1),
  );
});
const visible = computed(() =>
  filtered.value.slice(
    (currentPage.value - 1) * pageSize,
    currentPage.value * pageSize,
  ),
);
const hasFilters = computed(
  () => !!(search.value || method.value || category.value || role.value),
);
const parameterLocations: Record<string, string> = {
  path: "路径",
  query: "查询",
  header: "请求头",
  cookie: "Cookie",
};
async function load() {
  const sequence = ++request;
  loading.value = true;
  error.value = "";
  try {
    const result = await api<APIDocument>("/openapi.json");
    if (!result?.openapi || !result.paths || typeof result.paths !== "object")
      throw new Error("API 文档格式不正确。");
    if (alive && sequence === request) document.value = result;
  } catch (err) {
    if (
      alive &&
      sequence === request &&
      !(err instanceof DOMException && err.name === "AbortError")
    )
      error.value = errorText(err);
  } finally {
    if (alive && sequence === request) loading.value = false;
  }
}
function toggle(item: APIEntry) {
  update({
    operation: selected.value === item.id ? undefined : item.id,
    page: String(currentPage.value),
  });
}
function paginate(page: number) {
  update({ page: page > 1 ? String(page) : undefined, operation: undefined });
}
function reset() {
  void router.replace({ query: {} });
}
async function copy(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    notice("已复制。");
  } catch {
    notice("复制失败，请手动选中复制。");
  }
}
function link(item: APIEntry) {
  const url = new URL(location.href);
  url.hash = "/api-docs?" + new URLSearchParams({ operation: item.id });
  return url.href;
}
onMounted(load);
onBeforeUnmount(() => {
  alive = false;
});
</script>

<template>
  <section class="page api-docs">
    <div class="page-heading">
      <div>
        <p class="eyebrow">API REFERENCE</p>
        <h1>全站 API 列表</h1>
        <p class="muted">
          查询接口地址、调用身份、参数与响应结构。所有账号均可查看完整列表。
        </p>
      </div>
      <a
        class="api-download"
        href="/api/v1/openapi.json"
        download="openapi.json"
        >下载 OpenAPI JSON</a
      >
    </div>
    <div v-if="error" class="card">
      <p class="error" role="alert">{{ error }}</p>
      <button @click="load">重新加载</button>
    </div>
    <p v-else-if="loading" class="card empty" role="status" aria-busy="true">
      正在读取 API 文档…
    </p>
    <template v-else>
      <section class="card api-filter" aria-label="接口筛选">
        <label class="api-search"
          >搜索接口<input
            v-model="search"
            type="search"
            maxlength="160"
            placeholder="输入接口名称、路径或关键词"
        /></label>
        <label
          >请求方法<Select v-model="method" aria-label="请求方法"
            ><option value="">全部方法</option>
            <option v-for="value in methodOptions" :key="value" :value="value">
              {{ value }}
            </option></Select
          ></label
        >
        <label
          >接口分类<Select v-model="category" aria-label="接口分类"
            ><option value="">全部分类</option>
            <option v-for="value in categories" :key="value" :value="value">
              {{ value }}
            </option></Select
          ></label
        >
        <label
          >调用身份<Select v-model="role" aria-label="调用身份"
            ><option value="">全部身份</option>
            <option v-for="(label, value) in roles" :key="value" :value="value">
              {{ label }}
            </option></Select
          ></label
        >
      </section>
      <div class="api-count">
        <p class="muted small" role="status">
          共 {{ all.length }} 个接口 · 当前匹配 {{ filtered.length }} 个
        </p>
        <button v-if="hasFilters" @click="reset">清除筛选</button>
      </div>
      <p class="small muted">
        接口调用仍需满足对应身份和资源授权；管理员接口使用管理员会话，用户 Token
        不继承管理权限。
      </p>
      <p
        v-if="selected && !all.some((item) => item.id === selected)"
        class="warning"
        role="status"
      >
        该接口已不在当前版本的文档中，可通过搜索重新查找。
      </p>
      <section v-if="!filtered.length" class="card empty">
        <h2>没有匹配的接口</h2>
        <p class="muted">尝试缩短关键词，或清除筛选条件。</p>
        <button @click="reset">查看全部接口</button>
      </section>
      <div v-else class="api-list">
        <article
          v-for="item in visible"
          :key="item.id"
          class="card api-entry"
          :data-operation="item.id"
        >
          <h2>
            <button
              class="api-operation"
              :aria-expanded="selected === item.id"
              :aria-controls="`api-detail-${item.id}`"
              @click="toggle(item)"
            >
              <span class="api-method" :data-method="item.method">{{
                item.method
              }}</span
              ><span class="api-operation-main"
                ><code>{{ item.path }}</code
                ><span class="api-operation-name">{{ item.name }}</span></span
              ><span class="api-role">{{ roles[item.role] || item.role }}</span
              ><span class="api-expand" aria-hidden="true">{{
                selected === item.id ? "−" : "+"
              }}</span>
            </button>
          </h2>
          <p class="api-summary small muted">{{ item.summary }}</p>
          <div
            v-if="selected === item.id"
            :id="`api-detail-${item.id}`"
            class="api-detail"
          >
            <div class="api-actions">
              <span class="small muted">{{ item.category }}</span
              ><button @click="copy(origin + item.path)">复制接口地址</button
              ><button @click="copy(link(item))">复制文档链接</button>
            </div>
            <p v-if="item.description">{{ item.description }}</p>
            <h3>认证方式</h3>
            <p class="small">{{ authentication(item) }}</p>
            <p v-if="item['x-aliases']?.length" class="small">
              兼容地址：<code
                v-for="alias in item['x-aliases']"
                :key="alias"
                class="api-alias"
                >{{ alias }}</code
              >
            </p>
            <section class="api-detail-section">
              <h3>请求参数</h3>
              <div v-if="item.parameters?.length" class="api-parameters">
                <div
                  v-for="parameter in item.parameters"
                  :key="`${parameter.in}:${parameter.name}`"
                  class="api-parameter"
                >
                  <div class="api-parameter-heading">
                    <code>{{ parameter.name }}</code
                    ><span class="small muted"
                      >{{ parameterLocations[parameter.in] || parameter.in }} ·
                      {{ parameter.required ? "必填" : "按需填写" }} ·
                      {{ schemaType(parameter.schema, definitions) }}</span
                    >
                  </div>
                  <p v-if="parameter.description" class="small">
                    {{ parameter.description }}
                  </p>
                  <p v-if="parameter.schema" class="small muted">
                    {{
                      schemaConstraints(
                        resolveSchema(parameter.schema, definitions),
                      )
                    }}
                  </p>
                </div>
              </div>
              <p v-else class="small muted">无路径、查询或额外请求头参数。</p>
            </section>
            <section class="api-detail-section">
              <h3>
                请求体<span v-if="item.requestBody" class="small muted">
                  · {{ item.requestBody.required ? "必填" : "可选" }}</span
                >
              </h3>
              <div
                v-for="(media, contentType) in item.requestBody?.content"
                :key="contentType"
                class="api-media"
              >
                <p class="small">
                  <code>{{ contentType }}</code>
                </p>
                <ApiSchema :schema="media.schema" :definitions="definitions" />
              </div>
              <p v-if="!item.requestBody" class="small muted">无需请求体。</p>
            </section>
            <section class="api-detail-section">
              <h3>响应</h3>
              <details
                v-for="(response, status) in item.responses"
                :key="status"
                class="api-response"
                :open="status !== 'default'"
              >
                <summary>
                  <code>{{ status === "default" ? "其他状态" : status }}</code>
                  · {{ response.description }}
                </summary>
                <div
                  v-for="(media, contentType) in response.content"
                  :key="contentType"
                  class="api-media"
                >
                  <p class="small">
                    <code>{{ contentType }}</code>
                  </p>
                  <ApiSchema
                    :schema="media.schema"
                    :definitions="definitions"
                  />
                  <pre v-if="media.example !== undefined">{{
                    typeof media.example === "string"
                      ? media.example
                      : JSON.stringify(media.example, null, 2)
                  }}</pre>
                </div>
                <p v-if="!response.content" class="small muted">
                  {{
                    status === "101"
                      ? "协议升级后建立 WebSocket 连接。"
                      : "无响应体。"
                  }}
                </p>
              </details>
            </section>
            <details class="api-source">
              <summary>查看原始接口定义</summary>
              <pre>{{
                JSON.stringify(
                  document?.paths[item.path.slice("/api/v1".length)]?.[
                    item.method.toLowerCase()
                  ],
                  null,
                  2,
                )
              }}</pre>
            </details>
          </div>
        </article>
      </div>
      <nav v-if="filtered.length" class="api-pagination" aria-label="接口分页">
        <button :disabled="currentPage <= 1" @click="paginate(currentPage - 1)">
          上一页</button
        ><span class="small muted"
          >第 {{ currentPage }} / {{ pages }} 页 · 每页 {{ pageSize }} 个</span
        ><button
          :disabled="currentPage >= pages"
          @click="paginate(currentPage + 1)"
        >
          下一页
        </button>
      </nav>
    </template>
  </section>
</template>

<style scoped>
.api-docs {
  min-width: 0;
}
.api-download {
  white-space: nowrap;
  font-size: var(--text-sm);
}
.api-filter {
  display: grid;
  grid-template-columns: minmax(200px, 2fr) repeat(3, minmax(140px, 1fr));
  gap: 16px;
  align-items: end;
}
.api-filter label {
  margin: 0;
  min-width: 0;
}
.api-count,
.api-actions,
.api-pagination {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.api-count p {
  margin: 0;
}
.api-count {
  margin: 16px 0 8px;
}
.api-list {
  display: grid;
  gap: 12px;
}
.api-entry {
  padding: 0;
  margin-bottom: 0;
  min-width: 0;
  overflow: hidden;
}
.api-entry h2 {
  margin: 0;
}
.api-operation {
  display: flex;
  width: 100%;
  height: auto;
  min-height: 36px;
  align-items: flex-start;
  gap: 14px;
  padding: 18px 20px 8px;
  background: transparent;
  border: 0;
  box-shadow: none;
  text-align: left;
  border-radius: 0;
}
.api-operation:hover {
  background: var(--color-bg-2);
}
.api-operation-main {
  display: grid;
  gap: 6px;
  line-height: 1.35;
  min-width: 0;
  flex: 1;
}
.api-operation-main code {
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  overflow-wrap: anywhere;
}
.api-operation-name {
  display: block;
  overflow-wrap: anywhere;
  font-size: var(--text-xs);
  font-weight: 400;
  color: var(--color-ink-muted);
}
.api-method {
  padding: 3px 7px;
  border-radius: 5px;
  min-width: 62px;
  text-align: center;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--color-accent);
  background: var(--color-accent-soft);
}
.api-method[data-method="GET"] {
  color: var(--color-success);
  background: color-mix(in srgb, var(--color-success) 12%, transparent);
}
.api-method[data-method="DELETE"] {
  color: var(--color-danger);
  background: color-mix(in srgb, var(--color-danger) 12%, transparent);
}
.api-role {
  max-width: 180px;
  font-size: var(--text-xs);
  font-weight: 400;
  color: var(--color-ink-muted);
}
.api-expand {
  font-size: 20px;
  font-weight: 400;
  line-height: 1;
}
.api-summary {
  margin: 0;
  padding: 0 20px 16px;
}
.api-detail {
  padding: 20px;
  border-top: 1px solid var(--color-line-faint);
}
.api-detail code {
  font-family: var(--font-mono);
  overflow-wrap: anywhere;
}
.api-actions {
  justify-content: flex-start;
  margin-bottom: 20px;
}
.api-actions > span {
  margin-right: auto;
}
.api-actions button {
  font-size: var(--text-xs);
}
.api-detail-section {
  margin-top: 24px;
}
.api-alias {
  display: inline-block;
  margin-left: 8px;
}
.api-parameters {
  display: grid;
}
.api-parameter {
  padding: 12px 0;
  border-bottom: 1px solid var(--color-line-faint);
}
.api-parameter-heading {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 14px;
  margin-bottom: 8px;
}
.api-parameter p:last-child {
  margin: 0;
}
.api-media {
  padding: 12px 0;
}
.api-response {
  margin-top: 12px;
  padding: 12px;
  border: 1px solid var(--color-line-faint);
  border-radius: 8px;
}
.api-response summary,
.api-source summary {
  overflow-wrap: anywhere;
  font-size: var(--text-xs);
}
.api-source {
  margin-top: 20px;
}
.api-detail pre {
  padding: 12px;
  border-radius: 8px;
  background: var(--color-bg-0);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  max-height: 440px;
  overflow: auto;
}
.api-pagination {
  justify-content: center;
  margin-top: 20px;
}
@media (max-width: 1100px) {
  .api-filter {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 600px) {
  .api-filter {
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .api-operation {
    flex-wrap: wrap;
    gap: 10px;
    padding: 14px 12px 8px;
  }
  .api-operation-main {
    flex-basis: calc(100% - 82px);
  }
  .api-role {
    max-width: none;
    flex: 1;
  }
  .api-summary {
    padding: 0 12px 14px;
  }
  .api-detail {
    padding: 14px 12px;
  }
  .api-response {
    padding: 10px;
  }
}
</style>
