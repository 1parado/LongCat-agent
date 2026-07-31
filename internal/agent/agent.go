// Package agent 实现极轻量的通用 Agent 循环：
// 用户消息 → 系统提示 + 技能注入 → 生成回复（流式）。
package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/mcp"
	"LongCat-frontend/internal/skills"
	"LongCat-frontend/internal/workspace"
)

const systemPrompt = `你是 LongCat —— 一个智能、高效、全能的 AI 助手。

## 核心能力
- **编程开发**: 精通多种编程语言（Python、JavaScript/TypeScript、Go、Java、C++、Rust 等），熟悉主流框架和工具
- **前端开发**: React、Next.js、Vue 3、Svelte、Tailwind CSS、现代 UI/UX 设计
- **后端开发**: Node.js、Django、Flask、FastAPI、Spring Boot、Gin、数据库设计
- **DevOps**: Docker、Kubernetes、CI/CD、云服务（AWS、GCP、Azure）
- **数据科学**: 数据分析、机器学习、深度学习、可视化
- **文档写作**: 技术文档、API 文档、教程、博客文章
- **问题解决**: 调试、性能优化、架构设计、最佳实践

## 行为准则
1. **通用助手**: 能够处理各种领域的问题，不限于前端开发
2. **代码优先**: 直接给出可运行的完整代码，注重代码质量和最佳实践
3. **清晰沟通**: 用简洁明了的语言解释复杂概念，提供实用的建议
4. **主动思考**: 理解用户的真实需求，提供超出预期的解决方案
5. **持续学习**: 保持对新技术和最佳实践的关注
6. **中文友好**: 默认使用简体中文回答，代码注释精炼清晰

## 工作方式
- 提供完整、可运行的代码示例
- 解释关键概念和设计决策
- 指出潜在问题和优化方向
- 提供相关资源和扩展阅读
- 在必要时提供多种解决方案供选择

## 内置浏览器
- 当用户要求打开、预览或查看当前工作空间中的 HTML 页面时，必须调用 preview_file 工具，让桌面端内置浏览器打开页面；不要声称没有浏览器，也不要只给出手动打开文件的说明。`

// Session 一次对话会话。
type Session struct {
	Manager  *llm.Manager
	Skills   []frontend.Skill
	Messages []llm.Message
	// Mode 当前工作模式（react/nextjs/vue/python/go/等），空表示自动。
	// 由 /mode 命令或 Web UI 的模式选择设置，注入到系统提示。
	Mode string
	// ActiveSkill 手动激活的技能名，空则按关键词自动匹配。
	ActiveSkill string
	// Workspace 当前工作空间路径，空表示无工作空间。
	Workspace string
	// MCP is the project-scoped external tool registry.
	MCP *mcp.Manager
	// Agents contains discovered sub-agent definitions with precedence applied.
	Agents             []AgentDefinition
	Activity           *ActivityTracker
	OrchestrationDepth int
	Undo               *workspace.UndoStore
	DefinitionOverride string
}

// ToolEvent describes one tool invocation for clients that want to render the
// agent's progress. Result is populated after execution completes.
type ToolEvent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Status    string `json:"status"` // running, success, error
	Round     int    `json:"round,omitempty"`
}

// ToolEventFunc receives tool lifecycle updates.
type ToolEventFunc func(ToolEvent)

// modeDesc 模式到提示词片段的映射。
var modeDesc = map[string]string{
	// 前端开发
	"react":    "React（函数组件 + Hooks）",
	"nextjs":   "Next.js（App Router、Server Components、Server Actions）",
	"vue":      "Vue 3（组合式 API + <script setup>）",
	"svelte":   "Svelte / SvelteKit",
	"tailwind": "Tailwind CSS 优先的样式方案",
	// 后端开发
	"python": "Python（FastAPI、Django、Flask）",
	"go":     "Go（Gin、Echo、标准库）",
	"node":   "Node.js（Express、Fastify、NestJS）",
	"java":   "Java（Spring Boot、Maven、Gradle）",
	"rust":   "Rust（Actix、Rocket、Tokio）",
	// 数据科学
	"data": "数据科学（Pandas、NumPy、Matplotlib、Jupyter）",
	"ml":   "机器学习（Scikit-learn、TensorFlow、PyTorch）",
	// DevOps
	"docker": "Docker 容器化与容器编排",
	"k8s":    "Kubernetes 集群管理",
	"cicd":   "CI/CD 自动化部署",
	// 数据库
	"sql":   "SQL 数据库（PostgreSQL、MySQL、SQLite）",
	"nosql": "NoSQL 数据库（MongoDB、Redis、Cassandra）",
	// 其他
	"api":      "API 设计与开发（RESTful、GraphQL、gRPC）",
	"test":     "测试（单元测试、集成测试、E2E 测试）",
	"security": "安全性（认证、授权、加密、漏洞防护）",
}

// SetMode 切换工作模式；非法模式返回错误。
func (s *Session) SetMode(m string) error {
	if _, ok := modeDesc[m]; !ok {
		return fmt.Errorf("未知模式 %q，可选: react|nextjs|vue|svelte|tailwind|python|go|node|java|rust|data|ml|docker|k8s|cicd|sql|nosql|api|test|security", m)
	}
	s.Mode = m
	return nil
}

// NewSession 创建会话并加载技能：项目级 skillsDir（可选）+ 用户级
// ~/.longcat-frontend/skills/（Market 安装的技能）。
func NewSession(m *llm.Manager, skillsDir string) (*Session, error) {
	var loaded []frontend.Skill
	if skillsDir != "" {
		s, err := frontend.LoadSkills(skillsDir)
		if err != nil {
			return nil, fmt.Errorf("加载项目技能失败: %w", err)
		}
		loaded = append(loaded, s...)
	}
	// 用户级 skills（Market 安装）
	if st, err := skills.NewStore(); err == nil {
		if s, err := frontend.LoadSkills(st.Dir()); err == nil {
			loaded = append(loaded, s...)
		}
	}
	var agents []AgentDefinition
	userAgents := ""
	if dir, err := llm.ConfigDir(); err == nil {
		userAgents = filepath.Join(dir, "agents")
	}
	bundledAgents := ""
	if skillsDir != "" {
		bundledAgents = filepath.Join(filepath.Dir(skillsDir), "agents")
	}
	if discovered, err := DiscoverAgents("", userAgents, bundledAgents); err == nil {
		agents = discovered
	}
	return &Session{Manager: m, Skills: loaded, Agents: agents, Activity: NewActivityTracker()}, nil
}

// buildSystem 组装系统提示：基础提示 + 当前模式 + 工作空间 + 技能正文。
// ActiveSkill 非空时强制注入该技能；否则按关键词自动匹配。
func (s *Session) buildSystem(userInput string) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	if s.DefinitionOverride != "" {
		b.WriteString("\n\n## 当前 Agent 专长\n")
		b.WriteString(s.DefinitionOverride)
	}
	if s.Workspace != "" {
		b.WriteString("\n\n## 当前工作空间\n")
		b.WriteString(fmt.Sprintf("你当前正在工作空间 `%s` 中工作。所有文件操作和代码编辑都应该在这个目录下进行。", s.Workspace))
	}
	if s.Mode != "" {
		b.WriteString("\n\n## 当前模式\n本轮优先采用 ")
		b.WriteString(modeDesc[s.Mode])
		b.WriteString("。")
	}
	if len(s.Skills) > 0 {
		b.WriteString("\n\n## 可用技能\n需要某项专门知识时，主动调用 `load_skill` 工具读取正文。\n")
		for _, sk := range s.Skills {
			b.WriteString(fmt.Sprintf("- `%s`: %s\n", sk.Name, sk.Description))
		}
	}
	if s.ActiveSkill != "" {
		for _, sk := range s.Skills {
			if sk.Name == s.ActiveSkill {
				b.WriteString("\n\n## 已激活技能（手动选择）\n")
				b.WriteString(fmt.Sprintf("技能 `%s` 已选中；需要正文时调用 `load_skill`。\n", sk.Name))
				break
			}
		}
	} else if matched := frontend.Match(s.Skills, userInput, 3); len(matched) > 0 {
		b.WriteString("\n\n## 相关技能\n")
		for _, sk := range matched {
			b.WriteString(fmt.Sprintf("- `%s`: %s（需要时调用 load_skill）\n", sk.Name, sk.Description))
		}
	}
	if len(s.Agents) > 0 {
		b.WriteString("\n\n## 可委派的子 Agent\n需要专门角色时调用 `spawn_subagent`，并提供名称和清晰任务。\n")
		for _, definition := range s.Agents {
			b.WriteString(fmt.Sprintf("- `%s`: %s\n", definition.Name, definition.Description))
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
	return s.AskWithEvents(ctx, input, onDelta, nil)
}

// AskWithEvents is Ask with optional tool lifecycle notifications.
func (s *Session) AskWithEvents(ctx context.Context, input string, onDelta llm.StreamFunc, onTool ToolEventFunc) (string, error) {
	return s.AskWithAttachments(ctx, input, nil, onDelta, onTool)
}

// AskWithAttachments sends a user turn with optional multimodal attachments.
// The attachment metadata is persisted with the user message so restored
// sessions can render the same message cards.
func (s *Session) AskWithAttachments(ctx context.Context, input string, attachments []llm.Attachment, onDelta llm.StreamFunc, onTool ToolEventFunc) (string, error) {
	provider, err := s.Manager.Active()
	if err != nil {
		return "", err
	}
	msgs := []llm.Message{{Role: "system", Content: s.buildSystem(input)}}
	msgs = append(msgs, s.Messages...)
	msgs = append(msgs, llm.Message{Role: "user", Content: input, Attachments: attachments})

	exec := &ToolExecutor{Workspace: s.Workspace, Skills: s.Skills, MCP: s.MCP, Undo: s.Undo, Agents: s.Agents, Manager: s.Manager, Activity: s.Activity, OrchestrationDepth: s.OrchestrationDepth}
	var reply string
	for round := 0; round < 8; round++ {
		if err := ctx.Err(); err != nil {
			s.commitInterrupted(input, attachments, reply)
			return reply, err
		}
		result, err := llm.ChatWithTools(ctx, provider, llm.ChatOptions{Messages: msgs, Tools: exec.Definitions(), OnDelta: onDelta})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.commitInterrupted(input, attachments, reply)
			}
			return reply, err
		}
		reply += result.Content
		if len(result.ToolCalls) == 0 {
			break
		}
		// Preserve the assistant tool-call turn, then append one tool result per call.
		assistant := llm.Message{Role: "assistant", Content: result.Content, ToolCalls: result.ToolCalls}
		msgs = append(msgs, assistant)
		for index, call := range result.ToolCalls {
			if err := ctx.Err(); err != nil {
				s.commitInterrupted(input, attachments, reply)
				return reply, err
			}
			callID := call.ID
			if callID == "" {
				callID = fmt.Sprintf("tool-%d-%d", round, index)
			}
			if onTool != nil {
				onTool(ToolEvent{ID: callID, Name: call.Function.Name, Arguments: call.Function.Arguments, Status: "running", Round: round + 1})
			}
			out, callErr := exec.ExecuteContext(ctx, call.Function.Name, call.Function.Arguments)
			status := "success"
			if callErr != nil {
				out = "工具执行失败: " + callErr.Error()
				status = "error"
			}
			if onTool != nil {
				onTool(ToolEvent{ID: callID, Name: call.Function.Name, Arguments: call.Function.Arguments, Result: out, Status: status, Round: round + 1})
			}
			msgs = append(msgs, llm.Message{Role: "tool", Content: out, ToolCallID: call.ID, Name: call.Function.Name})
		}
	}
	s.Messages = append(s.Messages,
		llm.Message{Role: "user", Content: input, Attachments: attachments},
		llm.Message{Role: "assistant", Content: reply},
	)
	// 轻量会话：只保留最近 20 条，避免无限膨胀。
	if len(s.Messages) > 20 {
		s.Messages = s.Messages[len(s.Messages)-20:]
	}
	return reply, nil
}

func (s *Session) commitInterrupted(input string, attachments []llm.Attachment, reply string) {
	s.Messages = append(s.Messages, llm.Message{Role: "user", Content: input, Attachments: attachments})
	if reply != "" {
		s.Messages = append(s.Messages, llm.Message{Role: "assistant", Content: reply})
	}
	if len(s.Messages) > 20 {
		s.Messages = s.Messages[len(s.Messages)-20:]
	}
}

// Reset 清空会话历史。
func (s *Session) Reset() { s.Messages = nil }

// ReloadSkills 重新加载用户级 skills 并合并（覆盖同名、新增追加）。
// 安装/卸载 skill 后调用，使变更立即对后续对话生效。
func (s *Session) ReloadSkills() {
	st, err := skills.NewStore()
	if err != nil {
		return
	}
	userSkills, err := frontend.LoadSkills(st.Dir())
	if err != nil {
		return
	}
	byName := map[string]int{}
	for i, sk := range s.Skills {
		byName[sk.Name] = i
	}
	for _, sk := range userSkills {
		if idx, ok := byName[sk.Name]; ok {
			s.Skills[idx] = sk
		} else {
			s.Skills = append(s.Skills, sk)
		}
	}
}
