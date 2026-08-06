---
title: 'Agent loop：一次对话为什么不止一次请求'
description: 'LongCat-frontend 如何在最多 8 轮中推进模型、工具、结果和下一次决策。'
date: 2026-07-30
slug: agent-loop
order: 2
eyebrow: 'RUNTIME / LOOP'
tags: ['agent', 'loop', 'streaming']
---

普通聊天的形状是 `user → model → answer`。一旦模型需要读文件或修改代码，答案就不再是终点：模型得先提出动作，运行时执行动作，再把结果交回去。这就是 Agent loop。

## 循环的最小单元

`internal/agent/agent.go` 里真正执行循环的是 `runStream`：它在一个**独立的 goroutine** 中运行，把一次用户 turn 组织成最多 8 轮，并把过程中的 token 增量、工具事件、结束事件推入一个 `StreamEvent` channel。`AskWithAttachments` 则是消费这个 channel 的薄封装（把 `delta`/`tool` 事件转交回调），TUI 与子代理编排无需感知 channel 的存在。

伪代码对应 `runStream` 的核心：

```go
out := make(chan StreamEvent, 32)
go func() {
    defer close(out)
    // 同一份多轮循环
    for round := 0; round < 8; round++ {
        result := llm.ChatWithTools(ctx, provider, opts) // OnDelta 把 delta 推入 out
        if len(result.ToolCalls) == 0 {
            break // 模型给出了最终文本
        }

        msgs = append(msgs, assistantToolCall(result.ToolCalls))
        for _, call := range result.ToolCalls {
            out <- StreamEvent{Kind: "tool", Tool: running(call)}
            out <- StreamEvent{Kind: "tool", Tool: finished(call, executor.ExecuteContext(...))}
            msgs = append(msgs, toolResult(call.ID, out))
        }
    }
    out <- StreamEvent{Kind: "done", Final: reply}
}()
```

每一轮都有相同的节奏：

1. 当前消息、系统提示和可用工具送入模型。
2. 模型返回文本，或者返回一个/多个工具调用。
3. 运行时执行每个调用，并保留 `tool_call_id`。
4. 工具结果作为下一轮输入，模型决定继续还是收束。

## 为什么要保留 assistant 的 tool call

工具结果不能凭空出现在消息历史里。对于 OpenAI 风格协议，下一轮需要看到类似这样的成对消息：

```text
assistant: tool_calls = [{id: "call-1", function: {name: "read_file", ...}}]
tool:     tool_call_id = "call-1", content = "...file content..."
```

Anthropic 和 Responses API 的外层形状不同，但语义相同：先记录“模型想做什么”，再记录“工具做完返回什么”。因此 `llm.Message` 同时保留 `ToolCalls`、`ToolCallID` 和 `Name`，协议适配器在边界处转换，而不是在 loop 里写四套逻辑。

## 流式不是循环的替代品

循环在 goroutine 里跑，流式则是它和调用方之间的**通道（channel）**。每一块产出都被封装成一个 `StreamEvent`（`Kind` 为 `delta`、`tool` 或 `done`），推入 channel 后由消费方逐条取走——Web UI 的 `chat` 处理器就是 `for ev := range evCh { ... }` 这样边收边 flush 成 SSE 帧。`onDelta` 不再直接触达 UI，而是先把文本增量包成 `StreamEvent{Kind: "delta"}` 推入 channel：

```text
delta: "我先查看..."        ┐
tool:  { name: "list_directory", status: "running" }   ├── runStream goroutine ──→ chan StreamEvent ──→ 消费方 flush SSE
tool:  { name: "list_directory", status: "success", result: "..." }
delta: "目录里有..."        ┘
done                       （channel 关闭）
```

这两个事件不能混成一件事。文本增量是回答的视觉反馈，工具事件是系统行为的审计线索。前者让等待变得可接受，后者让“Agent 正在改什么”变得可理解。用 channel 而非直接回调的好处是：生产（goroutine 跑 loop）和消费（刷 SSE、TUI 渲染）解耦，调用方可以并发地边收边处理，也更容易在取消时通过关闭 channel 干净地收尾。

## 三个停止条件

- 模型不再返回工具调用，说明它认为任务已经可以用文本收束。
- `context.Context` 被取消，循环会保存已经产生的部分回复并返回取消错误。
- 达到 8 轮上限，避免模型在工具失败或自我重复时无限运行。

这个上限不是智能的上限，而是运行时的保险丝。真正的产品体验还可以在 UI 上把每一轮展示出来，让用户看到它是“完成了”，还是“因为预算停下了”。

## 阅读入口

- `internal/agent/agent.go`：`Stream` / `runStream`（goroutine + channel 流式核心）、`AskWithAttachments`（消费 channel 的回调封装）、`buildSystem`。
- `internal/agent/agent.go`：`commitInterrupted`，看取消时如何保留部分状态。
- `internal/server/server.go`：`chat` 处理器，看它如何用 `for ev := range evCh` 把 `delta`、`tool`、`done` 发往 Web UI。
