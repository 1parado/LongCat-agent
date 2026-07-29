---
name: Accessibility Checker
description: Web 可访问性（a11y）检查与修复：语义化、键盘导航、ARIA、对比度
keywords: a11y, accessibility, 可访问, 无障碍, aria, 键盘, contrast, 对比度, 语义
---

# Accessibility Checker

## 检查顺序（按影响面）
1. **语义化标签**：`button` 而非 `div onClick`；`nav/main/header/footer` 地标；标题层级不跳级。
2. **键盘可达**：所有交互元素可 Tab 到达、Enter/Space 触发；模态框做焦点陷阱 + Esc 关闭。
3. **可见焦点**：永远不写 `outline: none` 而不给替代 `focus-visible` 样式。
4. **图像与图标**：信息性图片必须有 `alt`；装饰性图标 `aria-hidden="true"`。
5. **表单**：每个输入必须有 `<label htmlFor>` 或 `aria-label`；错误信息用 `aria-describedby` 关联。
6. **对比度**：正文 ≥ 4.5:1，大字号 ≥ 3:1。
7. **动效**：尊重 `prefers-reduced-motion`。

## ARIA 三原则
- 能用原生 HTML 就不用 ARIA。
- `role` 一旦声明，必须实现其完整键盘协议（如 `role="tablist"` 需要方向键切换）。
- `aria-live="polite"` 用于异步状态通知（加载完成、错误提示）。

## 修复模板
```tsx
// ❌ <div className="btn" onClick={submit}>提交</div>
// ✅
<button type="submit" disabled={pending} aria-busy={pending}>
  {pending ? "提交中…" : "提交"}
</button>
```
