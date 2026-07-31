package llm

import "testing"

func TestMultimodalMessageAdapters(t *testing.T) {
	image := Attachment{Name: "screen.png", MIMEType: "image/png", Data: "data:image/png;base64,AAAA", Size: 4}
	pdf := Attachment{Name: "brief.pdf", MIMEType: "application/pdf", Data: "data:application/pdf;base64,BBBB"}
	text := Attachment{Name: "notes.md", MIMEType: "text/markdown", Text: "# Notes"}

	chat := openAIChatMessages([]Message{{Role: "user", Content: "请看图", Attachments: []Attachment{image}}})
	chatParts := chat[0].(map[string]any)["content"].([]any)
	if got := chatParts[1].(map[string]any)["type"]; got != "image_url" {
		t.Fatalf("chat image type = %v", got)
	}

	responses := openAIResponsesContent(Message{Role: "user", Content: "请分析", Attachments: []Attachment{image, pdf}})
	if got := responses[1].(map[string]any)["type"]; got != "input_image" {
		t.Fatalf("responses image type = %v", got)
	}
	if got := responses[2].(map[string]any)["type"]; got != "input_file" {
		t.Fatalf("responses file type = %v", got)
	}

	anthropic := anthropicMessages([]Message{{Role: "user", Content: "资料", Attachments: []Attachment{image, pdf}}})
	anthropicParts := anthropic[0]["content"].([]any)
	if got := anthropicParts[1].(map[string]any)["type"]; got != "image" {
		t.Fatalf("anthropic image type = %v", got)
	}
	if got := anthropicParts[2].(map[string]any)["type"]; got != "document" {
		t.Fatalf("anthropic document type = %v", got)
	}

	ollama := ollamaMessages([]Message{{Role: "user", Content: "看图", Attachments: []Attachment{image, text}}})
	if got := len(ollama[0]["images"].([]string)); got != 1 {
		t.Fatalf("ollama image count = %d", got)
	}
	if got := ollama[0]["content"].(string); got == "看图" {
		t.Fatal("ollama text attachment was not added to content")
	}
}
