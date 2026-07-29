---
name: Next.js App Router
description: Next.js App Router 架构：Server Components、数据获取、路由约定
keywords: next, nextjs, app router, server component, rsc, ssr, 服务端, 路由, page, layout
---

# Next.js App Router

## 原则
1. 默认 Server Component；只有需要交互/浏览器 API 时才加 `"use client"`，且尽量下推到叶子组件。
2. 数据获取在 Server Component 中直接 `await`，用 `fetch` 的 `next: { revalidate }` 控制缓存。
3. 路由约定：`page.tsx`（页面）、`layout.tsx`（共享布局）、`loading.tsx`（Suspense 骨架）、`error.tsx`（错误边界，必须是 client）。
4. 表单变更优先使用 Server Actions（`"use server"`），避免手写 API route。
5. 元数据用 `export const metadata` 或 `generateMetadata`，不手写 `<head>`。

## 目录骨架
```
app/
├── layout.tsx        # 根布局（html/body、字体、Provider）
├── page.tsx          # 首页 (Server)
├── loading.tsx       # 全局加载骨架
└── dashboard/
    ├── layout.tsx    # 嵌套布局（侧边栏）
    └── page.tsx
```

## 常见错误
- 在 Server Component 里使用 useState/useEffect → 拆出 client 叶子组件
- `"use client"` 放在根布局 → 整棵树失去 RSC 优势
- 在 client 组件里直接读取 process.env 私密变量 → 泄漏风险，仅 `NEXT_PUBLIC_` 可用
