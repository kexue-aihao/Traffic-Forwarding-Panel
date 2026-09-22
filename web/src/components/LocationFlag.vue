<script setup lang="ts">
import { computed } from "vue";
import { flagFor, known, countryName, FLAG_WIDTH, FLAG_HEIGHT } from "../core/flags";

/**
 * 位置图标。
 *
 * 探针页面按归属地给每台机器一个图标，用来快速区分设备。认得出地区就画旗，
 * 认不出就画一个写着地区码的中性徽章 —— 两者都能达到「一眼分出这是哪台」
 * 的目的，区别只在于要不要声称自己知道那是哪个国家。
 */
const props = withDefaults(
  defineProps<{
    code?: string | null;
    name?: string | null;
    size?: number;
  }>(),
  { size: 22 },
);
const flag = computed(() => flagFor(props.code));
const title = computed(() => {
  const label = props.name || countryName(props.code);
  if (!label) return "未知位置";
  return known(props.code) ? label : `${label}（无对应图标）`;
});
</script>
<template>
  <span v-if="!flag" class="location-flag unknown" :title="title" aria-hidden="true">
    <svg :width="size" :height="(size * FLAG_HEIGHT) / FLAG_WIDTH" viewBox="0 0 30 20">
      <rect x="0" y="0" width="30" height="20" rx="2" fill="currentColor" opacity="0.12" />
      <circle cx="15" cy="10" r="5.4" fill="none" stroke="currentColor" stroke-width="1.4" />
      <ellipse cx="15" cy="10" rx="2.4" ry="5.4" fill="none" stroke="currentColor" stroke-width="1.1" />
      <path d="M9.6 10h10.8" stroke="currentColor" stroke-width="1.1" />
    </svg>
  </span>
  <span v-else class="location-flag" :title="title" role="img" :aria-label="title">
    <svg :width="size" :height="(size * FLAG_HEIGHT) / FLAG_WIDTH" viewBox="0 0 30 20">
      <template v-for="(shape, i) in flag.shapes" :key="i">
        <rect
          v-if="shape.t === 'rect'"
          :x="shape.x"
          :y="shape.y"
          :width="shape.w"
          :height="shape.h"
          :fill="shape.fill"
        />
        <circle
          v-else-if="shape.t === 'circle'"
          :cx="shape.cx"
          :cy="shape.cy"
          :r="shape.r"
          :fill="shape.fill"
        />
        <path v-else :d="shape.d" :fill="shape.fill" />
      </template>
      <rect
        x="0.4"
        y="0.4"
        width="29.2"
        height="19.2"
        rx="2"
        fill="none"
        stroke="currentColor"
        stroke-width="0.8"
        opacity="0.35"
      />
    </svg>
    <b v-if="!known(code)" class="location-code">{{ flag.code }}</b>
  </span>
</template>
