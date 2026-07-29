---
name: Vue 3 Component
description: Vue 3 组合式 API 组件：script setup、响应式、组件通信
keywords: vue, vue3, composition, setup, ref, reactive, pinia, 组合式
---

# Vue 3 Component

## 原则
1. 一律 `<script setup lang="ts">`，禁用 Options API。
2. `ref` 优先于 `reactive`（避免解构失去响应性）；派生值用 `computed`。
3. Props 用 `defineProps<{...}>()` 类型声明 + `withDefaults`；事件用 `defineEmits<{...}>()`。
4. `v-model` 用 `defineModel()`（Vue 3.4+）。
5. 跨层通信：props/emits → provide/inject → Pinia，按复杂度递进，不上来就上全局状态。

## 模板
```vue
<script setup lang="ts">
interface Props {
  modelValue: string;
  placeholder?: string;
}
const props = withDefaults(defineProps<Props>(), { placeholder: "请输入…" });
const model = defineModel<string>();
</script>

<template>
  <input
    v-model="model"
    :placeholder="props.placeholder"
    class="rounded-lg border px-3 py-2 focus-visible:ring-2"
  />
</template>
```

## 常见错误
- 解构 `reactive` 对象丢失响应性 → 用 `toRefs` 或改用 `ref`
- 在 `computed` 里做副作用 → 移到 `watchEffect`
- `watch` 侦听 ref 对象属性忘了 getter → `watch(() => obj.value.x, ...)`
