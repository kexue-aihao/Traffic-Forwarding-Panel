<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, useId, watch } from "vue";

const props = defineProps<{
  label: string;
  refreshKey?: unknown;
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
  () => props.refreshKey,
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
    class="probe-tooltip-trigger"
    tabindex="0"
    role="group"
    :aria-label="label"
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
        class="probe-tooltip"
        role="tooltip"
        :style="position"
        @mouseenter="enter"
        @mouseleave="leave"
      >
        <div class="probe-tooltip-content"><slot name="tooltip" /></div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.probe-tooltip-trigger {
  cursor: help;
}
.probe-tooltip-trigger:focus-visible {
  outline: 2px solid var(--color-accent);
  outline-offset: 3px;
}
.probe-tooltip {
  position: fixed;
  z-index: 50;
  width: min(300px, calc(100vw - 24px));
  padding-block: 8px;
  box-sizing: border-box;
}
.probe-tooltip-content {
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
</style>
