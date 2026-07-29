---
name: React Component Generator
description: 生成现代、可访问、类型安全的 React 组件（函数组件 + Hooks + TypeScript）
keywords: react, 组件, component, hook, jsx, tsx, useState, useEffect
---

# React Component Generator

## 原则
1. 一律使用函数组件 + TypeScript，Props 用 `interface XxxProps` 显式声明。
2. 优先组合而非继承；子元素用 `children: React.ReactNode`。
3. 状态最小化：能派生的不存 state；跨组件状态先考虑提升，再考虑 context。
4. 事件处理器命名 `handleXxx`，Props 回调命名 `onXxx`。
5. 默认导出组件本身，类型随组件一起导出。

## 模板
```tsx
interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: "primary" | "secondary" | "ghost";
  size?: "sm" | "md" | "lg";
}

export function Button({ variant = "primary", size = "md", className, ...props }: ButtonProps) {
  return (
    <button
      className={cn(base, variants[variant], sizes[size], className)}
      {...props}
    />
  );
}
```

## 检查清单
- [ ] Props 是否有明确类型且透传原生属性（`...props`）
- [ ] 是否处理 loading / disabled / error 状态
- [ ] 交互元素是否可键盘操作（focus-visible 样式）
- [ ] 列表渲染是否有稳定 key（禁止 index 作为 key，除非静态列表）
