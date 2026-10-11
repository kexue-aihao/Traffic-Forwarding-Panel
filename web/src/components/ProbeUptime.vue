<script setup lang="ts">
import { computed } from "vue";
import ProbeTooltip from "./ProbeTooltip.vue";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";

const props = defineProps<{
  probe: {
    uptime_seconds?: string | null;
    booted_at?: string | null;
    sampled_at: string;
  };
  now: number;
  status: string;
}>();
const seconds = computed(() => {
  const value = props.probe.uptime_seconds;
  if (value == null || !/^\d+$/.test(value)) return null;
  const n = Number(value);
  return Number.isSafeInteger(n) && n >= 0 ? n : null;
});
const uptime = computed(() => {
  const n = seconds.value;
  return n == null
    ? "未知"
    : `${Math.floor(n / 86400)} 天 ${Math.floor((n % 86400) / 3600)} 小时`;
});
const reportedBoot = computed(() => Date.parse(props.probe.booted_at || ""));
const bootTime = computed(() => {
  if (Number.isFinite(reportedBoot.value)) return reportedBoot.value;
  if (seconds.value == null) return undefined;
  // Use the Agent's sampling time, never the advancing browser clock, so a
  // legacy/offline Agent's estimated boot time does not drift every 3 seconds.
  const sampled = Date.parse(props.probe.sampled_at);
  const boot = sampled - seconds.value * 1000;
  return Number.isFinite(sampled) && Number.isFinite(new Date(boot).getTime())
    ? boot
    : undefined;
});
</script>

<template>
  <ProbeTooltip
    class="probe-uptime-value"
    label="开机时长及机器时间"
    :refresh-key="[probe, now, status]"
  >
    <strong>{{ uptime }}</strong>
    <template #tooltip>
      <strong>机器时间</strong>
      <dl>
        <div>
          <dt>启动时间</dt>
          <dd>{{ formatDateTime(bootTime) }}</dd>
        </div>
        <div>
          <dt>当前时间</dt>
          <dd>{{ formatDateTime(now) }}</dd>
        </div>
      </dl>
      <p>{{ displayTimeZoneLabel }} · 当前时间每 3 秒更新</p>
      <p>当前时间取自浏览器时钟。</p>
      <p v-if="!Number.isFinite(reportedBoot) && bootTime !== undefined">
        启动时间按机器采样时间和开机时长估算。
      </p>
      <p>采样于 {{ formatDateTime(probe.sampled_at) }} · {{ status }}</p>
      <p v-if="status !== '在线'">启动时间和开机时长保留最后上报值。</p>
    </template>
  </ProbeTooltip>
</template>

<style scoped>
.probe-uptime-value {
  display: inline-block;
}
dl {
  margin: 8px 0;
}
dl > div {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: 4px 12px;
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
