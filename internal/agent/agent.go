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
	// 上下文压缩（按上下文窗口百分比自动压缩旧历史）
	CompressEnabled   bool    // 是否启用自动压缩
	ContextWindow     int     // 模型上下文窗口（token），0 表示用默认 500000
	CompressThreshold float64 // 触发压缩的占比（0~1），0 表示用默认 0.8
	// PlanMode 规划模式开关：开启后只做规划/阅读/产出计划文档，禁止修改代码、
	// 派生子 Agent、调用外部 MCP 工具。可用 /plan 与 /execute 切换，
	// 也可 /mode plan|execute 切换。
	PlanMode bool
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

// StreamEvent is a single item emitted while a turn streams. The producer
// (runStream, running in its own goroutine) sends these into a channel that is
// closed once the turn ends (success, error, or cancellation).
type StreamEvent struct {
	Kind  string    // "delta" | "tool" | "done"
	Delta string    // set when Kind == "delta"
	Tool  ToolEvent // set when Kind == "tool"
	Final string    // full reply, set when Kind == "done"
	Err   error     // non-nil on Kind == "done" indicates failure/cancellation
}

// Stream runs a user turn and streams its progress through a channel. The
// producer runs in a dedicated goroutine so the caller can consume events
// concurrently (e.g. flush SSE frames). Synchronous setup errors such as a
// missing active provider are returned directly and no goroutine is started.
func (s *Session) Stream(ctx context.Context, input string, attachments []llm.Attachment) (<-chan StreamEvent, error) {
	if _, err := s.Manager.Active(); err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 32)
	go func() {
		defer close(out)
		s.runStream(ctx, input, attachments, out)
	}()
	return out, nil
}

// sendEvent 向流通道发送事件。若上下文已取消（消费者中途断开/不再读取），
// 直接返回 false 让生产者及时退出——避免在 out <- 上永久阻塞导致 goroutine 泄漏。
// 正常发送返回 true。通道的最终关闭仍由 Stream 的 defer close(out) 负责。
func sendEvent(ctx context.Context, out chan<- StreamEvent, ev StreamEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// runStream executes the multi-round agent loop, pushing deltas and tool events
// into the channel and a final "done" event before returning. The channel is
// always closed by the caller (Stream) via defer, even on cancellation/panic.
// 所有 out <- 发送都经 sendEvent 包裹，确保消费者断开时生产者能及时退出，不泄漏。
func (s *Session) runStream(ctx context.Context, input string, attachments []llm.Attachment, out chan<- StreamEvent) {
	provider, err := s.Manager.Active()
	if err != nil {
		sendEvent(ctx, out, StreamEvent{Kind: "done", Err: err})
		return
	}
	s.maybeCompress(ctx, provider)
	msgs := []llm.Message{{Role: "system", Content: s.buildSystem(input)}}
	msgs = append(msgs, s.Messages...)
	msgs = append(msgs, llm.Message{Role: "user", Content: input, Attachments: attachments})

	exec := &ToolExecutor{Workspace: s.Workspace, Skills: s.Skills, MCP: s.MCP, Undo: s.Undo, Agents: s.Agents, Manager: s.Manager, Activity: s.Activity, OrchestrationDepth: s.OrchestrationDepth, PlanMode: s.PlanMode}
	onDelta := func(d string) {
		if !sendEvent(ctx, out, StreamEvent{Kind: "delta", Delta: d}) {
			return
		}
	}
	var reply string
	for round := 0; round < 8; round++ {
		if err := ctx.Err(); err != nil {
			s.commitInterrupted(input, attachments, reply)
			sendEvent(ctx, out, StreamEvent{Kind: "done", Err: err})
			return
		}
		result, err := llm.ChatWithTools(ctx, provider, llm.ChatOptions{Messages: msgs, Tools: exec.Definitions(), OnDelta: onDelta})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.commitInterrupted(input, attachments, reply)
			}
			sendEvent(ctx, out, StreamEvent{Kind: "done", Err: err})
			return
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
				sendEvent(ctx, out, StreamEvent{Kind: "done", Err: err})
				return
			}
			callID := call.ID
			if callID == "" {
				callID = fmt.Sprintf("tool-%d-%d", round, index)
			}
			if !sendEvent(ctx, out, StreamEvent{Kind: "tool", Tool: ToolEvent{ID: callID, Name: call.Function.Name, Arguments: call.Function.Arguments, Status: "running", Round: round + 1}}) {
				return
			}
			toolOut, callErr := exec.ExecuteContext(ctx, call.Function.Name, call.Function.Arguments)
			status := "success"
			if callErr != nil {
				toolOut = "工具执行失败: " + callErr.Error()
				status = "error"
			}
			if !sendEvent(ctx, out, StreamEvent{Kind: "tool", Tool: ToolEvent{ID: callID, Name: call.Function.Name, Arguments: call.Function.Arguments, Result: toolOut, Status: status, Round: round + 1}}) {
				return
			}
			msgs = append(msgs, llm.Message{Role: "tool", Content: toolOut, ToolCallID: call.ID, Name: call.Function.Name})
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
	sendEvent(ctx, out, StreamEvent{Kind: "done", Final: reply})
}

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

// SetPlanMode 切换规划/执行模式。on=true 进入 Plan 模式（只规划、可建文档、不改代码）。
func (s *Session) SetPlanMode(on bool) {
	s.PlanMode = on
}

// PlanLabel 返回当前规划模式的人类可读标签，用于状态栏/提示。
func (s *Session) PlanLabel() string {
	if s.PlanMode {
		return "plan"
	}
	return "execute"
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
	return &Session{Manager: m, Skills: loaded, Agents: agents, Activity: NewActivityTracker(), CompressEnabled: true, ContextWindow: 500000, CompressThreshold: 0.8}, nil
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
	if s.PlanMode {
		b.WriteString("\n\n## 当前为 Plan 规划模式（只读）\n" +
			"你处于规划模式，**只能做规划，不能改动代码**。具体约束：\n" +
			"1. 可以阅读代码、列出目录、加载技能、预览页面（list_directory / read_file / load_skill / preview_file 可用）。\n" +
			"2. 可以使用 write_file **仅创建文档类文件**（如 .md / .txt 计划文档），**严禁**用它修改任何代码文件（.go/.ts/.js/.py/.vue/.css/.json 等）。\n" +
			"3. 禁止派生子 Agent（spawn_subagent）、禁止调用外部 MCP 工具。\n" +
			"4. 你的输出应是一份清晰、可执行的方案：目标、改动点、文件清单、步骤、风险与待确认项。必要时把方案写入工作空间下的 PLAN.md 等文档供用户审阅。\n" +
			"5. 不要假装已经实施了改动；如实说明哪些是计划、哪些尚未执行。")
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
// It is a callback-style convenience wrapper over Stream: it consumes the
// goroutine-driven event channel and invokes the supplied callbacks. The
// attachment metadata is persisted with the user message so restored sessions
// can render the same message cards.
func (s *Session) AskWithAttachments(ctx context.Context, input string, attachments []llm.Attachment, onDelta llm.StreamFunc, onTool ToolEventFunc) (string, error) {
	ch, err := s.Stream(ctx, input, attachments)
	if err != nil {
		return "", err
	}
	var final string
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			if onDelta != nil {
				onDelta(ev.Delta)
			}
		case "tool":
			if onTool != nil {
				onTool(ev.Tool)
			}
		case "done":
			final = ev.Final
			if ev.Err != nil {
				return final, ev.Err
			}
		}
	}
	return final, nil
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

// ---------- 上下文压缩（自动会话压缩） ----------

const (
	compressKeepRecent = 6 // 压缩时保留的最近消息条数
	compressMinHistory = 8 // 历史少于此值不压缩
)

// summarizeFunc 可被测试替换，避免依赖真实 LLM。
var summarizeFunc = summarize

// maybeCompress 在每轮对话开始前检查上下文用量，若超过阈值则把较早的历史
// 压缩成一条摘要消息，仅保留最近 compressKeepRecent 条原始消息。
// 压缩失败（如 LLM 调用出错）时静默跳过，不阻塞正常对话。
func (s *Session) maybeCompress(ctx context.Context, provider llm.Provider) {
	if !s.CompressEnabled {
		return
	}
	cw := s.ContextWindow
	if cw <= 0 {
		cw = 500000
	}
	th := s.CompressThreshold
	if th <= 0 {
		th = 0.8
	}
	if len(s.Messages) < compressMinHistory {
		return
	}
	est := estimateTokens(append([]llm.Message{{Role: "system", Content: s.buildSystem("")}}, s.Messages...))
	if float64(est) < float64(cw)*th {
		return
	}
	if len(s.Messages) <= compressKeepRecent {
		return
	}
	older := s.Messages[:len(s.Messages)-compressKeepRecent]
	recent := s.Messages[len(s.Messages)-compressKeepRecent:]
	summary, err := summarizeFunc(ctx, provider, renderConversation(older))
	if err != nil || summary == "" {
		return
	}
	s.Messages = append([]llm.Message{{
		Role:    "user",
		Content: "[历史对话压缩摘要]\n" + summary,
	}}, recent...)
}

// estimateTokens 用粗略启发式估算 token 数（不引入分词依赖）。
// 经验公式：每 ~4 个字符 ≈ 1 token，并计入附件与工具调用开销。
func estimateTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len([]rune(m.Content))/4 + 1
		for _, a := range m.Attachments {
			n += len([]rune(a.Text)) / 4
			if a.Data != "" {
				n += len(a.Data) / 4
			}
		}
		for range m.ToolCalls {
			n += 8
		}
	}
	return n
}

// renderConversation 把消息列表渲染为纯文本，供压缩器消费。
func renderConversation(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		role := m.Role
		if role == "tool" {
			role = "tool(" + m.Name + ")"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// summarize 调用 LLM 将一段对话压缩为简洁摘要（中文）。
func summarize(ctx context.Context, provider llm.Provider, conversation string) (string, error) {
	const sys = "你是会话压缩器。请把下面的对话压缩为简洁摘要，保留：用户的关键需求与意图、已做出的决策与结论、重要的代码片段与文件路径、待办事项。去掉寒暄、冗余与重复内容。使用中文，长度不超过原文的 1/3。"
	resp, err := llm.Chat(ctx, provider, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: conversation},
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp), nil
}
