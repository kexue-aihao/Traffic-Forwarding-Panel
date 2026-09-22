<script setup lang="ts">
import { computed, ref } from "vue";
import Select from "./Select.vue";
import OnboardCommand from "./OnboardCommand.vue";

/**
 * 设备接入面板：先按场景选接入方式，再给出对应的那条命令。
 *
 * 三种方式的差别不在「装哪个二进制」—— 都是同一个 Agent —— 而在这台设备
 * 之后扮演什么角色，以及因此需要哪些参数：
 *
 *   入口直出  入口设备，规则直接把流量转发到目标。除了可选的 CA，什么都不用填。
 *   入口      入口设备，规则把流量交给出口隧道。出口用私有 CA 签名时需要 -c。
 *   隧道      出口设备（隧道端点），要证书、私钥、出口令牌与允许目标白名单 ——
 *             它不跟面板通信，所以命令里同时带上接入密钥，用来装注册用的 Agent。
 */
const props = defineProps<{ accessKey: string }>();

const origin = location.origin;
type Mode = "direct" | "entry" | "exit";
const mode = ref<Mode>("direct");
const modes: { id: Mode; label: string; summary: string }[] = [
  {
    id: "direct",
    label: "入口直出",
    summary: "设备作为入口，直接把流量转发到目标，不经过出口",
  },
  {
    id: "entry",
    label: "入口",
    summary: "设备作为入口，把流量交给出口隧道（建议出口有公网域名）",
  },
  {
    id: "exit",
    label: "隧道",
    summary: "设备作为出口端点，供入口机器连接；出口服务本身不连面板",
  },
];

// ── 入口两种方式共用的可选 CA ────────────────────────────────────
// 出口（或面板）用私有 CA 签发证书时才需要，公共 CA 留空即可。
const caURL = ref("");
const caArg = computed(() => (caURL.value ? ` -c '${caURL.value}'` : ""));

// ── 出口方式的参数 ──────────────────────────────────────────────
// 出口令牌由这里生成、并且**同一个值**要填进出口管理的表单；入口建规则时
// 再用一次。可编辑，操作方想自己定就用自己定的。
function randomToken() {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}
const exitToken = ref(randomToken());
const serverName = ref("");
const listen = ref("0.0.0.0:9443");
const transport = ref("tls");
const certPath = ref("/etc/ssl/exit.crt");
const keyPath = ref("/etc/ssl/exit.key");
const allow = ref("");

const entryCommand = computed(
  () =>
    `bash <(curl -fLsS ${origin}/download/agent-install.sh) -t '${props.accessKey}' -u '${origin}'${caArg.value}`,
);
const entryManual = computed(
  () =>
    `TFP_ENROLLMENT_TOKEN='${props.accessKey}' tfp-agent -panel '${origin}'${caArg.value ? ` -ca ./ca.pem` : ""}`,
);

const exitReady = computed(
  () => serverName.value.trim() !== "" && allow.value.trim() !== "",
);
const exitCommand = computed(
  () =>
    `bash <(curl -fLsS ${origin}/download/agent-install.sh) -t '${props.accessKey}' -u '${origin}' -m exit` +
    ` -S '${serverName.value.trim()}' -e '${exitToken.value}'` +
    ` -C '${certPath.value}' -K '${keyPath.value}' -w '${allow.value.trim()}'` +
    ` -l '${listen.value}' -p '${transport.value}'`,
);
const exitManual = computed(
  () =>
    `TFP_ENROLLMENT_TOKEN='${props.accessKey}' TFP_EXIT_TOKEN='${exitToken.value}' tfp-agent \\\n` +
    `  -mode exit -exit-id "$(hostname)" -listen '${listen.value}' -transport '${transport.value}' \\\n` +
    `  -cert '${certPath.value}' -key '${keyPath.value}' -allow '${allow.value.trim()}'`,
);

const exitTokenCopied = ref(false);
let timer: ReturnType<typeof setTimeout> | undefined;
async function copyExitToken() {
  clearTimeout(timer);
  try {
    await navigator.clipboard.writeText(exitToken.value);
    exitTokenCopied.value = true;
  } catch {
    exitTokenCopied.value = false;
    return;
  }
  timer = setTimeout(() => (exitTokenCopied.value = false), 2000);
}
</script>

<template>
  <div class="onboard-panel">
    <fieldset class="onboard-modes">
      <legend class="sr-only">接入方式</legend>
      <label v-for="m in modes" :key="m.id" class="onboard-mode">
        <input
          v-model="mode"
          type="radio"
          name="onboard-mode"
          :value="m.id"
          :aria-label="m.label"
        />
        <span>
          <strong>{{ m.label }}</strong>
          <small>{{ m.summary }}</small>
        </span>
      </label>
    </fieldset>

    <template v-if="mode !== 'exit'">
      <div class="onboard-field">
        <label
          >出口/面板使用私有 CA 时的根证书地址（留空即用公共 CA）
          <input
            v-model="caURL"
            type="url"
            placeholder="https://panel.example.com/download/ca.pem"
            aria-label="根证书地址"
          />
        </label>
      </div>
      <OnboardCommand
        :command="entryCommand"
        :manual="entryManual"
        fixed
        :summary="
          mode === 'direct'
            ? '装好后建规则时把「出口选择」留成直接转发，流量由这台设备直接发往目标。这一段不额外加密 —— 传输安全由业务协议自身决定。'
            : '装好后建规则时选一条出口，流量走隧道到出口再出去；入口到出口全程 TLS 1.3。出口用私有 CA 签名时，上面的 CA 地址要填。'
        "
      />
    </template>

    <template v-else>
      <p class="muted small">
        出口是流量的中转端点。出口服务自己不连面板，所以下面这条命令同时带上接入
        密钥，用来多装一个注册用的 Agent —— 没有它，设备不会出现在控制台里，
        出口列表的「在线」列永远是离线。
      </p>
      <div class="onboard-grid">
        <label
          >出口域名（证书必须覆盖它）
          <input
            v-model="serverName"
            placeholder="exit.example.com"
            aria-label="出口域名"
          />
        </label>
        <label
          >承载
          <Select v-model="transport" aria-label="出口承载">
            <option value="tls">tls</option>
            <option value="ws">ws</option>
            <option value="wss">wss</option>
            <option value="http">http</option>
          </Select>
        </label>
        <label
          >证书路径（设备上的绝对路径）
          <input v-model="certPath" aria-label="证书路径" />
        </label>
        <label
          >私钥路径
          <input v-model="keyPath" aria-label="私钥路径" />
        </label>
        <label
          >监听地址
          <input v-model="listen" aria-label="监听地址" />
        </label>
        <label
          >出口令牌（至少 16 字符）
          <input v-model="exitToken" aria-label="出口令牌" />
        </label>
      </div>
      <label
        >允许转发的目标（精确匹配，逗号分隔）
        <input
          v-model="allow"
          placeholder="tcp|10.20.0.11:27015,udp|10.20.0.11:5353"
          aria-label="允许目标"
        />
      </label>
      <p class="muted small">
        出口只转发这里列出的目标，没有通配符。证书与私钥由你自己的 CA 签发，
        面板不代为签发也不保存私钥。
      </p>

      <OnboardCommand
        v-if="exitReady"
        :command="exitCommand"
        :manual="exitManual"
        fixed
      >
        <template #after>
          <div class="actions">
            <button type="button" @click="copyExitToken">
              {{ exitTokenCopied ? "令牌已复制" : "复制出口令牌" }}
            </button>
            <span class="muted small">
              下面「出口管理 → 新增」里的「令牌」要填同一个值
            </span>
          </div>
        </template>
      </OnboardCommand>
      <p v-else class="empty">填好出口域名与允许目标后生成命令。</p>
    </template>
  </div>
</template>

<style scoped>
.onboard-panel > * + * {
  margin-top: var(--space-4);
}

.onboard-modes {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--space-2);
  border: 0;
  padding: 0;
  margin: 0;
}
.onboard-mode {
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
  margin: 0;
  padding: var(--space-3);
  border: 0;
  border-radius: var(--radius-lg);
  cursor: pointer;
  transition: background-color var(--duration-micro) var(--ease-state);
}
.onboard-mode:hover {
  background: var(--color-hover);
}
.onboard-mode:has(input:checked) {
  background: var(--color-active);
}
.onboard-mode input {
  width: 16px;
  height: 16px;
  min-height: 0;
  margin: 2px 0 0;
  flex: none;
  accent-color: var(--color-accent);
}
.onboard-mode strong {
  display: block;
  font-size: var(--text-sm);
  line-height: var(--text-sm--line-height);
  font-weight: 600;
  color: var(--color-ink);
}
.onboard-mode small {
  display: block;
  font-size: var(--text-2xs);
  line-height: var(--text-2xs--line-height);
  color: var(--color-ink-subtle);
}

.onboard-field label,
.onboard-panel > label {
  margin-bottom: 0;
}
.onboard-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 0 var(--space-4);
}

@media (max-width: 640px) {
  .onboard-modes,
  .onboard-grid {
    grid-template-columns: 1fr;
  }
}
</style>
