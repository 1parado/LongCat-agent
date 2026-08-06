package agent

import (
	"context"
	"strings"
	"testing"

	"LongCat-frontend/internal/llm"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "你好世界，这是一段用于估算的中文文本。"},
		{Role: "assistant", Content: "Hello world, this is a longer English sentence used for token estimation testing purposes only."},
	}
	if n := estimateTokens(msgs); n <= 0 {
		t.Fatalf("estimateTokens = %d, want > 0", n)
	}
}

func TestMaybeCompressSkipsWhenUnderThreshold(t *testing.T) {
	s := &Session{CompressEnabled: true, ContextWindow: 128000, CompressThreshold: 0.8}
	for i := 0; i < 10; i++ {
		s.Messages = append(s.Messages, llm.Message{Role: "user", Content: "短消息"})
	}
	s.maybeCompress(context.Background(), llm.Provider{Protocol: llm.ProtocolOllama})
	if len(s.Messages) != 10 {
		t.Fatalf("under-threshold should not compress, got %d messages", len(s.Messages))
	}
}

func TestMaybeCompressTriggers(t *testing.T) {
	orig := summarizeFunc
	defer func() { summarizeFunc = orig }()
	summarizeFunc = func(ctx context.Context, p llm.Provider, conv string) (string, error) {
		return "SUMMARY", nil
	}
	s := &Session{CompressEnabled: true, ContextWindow: 50, CompressThreshold: 0.8}
	for i := 0; i < 30; i++ {
		s.Messages = append(s.Messages, llm.Message{Role: "user", Content: strings.Repeat("内容", 20)})
	}
	s.maybeCompress(context.Background(), llm.Provider{Protocol: llm.ProtocolOllama})
	if len(s.Messages) != compressKeepRecent+1 {
		t.Fatalf("after compress want %d messages, got %d", compressKeepRecent+1, len(s.Messages))
	}
	if !strings.Contains(s.Messages[0].Content, "SUMMARY") {
		t.Fatalf("first message should be the summary, got %q", s.Messages[0].Content)
	}
}
