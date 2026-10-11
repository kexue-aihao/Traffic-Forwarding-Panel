<script setup lang="ts">
import ProbeTooltip from "./ProbeTooltip.vue";
import { formatDateTime } from "../core/format";
defineProps<{
  probe: {
    tcp_connections?: string | null;
    udp_connections?: string | null;
    sampled_at: string;
  };
  status: string;
}>();
</script>

<template>
  <ProbeTooltip
    class="probe-connections"
    label="实时速率及连接数量"
    :refresh-key="[
      probe.tcp_connections,
      probe.udp_connections,
      probe.sampled_at,
      status,
    ]"
  >
    <slot />
    <template #tooltip>
      <strong>机器连接数量</strong>
      <dl>
        <div>
          <dt>TCP 连接</dt>
          <dd>{{ probe.tcp_connections ?? "未知" }}</dd>
        </div>
        <div>
          <dt>UDP 套接字</dt>
          <dd>{{ probe.udp_connections ?? "未知" }}</dd>
        </div>
      </dl>
      <p>TCP 含已建立及等待关闭的连接；UDP 按套接字统计。含 IPv4/IPv6。</p>
      <p>采样于 {{ formatDateTime(probe.sampled_at) }} · {{ status }}</p>
      <p v-if="probe.tcp_connections == null || probe.udp_connections == null">
        未上报或采集不可用时显示未知；旧 Agent 请先升级。
      </p>
    </template>
  </ProbeTooltip>
</template>

<style scoped>
dl {
  margin: 8px 0;
}
dl > div {
  display: flex;
  justify-content: space-between;
  gap: 16px;
}
dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
  font-weight: 600;
}
p {
  margin: 6px 0 0;
  color: var(--color-ink-muted);
}
</style>
