---
title: 'Protocol adapters：四种 API，一种对话形状'
description: 'OpenAI Chat、Responses、Anthropic 和 Ollama 如何共享 Message，同时保留各自的流式与工具语义。'
date: 2026-07-24
slug: protocol-adapters
order: 8
eyebrow: 'LLM / PROTOCOLS'
tags: ['llm', 'protocol', 'streaming', 'resilience']
---

LongCat-frontend 支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 和 Ollama。真正有价值的不是“列表很长”，而是上层 Agent 不需要知道本轮请求发给谁。

## 先定义内部语言

`internal/llm/llm.go` 用 `Message`、`Attachment`、`Tool`、`ToolCall` 定义内部通用语言。上层只关心：

```text
system / user / assistant / tool
content
attachments
assistant tool calls
tool call id + result
```

协议差异在 `internal/llm/protocols.go` 的 mapper 中解决：`openAIChatMessages`、`openAIResponsesInput`、`anthropicMessages`、`ollamaMessages`。

## 同一份附件，四种翻译

以一张图片和一个 Markdown 文件为例：

| Provider | 图片 | 文本/文件 |
| --- | --- | --- |
| OpenAI Chat | `image_url` data URL | text part / compact marker |
| Responses | `input_image` | `input_file` 或 `input_text` |
| Anthropic | base64 image block | document block 或 text |
| Ollama | `images` base64 | 拼进 content |

上层仍然只构造 `Attachment{Name, MIMEType, Data/Text}`。这种“先统一，再在边界展开”的策略，避免业务层写大量 provider 分支。

## 流式协议也被藏在边界里

OpenAI、Anthropic 和 Responses 使用 SSE，Ollama 使用 NDJSON。所有响应最终都通过 `StreamFunc` 把 delta 回调给 Agent/server：

```text
SSE data: ...       ┐
SSE event: ...      ├── protocol adapter ──→ onDelta(delta)
NDJSON line ...     ┘
```

`scanSSE` 只负责逐行取出 data payload，具体 adapter 再解析自己的事件类型。这个层次划分让流式 UI 不需要知道“当前是一条 `response.output_text.delta` 还是一段 `content_block_delta`”。

注意这里“回调给 Agent/server”发生在 LLM 边界：协议函数通过 `StreamFunc onDelta` 把增量交回调用方。到了 Agent 边界，这些增量不再被直接传给 UI，而是由 `runStream` 的 goroutine 包成 `StreamEvent` 推入一个 channel，最终由 Web UI 的 `chat` 处理器 `range` 消费并 flush 成 SSE——LLM 层的 `StreamFunc` 本身保持不变。

## Tool call 的语义比 envelope 更重要

四种协议的工具响应名字不一样：`tool_calls`、`tool_use`、`function_call`、Ollama 的 `message.tool_calls`。适配器把它们都变成 `ChatResult{Content, ToolCalls}`，然后交给 Agent loop。

协议函数**不会执行工具**。`ChatWithTools` 的注释直接写明：tool calls 返回给调用方执行。这样 LLM adapter 可以被测试为纯粹的消息转换器，真实权限仍集中在 `ToolExecutor`。

## 边界还要扛得住失败：自动重试与出向限流

适配器把“外部世界的不整齐”收拢成统一形状，但外部世界还会**直接拒绝服务**：业务高峰期大模型 API 经常返回 429 Too Many Requests，或 500/502/503/504。这些错误不该直接抛给用户，而应在边界内被消化。

所有协议与流式调用都汇聚到 `internal/llm/protocols.go` 唯一的 HTTP 出口 `postJSON`，所以重试与限流只需在这一点落地，即可覆盖 OpenAI Chat / Responses、Anthropic、Ollama 四套协议与 SSE 流，上层 Agent 与 UI 完全无感。

### 自动重试（Retry）

`postJSON` 在出错时按 `DefaultRetry`（`internal/llm/resilience.go`）重试：

- **触发条件**：HTTP 429 / 500 / 502 / 503 / 504，以及网络错误（连接重置、超时、DNS 失败）。
- **退避策略**：默认最多 3 次尝试（1 首发 + 2 重试），指数退避 500ms 起、上限 8s，并叠加**全抖动**避免大量请求同时重试造成惊群。
- **尊重服务端信号**：若响应携带 `Retry-After`（秒或 HTTP-date），优先采用该等待时长（受 `MaxRetryAfter = 30s` 约束）。
- **可取消**：退避等待期间若 `ctx` 被取消（如用户点了停止），立即返回 `ctx.Err()`。
- **流式边界**：流式（SSE）响应一旦开始读取便不再重试，仅对连接建立 / 首字节前的失败重试，避免重复发射已下发的 token。

```text
postJSON(ctx, url, headers, body)
   └─ waitRateLimit(ctx)              # 出向限流：取一个令牌
   └─ httpClient.Do(req)
        ├─ 网络错误 / 429 / 5xx  ──→ 退避(sleepWithCtx) ──→ 重试（最多 MaxAttempts-1 次）
        └─ 2xx                  ──→ 返回 *http.Response（交给上层解析）
```

### 出向限流（Rate limiting）

重试能兜住瞬时失败，但更优的策略是**从源头降低被限流的概率**。`internal/llm/resilience.go` 提供一个零依赖的令牌桶 `Limiter`，在每次请求发起前先取令牌：

- 默认 **10 请求/秒**（突发容量同为 10），`SetRateLimit(rps)` 可按所用供应商的 RPM/TPM 配额调整；`SetRateLimit(0)` 关闭限流。
- 限流发生在 **client-side（出向 LLM 请求）**，不与本地 Web UI 的入向流量混淆。

```go
import "LongCat-frontend/internal/llm"

llm.SetRateLimit(3) // 例如供应商限额 3 QPS
```

把“重试消化失败 + 限流抑制失败”放在 LLM 边界，是适配器哲学的延伸：让上层只面对稳定的语义，把外部世界的不整齐与不稳定都挡在边界之内。

## 兼容性优先于完美抽象

不同供应商的能力并不完全相同：有的支持图片、有的支持任意文件，有的流式传工具参数、有的只在最终响应返回。内部模型只承诺一个足够小的 common subset，无法完整映射的内容会退化为文本标记，至少保证模型能理解发生了什么。

这是一种实用的适配器哲学：让核心 loop 只面对稳定的语义；让协议层承担外部世界的不整齐。
