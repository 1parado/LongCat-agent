// Package agent 实现极轻量的前端 Agent 循环：
// 用户消息 → 前端优化系统提示 + 技能注入 → 生成回复（流式）。
package agent

import (
	"context"
	"fmt"
	"strings"

	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/llm"
)

const systemPrompt = `你是 LongCat-frontend —— 一个专精 Web 前端开发的轻量级助手。

## 专长范围（严格限定）
- 主力: React + Next.js（App Router、Server Components、Server Actions）
- 支持: Vue 3（组合式 API）、Svelte、原生 JS/HTML/CSS
- 样式: Tailwind CSS、设计系统、shadcn/ui、Radix
- 关注: 组件生成、可访问性(a11y)、性能优化、现代简洁 UI

## 行为准则
1. 只回答前端问题；后端/移动端/运维问题请礼貌说明超出范围。
2. 代码优先: 直接给出可运行的完整代码，TypeScript 优先。
3. 现代简洁审美: 深色模式友好、微动画、克制的配色。
4. 可访问性是底线，不是可选项。
5. 回答用简体中文，代码注释精炼。`

// Session 一次对话会话。
type Session struct {
	Manager  *llm.Manager
	Skills   []frontend.Skill
	Messages []llm.Message
}

// NewSession 创建会话并加载 skillsDir 下的技能。
func NewSession(m *llm.Manager, skillsDir string) (*Session, error) {
	skills, err := frontend.LoadSkills(skillsDir)
	if err != nil {
		return nil, fmt.Errorf("加载技能失败: %w", err)
	}
	return &Session{Manager: m, Skills: skills}, nil
}

// buildSystem 组装系统提示：基础提示 + 命中的技能正文。
func (s *Session) buildSystem(userInput string) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	matched := frontend.Match(s.Skills, userInput, 3)
	if len(matched) > 0 {
		b.WriteString("\n\n## 已激活技能\n")
		for _, sk := range matched {
			b.WriteString(fmt.Sprintf("\n### %s\n%s\n", sk.Title, sk.Body))
		}
	}
	return b.String()
}

// MatchedSkills 返回本轮输入命中的技能标题（用于 UI 展示）。
func (s *Session) MatchedSkills(userInput string) []string {
	var names []string
	for _, sk := range frontend.Match(s.Skills, userInput, 3) {
		names = append(names, sk.Title)
	}
	return names
}

// Ask 发送用户消息，onDelta 流式回调，返回完整回复并记入历史。
func (s *Session) Ask(ctx context.Context, input string, onDelta llm.StreamFunc) (string, error) {
	provider, err := s.Manager.Active()
	if err != nil {
		return "", err
	}
	msgs := []llm.Message{{Role: "system", Content: s.buildSystem(input)}}
	msgs = append(msgs, s.Messages...)
	msgs = append(msgs, llm.Message{Role: "user", Content: input})

	reply, err := llm.Chat(ctx, provider, msgs, onDelta)
	if err != nil {
		return "", err
	}
	s.Messages = append(s.Messages,
		llm.Message{Role: "user", Content: input},
		llm.Message{Role: "assistant", Content: reply},
	)
	// 轻量会话：只保留最近 20 条，避免无限膨胀。
	if len(s.Messages) > 20 {
		s.Messages = s.Messages[len(s.Messages)-20:]
	}
	return reply, nil
}

// Reset 清空会话历史。
func (s *Session) Reset() { s.Messages = nil }
