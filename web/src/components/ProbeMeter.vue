<script setup lang="ts">
import { computed } from "vue";
const props = defineProps<{
  label: string;
  value: number | null;
  detail?: string;
}>();
const value = computed(() =>
  props.value === null || !Number.isFinite(props.value)
    ? null
    : Math.max(0, Math.min(100, props.value)),
);
</script>
<template>
  <div class="probe-meter" :title="detail">
    <div>
      <span>{{ label }}</span
      ><strong>{{ value === null ? "未知" : value.toFixed(1) + "%" }}</strong>
    </div>
    <meter
      v-if="value !== null"
      :min="0"
      :max="100"
      :value="value"
      :aria-label="label"
      :class="{ high: value >= 90 }"
    />
    <span v-else class="meter-unknown" />
    <small v-if="detail" class="muted">{{ detail }}</small>
  </div>
</template>
