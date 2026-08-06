package agent

import (
	"context"
	"testing"
)

// TestSendEventSends 验证正常路径会把事件送入通道并返回 true。
func TestSendEventSends(t *testing.T) {
	ctx := context.Background()
	ch := make(chan StreamEvent, 1)
	if !sendEvent(ctx, ch, StreamEvent{Kind: "delta", Delta: "x"}) {
		t.Fatal("sendEvent should return true on successful send")
	}
	if got := (<-ch).Delta; got != "x" {
		t.Fatalf("event not delivered, got %q", got)
	}
}

// TestSendEventCancelled 验证上下文取消时 sendEvent 立即返回 false，
// 而不会阻塞在 out <- 上——这正是防止生产者 goroutine 泄漏的关键。
func TestSendEventCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan StreamEvent, 1) // 满缓冲也不应阻塞
	cancel()
	if sendEvent(ctx, ch, StreamEvent{Kind: "delta", Delta: "x"}) {
		t.Fatal("sendEvent should return false when ctx is already cancelled")
	}
}
