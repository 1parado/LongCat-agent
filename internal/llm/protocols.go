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

type ChatOptions struct {
	Messages []Message
	Tools    []Tool
	OnDelta  StreamFunc
}

type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
}

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

// ChatWithTools is the tool-capable counterpart of Chat. Tool calls are
// returned to the caller for execution; the protocol layer never executes them.
func ChatWithTools(ctx context.Context, p Provider, opts ChatOptions) (ChatResult, error) {
	if len(opts.Tools) == 0 {
		text, err := Chat(ctx, p, opts.Messages, opts.OnDelta)
		return ChatResult{Content: text}, err
	}
	switch p.Protocol {
	case ProtocolOpenAIChat:
		return openAIChatWithTools(ctx, p, opts)
	case ProtocolAnthropic:
		return anthropicChatWithTools(ctx, p, opts)
	case ProtocolOpenAIResponses:
		return openAIResponsesWithTools(ctx, p, opts)
	case ProtocolOllama:
		return ollamaChatWithTools(ctx, p, opts)
	default:
		text, err := Chat(ctx, p, opts.Messages, opts.OnDelta)
		return ChatResult{Content: text}, err
	}
}

func ollamaChatWithTools(ctx context.Context, p Provider, opts ChatOptions) (ChatResult, error) {
	body := map[string]any{"model": p.Model, "messages": ollamaMessages(opts.Messages), "tools": opts.Tools, "stream": opts.OnDelta != nil}
	resp, err := postJSON(ctx, endpoint(p.URL, "/api/chat"), nil, body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	if opts.OnDelta == nil {
		var out struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return ChatResult{}, err
		}
		return ChatResult{Content: out.Message.Content, ToolCalls: out.Message.ToolCalls}, nil
	}
	var full strings.Builder
	calls := map[int]*ToolCall{}
	dec := json.NewDecoder(resp.Body)
	for {
		var c struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if err := dec.Decode(&c); err != nil {
			if err == io.EOF {
				break
			}
			return ChatResult{Content: full.String()}, err
		}
		if c.Message.Content != "" {
			full.WriteString(c.Message.Content)
			opts.OnDelta(c.Message.Content)
		}
		for i, tc := range c.Message.ToolCalls {
			b, _ := json.Marshal(tc.Function.Arguments)
			x := &ToolCall{ID: fmt.Sprintf("ollama-%d", i), Type: "function"}
			x.Function.Name = tc.Function.Name
			x.Function.Arguments = string(b)
			calls[i] = x
		}
		if c.Done {
			break
		}
	}
	ordered := make([]ToolCall, 0, len(calls))
	for i := 0; i < len(calls); i++ {
		if c, ok := calls[i]; ok {
			ordered = append(ordered, *c)
		}
	}
	return ChatResult{Content: full.String(), ToolCalls: ordered}, nil
}

func endpoint(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func attachmentMIME(a Attachment) string {
	if a.MIMEType != "" {
		return a.MIMEType
	}
	return "application/octet-stream"
}

func attachmentDataURL(a Attachment) string {
	if a.Data == "" {
		return ""
	}
	if strings.HasPrefix(a.Data, "data:") {
		return a.Data
	}
	return "data:" + attachmentMIME(a) + ";base64," + a.Data
}

func attachmentBase64(a Attachment) string {
	data := attachmentDataURL(a)
	if comma := strings.IndexByte(data, ','); comma >= 0 {
		return data[comma+1:]
	}
	return data
}

func attachmentText(a Attachment) string {
	if a.Text != "" {
		return a.Text
	}
	return fmt.Sprintf("[附件: %s (%s)]", a.Name, attachmentMIME(a))
}

// openAIChatMessages maps our common message shape to Chat Completions. This
// API supports image_url parts; text and unsupported file types remain
// understandable through extracted text or a compact attachment marker.
func openAIChatMessages(msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Attachments) == 0 {
			out = append(out, m)
			continue
		}
		parts := make([]any, 0, 1+len(m.Attachments))
		if m.Content != "" {
			parts = append(parts, map[string]any{"type": "text", "text": m.Content})
		}
		for _, a := range m.Attachments {
			if strings.HasPrefix(strings.ToLower(attachmentMIME(a)), "image/") && attachmentDataURL(a) != "" {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": attachmentDataURL(a)},
				})
				continue
			}
			parts = append(parts, map[string]any{"type": "text", "text": attachmentText(a)})
		}
		msg := map[string]any{"role": m.Role, "content": parts}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		out = append(out, msg)
	}
	return out
}

func openAIResponsesContent(m Message) []any {
	parts := make([]any, 0, 1+len(m.Attachments))
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": m.Content})
	}
	for _, a := range m.Attachments {
		mimeType := strings.ToLower(attachmentMIME(a))
		if strings.HasPrefix(mimeType, "image/") && attachmentDataURL(a) != "" {
			parts = append(parts, map[string]any{"type": "input_image", "image_url": attachmentDataURL(a)})
		} else if attachmentDataURL(a) != "" {
			parts = append(parts, map[string]any{
				"type": "input_file", "filename": a.Name, "file_data": attachmentDataURL(a),
			})
		} else {
			parts = append(parts, map[string]any{"type": "input_text", "text": attachmentText(a)})
		}
	}
	return parts
}

func openAIResponsesInput(msgs []Message) []any {
	input := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments})
			}
			continue
		}
		input = append(input, map[string]any{"role": m.Role, "content": openAIResponsesContent(m)})
	}
	return input
}

func anthropicMessages(msgs []Message) []map[string]any {
	conv := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		if m.Role == "tool" {
			conv = append(conv, map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content,
			}}})
			continue
		}
		if len(m.ToolCalls) > 0 {
			blocks := make([]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				var input map[string]any
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
			}
			conv = append(conv, map[string]any{"role": "assistant", "content": blocks})
			continue
		}
		if len(m.Attachments) == 0 {
			conv = append(conv, map[string]any{"role": m.Role, "content": m.Content})
			continue
		}
		blocks := make([]any, 0, 1+len(m.Attachments))
		if m.Content != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
		}
		for _, a := range m.Attachments {
			mimeType := strings.ToLower(attachmentMIME(a))
			if strings.HasPrefix(mimeType, "image/") && attachmentBase64(a) != "" {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": attachmentMIME(a), "data": attachmentBase64(a),
				}})
			} else if (mimeType == "application/pdf" || strings.HasPrefix(mimeType, "text/")) && attachmentBase64(a) != "" && a.Text == "" {
				blocks = append(blocks, map[string]any{"type": "document", "source": map[string]any{
					"type": "base64", "media_type": attachmentMIME(a), "data": attachmentBase64(a),
				}})
			} else {
				blocks = append(blocks, map[string]any{"type": "text", "text": attachmentText(a)})
			}
		}
		conv = append(conv, map[string]any{"role": m.Role, "content": blocks})
	}
	return conv
}

func ollamaMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		msg := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		var images []string
		for _, a := range m.Attachments {
			if strings.HasPrefix(strings.ToLower(attachmentMIME(a)), "image/") && attachmentBase64(a) != "" {
				images = append(images, attachmentBase64(a))
			} else {
				msg["content"] = strings.TrimSpace(fmt.Sprintf("%v\n\n%s", msg["content"], attachmentText(a)))
			}
		}
		if len(images) > 0 {
			msg["images"] = images
		}
		out = append(out, msg)
	}
	return out
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
		"messages": openAIChatMessages(msgs),
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
	for _, m := range msgs {
		if m.Role == "system" {
			sys = m.Content
		}
	}
	body := map[string]any{
		"model":  p.Model,
		"input":  openAIResponsesInput(msgs),
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
	for _, m := range msgs {
		if m.Role == "system" {
			sys = m.Content
		}
	}
	body := map[string]any{
		"model":      p.Model,
		"max_tokens": 8192,
		"messages":   anthropicMessages(msgs),
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
		"messages": ollamaMessages(msgs),
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

func openAIChatWithTools(ctx context.Context, p Provider, opts ChatOptions) (ChatResult, error) {
	body := map[string]any{"model": p.Model, "messages": openAIChatMessages(opts.Messages), "tools": opts.Tools, "stream": opts.OnDelta != nil}
	resp, err := postJSON(ctx, endpoint(p.URL, "/chat/completions"), map[string]string{"Authorization": "Bearer " + p.APIKey}, body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	if opts.OnDelta == nil {
		var out struct {
			Choices []struct {
				Message struct {
					Content   string     `json:"content"`
					ToolCalls []ToolCall `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return ChatResult{}, err
		}
		if len(out.Choices) == 0 {
			return ChatResult{}, fmt.Errorf("响应中没有 choices")
		}
		return ChatResult{Content: out.Choices[0].Message.Content, ToolCalls: out.Choices[0].Message.ToolCalls}, nil
	}
	var full strings.Builder
	calls := map[int]*ToolCall{}
	err = scanSSE(resp.Body, func(payload string) bool {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			return true
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			full.WriteString(d.Content)
			opts.OnDelta(d.Content)
		}
		for _, tc := range d.ToolCalls {
			c := calls[tc.Index]
			if c == nil {
				c = &ToolCall{Type: tc.Type, ID: tc.ID}
				calls[tc.Index] = c
			}
			if tc.ID != "" {
				c.ID = tc.ID
			}
			if tc.Type != "" {
				c.Type = tc.Type
			}
			c.Function.Name += tc.Function.Name
			c.Function.Arguments += tc.Function.Arguments
		}
		return true
	})
	ordered := make([]ToolCall, 0, len(calls))
	for i := 0; i < len(calls); i++ {
		if c, ok := calls[i]; ok {
			ordered = append(ordered, *c)
		}
	}
	return ChatResult{Content: full.String(), ToolCalls: ordered}, err
}

func anthropicChatWithTools(ctx context.Context, p Provider, opts ChatOptions) (ChatResult, error) {
	var system string
	for _, m := range opts.Messages {
		if m.Role == "system" {
			system = m.Content
		}
	}
	tools := make([]map[string]any, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		tools = append(tools, map[string]any{"name": t.Function.Name, "description": t.Function.Description, "input_schema": t.Function.Parameters})
	}
	body := map[string]any{"model": p.Model, "max_tokens": 8192, "messages": anthropicMessages(opts.Messages), "tools": tools, "stream": opts.OnDelta != nil}
	if system != "" {
		body["system"] = system
	}
	resp, err := postJSON(ctx, endpoint(p.URL, "/v1/messages"), map[string]string{"x-api-key": p.APIKey, "anthropic-version": "2023-06-01"}, body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	if opts.OnDelta == nil {
		var out struct {
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return ChatResult{}, err
		}
		var full strings.Builder
		var calls []ToolCall
		for _, c := range out.Content {
			if c.Type == "text" {
				full.WriteString(c.Text)
			}
			if c.Type == "tool_use" {
				b, _ := json.Marshal(c.Input)
				tc := ToolCall{ID: c.ID, Type: "function"}
				tc.Function.Name = c.Name
				tc.Function.Arguments = string(b)
				calls = append(calls, tc)
			}
		}
		return ChatResult{Content: full.String(), ToolCalls: calls}, nil
	}
	var full strings.Builder
	calls := map[string]*ToolCall{}
	var current string
	err = scanSSE(resp.Body, func(payload string) bool {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return true
		}
		if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" {
			current = ev.ContentBlock.ID
			calls[current] = &ToolCall{ID: current, Type: "function"}
			calls[current].Function.Name = ev.ContentBlock.Name
		}
		if ev.Type == "content_block_delta" {
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				full.WriteString(ev.Delta.Text)
				opts.OnDelta(ev.Delta.Text)
			}
			if ev.Delta.Type == "input_json_delta" && current != "" {
				calls[current].Function.Arguments += ev.Delta.PartialJSON
			}
		}
		return true
	})
	ordered := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		ordered = append(ordered, *c)
	}
	return ChatResult{Content: full.String(), ToolCalls: ordered}, err
}

func openAIResponsesWithTools(ctx context.Context, p Provider, opts ChatOptions) (ChatResult, error) {
	// Responses has a different function-call envelope; map the common subset.
	var sys string
	for _, m := range opts.Messages {
		if m.Role == "system" {
			sys = m.Content
		}
	}
	body := map[string]any{"model": p.Model, "input": openAIResponsesInput(opts.Messages), "tools": opts.Tools, "stream": opts.OnDelta != nil}
	if sys != "" {
		body["instructions"] = sys
	}
	resp, err := postJSON(ctx, endpoint(p.URL, "/responses"), map[string]string{"Authorization": "Bearer " + p.APIKey}, body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	if opts.OnDelta == nil {
		var out struct {
			Output []struct {
				Type      string `json:"type"`
				ID        string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return ChatResult{}, err
		}
		var full strings.Builder
		var calls []ToolCall
		for _, o := range out.Output {
			if o.Type == "message" {
				for _, c := range o.Content {
					if c.Type == "output_text" {
						full.WriteString(c.Text)
					}
				}
			}
			if o.Type == "function_call" {
				tc := ToolCall{ID: o.ID, Type: "function"}
				tc.Function.Name = o.Name
				tc.Function.Arguments = o.Arguments
				calls = append(calls, tc)
			}
		}
		return ChatResult{Content: full.String(), ToolCalls: calls}, nil
	}
	var full strings.Builder
	callArgs := map[string]*ToolCall{}
	err = scanSSE(resp.Body, func(payload string) bool {
		var ev struct {
			Type   string `json:"type"`
			Delta  string `json:"delta"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		if json.Unmarshal([]byte(payload), &ev) == nil {
			if ev.Type == "response.output_text.delta" && ev.Delta != "" {
				full.WriteString(ev.Delta)
				opts.OnDelta(ev.Delta)
			}
			if ev.Type == "response.function_call_arguments.delta" {
				c := callArgs[ev.CallID]
				if c == nil {
					c = &ToolCall{ID: ev.CallID, Type: "function"}
					c.Function.Name = ev.Name
					callArgs[ev.CallID] = c
				}
				c.Function.Arguments += ev.Delta
			}
		}
		return true
	})
	calls := make([]ToolCall, 0, len(callArgs))
	for _, c := range callArgs {
		calls = append(calls, *c)
	}
	return ChatResult{Content: full.String(), ToolCalls: calls}, err
}
