---
name: Tailwind Styling
description: Tailwind CSS 实用类的组织、复用与设计令牌化
keywords: tailwind, css, 样式, style, 主题, theme, 深色, dark, 响应式, responsive
---

# Tailwind Styling

## 原则
1. 类名顺序：布局 → 盒模型 → 排版 → 颜色 → 效果 → 状态（`flex items-center gap-2 px-4 py-2 text-sm font-medium text-zinc-100 bg-zinc-900 rounded-lg hover:bg-zinc-800`）。
2. 深色模式默认：以 `dark:` 为一等公民设计，或直接以暗色为基底。
3. 重复 3 次以上的类组合抽成组件或 `cn()` 变体表，不写 `@apply` 大杂烩。
4. 颜色只用语义化令牌（`bg-background text-foreground border-border`），禁止散落魔法色值。
5. 响应式移动优先：先写基础样式，再叠加 `md:` / `lg:`。

## cn 工具
```ts
export function cn(...inputs: (string | undefined | false)[]) {
  return inputs.filter(Boolean).join(" ");
}
```

## 现代简洁质感速查
- 卡片: `rounded-xl border border-zinc-800 bg-zinc-900/60 backdrop-blur-sm shadow-sm`
- 微动画: `transition-colors duration-150` / `transition-transform hover:-translate-y-0.5`
- 焦点态: `focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500`
