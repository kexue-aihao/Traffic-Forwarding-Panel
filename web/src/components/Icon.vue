<script setup lang="ts">
defineProps<{ name: string }>();
</script>
<template>
  <svg
    class="icon"
    aria-hidden="true"
    fill="none"
    stroke="currentColor"
    stroke-width="1.7"
    stroke-linecap="round"
    stroke-linejoin="round"
  >
    <use :href="`/assets/icons.svg#${name}`" />
  </svg>
</template>
