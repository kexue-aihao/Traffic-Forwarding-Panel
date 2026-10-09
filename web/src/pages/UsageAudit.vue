<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { api, errorText } from "../core/api";
import { formatDateTime } from "../core/format";
interface Usage {
  id: string;
  rule_id: string;
  lease_id: string;
  node_id: string;
  entitlement_id: string;
  kind?: string;
  window_id?: string;
  window_sequence?: number;
  upload_bytes: string;
  download_bytes: string;
  started_at: string;
  ended_at: string;
}
const items = ref<Usage[]>([]),
  total = ref(0),
  page = ref(1),
  ruleID = ref(""),
  error = ref("");
const recoveryBytes = computed(() =>
  items.value
    .filter((u) => u.kind === "recovery")
    .reduce(
      (sum, u) => sum + BigInt(u.upload_bytes) + BigInt(u.download_bytes),
      0n,
    )
    .toString(),
);
let loadSequence = 0;
async function load() {
  const sequence = ++loadSequence;
  try {
    error.value = "";
    const result = await api<{ items: Usage[]; total: number }>(
      `/usage-audit?page=${page.value}&rule_id=${encodeURIComponent(ruleID.value.trim())}`,
    );
    if (sequence !== loadSequence) return;
    items.value = result.items;
    total.value = result.total;
  } catch (e) {
    if (sequence === loadSequence) error.value = errorText(e);
  }
}
async function filter() {
  page.value = 1;
  await load();
}
onMounted(load);
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">USAGE</p>
        <h1>流量计量审计</h1>
      </div>
      <button @click="load">刷新</button>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <section class="card">
      <p class="muted">
        普通记录按发送前授权的业务流量结算。崩溃恢复记录包含未能确认是否已使用的预留额度，可能高于实际转发量。
      </p>
      <form class="toolbar" @submit.prevent="filter">
        <label
          >规则 ID<input
            v-model="ruleID"
            maxlength="64"
            placeholder="留空查询全部" /></label
        ><button>查询</button>
      </form>
      <p class="muted">本页崩溃保守结算合计 {{ recoveryBytes }} B</p>
      <p v-if="!items.length">暂无计量记录。</p>
      <div v-for="u in items" :key="u.id" class="task-row">
        <div>
          <strong>{{
            u.kind === "recovery" ? "崩溃保守结算" : "普通放行计量"
          }}</strong>
          <p>{{ u.rule_id }} · 节点 {{ u.node_id }}</p>
          <p>
            上传 {{ u.upload_bytes }} B · 下载 {{ u.download_bytes }} B ·
            {{ formatDateTime(u.ended_at) }}
          </p>
          <p class="muted small">记录 {{ u.id }} · 租约 {{ u.lease_id }}</p>
          <p class="muted small">权益周期 {{ u.entitlement_id }}</p>
          <p v-if="u.window_id" class="muted small">
            窗口 {{ u.window_id }} · 序号 {{ u.window_sequence }}
          </p>
        </div>
      </div>
      <div class="toolbar">
        <button
          :disabled="page <= 1"
          @click="
            page--;
            load();
          "
        >
          上一页</button
        ><span>{{ page }} · 共 {{ total }} 条</span
        ><button
          :disabled="page * 20 >= total"
          @click="
            page++;
            load();
          "
        >
          下一页
        </button>
      </div>
    </section>
  </section>
</template>
