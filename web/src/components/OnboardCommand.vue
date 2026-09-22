<script setup lang="ts">
import { computed, onUnmounted, ref } from "vue";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";

/**
 * 一条可复制的接入命令。
 *
 * 只负责渲染与复制 —— 命令文本由调用方拼。三种接入方式的参数差异很大
 * （入口两种只差一个可选的 CA，出口那种要带证书、令牌与白名单），把拼装
 * 留在各自的场景里，这里就不会长出一堆互斥的开关。
 */
const props = withDefaults(
  defineProps<{
    command: string;
    /** 同一条命令的手工版：不走脚本，自行放好二进制后用凭据启动一次 */
    manual: string;
    /** 长期有效的接入密钥，而不是 15 分钟的一次性令牌 */
    fixed?: boolean;
    expiresAt?: string;
    /** 命令上方那句话说清楚这条命令装出来的是什么 */
    summary?: string;
  }>(),
  { fixed: false, expiresAt: "", summary: "" },
);

// 复制反馈只有两种结果，用一个状态表示就够了；定时器在卸载时清掉，
// 否则关闭弹窗后它还会去写一个已经卸载的响应式引用。
const copied = ref<"" | "command" | "manual" | "failed">("");
let timer: ReturnType<typeof setTimeout> | undefined;
async function copy(text: string, which: "command" | "manual") {
  clearTimeout(timer);
  try {
    await navigator.clipboard.writeText(text);
    copied.value = which;
    timer = setTimeout(() => (copied.value = ""), 2000);
  } catch {
    // 非安全上下文或用户拒绝授权时，退回让用户自己选中
    copied.value = "failed";
    timer = setTimeout(() => (copied.value = ""), 4000);
  }
}
onUnmounted(() => clearTimeout(timer));

const expiry = computed(() =>
  props.expiresAt
    ? `有效期至 ${formatDateTime(props.expiresAt)}（${displayTimeZoneLabel}）`
    : "",
);
</script>

<template>
  <div class="onboard">
    <p v-if="summary" class="muted small">{{ summary }}</p>
    <p v-if="fixed" class="muted small">
      这条凭据长期不变，装失败、断线或重装都可以直接再跑一遍，不必回控制台。
    </p>
    <p v-else class="warning">
      令牌 15 分钟内有效，且只能使用一次。请在此之前到目标设备上执行下面的命令；
      超时或执行失败都需要重新生成。
    </p>

    <h3>在目标设备上执行</h3>
    <pre class="terminal-output" aria-label="设备接入命令">{{ command }}</pre>
    <div class="actions">
      <button class="primary" type="button" @click="copy(command, 'command')">
        {{ copied === "command" ? "已复制" : "复制接入命令" }}
      </button>
      <span v-if="copied === 'failed'" class="error" role="alert">
        复制失败，请手动选中上面的命令
      </span>
    </div>
    <p class="muted small">
      脚本会按设备架构从本面板下载 Agent，装成 systemd 服务并开机自启。
      目标设备需要 Linux、systemd 与 root 权限。
    </p>
    <p v-if="expiry" class="muted small">{{ expiry }}</p>
    <slot name="after" />

    <details>
      <summary>手动安装</summary>
      <p class="muted small">
        自行构建或拷贝 Agent 到目标设备后，用凭据启动一次即可。注册成功后节点身份
        会持久化到状态文件，之后重启不再需要它 —— 所以那个状态文件必须保留。
      </p>
      <pre class="terminal-output" aria-label="手动接入命令">{{ manual }}</pre>
      <div class="actions">
        <button type="button" @click="copy(manual, 'manual')">
          {{ copied === "manual" ? "已复制" : "复制" }}
        </button>
      </div>
    </details>
  </div>
</template>

<style scoped>
.onboard h3 {
  margin: var(--space-5) 0 var(--space-3);
  font-size: var(--text-sm);
  line-height: var(--text-sm--line-height);
  font-weight: 600;
}
.onboard h3:first-of-type {
  margin-top: 0;
}
.onboard .actions {
  margin: var(--space-3) 0;
}
.onboard details {
  margin-top: var(--space-4);
  border-top: 1px solid var(--color-line-faint);
  padding-top: var(--space-4);
}
.onboard summary {
  font-size: var(--text-sm);
  line-height: var(--text-sm--line-height);
  color: var(--color-ink-muted);
}
.onboard summary:hover {
  color: var(--color-ink);
}
.onboard details > *:not(summary) {
  margin-top: var(--space-3);
}
</style>
