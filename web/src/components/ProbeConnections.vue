<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, useId, watch } from "vue";
import { formatDateTime } from "../core/format";

const props = defineProps<{
  probe: {
    tcp_connections?: string | null;
    udp_connections?: string | null;
    sampled_at: string;
  };
  status: string;
}>();
const id = useId();
const root = ref<HTMLElement>();
const tooltip = ref<HTMLElement>();
const hovered = ref(false);
const focused = ref(false);
const dismissed = ref(false);
const visible = computed(
  () => (hovered.value || focused.value) && !dismissed.value,
);
const position = ref({ left: "0px", top: "0px" });
let leaveTimer: ReturnType<typeof setTimeout> | undefined;

function place() {
  if (!root.value || !tooltip.value) return;
  const rect = root.value.getBoundingClientRect();
  const tip = tooltip.value.getBoundingClientRect();
  const left = Math.max(12, Math.min(rect.left, innerWidth - tip.width - 12));
  const top =
    rect.bottom + tip.height <= innerHeight - 12
      ? rect.bottom
      : Math.max(12, rect.top - tip.height);
  position.value = { left: `${left}px`, top: `${top}px` };
}
function enter() {
  clearTimeout(leaveTimer);
  hovered.value = true;
  dismissed.value = false;
}
function leave() {
  clearTimeout(leaveTimer);
  leaveTimer = setTimeout(() => {
    hovered.value = false;
  }, 120);
}
function focus() {
  focused.value = true;
  dismissed.value = false;
}
function tap() {
  root.value?.focus();
  focus();
}
function escape(event: KeyboardEvent) {
  if (event.key === "Escape") dismissed.value = true;
}
function listen(add: boolean) {
  if (add) {
    document.addEventListener("keydown", escape);
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
  } else {
    document.removeEventListener("keydown", escape);
    window.removeEventListener("resize", place);
    window.removeEventListener("scroll", place, true);
  }
}
watch(visible, async (show) => {
  listen(show);
  if (show) {
    await nextTick();
    place();
  }
});
watch(
  () => [
    props.probe.tcp_connections,
    props.probe.udp_connections,
    props.probe.sampled_at,
    props.status,
  ],
  async () => {
    if (visible.value) {
      await nextTick();
      place();
    }
  },
);
onUnmounted(() => {
  clearTimeout(leaveTimer);
  listen(false);
});
</script>

<template>
  <div
    ref="root"
    class="probe-connections"
    tabindex="0"
    role="group"
    aria-label="实时速率及连接数量"
    :aria-describedby="visible ? id : undefined"
    @mouseenter="enter"
    @mouseleave="leave"
    @focus="focus"
    @blur="focused = false"
    @click="tap"
  >
    <slot />
    <Teleport to="body">
      <div
        v-if="visible"
        :id="id"
        ref="tooltip"
        class="probe-connections-tooltip"
        role="tooltip"
        :style="position"
        @mouseenter="enter"
        @mouseleave="leave"
      >
        <div class="probe-connections-content">
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
          <p
            v-if="
              probe.tcp_connections == null || probe.udp_connections == null
            "
          >
            未上报或采集不可用时显示未知；旧 Agent 请先升级。
          </p>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.probe-connections {
  cursor: help;
}
.probe-connections:focus-visible {
  outline: 2px solid var(--color-accent);
  outline-offset: 3px;
}
.probe-connections-tooltip {
  position: fixed;
  z-index: 50;
  width: min(300px, calc(100vw - 24px));
  padding-block: 8px;
  box-sizing: border-box;
}
.probe-connections-content {
  padding: 12px 14px;
  border: 1px solid var(--color-line-strong);
  border-radius: var(--radius-lg);
  background: var(--color-bg-1);
  color: var(--color-ink);
  box-shadow: var(--shadow-raise);
  font-size: 12px;
  line-height: 1.5;
  white-space: normal;
  overflow-wrap: anywhere;
}
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
