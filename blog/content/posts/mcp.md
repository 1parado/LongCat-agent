---
title: 'MCP：把外部工具接进同一条调用链'
description: 'MCP manager 如何合并配置、做健康检查、发现工具，并把远程能力命名空间化。'
date: 2026-07-27
slug: mcp
order: 5
eyebrow: 'BOUNDARY / MCP'
tags: ['mcp', 'json-rpc', 'integration']
---

Skills 扩展的是模型的知识，MCP 扩展的是模型能使用的外部工具。LongCat-frontend 没有把 MCP 当作“神奇插件”，而是实现了一个窄小、可观察的 HTTP JSON-RPC registry。

## 配置从哪里来

`mcp.Manager.Load()` 合并用户级配置和当前工作区的 `.longcat-frontend/mcp.json`：

```text
~/.longcat-frontend/mcp.json
        +
workspace/.longcat-frontend/mcp.json
        ↓ project entry wins on duplicate ID
    map[string]MCPServer
```

配置里的 secret header 只进入内存；`List()` 返回给 UI 时会主动清掉 headers。这是控制面 API 的小细节，却决定了控制室页面不会把凭据重新暴露出去。

## 发现，而不是猜测

MCP server 配置只提供连接信息。启动健康检查后，manager 会：

1. 校验 URL 和协议。
2. 发起健康探测。
3. 对健康的 HTTP endpoint 调用 `tools/list`。
4. 把服务返回的 `name`、`description`、`inputSchema` 保存到 server。

失败的可选 MCP 服务不会阻塞原生工具继续工作；UI 可以看到 `ok`、`warn`、`auth_required` 或 `error` 的状态，并显示最近一次检查。

## 命名空间是安全线，也是可读性

发现的远程工具不会直接使用裸名字，而是变成：

```text
mcp_<server-id>__<tool-name>
```

例如 `figma` server 的 `get_file` 会变成 `mcp_figma__get_file`。`ToolName` 会清理非法字符，`splitToolName` 再把 server ID 和 tool name 拆出来。

这样做同时解决两个问题：

- 不同 server 可以拥有同名工具而不互相覆盖。
- `Execute` 能够先定位 server，再确认这个 tool 确实来自该 server 的 discovered list。

## 调用仍由本地 runtime 掌握

模型看到的 MCP tool 和原生 tool 没有本质区别，都会进入 `exec.Definitions()`。当模型返回 `mcp_...` 调用时，`ToolExecutor` 把它转给 `Manager.Execute`，后者组装 JSON-RPC `tools/call` 请求。

```text
model: mcp_figma__get_file({"file_key":"..."})
  ↓
ToolExecutor: recognize MCP prefix
  ↓
Manager: validate server + discovered tool
  ↓
HTTP POST: method = tools/call
  ↓
JSON result → tool message → next model round
```

注意：当前实现明确拒绝尚未配置可执行命令的 stdio 服务。宁可显示“尚未启用”，也不把一个协议分支伪装成已经安全可用。

MCP 的价值因此不只是“连接更多工具”，而是把外部能力纳入同一个工具契约、同一个事件流和同一个 loop。
