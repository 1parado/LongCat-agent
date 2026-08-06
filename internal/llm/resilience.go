package llm

import (
	"context"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RetryConfig 控制对大模型 API 请求失败时的自动重试行为。
type RetryConfig struct {
	MaxAttempts   int           // 最大尝试次数（含首次），<=1 表示不重试
	BaseDelay     time.Duration // 指数退避基准
	MaxDelay      time.Duration // 单次退避上限
	MaxRetryAfter time.Duration // 尊重 Retry-After 时的上限
}

// DefaultRetry 是 postJSON 使用的默认重试配置：对 429 / 5xx 及网络错误
// 重试最多 2 次，退避 500ms 起、上限 8s，并尊重服务端 Retry-After 头。
var DefaultRetry = RetryConfig{
	MaxAttempts:   3,
	BaseDelay:     500 * time.Millisecond,
	MaxDelay:      8 * time.Second,
	MaxRetryAfter: 30 * time.Second,
}

// retryableStatus 报告该 HTTP 状态码是否值得重试。
// 覆盖业务高峰期最常见的可恢复错误：
// 429 Too Many Requests、500、502、503、504。
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// backoff 计算第 attempt 次（从 0 开始）失败后的等待时长。
// 若响应携带 Retry-After，则优先采用（秒或 HTTP-date），并受 MaxRetryAfter 约束；
// 否则使用指数退避 + 全抖动，避免大量请求同时重试造成惊群。
func backoff(rc RetryConfig, attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				d := time.Duration(secs) * time.Second
				if d <= rc.MaxRetryAfter {
					return d
				}
				return rc.MaxRetryAfter
			}
			if t, err := http.ParseTime(ra); err == nil {
				if d := time.Until(t); d > 0 && d <= rc.MaxRetryAfter {
					return d
				}
			}
		}
	}
	d := rc.BaseDelay * time.Duration(1<<uint(attempt)) // 指数退避
	if d > rc.MaxDelay {
		d = rc.MaxDelay
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d))) // 全抖动 0..d
}

// Limiter 是一个极简令牌桶限流器，用于约束对大模型 API 的出向请求速率，
// 从源头降低被供应商限流（429）的概率。零依赖、标准库实现。
type Limiter struct {
	tokens   chan struct{}
	stop     chan struct{}
	capacity int
	ticker   *time.Ticker
}

// NewLimiter 创建每秒 rps 个令牌、突发容量为 rps 的限流器；rps<=0 表示不限流。
func NewLimiter(rps int) *Limiter {
	if rps <= 0 {
		return &Limiter{capacity: 0}
	}
	l := &Limiter{
		tokens:   make(chan struct{}, rps),
		stop:     make(chan struct{}),
		capacity: rps,
		ticker:   time.NewTicker(time.Second / time.Duration(rps)),
	}
	for i := 0; i < rps; i++ {
		l.tokens <- struct{}{}
	}
	go func() {
		for {
			select {
			case <-l.stop:
				return
			case <-l.ticker.C:
				select {
				case l.tokens <- struct{}{}:
				default: // 桶已满，丢弃
				}
			}
		}
	}()
	return l
}

// Wait 在上下文取消前阻塞，直到获取到一个令牌；不限流时立即返回。
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil || l.capacity <= 0 {
		return nil
	}
	select {
	case <-l.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 停止后台补充协程（仅用于测试或进程退出）。
func (l *Limiter) Stop() {
	if l == nil || l.ticker == nil {
		return
	}
	l.ticker.Stop()
	close(l.stop)
}

var (
	limiterMu      sync.RWMutex
	defaultLimiter = NewLimiter(10) // 默认：出向 10 请求/秒，可按供应商配额调整
)

// waitRateLimit 在发起请求前获取一个出向令牌。
func waitRateLimit(ctx context.Context) error {
	limiterMu.RLock()
	l := defaultLimiter
	limiterMu.RUnlock()
	if l == nil {
		return nil
	}
	return l.Wait(ctx)
}

// SetRateLimit 调整全局出向限流（每秒请求数）。rps<=0 关闭限流。
// 建议根据所用供应商的 RPM/TPM 配额设置，例如 SetRateLimit(3)。
func SetRateLimit(rps int) {
	limiterMu.Lock()
	defer limiterMu.Unlock()
	if defaultLimiter != nil {
		defaultLimiter.Stop()
	}
	defaultLimiter = NewLimiter(rps)
}
