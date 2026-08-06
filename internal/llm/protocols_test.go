package llm

import "testing"

func TestAnthropicCacheControl(t *testing.T) {
	// 只有 Anthropic 需要在请求里显式打 cache_control 断点。
	if !anthropicCacheControl(ProtocolAnthropic) {
		t.Error("anthropic should use explicit cache_control marker")
	}
	// OpenAI Chat 前缀缓存由服务端自动生效，不注入 cache_control 字段。
	if anthropicCacheControl(ProtocolOpenAIChat) {
		t.Error("openai_chat must NOT use explicit cache_control marker")
	}
	if anthropicCacheControl(ProtocolOllama) {
		t.Error("ollama should NOT use cache_control marker")
	}
	if anthropicCacheControl(ProtocolOpenAIResponses) {
		t.Error("openai_responses should NOT use explicit cache_control marker")
	}
}
