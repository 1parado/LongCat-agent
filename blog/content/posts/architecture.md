---
title: '先画一张地图：LongCat-frontend 的 Agent 边界'
description: '一个请求如何从入口穿过会话、模型、工具和工作区，最后以可观察的结果返回。'
date: 2026-07-31
slug: architecture
order: 1
eyebrow: 'ARCHITECTURE / MAP'
tags: ['architecture', 'go', 'tauri']
---

很多 Agent 项目从一个 `for` 循环开始，最后却长成了一个“什么都能碰”的黑盒。LongCat-frontend 选择把边界画出来：模型负责提出下一步，运行时负责决定这一步能否、应该如何发生。

## 一条请求的完整路径

从用户视角看，只有一条消息；从代码视角看，它会经过四层：

```text
Web UI / TUI / IM
        │
        ▼
  server + session          组织上下文、流式事件、持久化
        │
        ▼
  llm protocol adapter      把不同供应商映射成 common message
        │
        ▼
  agent loop                请求模型 → 执行工具 → 把结果送回模型
        │
        ├── ToolExecutor     read / write / preview / diff / undo
        ├── Skills            按需加载专门知识
        ├── MCP               发现并调用外部工具
        └── workspace         路径边界、变更记录、撤销
```

这张图有一个关键含义：**Skills 和 MCP 不是两条平行的 Agent 路径，而是进入工具与上下文层的两种扩展方式。** IM 也不是另一个 Agent，它只是换了一个输入输出通道。

## 源码从哪里开始读

推荐按这个顺序走读：

1. `internal/server/server.go`：看 HTTP API 如何创建 session，以及 `/api/chat` 如何转发流式事件。
2. `internal/agent/agent.go`：看 `Session` 如何构造 system prompt、维护消息和推进最多 8 轮的循环。
3. `internal/agent/tools.go`：看模型可见的工具定义和真实执行分发。
4. `internal/llm/protocols.go`：看同一个 `Message` / `ToolCall` 如何适配四种协议。
5. `internal/workspace/`：看“能不能写”与“写了如何回退”不由模型决定。

## 为什么要分成这些边界

因为一个本地 Agent 的风险不在于它能不能生成文本，而在于它能不能把模糊的文本变成不可逆的动作。把工具执行留在 `ToolExecutor`，至少带来三件事：

- **可检查**：所有文件动作都有同一个 workspace 入口。
- **可观察**：`ToolEvent` 在执行前后发出，Web UI 可以显示 running / success / error。
- **可替换**：模型协议可以变化，工具实现和工作区安全不需要跟着重写。

所以这不是“为了架构而架构”。这是把智能和权限放在两个不同的房间里：模型可以建议，但只有运行时能开门。

## 下一站

接下来读 [Agent loop：一次对话为什么不止一次请求](/frontend-agent/notes/agent-loop/)，看这张地图中最核心的往返是怎么工作的。
