<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import Modal from "../components/Modal.vue";
import { api, errorText } from "../core/api";
import { state, notice } from "../core/state";
import { displayTimeZoneLabel, formatDateTime } from "../core/format";
interface Token {
  id: string;
  name: string;
  // 明文只在创建/重置那一次返回；列表里只有前缀，用来分辨「这把是哪一把」。
  prefix: string;
  scope: string;
  created_at: string;
  expires_at: string | null;
  permanent: boolean;
  last_used_at?: string | null;
}
const tokens = ref<Token[]>([]);
const error = ref("");
const formError = ref("");
const busy = ref(false);
const mode = ref<"password" | "token" | null>(null);
const current = ref("");
const password = ref("");
const confirm = ref("");
const name = ref("");
const days = ref(30);
// 永久凭据是明确的选择，不是默认值：忘了填有效期不该悄悄发一把不过期的钥匙。
const permanent = ref(false);
const secret = ref("");
const scopeGroups = ref<string[]>([]);
const groups = ref<{ id: string; name: string }[]>([]);
const revoke = ref<Token | null>(null);
const dirty = computed(
  () =>
    !!(current.value || password.value || confirm.value || name.value) &&
    !secret.value,
);
async function load() {
  error.value = "";
  try {
    const all: Token[] = [];
    let page = 1;
    while (true) {
      const result = await api<{ items: Token[]; total: number }>(
        `/auth/tokens?page_size=100&page=${page}`,
      );
      all.push(...result.items);
      if (!result.items.length || all.length >= result.total) break;
      page++;
    }
    tokens.value = all;
  } catch (e) {
    error.value = errorText(e);
  }
}
function open(next: "password" | "token") {
  mode.value = next;
  current.value = "";
  password.value = "";
  confirm.value = "";
  name.value = "";
  permanent.value = false;
  secret.value = "";
  formError.value = "";
  scopeGroups.value = [];
  if (next === "token") void loadGroups();
}
// 凭据可以只覆盖一部分设备组：脚本只碰一个组时，没必要给它账号的全量权限。
// 不勾就是跟随账号（默认，也是升级上来的旧凭据的行为）。
async function loadGroups() {
  if (groups.value.length) return;
  try {
    const result = await api<{ items: { id: string; name: string }[] }>(
      "/groups?page=1&page_size=100",
    );
    groups.value = result.items;
  } catch (e) {
    formError.value = errorText(e);
  }
}
async function save() {
  if (busy.value) return;
  formError.value = "";
  if (mode.value === "password") {
    const bytes = new TextEncoder().encode(password.value).length;
    if (bytes < 12 || bytes > 72) {
      formError.value = "新密码需要 12–72 字节，中文等字符可能占多个字节。";
      return;
    }
    if (password.value !== confirm.value) {
      formError.value = "两次输入的新密码不一致。";
      return;
    }
  }
  busy.value = true;
  try {
    if (mode.value === "password") {
      await api("/auth/password", "POST", {
        current_password: current.value,
        password: password.value,
      });
      mode.value = null;
      state.user = null;
      current.value = "";
      password.value = "";
      confirm.value = "";
      notice("密码已修改，所有会话与 API Token 已撤销。请使用新密码登录。");
    } else {
      const result = await api<{ token: string }>("/auth/tokens", "POST", {
        name: name.value,
        ...(scopeGroups.value.length
          ? { group_ids: [...scopeGroups.value] }
          : {}),
        ...(permanent.value
          ? { permanent: true }
          : {
              expires_at: new Date(
                Date.now() + days.value * 86400000,
              ).toISOString(),
            }),
      });
      secret.value = result.token;
      await load();
    }
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
async function remove() {
  if (!revoke.value || busy.value) return;
  busy.value = true;
  formError.value = "";
  try {
    await api("/auth/tokens/" + encodeURIComponent(revoke.value.id), "DELETE");
    revoke.value = null;
    notice("API Token 已撤销。");
    await load();
  } catch (e) {
    formError.value = errorText(e);
  } finally {
    busy.value = false;
  }
}
onMounted(load);
</script>
<template>
  <section class="page">
    <div class="page-heading">
      <div>
        <p class="eyebrow">ACCOUNT & ACCESS</p>
        <h1>账号与 API</h1>
        <p class="muted">
          {{ state.user?.username }} ·
          {{ state.user?.role === "admin" ? "管理员" : "普通用户" }}
        </p>
      </div>
      <button @click="open('password')">修改密码</button>
    </div>
    <div class="card">
      <div class="section-heading">
        <h2>API Token</h2>
        <button class="primary" @click="open('token')">创建 Token</button>
      </div>
      <p class="muted">
        Token
        仅访问账号自己的资源。即使由管理员创建，也不具有管理员操作权限。密钥只在创建或重置的那一次展示，
        之后连管理员也取不回来 —— 遗失或泄露只能重置。有效期可以是有限时长，也可以设为永久。
        时间显示使用{{ displayTimeZoneLabel }}。
      </p>
      <p v-if="error" role="alert" class="error">
        无法读取 Token 列表：{{ error }} <button @click="load">重试</button>
      </p>
      <p v-else-if="!tokens.length" class="empty">暂无 API Token。</p>
      <div v-else class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>名称</th>
              <th>密钥前缀</th>
              <th>有效期</th>
              <th>最近使用</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="token in tokens" :key="token.id">
              <td data-label="名称">{{ token.name }}</td>
              <td data-label="密钥前缀">
                <code>{{ token.prefix || "—" }}</code>
              </td>
              <td data-label="有效期">
                {{ token.permanent ? "永久有效" : formatDateTime(token.expires_at) }}
              </td>
              <td data-label="最近使用">
                {{ token.last_used_at ? formatDateTime(token.last_used_at) : "尚未使用" }}
              </td>
              <td data-label="操作">
                <button
                  class="danger"
                  @click="
                    revoke = token;
                    formError = '';
                  "
                >
                  撤销
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
    <p class="small muted">
      账号自助恢复尚未接入。遗失登录凭据请联系部署管理员。
    </p>
    <Modal
      v-if="mode"
      :title="mode === 'password' ? '修改密码' : '创建 API Token'"
      :busy="busy"
      :dirty="dirty"
      @close="
        mode = null;
        secret = '';
      "
    >
      <form @submit.prevent="save">
        <p v-if="formError" role="alert" class="error">{{ formError }}</p>
        <template v-if="secret"
          ><p class="warning">请立即保存。关闭后无法再次查看此密钥。</p>
          <textarea
            aria-label="API Token 密钥"
            :value="secret"
            readonly
            rows="4"
          /><button
            type="button"
            @click="
              mode = null;
              secret = '';
            "
          >
            已保存，关闭
          </button></template
        ><template v-else
          ><template v-if="mode === 'password'"
            ><p class="warning">
              修改成功后，所有会话和 API Token 都会被撤销，需要重新登录。
            </p>
            <label
              >当前密码<input
                v-model="current"
                type="password"
                autocomplete="current-password"
                required /></label
            ><label
              >新密码<input
                v-model="password"
                type="password"
                autocomplete="new-password"
                required /></label
            ><label
              >确认新密码<input
                v-model="confirm"
                type="password"
                autocomplete="new-password"
                required /></label></template
          ><template v-else
            ><p class="small muted">
              有效期从提交时起计算，每天为 24 小时；到期时间显示为{{
                displayTimeZoneLabel
              }}。
            </p>
            <label
              >Token 名称<input
                v-model="name"
                required
                maxlength="190" /></label
            ><label class="check"
              ><input
                v-model="permanent"
                type="checkbox"
              />永久有效（不过期）</label
            ><label v-if="!permanent"
              >有效天数<input
                v-model.number="days"
                type="number"
                min="1"
                max="364"
                required /></label
            ><p v-else class="warning">
              永久 Token 不会自动失效，泄露后风险一直存在。脚本用不上了要记得撤销。
            </p>
            <fieldset v-if="groups.length">
              <legend>限定设备组（可多选）</legend>
              <label v-for="item in groups" :key="item.id" class="check">
                <input
                  v-model="scopeGroups"
                  type="checkbox"
                  :value="item.id"
                  :aria-label="`限定到 ${item.name}`"
                />{{ item.name }}
              </label>
              <p class="small muted">
                不勾选表示这把凭据跟随账号的全部授权；勾了之后就只覆盖这几个设备组，
                范围外的机器、规则与设备地址接口都对它不可见。
              </p>
            </fieldset></template
          ><div class="form-actions">
            <button
              class="primary"
              :disabled="busy"
              :data-busy="String(busy)"
              :aria-busy="busy"
            >
              确认提交
            </button>
          </div></template
        >
      </form>
    </Modal>
    <Modal
      v-if="revoke"
      title="撤销 API Token"
      :busy="busy"
      @close="revoke = null"
      ><p>撤销 {{ revoke.name }} 后，使用它的自动化调用将失效。</p>
      <p v-if="formError" role="alert" class="error">{{ formError }}</p>
      <div class="form-actions">
        <button class="danger" :disabled="busy" @click="remove">
          确认撤销
        </button>
      </div></Modal
    >
  </section>
</template>
