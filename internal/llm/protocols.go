package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// StreamFunc 在流式响应期间被逐块回调。
type StreamFunc func(delta string)

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// Chat 使用指定供应商发起对话。onDelta 非 nil 时使用流式（SSE/NDJSON），
// 返回完整的助手回复文本。
func Chat(ctx context.Context, p Provider, msgs []Message, onDelta StreamFunc) (string, error) {
	switch p.Protocol {
	case ProtocolOpenAIChat:
		return openAIChat(ctx, p, msgs, onDelta)
	case ProtocolOpenAIResponses:
		return openAIResponses(ctx, p, msgs, onDelta)
	case ProtocolAnthropic:
		return anthropicChat(ctx, p, msgs, onDelta)
	case ProtocolOllama:
		return ollamaChat(ctx, p, msgs, onDelta)
	default:
		return "", fmt.Errorf("不支持的协议: %s", p.Protocol)
	}
}

func endpoint(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func postJSON(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// scanSSE 逐行读取 SSE 流，把每个 data: 载荷交给 handle；
// handle 返回 false 时提前结束。
func scanSSE(body io.Reader, handle func(payload string) bool) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if !handle(payload) {
			return nil
		}
	}
	return sc.Err()
}

// ---------- OpenAI Chat Completions ----------

func openAIChat(ctx context.Context, p Provider, msgs []Message, onDelta StreamFunc) (string, error) {
	body := map[string]any{
		"model":    p.Model,
		"messages": msgs,
		"stream":   onDelta != nil,
	}
	headers := map[string]string{"Authorization": "Bearer " + p.APIKey}
	resp, err := postJSON(ctx, endpoint(p.URL, "/chat/completions"), headers, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if onDelta == nil {
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("响应中没有 choices")
		}
		return out.Choices[0].Message.Content, nil
	}

	var full strings.Builder
	err = scanSSE(resp.Body, func(payload string) bool {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			return true
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			full.WriteString(chunk.Choices[0].Delta.Content)
			onDelta(chunk.Choices[0].Delta.Content)
		}
		return true
	})
	return full.String(), err
}

// ---------- OpenAI Responses API ----------

func openAIResponses(ctx context.Context, p Provider, msgs []Message, onDelta StreamFunc) (string, error) {
	// Responses API 使用 instructions + input 结构。
	var sys string
	input := make([]map[string]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			sys = m.Content
			continue
		}
		input = append(input, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{
		"model":  p.Model,
		"input":  input,
		"stream": onDelta != nil,
	}
	if sys != "" {
		body["instructions"] = sys
	}
	headers := map[string]string{"Authorization": "Bearer " + p.APIKey}
	resp, err := postJSON(ctx, endpoint(p.URL, "/responses"), headers, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if onDelta == nil {
		var out struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		var full strings.Builder
		for _, o := range out.Output {
			if o.Type != "message" {
				continue
			}
			for _, c := range o.Content {
				if c.Type == "output_text" {
					full.WriteString(c.Text)
				}
			}
		}
		return full.String(), nil
	}

	var full strings.Builder
	err = scanSSE(resp.Body, func(payload string) bool {
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return true
		}
		if ev.Type == "response.output_text.delta" && ev.Delta != "" {
			full.WriteString(ev.Delta)
			onDelta(ev.Delta)
		}
		return true
	})
	return full.String(), err
}

// ---------- Anthropic Messages API ----------

func anthropicChat(ctx context.Context, p Provider, msgs []Message, onDelta StreamFunc) (string, error) {
	var sys string
	conv := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			sys = m.Content
			continue
		}
		conv = append(conv, m)
	}
	body := map[string]any{
		"model":      p.Model,
		"max_tokens": 8192,
		"messages":   conv,
		"stream":     onDelta != nil,
	}
	if sys != "" {
		body["system"] = sys
	}
	headers := map[string]string{
		"x-api-key":         p.APIKey,
		"anthropic-version": "2023-06-01",
	}
	resp, err := postJSON(ctx, endpoint(p.URL, "/v1/messages"), headers, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if onDelta == nil {
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		var full strings.Builder
		for _, c := range out.Content {
			if c.Type == "text" {
				full.WriteString(c.Text)
			}
		}
		return full.String(), nil
	}

	var full strings.Builder
	err = scanSSE(resp.Body, func(payload string) bool {
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return true
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			full.WriteString(ev.Delta.Text)
			onDelta(ev.Delta.Text)
		}
		return true
	})
	return full.String(), err
}

// ---------- Ollama Chat（NDJSON 流） ----------

func ollamaChat(ctx context.Context, p Provider, msgs []Message, onDelta StreamFunc) (string, error) {
	body := map[string]any{
		"model":    p.Model,
		"messages": msgs,
		"stream":   onDelta != nil,
	}
	resp, err := postJSON(ctx, endpoint(p.URL, "/api/chat"), nil, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	type chunk struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Done bool `json:"done"`
	}

	if onDelta == nil {
		var out chunk
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		return out.Message.Content, nil
	}

	var full strings.Builder
	dec := json.NewDecoder(resp.Body)
	for {
		var c chunk
		if err := dec.Decode(&c); err != nil {
			if err == io.EOF {
				break
			}
			return full.String(), err
		}
		if c.Message.Content != "" {
			full.WriteString(c.Message.Content)
			onDelta(c.Message.Content)
		}
		if c.Done {
			break
		}
	}
	return full.String(), nil
}
