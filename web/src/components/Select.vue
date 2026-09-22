<script setup lang="ts">
import {
  computed,
  onBeforeUnmount,
  onMounted,
  onUpdated,
  ref,
  useAttrs,
} from "vue";

/**
 * 下拉框。
 *
 * ── 为什么需要它 ────────────────────────────────────────────
 *
 * 原生 <select> 的**弹出层由操作系统绘制**：选中项的高亮用系统强调色（Windows
 * 上是一条亮蓝）、文字颜色也跟着系统走，CSS 完全碰不到。结果是这套暗色界面里
 * 会突然弹出一块系统风格的列表。`appearance: none` 只能换掉收起来时的箭头。
 *
 * ── 做法 ────────────────────────────────────────────────────
 *
 * 原生 <select> **仍然是那个控件**：标签、键盘、表单语义、辅助技术、自动化
 * 全部照旧走它，只是把它的原生弹层按掉（`mousedown` 上 preventDefault），
 * 换成下面这一层自己画的列表。列表标了 aria-hidden —— 它对屏幕阅读器没有
 * 意义，真正可读的是原生 select 自己。
 *
 * 不换成「按钮 + 隐藏 select」那种双控件写法：那样同一个标签会匹配到两个
 * 元素，辅助技术也会读出两个控件，得不偿失。
 */
defineOptions({ inheritAttrs: false });

const props = defineProps<{ modelValue?: string | number }>();
const emit = defineEmits<{ "update:modelValue": [string]; change: [string] }>();
const attrs = useAttrs();

interface Item {
  value: string;
  label: string;
  disabled: boolean;
}

// 有的调用点写 :value 而不是 v-model，两者都要认。attrs 里的 value 由
// v-bind 落成属性，显式绑定必须排在它后面才能覆盖。
const bound = computed(
  () => props.modelValue ?? (attrs.value as string | number | undefined),
);

const native = ref<HTMLSelectElement | null>(null);
const root = ref<HTMLElement | null>(null);
const open = ref(false);
const active = ref(0);
const items = ref<Item[]>([]);

// 选项是父组件通过插槽渲染的 <option>，每次打开时重读一遍，
// 这样 v-for 出来的、label 动态变化的都能拿到当前值。
function read() {
  const el = native.value;
  items.value = el
    ? Array.from(el.options).map((o) => ({
        value: o.value,
        label: o.text,
        disabled: o.disabled,
      }))
    : [];
}

function show() {
  read();
  active.value = Math.max(
    0,
    items.value.findIndex((i) => i.value === String(bound.value ?? "")),
  );
  open.value = true;
}
function close() {
  open.value = false;
}

// 提交一个值：改原生 select 再派发 change，父组件的 v-model 与所有监听
// change 的地方都照常工作。
function pick(item: Item) {
  const el = native.value;
  if (item.disabled || !el) return;
  el.value = item.value;
  // 只派发原生 change，由下面唯一的那个处理器统一往外发事件 ——
  // 这样鼠标选、键盘选、自动化 selectOption 三条路走的是同一条。
  el.dispatchEvent(new Event("change", { bubbles: true }));
  close();
  el.focus();
}

// 拦掉原生弹层，但仍然要把焦点给它 —— mousedown 的 preventDefault 会连
// 聚焦一起取消，所以这里手动补上。
function onMouseDown(event: MouseEvent) {
  const el = native.value;
  if (!el || el.disabled) return;
  event.preventDefault();
  el.focus();
  open.value ? close() : show();
}

/**
 * 值是外部改的（自动化 selectOption）还是我们改的（pick），最终都会走到这里：
 * 统一往外发 update:modelValue 与 change 两种事件。
 *
 * 有的调用点写 v-model，有的写 :value + @change="handler" 直接取值 —— 两条路
 * 都得通，否则其中一种写法会静默失效。
 */
function onNativeChange() {
  const value = native.value?.value ?? "";
  emit("update:modelValue", value);
  emit("change", value);
}

function onKey(event: KeyboardEvent) {
  const el = native.value;
  if (!el || el.disabled) return;
  const last = items.value.length - 1;
  const move = (from: number, step: number) => {
    let i = from;
    for (let n = 0; n <= last; n++) {
      i = (i + step + items.value.length) % items.value.length;
      if (!items.value[i]?.disabled) return i;
    }
    return from;
  };
  switch (event.key) {
    case "ArrowDown":
    case "ArrowUp":
      // 自己接管这两个键：原生在闭合状态下会直接改值但不给任何可见反馈，
      // 而这里要的是「展开并按方向移动高亮」。
      event.preventDefault();
      if (!open.value) show();
      else active.value = move(active.value, event.key === "ArrowDown" ? 1 : -1);
      break;
    case "Home":
    case "End":
      if (open.value) {
        event.preventDefault();
        active.value = move(event.key === "Home" ? -1 : 0, event.key === "Home" ? 1 : -1);
      }
      break;
    case "Enter":
      if (open.value && items.value[active.value]) {
        event.preventDefault();
        pick(items.value[active.value]);
      }
      break;
    case "Escape":
      if (open.value) {
        event.preventDefault();
        close();
      }
      break;
    case "Tab":
      close();
      break;
    default:
      // 首字母跳转：原生 select 自带，接管之后要补回来
      if (open.value && event.key.length === 1 && !event.metaKey && !event.ctrlKey) {
        const i = items.value.findIndex((o) =>
          o.label.toLowerCase().startsWith(event.key.toLowerCase()),
        );
        if (i >= 0) active.value = i;
      }
  }
}

// 选项是父组件稍后渲染进来的。如果 :value 先于 <option> 应用，浏览器会直接
// 丢掉它（赋值时那个值还不在选项里），绑定就静默失效了 —— 表现出来是「选中的
// 是空」。每次更新后再对一次，成本可以忽略。
onUpdated(() => {
  const el = native.value;
  const want = bound.value;
  if (el && want !== undefined && el.value !== String(want)) el.value = String(want);
});

// 点外面关掉。用 pointerdown 而不是 click：click 在拖选等场景下会迟到。
function onDocumentPointer(event: PointerEvent) {
  if (open.value && !root.value?.contains(event.target as Node)) close();
}
onMounted(() => document.addEventListener("pointerdown", onDocumentPointer));
onBeforeUnmount(() =>
  document.removeEventListener("pointerdown", onDocumentPointer),
);
</script>

<template>
  <span ref="root" class="select">
    <select
      ref="native"
      v-bind="attrs"
      :value="bound"
      @mousedown="onMouseDown"
      @keydown="onKey"
      @blur="close"
      @change="onNativeChange"
    >
      <slot />
    </select>
    <!-- 纯视觉的替代品，对辅助技术没有意义 —— 可读的是上面那个原生 select -->
    <ul v-if="open" class="select-list" aria-hidden="true">
      <li
        v-for="(item, index) in items"
        :key="item.value"
        class="select-option"
        :class="{
          'is-active': index === active,
          'is-selected': item.value === String(bound ?? ''),
        }"
        @pointerenter="active = index"
        @click="pick(item)"
      >
        {{ item.label }}
      </li>
    </ul>
  </span>
</template>

<style scoped>
.select {
  position: relative;
  display: block;
  min-width: 0;
  max-width: 100%;
}

/* 多了一层包裹之后，下拉本身不再自动撑满容器（它以前是 flex 列的直接
   子元素）。这里补回来，否则控件会缩成内容宽度。 */
.select > select {
  width: 100%;
}

/* 弹出层。这个组件存在的全部理由：原生那一层由系统绘制，选中高亮用的是
   系统强调色，CSS 碰不到。这里用面板自己的表面与圆角。 */
.select-list {
  position: absolute;
  z-index: 40;
  top: calc(100% + 4px);
  left: 0;
  right: 0;
  max-height: min(280px, 40dvh);
  overflow-y: auto;
  margin: 0;
  padding: var(--space-1);
  list-style: none;
  border-radius: var(--radius-xl);
  background: color-mix(
    in oklab,
    var(--glass-deep-base) var(--glass-deep-alpha),
    transparent
  );
  backdrop-filter: blur(32px) saturate(190%);
  box-shadow:
    inset 0 1px 0 var(--color-highlight),
    var(--shadow-raise);
}

/* 与侧栏导航项同一套语言：中性叠加，不用品牌色着色 */
.select-option {
  padding: var(--space-2) 10px;
  border-radius: var(--radius-lg);
  color: var(--color-ink-muted);
  font-size: var(--text-sm);
  line-height: var(--text-sm--line-height);
  cursor: pointer;
  transition: background-color var(--duration-micro) var(--ease-state);
}
.select-option.is-active {
  background: var(--color-hover);
  color: var(--color-ink);
}
.select-option.is-selected {
  background: var(--color-active);
  color: var(--color-ink);
  font-weight: 500;
}
</style>
