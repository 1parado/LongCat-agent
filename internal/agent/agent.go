// Package agent 实现极轻量的通用 Agent 循环：
// 用户消息 → 系统提示 + 技能注入 → 生成回复（流式）。
package agent

import (
	"context"
	"fmt"
	"strings"

	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/skills"
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
- 在必要时提供多种解决方案供选择`

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
	"python":   "Python（FastAPI、Django、Flask）",
	"go":       "Go（Gin、Echo、标准库）",
	"node":     "Node.js（Express、Fastify、NestJS）",
	"java":     "Java（Spring Boot、Maven、Gradle）",
	"rust":     "Rust（Actix、Rocket、Tokio）",
	// 数据科学
	"data":     "数据科学（Pandas、NumPy、Matplotlib、Jupyter）",
	"ml":       "机器学习（Scikit-learn、TensorFlow、PyTorch）",
	// DevOps
	"docker":   "Docker 容器化与容器编排",
	"k8s":      "Kubernetes 集群管理",
	"cicd":     "CI/CD 自动化部署",
	// 数据库
	"sql":      "SQL 数据库（PostgreSQL、MySQL、SQLite）",
	"nosql":    "NoSQL 数据库（MongoDB、Redis、Cassandra）",
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
	return &Session{Manager: m, Skills: loaded}, nil
}

// buildSystem 组装系统提示：基础提示 + 当前模式 + 工作空间 + 技能正文。
// ActiveSkill 非空时强制注入该技能；否则按关键词自动匹配。
func (s *Session) buildSystem(userInput string) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	if s.Workspace != "" {
		b.WriteString("\n\n## 当前工作空间\n")
		b.WriteString(fmt.Sprintf("你当前正在工作空间 `%s` 中工作。所有文件操作和代码编辑都应该在这个目录下进行。", s.Workspace))
	}
	if s.Mode != "" {
		b.WriteString("\n\n## 当前模式\n本轮优先采用 ")
		b.WriteString(modeDesc[s.Mode])
		b.WriteString("。")
	}
	if s.ActiveSkill != "" {
		for _, sk := range s.Skills {
			if sk.Name == s.ActiveSkill {
				b.WriteString("\n\n## 已激活技能（手动选择）\n")
				b.WriteString(fmt.Sprintf("\n### %s\n%s\n", sk.Title, sk.Body))
				break
			}
		}
	} else if matched := frontend.Match(s.Skills, userInput, 3); len(matched) > 0 {
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
