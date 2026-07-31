---
title: 'Function calling：模型只提案，运行时才执行'
description: '从 JSON Schema 到 ToolExecutor，拆开 function calling 的定义、翻译、执行三段式。'
date: 2026-07-29
slug: function-calling
order: 3
eyebrow: 'MODEL / FUNCTION CALLING'
tags: ['tools', 'function-calling', 'safety']
---

Function calling 最容易被误解成“模型可以调用函数”。更准确的说法是：模型可以返回一个结构化的函数调用提案；**它没有本地进程权限**。真正的函数由 Agent runtime 解析、校验并执行。

## 三个对象，三条责任线

在 `internal/llm/llm.go` 里，工具被拆成三层数据：

```go
type Tool struct {
    Type     string             `json:"type"`
    Function FunctionDefinition `json:"function"`
}

type FunctionDefinition struct {
    Name        string
    Description string
    Parameters  map[string]any // JSON Schema
}

type ToolCall struct {
    ID       string
    Function struct {
        Name      string
        Arguments string // JSON string, execute 前再解析
    }
}
```

`Tool` 是“给模型看的目录”；`ToolCall` 是模型回传的“选择结果”。它们之间故意不是同一个类型，因为描述能力和执行请求的生命周期不同。

## 工具定义在 Agent 侧汇总

`ToolExecutor.Definitions()` 暴露内置能力，比如 `read_file`、`write_file`、`preview_file`、`load_skill`。如果有 MCP manager，它会把发现到的远程工具继续 append 进来；如果配置了子 Agent，还会增加 `spawn_subagent`。

这意味着模型每一轮看到的是当前运行时的能力快照，而不是一张硬编码的全局函数表：

```text
native tools
    + loaded skills entry point
    + MCP discovered tools
    + orchestration tools
                ↓
        exec.Definitions()
                ↓
        llm.ChatWithTools(...)
```

## 只让协议层做翻译

`llm.ChatWithTools` 根据 provider 的协议选择适配器：

- OpenAI Chat Completions：`tools` + `tool_calls`。
- Anthropic Messages：`tools` + `tool_use` / `tool_result blocks`。
- OpenAI Responses：`function_call` / `function_call_output`。
- Ollama：`message.tool_calls`，流式时使用 NDJSON。

协议层只做 envelope mapping，明确不会执行函数。这样可以保证供应商 API 的差异停在 `internal/llm/protocols.go`，而 `ToolExecutor` 不需要知道当前模型来自哪里。

## 执行边界在哪里

模型返回 JSON 后，`ExecuteContext` 先反序列化参数，再按名称分发：

```text
raw arguments
    ↓ json.Unmarshal
tool name switch
    ├─ native file operation
    ├─ preview / diff / undo
    ├─ load_skill
    ├─ spawn_subagent
    └─ mcp_... → MCP Manager
```

文件工具接下来还要进入 workspace 路径检查；MCP 工具则要验证 server 和 discovered tool 是否存在。即使模型给出了一个看起来合理的名字，也不能跳过这层检查。

## 一个值得保留的“笨”设计

`Arguments` 是字符串，而不是直接使用 `map[string]any`。这让不同协议的增量 JSON 可以先拼接完整，再在执行边界统一解析；同时错误会被当作工具错误返回给模型，而不是让整个进程崩溃。

Function calling 的核心不是让模型“会编程”，而是给它一份受限的操作语言，再让本地 runtime 成为解释器。语言可以扩展，解释器必须保持清醒。
