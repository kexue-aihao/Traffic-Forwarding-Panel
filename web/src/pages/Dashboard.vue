<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { api, errorText } from "../core/api";
import { state } from "../core/state";
const stats = ref<{ label: string; value: number; to: string }[]>([]);
// 设备组页只有管理员能进：普通账号点进去只会看到「无权访问」，那就把他带到
// 探针页 —— 那里列的就是他有权查看的那几台机器。
const groupsTo = computed(() => (state.user?.role === "admin" ? "/groups" : "/probes"));
const error = ref("");
const busy = ref(true);
async function load() {
  busy.value = true;
  error.value = "";
  try {
    const [rules, nodes, groups] = await Promise.all([
      api<{ total: number }>("/rules?page_size=1"),
      api<{ total: number }>("/nodes?page_size=1"),
      api<{ total: number }>("/groups?page_size=1"),
    ]);
    stats.value = [
      { label: "转发规则", value: rules.total, to: "/rules" },
      { label: "入口设备", value: nodes.total, to: "/probes" },
      { label: "设备组", value: groups.total, to: groupsTo.value },
    ];
  } catch (e) {
    error.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
onMounted(load);
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">NETWORK OVERVIEW</p>
        <h1>你好，{{ state.user?.username }}</h1>
        <p class="muted">你的网络运行状态，集中呈现。</p>
      </div>
      <RouterLink class="button primary" to="/rules">管理转发</RouterLink>
    </div>
    <p v-if="error" class="error" role="alert">
      {{ error }} <button @click="load">重试</button>
    </p>
    <div v-else-if="busy" class="stats" aria-busy="true">
      <p class="sr-only" role="status">正在读取数据</p>
      <div v-for="n in 3" :key="n" class="skeleton skeleton-stat" />
    </div>
    <div v-else class="stats">
      <RouterLink
        v-for="item in stats"
        :key="item.label"
        class="card stat"
        :to="item.to"
        ><span class="muted">{{ item.label }}</span
        ><strong>{{ item.value }}</strong
        ><span class="small">查看详情 →</span></RouterLink
      >
    </div>
    <div class="card intro">
      <p class="eyebrow">LIVE OBSERVABILITY</p>
      <h2>每一条连接，都有迹可循</h2>
      <p class="muted">
        实时探针展示节点实际采样数据。配置保存和节点实际应用分别显示，方便定位异常。
      </p>
      <a href="#/probes" target="_blank" rel="noopener">打开实时探针 →</a>
    </div>
  </section>
</template>
