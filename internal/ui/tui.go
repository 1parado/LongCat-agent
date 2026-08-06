// Package ui 实现现代简洁风格的终端 UI（TUI）。
//
// TTY 环境下使用 bubbletea 提供：
//   - Markdown 渲染（glamour）
//   - 多窗格布局（对话区 + 侧栏：供应商 / 技能 / 状态）
//   - 鼠标支持（滚轮滚动、点击顶栏 [≡] 切换侧栏）
//   - 流式对话、历史、命令系统、主题切换
//
// 非 tty（管道 / 重定向）自动回退到行模式，保证可脚本化。
//
// 依赖：bubbletea / bubbles / lipgloss / glamour + stdlib + go-isatty。
package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/skills"
	"LongCat-frontend/internal/utils"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/mattn/go-isatty"

	"LongCat-frontend/internal/plugin"
)

// ---------- 主题配色（lipgloss 用 ANSI 256 色号） ----------

type stylePalette struct {
	user, muted, primary, accent, ok, err lipgloss.Color
}

var styles = map[string]stylePalette{
	"dark": {
		user: "117", muted: "245", primary: "111",
		accent: "183", ok: "114", err: "210",
	},
	"light": {
		user: "26", muted: "242", primary: "25",
		accent: "91", ok: "29", err: "124",
	},
}

const sidebarWidth = 30

// ---------- 模型 ----------

type block struct {
	role string // user | assistant | system
	raw  string
}

type wizardState struct {
	kind   string // "add" | "update"
	id     string
	fields []string
	idx    int
	values map[string]string
}

type model struct {
	tui         *TUI
	session     *agent.Session
	ansi        bool
	width       int
	height      int
	themeName   string
	viewport    viewport.Model
	input       textarea.Model
	conv        []block
	pendingRaw  string
	streaming   bool
	showSidebar bool
	wizard      *wizardState
	ready       bool
	prog        *tea.Program
}

type deltaMsg struct{ text string }
type doneMsg struct{ err error }

func (m model) Init() tea.Cmd { return textarea.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.layout()
		return m, nil

	case tea.KeyMsg:
		// 全局快捷键优先于输入框
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
			if m.input.Value() == "" {
				return m, tea.Quit
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+d"))):
			return m, tea.Quit
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+b"))):
			m.showSidebar = !m.showSidebar
			m.layout()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+t"))):
			m.setTheme(toggleTheme(m.themeName))
			m.updateViewport()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) && m.wizard != nil:
			m.wizard = nil
			m.input.Reset()
			m.conv = append(m.conv, block{"system", "已取消"})
			m.updateViewport()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) && !msg.Alt:
			if m.wizard != nil {
				return m, m.handleWizardEnter()
			}
			return m, m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tea.MouseMsg:
		if msg.Type == tea.MouseLeft && msg.Y == 0 && msg.X >= m.width-4 {
			m.showSidebar = !m.showSidebar
			m.layout()
			return m, nil
		}
		if msg.Type == tea.MouseWheelUp || msg.Type == tea.MouseWheelDown {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case deltaMsg:
		m.pendingRaw += msg.text
		m.updateViewport()
		return m, nil

	case doneMsg:
		if m.streaming {
			if msg.err != nil {
				m.conv = append(m.conv, block{"assistant", m.pendingRaw})
				m.conv = append(m.conv, block{"system", "✖ " + msg.err.Error()})
			} else {
				m.conv = append(m.conv, block{"assistant", m.pendingRaw})
			}
			m.pendingRaw = ""
			m.streaming = false
			m.updateViewport()
		}
		return m, nil
	}
	return m, nil
}

func (m model) View() string {
	if !m.ready {
		return "loading…"
	}
	status := m.renderStatus()
	var body string
	if m.showSidebar {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.viewport.View())
	} else {
		body = m.viewport.View()
	}
	return lipgloss.JoinVertical(lipgloss.Left, status, body, m.input.View())
}

// ---------- 布局与渲染 ----------

func (m *model) layout() {
	statusH, inputH := 1, 3
	chatH := m.height - statusH - inputH
	if chatH < 3 {
		chatH = 3
	}
	chatW := m.width - 2
	if m.showSidebar {
		chatW = m.width - sidebarWidth - 1
	}
	if chatW < 20 {
		chatW = 20
	}
	m.viewport.Width = chatW
	m.viewport.Height = chatH
	m.input.SetWidth(m.width - 2)
	m.input.SetHeight(2)
	m.updateViewport()
}

func (m *model) chatWidth() int {
	w := m.width - 2
	if m.showSidebar {
		w = m.width - sidebarWidth - 1
	}
	if w < 20 {
		w = 20
	}
	return w
}

func (m *model) renderMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	w := m.chatWidth()
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(m.themeName), glamour.WithWordWrap(w))
	if err != nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return out
}

func (m *model) renderConv() string {
	var b strings.Builder
	for _, blk := range m.conv {
		switch blk.role {
		case "user":
			b.WriteString(lipgloss.NewStyle().Foreground(styles[m.themeName].user).Render("❯ "+blk.raw) + "\n")
		case "assistant":
			b.WriteString(m.renderMarkdown(blk.raw))
		case "system":
			b.WriteString(lipgloss.NewStyle().Foreground(styles[m.themeName].muted).Render(blk.raw) + "\n")
		}
	}
	if m.streaming {
		b.WriteString(m.renderMarkdown(m.pendingRaw))
	}
	return b.String()
}

func (m *model) updateViewport() {
	m.viewport.SetContent(m.renderConv())
	m.viewport.GotoBottom()
}

func (m *model) renderStatus() string {
	p := "—"
	if prov, err := m.session.Manager.Active(); err == nil {
		p = prov.ID
	}
	line := fmt.Sprintf(" ● 供应商: %s  ·  技能 %d · 会话 %d · 模式 %s · 主题 %s   [≡]",
		p, len(m.session.Skills), len(m.session.Messages), m.modeLabel(), m.themeName)
	return lipgloss.NewStyle().
		Foreground(styles[m.themeName].muted).
		Background(lipgloss.Color("235")).
		Render(line)
}

func (m *model) renderSidebar() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(styles[m.themeName].primary).Render("侧栏 / Sidebar") + "\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles[m.themeName].muted).Render("供应商:") + "\n")
	for _, pr := range m.session.Manager.List() {
		mark := "  "
		if pr.ID == m.session.Manager.ActiveID() {
			mark = "● "
		}
		b.WriteString(mark + pr.ID + "\n")
	}
	b.WriteString("\n" + lipgloss.NewStyle().Foreground(styles[m.themeName].muted).Render("技能:") + "\n")
	for _, s := range m.session.Skills {
		b.WriteString("◆ " + s.Title + "\n")
	}
	return lipgloss.NewStyle().Width(sidebarWidth).Render(b.String())
}

func (m *model) setTheme(name string) {
	if _, ok := styles[name]; !ok {
		return
	}
	m.themeName = name
}

func toggleTheme(name string) string {
	if name == "light" {
		return "dark"
	}
	return "light"
}

func (m *model) modeLabel() string {
	if md := m.session.Mode; md != "" {
		return md
	}
	return "auto"
}

// ---------- 提交与流式 ----------

func (m *model) submit() tea.Cmd {
	val := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if val == "" {
		return nil
	}
	if strings.HasPrefix(val, "/") {
		if quit := m.tui.execCommand(m, val); quit {
			return tea.Quit
		}
		return nil
	}
	if names := m.session.MatchedSkills(val); len(names) > 0 {
		m.conv = append(m.conv, block{"system", "◆ 激活技能: " + strings.Join(names, " · ")})
	}
	m.conv = append(m.conv, block{"user", val})
	m.streaming = true
	m.pendingRaw = ""
	m.updateViewport()
	return m.stream(val)
}

func (m *model) stream(input string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			_, err := m.session.Ask(context.Background(), input, func(d string) {
				if m.prog != nil {
					m.prog.Send(deltaMsg{d})
				}
			})
			if m.prog != nil {
				m.prog.Send(doneMsg{err})
			}
		}()
		return nil
	}
}

// ---------- 向导 ----------

func (m *model) handleWizardEnter() tea.Cmd {
	field := m.wizard.fields[m.wizard.idx]
	m.wizard.values[field] = strings.TrimSpace(m.input.Value())
	m.input.Reset()
	m.wizard.idx++
	if m.wizard.idx < len(m.wizard.fields) {
		m.conv = append(m.conv, block{"system", m.wizardPrompt()})
		m.updateViewport()
		return nil
	}
	result := m.tui.finalizeWizard(m.wizard)
	m.wizard = nil
	m.conv = append(m.conv, block{"system", result})
	m.updateViewport()
	return nil
}

func (m *model) wizardPrompt() string {
	f := m.wizard.fields[m.wizard.idx]
	total := len(m.wizard.fields)
	switch f {
	case "id":
		return fmt.Sprintf("添加供应商 — 步骤 %d/%d: ID", m.wizard.idx+1, total)
	case "url":
		return fmt.Sprintf("步骤 %d/%d: Base URL", m.wizard.idx+1, total)
	case "key":
		return fmt.Sprintf("步骤 %d/%d: API Key", m.wizard.idx+1, total)
	case "protocol":
		return fmt.Sprintf("步骤 %d/%d: Protocol（默认 openai-chat）", m.wizard.idx+1, total)
	case "model":
		return fmt.Sprintf("步骤 %d/%d: Model", m.wizard.idx+1, total)
	}
	return fmt.Sprintf("步骤 %d/%d: %s", m.wizard.idx+1, total, f)
}

// ---------- 运行入口 ----------

// TUI 终端交互界面。
type TUI struct {
	session   *agent.Session
	ansi      bool
	width     int
	themeName string
	pal       palette
	market    *skills.Market
	plugins   *plugin.Manager
}

// SetPlugins 注入插件管理器，使 /plugins 命令可列出与启停插件。
func (t *TUI) SetPlugins(pm *plugin.Manager) *TUI {
	t.plugins = pm
	return t
}

// New 创建 TUI。
func New(s *agent.Session, ansi bool) *TUI {
	w, _ := utils.Size()
	if w < 40 {
		w = 80
	}
	name := "dark"
	return &TUI{session: s, ansi: ansi, width: w, themeName: name, pal: palettes[name]}
}

// Run 启动交互循环。TTY 走 bubbletea，非 tty 回退行模式。
func (t *TUI) Run() error {
	if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return t.runLineMode()
	}
	return t.runBubble()
}

func (t *TUI) runBubble() error {
	m := &model{
		tui:         t,
		session:     t.session,
		ansi:        t.ansi,
		themeName:   t.themeName,
		showSidebar: true,
		input:       textarea.New(),
		viewport:    viewport.New(0, 0),
	}
	m.input.Placeholder = "输入消息或 /help · Enter 发送 · Shift+Enter 换行 · Ctrl+B 侧栏 · Ctrl+T 主题"
	m.input.Focus()
	m.input.ShowLineNumbers = false
	m.input.SetHeight(2)
	m.viewport.MouseWheelEnabled = true
	m.conv = append(m.conv, block{"system", "⚡ LongCat-frontend · React · Next.js · Vue 3 · Svelte · Tailwind"})

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.prog = p
	_, err := p.Run()
	return err
}

// ---------- 命令与向导（TTY / 行模式共用逻辑） ----------

// execCommand 在 bubbletea 模式下分发命令。
func (t *TUI) execCommand(m *model, line string) (quit bool) {
	fields := strings.Fields(line)
	if len(fields) > 0 && (fields[0] == "/theme") {
		if len(fields) >= 2 {
			m.setTheme(fields[1])
		} else {
			m.setTheme(toggleTheme(m.themeName))
		}
		m.updateViewport()
		return false
	}
	quit, out, w := t.dispatch(line)
	for _, l := range out {
		m.conv = append(m.conv, block{"system", l})
	}
	if w != nil {
		m.wizard = w
	}
	if quit {
		return true
	}
	m.updateViewport()
	return false
}

// dispatch 返回输出行与可选向导；两种运行模式共用。
func (t *TUI) dispatch(line string) (quit bool, out []string, w *wizardState) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, nil, nil
	}
	cmd, args := fields[0], fields[1:]
	switch cmd {
	case "/quit", "/exit", "/q":
		return true, nil, nil
	case "/help", "/h":
		return false, t.helpLines(), nil
	case "/skills":
		return false, t.skillsLines(), nil
	case "/providers", "/p":
		return false, t.providersLines(), nil
	case "/use":
		if len(args) != 1 {
			return false, []string{"✖ 用法: /use <id>"}, nil
		}
		if err := t.session.Manager.SetActive(args[0]); err != nil {
			return false, []string{"✖ " + err.Error()}, nil
		}
		return false, []string{"✔ 已切换供应商 " + args[0]}, nil
	case "/add":
		wz := &wizardState{kind: "add", fields: []string{"id", "url", "key", "protocol", "model"}, idx: 0, values: map[string]string{}}
		return false, []string{"添加供应商 — 步骤 1/5: ID"}, wz
	case "/update", "/edit":
		if len(args) != 1 {
			return false, []string{"✖ 用法: /update <id>"}, nil
		}
		p, ok := t.session.Manager.Get(args[0])
		if !ok {
			return false, []string{"✖ 供应商不存在"}, nil
		}
		wz := &wizardState{kind: "update", id: args[0], fields: []string{"url", "key", "protocol", "model"}, idx: 0,
			values: map[string]string{"url": p.URL, "protocol": string(p.Protocol), "model": p.Model}}
		return false, []string{"修改供应商 " + args[0] + " — 步骤 1/4: Base URL（回车保留）"}, wz
	case "/remove", "/rm":
		if len(args) != 1 {
			return false, []string{"✖ 用法: /remove <id>"}, nil
		}
		if err := t.session.Manager.Remove(args[0]); err != nil {
			return false, []string{"✖ " + err.Error()}, nil
		}
		return false, []string{"✔ 已删除供应商 " + args[0]}, nil
	case "/model":
		if len(args) == 0 {
			p, err := t.session.Manager.Active()
			if err != nil {
				return false, []string{"✖ " + err.Error()}, nil
			}
			return false, []string{fmt.Sprintf("当前模型: %s（供应商 %s）", p.Model, p.ID)}, nil
		}
		id := t.session.Manager.ActiveID()
		if id == "" {
			return false, []string{"✖ 无激活供应商，先 /use 切换或 /add 添加"}, nil
		}
		if err := t.session.Manager.SetModel(id, args[0]); err != nil {
			return false, []string{"✖ " + err.Error()}, nil
		}
		return false, []string{"✔ 模型已切换: " + args[0]}, nil
	case "/mode":
		if len(args) == 0 {
			return false, []string{"当前模式: " + t.modeLabel() + "（框架可选: react|nextjs|vue|tailwind|svelte；规划切换: plan|execute）"}, nil
		}
		switch args[0] {
		case "plan":
			t.session.SetPlanMode(true)
			return false, []string{"✔ 已切换到 Plan 规划模式：只规划、可创建文档，不修改代码。用 /execute 恢复执行。"}, nil
		case "execute":
			t.session.SetPlanMode(false)
			return false, []string{"✔ 已切换到 Execute 执行模式：可正常执行代码改动。"}, nil
		default:
			if err := t.session.SetMode(args[0]); err != nil {
				return false, []string{"✖ " + err.Error()}, nil
			}
			return false, []string{"✔ 模式已切换: " + args[0]}, nil
		}
	case "/plan":
		t.session.SetPlanMode(true)
		return false, []string{"✔ 已切换到 Plan 规划模式：只规划、可创建文档，不修改代码。用 /execute 恢复执行。"}, nil
	case "/execute":
		t.session.SetPlanMode(false)
		return false, []string{"✔ 已切换到 Execute 执行模式：可正常执行代码改动。"}, nil
	case "/clear", "/reset":
		t.session.Reset()
		return false, []string{"✔ 会话已重置"}, nil
	case "/plugins":
		return false, t.pluginsLines(), nil
	case "/plugin":
		if len(args) < 2 {
			return false, []string{"✖ 用法: /plugin <enable|disable> <id>"}, nil
		}
		id := args[1]
		var err error
		if args[0] == "enable" {
			err = t.plugins.Enable(id)
		} else if args[0] == "disable" {
			err = t.plugins.Disable(id)
		} else {
			return false, []string{"✖ 用法: /plugin <enable|disable> <id>"}, nil
		}
		if err != nil {
			return false, []string{"✖ " + err.Error()}, nil
		}
		return false, []string{"✔ 插件 " + id + " 已" + args[0]}, nil
	default:
		return false, []string{"✖ 未知命令 " + cmd + "，输入 /help 查看"}, nil
	}
}

func (t *TUI) finalizeWizard(w *wizardState) string {
	v := w.values
	id := w.id
	if w.kind == "add" {
		id = v["id"]
	}
	if strings.TrimSpace(id) == "" {
		return "✖ ID 不能为空"
	}
	url := v["url"]
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	proto := v["protocol"]
	if proto == "" {
		proto = string(llm.ProtocolOpenAIChat)
	}
	key := v["key"]
	model := v["model"]
	if w.kind == "add" {
		if err := t.session.Manager.Add(llm.Provider{ID: id, URL: url, APIKey: key, Protocol: llm.Protocol(proto), Model: model}); err != nil {
			return "✖ " + err.Error()
		}
		return "✔ 供应商 " + id + " 已保存"
	}
	p, ok := t.session.Manager.Get(w.id)
	if !ok {
		return "✖ 供应商不存在"
	}
	if key == "" {
		key = p.APIKey
	}
	if url == "" {
		url = p.URL
	}
	if proto == "" {
		proto = string(p.Protocol)
	}
	if model == "" {
		model = p.Model
	}
	if err := t.session.Manager.Update(llm.Provider{ID: p.ID, URL: url, APIKey: key, Protocol: llm.Protocol(proto), Model: model, Priority: p.Priority}); err != nil {
		return "✖ " + err.Error()
	}
	return "✔ 供应商 " + w.id + " 已更新"
}

func (t *TUI) modeLabel() string {
	mode := "auto"
	if m := t.session.Mode; m != "" {
		mode = m
	}
	if t.session.PlanMode {
		return mode + " · plan"
	}
	return mode
}

// ---------- 行模式（非 tty 回退） ----------

func (t *TUI) runLineMode() error {
	t.banner()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		t.showStatus()
		fmt.Print(t.c(t.pal.prompt, "❯ "))
		if !sc.Scan() {
			fmt.Println(t.c(t.pal.muted, "  再见 👋"))
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if quit := t.execLine(line); quit {
				fmt.Println(t.c(t.pal.muted, "  再见 👋"))
				return nil
			}
			continue
		}
		t.chatLine(line)
	}
}

func (t *TUI) execLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) > 0 && fields[0] == "/theme" {
		if len(fields) >= 2 {
			t.setThemeLine(fields[1])
		} else {
			t.themeName = toggleTheme(t.themeName)
		}
		t.okf("主题: %s", t.themeName)
		return false
	}
	quit, out, w := t.dispatch(line)
	for _, l := range out {
		t.printSystem(l)
	}
	if w != nil {
		t.runWizardLine(w)
	}
	return quit
}

func (t *TUI) runWizardLine(w *wizardState) {
	for w.idx < len(w.fields) {
		t.printSystem(t.wizardPromptLine(w))
		fmt.Print(t.c(t.pal.muted, "  > "))
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			return
		}
		w.values[w.fields[w.idx]] = strings.TrimSpace(sc.Text())
		w.idx++
	}
	t.printSystem(t.finalizeWizard(w))
}

func (t *TUI) wizardPromptLine(w *wizardState) string {
	f := w.fields[w.idx]
	total := len(w.fields)
	switch f {
	case "id":
		return fmt.Sprintf("添加供应商 — 步骤 %d/%d: ID", w.idx+1, total)
	case "url":
		return fmt.Sprintf("步骤 %d/%d: Base URL", w.idx+1, total)
	case "key":
		return fmt.Sprintf("步骤 %d/%d: API Key", w.idx+1, total)
	case "protocol":
		return fmt.Sprintf("步骤 %d/%d: Protocol（默认 openai-chat）", w.idx+1, total)
	case "model":
		return fmt.Sprintf("步骤 %d/%d: Model", w.idx+1, total)
	}
	return fmt.Sprintf("步骤 %d/%d: %s", w.idx+1, total, f)
}

func (t *TUI) chatLine(input string) {
	if names := t.session.MatchedSkills(input); len(names) > 0 {
		fmt.Println(t.c(t.pal.accent, "  ◆ 激活技能: ") + t.c(t.pal.muted, strings.Join(names, " · ")))
	}
	fmt.Println(t.c(bold+t.pal.accent, "  ⚡ Agent"))
	_, err := t.session.Ask(context.Background(), input, func(delta string) {
		fmt.Print(delta)
	})
	fmt.Println()
	if err != nil {
		t.errorf("%v", err)
		return
	}
	fmt.Println(t.line("─"))
}

func (t *TUI) setThemeLine(name string) {
	if _, ok := palettes[name]; !ok {
		t.errorf("未知主题 %s，可选: dark / light", name)
		return
	}
	t.themeName = name
	t.pal = palettes[name]
}

func (t *TUI) printSystem(s string) {
	fmt.Println(t.c(t.pal.muted, "  "+s))
}

func (t *TUI) showStatus() {
	fmt.Println(t.line("─"))
	if p, err := t.session.Manager.Active(); err == nil {
		fmt.Print(t.c(t.pal.muted, "  ● 供应商: ") + t.c(bold, p.ID) +
			t.c(dim+t.pal.muted, fmt.Sprintf(" (%s · %s)", p.Protocol, p.Model)) + "\n")
	}
	fmt.Print(t.c(t.pal.accent, "  ◆ ") + t.c(t.pal.muted, fmt.Sprintf(
		"技能 %d · 会话 %d · 模式 %s · 主题 %s",
		len(t.session.Skills), len(t.session.Messages), t.modeLabel(), t.themeName)) + "\n")
}

// ---------- 行模式辅助（ANSI） ----------

func (t *TUI) helpLines() []string {
	rows := [][2]string{
		{"/providers", "列出全部 LLM 供应商"},
		{"/add", "添加供应商（向导）"},
		{"/update <id>", "修改供应商字段"},
		{"/use <id>", "切换当前供应商"},
		{"/model <name>", "切换当前供应商的模型"},
		{"/remove <id>", "删除供应商"},
		{"/skills", "列出前端技能"},
		{"/mode <fw>", "切换框架模式: react|nextjs|vue|tailwind|svelte"},
		{"/plan", "切换到 Plan 规划模式（只规划、可建文档、不改代码）"},
		{"/execute", "切换到 Execute 执行模式（可正常修改代码）"},
		{"/mode plan|execute", "切换规划/执行模式"},
		{"/theme [d|l]", "切换深色/浅色主题"},
		{"/clear", "重置会话上下文"},
		{"/help", "显示帮助"},
		{"/quit", "退出"},
	}
	var out []string
	out = append(out, "命令:")
	for _, r := range rows {
		out = append(out, fmt.Sprintf("  %-16s %s", r[0], r[1]))
	}
	out = append(out, "快捷键:")
	out = append(out, "  Enter 发送 · Shift+Enter 换行 · Ctrl+B 侧栏 · Ctrl+T 主题 · Ctrl+C 退出")
	return out
}

func (t *TUI) skillsLines() []string {
	if len(t.session.Skills) == 0 {
		return []string{"frontend-skills/ 目录为空"}
	}
	out := []string{"前端技能:"}
	for _, s := range t.session.Skills {
		out = append(out, fmt.Sprintf("  ◆ %s — %s", s.Title, s.Description))
	}
	return out
}

// pluginsLines 列出已发现插件及其状态；未接入插件管理器时给出提示。
func (t *TUI) pluginsLines() []string {
	if t.plugins == nil {
		return []string{"未加载插件管理器"}
	}
	list := t.plugins.List()
	if len(list) == 0 {
		return []string{"未安装插件。插件放入 ~/.longcat-frontend/plugins/<id>/ 或 <项目>/.longcat-frontend/plugins/<id>/", "详见 PLUGINS.md"}
	}
	out := []string{"已安装插件:"}
	for _, p := range list {
		state := "● 启用"
		if !p.Active {
			state = "○ 禁用"
		}
		line := fmt.Sprintf("  %s %s — %s", state, p.ID, p.Description)
		out = append(out, line)
		if len(p.Errors) > 0 {
			for _, e := range p.Errors {
				out = append(out, t.c(t.pal.err, "      ✖ "+e))
			}
		}
	}
	out = append(out, "启停: /plugin enable <id> · /plugin disable <id>")
	return out
}

func (t *TUI) providersLines() []string {
	list := t.session.Manager.List()
	if len(list) == 0 {
		return []string{"暂无供应商，输入 /add 添加"}
	}
	out := []string{"LLM 供应商:"}
	active := t.session.Manager.ActiveID()
	for _, p := range list {
		mark := "  "
		if p.ID == active {
			mark = "● "
		}
		out = append(out, fmt.Sprintf("  %s%-12s %s · %s · %s · key %s", mark, p.ID, p.Protocol, p.Model, p.URL, p.Redacted()))
	}
	return out
}

func (t *TUI) banner() {
	fmt.Println()
	fmt.Println("  " + t.c(bold+t.pal.primary, "⚡ LongCat-frontend") + "  " + t.c(dim+t.pal.muted, "v0.2.0"))
	fmt.Println("  " + t.c(t.pal.muted, "React · Next.js · Vue 3 · Svelte · Tailwind"))
}

// ---------- ANSI 基础码（行模式用） ----------

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	italic = "\x1b[3m"
)

type palette struct {
	primary, accent, ok, warn, err, muted, code, user, prompt string
}

var palettes = map[string]palette{
	"dark": {
		primary: "\x1b[38;5;111m", accent: "\x1b[38;5;183m",
		ok: "\x1b[38;5;114m", warn: "\x1b[38;5;215m", err: "\x1b[38;5;210m",
		muted: "\x1b[38;5;245m", code: "\x1b[38;5;222m", user: "\x1b[38;5;117m",
		prompt: "\x1b[1;38;5;111m",
	},
	"light": {
		primary: "\x1b[38;5;25m", accent: "\x1b[38;5;91m",
		ok: "\x1b[38;5;29m", warn: "\x1b[38;5;130m", err: "\x1b[38;5;124m",
		muted: "\x1b[38;5;242m", code: "\x1b[38;5;94m", user: "\x1b[38;5;26m",
		prompt: "\x1b[1;38;5;25m",
	},
}

func (t *TUI) c(code, s string) string {
	if !t.ansi {
		return s
	}
	return code + s + reset
}

func (t *TUI) line(ch string) string {
	return t.c(dim+t.pal.muted, strings.Repeat(ch, t.width))
}

func (t *TUI) okf(format string, a ...any) {
	fmt.Print(t.c(t.pal.ok, "  ✔ ") + fmt.Sprintf(format, a...) + "\n")
}

func (t *TUI) errorf(format string, a ...any) {
	fmt.Print(t.c(t.pal.err, "  ✖ ") + t.c(t.pal.err, fmt.Sprintf(format, a...)) + "\n")
}
