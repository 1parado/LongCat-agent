---
title: '弹性层：重试与限流如何保护大模型调用'
description: '一次 provider 抖动不该拖垮整个 Agent loop。LongCat-frontend 在 postJSON 这一层用指数退避重试和令牌桶限流把不稳定挡在边界。'
date: 2026-08-07
slug: resilience
order: 9
eyebrow: 'LLM / RESILIENCE'
tags: ['llm', 'retry', 'rate-limit', 'resilience']
---

普通聊天里，一次 HTTP 失败就是一次失败。Agent loop 里，一次 provider 抖动可能中断一个正在改代码的 turn，把已经产生的部分结果也一起带走。所以大模型调用这一层必须比调用方更“皮实”：出错时自己兜底，而不是把 429 或超时直接甩给 loop。

## 不稳定的边界应该收在哪里

所有 provider 调用最终都经过 `internal/llm/protocols.go` 里的 `postJSON`。它是整个 LLM 层唯一真正发起 HTTP 的地方。把重试和限流都收敛到这一个函数，意味着 agent loop、协议适配器、工具执行都不必各自处理限流或超时——它们只看到一个可能更慢、但更可靠的响应。

```go
// postJSON 发起一次 POST 请求，并在出错时按 DefaultRetry 自动重试。
// 重试覆盖：网络错误，以及 429 / 500 / 502 / 503 / 504 等可恢复状态码；
// 重试前会先经过出向限流器（waitRateLimit）以从源头降低被供应商限流的概率。
// 注意：流式（SSE）响应一旦开始读取便不再重试，仅对连接/首字节前的失败重试。
func postJSON(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error)
```

## 重试：指数退避 + 全抖动

`DefaultRetry` 对 429 / 500 / 502 / 503 / 504 以及网络错误，最多重试 2 次（共 3 次尝试），退避从 500ms 起、上限 8s。`backoff` 先读响应里的 `Retry-After` 头（秒或 HTTP-date，受 `MaxRetryAfter` 30s 约束），没有就走指数退避，再用全抖动 `rand.Int63n(d)` 把等待打散，避免大量并发请求同时重试造成惊群。

```go
rc := DefaultRetry // MaxAttempts:3, BaseDelay:500ms, MaxDelay:8s, MaxRetryAfter:30s
for attempt := 0; attempt < rc.MaxAttempts; attempt++ {
    if err := waitRateLimit(ctx); err != nil { // 先拿限流令牌
        return nil, err
    }
    resp, err := do(ctx, url, data)
    if err != nil { // 网络错误：退避后重试
        if w := sleepWithCtx(ctx, backoff(rc, attempt, nil)); w != nil {
            return nil, w
        }
        continue
    }
    if resp.StatusCode >= 400 {
        if retryableStatus(resp.StatusCode) && attempt < rc.MaxAttempts-1 {
            resp.Body.Close()
            if w := sleepWithCtx(ctx, backoff(rc, attempt, resp)); w != nil {
                return nil, w
            }
            continue
        }
        return resp, nil
    }
    return resp, nil
}
```

`retryableStatus` 只认那几个可恢复码；4xx 里的 400/401/403 不可重试，直接返回交给上层判断。这样重试循环只在“还没产生副作用”的安全区里转。

## 限流：令牌桶从源头降 429

重试是“出事之后再救”，限流是“尽量别出事”。`Limiter` 是一个零依赖的令牌桶：每秒放出 `rps` 个令牌，突发容量等于 `rps`。默认全局限流器 `defaultLimiter = NewLimiter(10)`，即出向 10 请求/秒。`postJSON` 在每次尝试前先 `waitRateLimit(ctx)` 取一个令牌；拿不到就阻塞到上下文取消。

```go
// 默认：出向 10 请求/秒，可按供应商配额调整
defaultLimiter = NewLimiter(10)

// 按供应商 RPM/TPM 配额设置，例如 SetRateLimit(3)
func SetRateLimit(rps int) {
    limiterMu.Lock()
    defer limiterMu.Unlock()
    if defaultLimiter != nil {
        defaultLimiter.Stop()
    }
    defaultLimiter = NewLimiter(rps)
}
```

`rps<=0` 表示不限流，便于本地或自托管模型关掉这层开销。补充令牌的后台 goroutine 在桶满时直接丢弃（`default` 分支），不会积压，也不会因为补令牌而阻塞主流程。

## 边界上的取舍：流式不重试

注释里写明了：`postJSON` 只对“连接/首字节前”的失败重试；SSE 流式响应一旦开始读取就不再重试。原因是流式已经把数据推给调用方（见 Agent loop 那篇的 `StreamEvent` channel），中途重试会破坏顺序，也会让已经消费的文本增量无法回滚。这个取舍让重试逻辑始终停留在“还没产生对外副作用”的窗口内。

## 阅读入口

- `internal/llm/resilience.go`：`RetryConfig` / `DefaultRetry`、`retryableStatus`、`backoff`、`Limiter` / `NewLimiter`、`SetRateLimit`。
- `internal/llm/protocols.go`：`postJSON`，看重试循环与 `waitRateLimit` 的接入位置。
