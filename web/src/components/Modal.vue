<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from "vue";
import { state } from "../core/state";
const props = defineProps<{ title: string; busy?: boolean; dirty?: boolean }>();
const emit = defineEmits<{ close: [] }>();
const dialog = ref<HTMLDialogElement>();
const discard = ref(false);
let previous: Element | null = null;
function close() {
  if (props.busy) return;
  if (props.dirty && !discard.value) {
    discard.value = true;
    return;
  }
  emit("close");
}
onMounted(() => {
  previous = document.activeElement;
  state.modalOpen = true;
  dialog.value?.showModal();
});
onUnmounted(() => {
  state.modalOpen = false;
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
