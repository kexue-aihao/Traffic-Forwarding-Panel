<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from "vue";
import { state } from "../core/state";
const props = defineProps<{ title: string; busy?: boolean; dirty?: boolean }>();
const emit = defineEmits<{ close: [] }>();
const dialog = ref<HTMLDialogElement>();
const discard = ref(false);
let previous: Element | null = null;
let exitTimer: ReturnType<typeof setTimeout> | undefined;

/**
 * 退场的时长，取自 CSS 的 --duration-state。
 *
 * 令牌是唯一的来源，这里只是把它读出来；读不到就退回 200ms —— 宁可动画早
 * 结束一点，也不要让弹窗卡在屏幕上。
 */
function exitDuration() {
  const d = dialog.value;
  const raw = d
    ? getComputedStyle(d).getPropertyValue("--duration-state").trim()
    : "";
  const ms = Number.parseFloat(raw);
  if (!Number.isFinite(ms) || ms <= 0) return 200;
  return raw.endsWith("ms") ? ms : ms * 1000;
}

/**
 * 关闭。
 *
 * 顺序很关键：先 close() 让 CSS 的 allow-discrete 过渡把退场播完，再通知父
 * 组件。反过来写的话，父组件一收到 close 就 v-if 掉这个组件，元素从 DOM 上
 * 消失，任何过渡都来不及跑 —— 弹窗会「啪」地不见。
 */
function close() {
  if (props.busy) return;
  if (props.dirty && !discard.value) {
    discard.value = true;
    return;
  }
  // 已经在退场了：连按 Esc 不该把它截断
  if (exitTimer) return;
  const d = dialog.value;
  if (d?.open) {
    d.close();
    exitTimer = setTimeout(() => emit("close"), exitDuration());
    return;
  }
  emit("close");
}
onMounted(() => {
  previous = document.activeElement;
  state.modalDepth++;
  dialog.value?.showModal();
});
onUnmounted(() => {
  clearTimeout(exitTimer);
  state.modalDepth = Math.max(0, state.modalDepth - 1);
  dialog.value?.close();
  if (previous instanceof HTMLElement) previous.focus();
});
watch(
  () => state.user,
  (user) => {
    if (!user) emit("close");
  },
);
</script>
<template>
  <dialog ref="dialog" aria-labelledby="modal-title" @cancel.prevent="close">
    <div class="dialog-head">
      <h2 id="modal-title">{{ title }}</h2>
      <button
        type="button"
        :disabled="busy"
        aria-label="关闭对话框"
        @click="close"
      >
        ×
      </button>
    </div>
    <p v-if="discard" class="warning">尚有未保存内容。再次关闭将放弃修改。</p>
    <slot />
  </dialog>
</template>
