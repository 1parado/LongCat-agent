// Package ui 实现现代简洁风格的 ANSI TUI。
//
// 基于 raw mode 字符级行编辑器（editor.go），实现 PRD/tui-optimization-prd.md
// 要求的全部核心交互：
//   - 多行输入（Enter 发送，Alt+Enter / Shift+Enter / Ctrl+J 换行）
//   - 历史翻页（↑↓ / Ctrl+P/N）
//   - 命令 Tab 补全
//   - 实时状态栏（供应商 / 技能数 / 会话长度 / 模式 / 主题）
//   - 技能激活高亮
//   - 彩色代码块流式渲染（跨 delta 边界正确处理 ```）
//   - 快捷键（Ctrl+K 技能 / Ctrl+L 清屏 / Ctrl+C 退出）
//   - 深/浅主题切换（/theme）与前端模式切换（/mode）
//
// 依赖：纯 stdlib + ANSI + internal/utils（raw mode）+ go-isatty（tty 检测）。
// 非 tty（管道/重定向）时自动回退到行模式，保证可脚本化。
package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/skills"
	"LongCat-frontend/internal/utils"

	"github.com/mattn/go-isatty"
)

// ---------- ANSI 基础码 ----------
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	italic = "\x1b[3m"
)

// palette 一套主题配色（ANSI 256 色）。
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

// TUI 终端交互界面。
type TUI struct {
	session   *agent.Session
	ansi      bool
	width     int
	themeName string
	pal       palette
	editor    *Editor
	market    *skills.Market
}

// New 创建 TUI。
func New(s *agent.Session, ansi bool) *TUI {
	w, _ := utils.Size()
	if w < 40 {
		w = 80
	}
	name := "dark"
	return &TUI{
		session: s, ansi: ansi, width: w,
		themeName: name, pal: palettes[name],
	}
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

func (t *TUI) setTheme(name string) {
	p, ok := palettes[name]
	if !ok {
		t.errorf("未知主题 %s，可选: dark / light", name)
		return
	}
	t.themeName = name
	t.pal = p
	if t.editor != nil {
		t.editor.PromptStyle = t.pal.prompt
	}
}

// ==================== 主循环 ====================

// Run 启动交互循环。优先进入 raw mode；非 tty 时回退到行模式。
func (t *TUI) Run() error {
	t.banner()

	if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return t.runLineMode()
	}

	restore, rawErr := utils.EnableRaw()
	if rawErr != nil || restore == nil {
		reason := ""
		if rawErr != nil {
			reason = "（" + rawErr.Error() + "）"
		}
		fmt.Println(t.c(t.pal.warn, "  ⚠ raw mode 不可用"+reason+"，回退行模式（多行/历史/补全不可用）"))
		return t.runLineMode()
	}
	defer restore()

	in := newStdinReader()
	ed := NewEditor(in, t.width)
	ed.PromptStyle = t.pal.prompt
	ed.OnAppKey = t.handleAppKey
	ed.SetComplete(t.autocomplete)
	t.editor = ed

	for {
		t.showStatus()
		line, err := ed.ReadLine()
		if err == io.EOF || err == errInterrupt {
			fmt.Println(t.c(t.pal.muted, "  再见 👋"))
			return nil
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		ed.AddHistory(line)

		if strings.HasPrefix(strings.TrimSpace(line), "/") {
			if quit := t.command(line); quit {
				fmt.Println(t.c(t.pal.muted, "  再见 👋"))
				return nil
			}
			continue
		}
		t.chat(line)
	}
}

// runLineMode 非 tty（管道/重定向）回退：行缓冲读取，保留基础功能。
func (t *TUI) runLineMode() error {
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
			if quit := t.command(line); quit {
				fmt.Println(t.c(t.pal.muted, "  再见 👋"))
				return nil
			}
			continue
		}
		t.chat(line)
	}
}

// ==================== 横幅与状态栏 ====================

func (t *TUI) banner() {
	fmt.Println()
	fmt.Println("  " + t.c(bold+t.pal.primary, "⚡ LongCat-frontend") + "  " + t.c(dim+t.pal.muted, "v0.2.0"))
	fmt.Println("  " + t.c(t.pal.muted, "React · Next.js · Vue 3 · Svelte · Tailwind"))
}

// showStatus 实时状态栏：每轮输入前刷新。
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

func (t *TUI) modeLabel() string {
	if m := t.session.Mode; m != "" {
		return m
	}
	return "auto"
}

// ==================== 命令系统 ====================

func (t *TUI) command(input string) (quit bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	cmd, args := fields[0], fields[1:]
	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help", "/h":
		t.help()
	case "/skills":
		t.cmdSkills(args)
	case "/providers", "/p":
		t.providers()
	case "/use":
		if len(args) != 1 {
			t.errorf("用法: /use <id>")
			break
		}
		if err := t.session.Manager.SetActive(args[0]); err != nil {
			t.errorf("%v", err)
		} else {
			t.okf("已切换到供应商 %s", args[0])
		}
	case "/add":
		t.addProvider()
	case "/remove", "/rm":
		if len(args) != 1 {
			t.errorf("用法: /remove <id>")
			break
		}
		if err := t.session.Manager.Remove(args[0]); err != nil {
			t.errorf("%v", err)
		} else {
			t.okf("已删除供应商 %s", args[0])
		}
	case "/update", "/edit":
		t.updateProvider(args)
	case "/clear", "/reset":
		t.session.Reset()
		t.okf("会话已重置")
	case "/mode":
		t.cmdMode(args)
	case "/model":
		t.cmdModel(args)
	case "/theme":
		t.cmdTheme(args)
	default:
		t.errorf("未知命令 %s，输入 /help 查看", cmd)
	}
	return false
}

func (t *TUI) cmdMode(args []string) {
	if len(args) == 0 {
		t.okf("当前模式: %s（可选: react|nextjs|vue|tailwind|svelte）", t.modeLabel())
		return
	}
	if err := t.session.SetMode(args[0]); err != nil {
		t.errorf("%v", err)
		return
	}
	t.okf("模式已切换: %s", args[0])
}

func (t *TUI) cmdTheme(args []string) {
	if len(args) == 0 {
		next := "light"
		if t.themeName == "light" {
			next = "dark"
		}
		t.setTheme(next)
		t.okf("主题: %s", t.themeName)
		return
	}
	t.setTheme(args[0])
}

func (t *TUI) cmdModel(args []string) {
	if len(args) == 0 {
		p, err := t.session.Manager.Active()
		if err != nil {
			t.errorf("%v", err)
			return
		}
		t.okf("当前模型: %s（供应商 %s）", p.Model, p.ID)
		return
	}
	id := t.session.Manager.ActiveID()
	if id == "" {
		t.errorf("无激活供应商，先 /use 切换或 /add 添加")
		return
	}
	if err := t.session.Manager.SetModel(id, args[0]); err != nil {
		t.errorf("%v", err)
		return
	}
	t.okf("模型已切换: %s", args[0])
}

// autocomplete 提供命令前缀补全候选。
func (t *TUI) autocomplete(prefix string) []string {
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	cmds := []string{
		"/help", "/skills", "/providers", "/use", "/add", "/update", "/remove",
		"/model", "/mode", "/theme", "/clear", "/quit",
		"/h", "/p", "/rm", "/q", "/exit", "/reset",
	}
	var match []string
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			match = append(match, c)
		}
	}
	return match
}

// ==================== 应用级快捷键（Ctrl+K / Ctrl+L） ====================

// handleAppKey 在行编辑期间处理应用级 Ctrl 快捷键。
// 约定：返回前确保光标位于新行行首，以便编辑器重画 buffer。
func (t *TUI) handleAppKey(ch rune) bool {
	switch ch {
	case 'k': // 技能面板
		fmt.Println()
		t.skills()
		fmt.Println()
		return true
	case 'l': // 清屏
		fmt.Print("\x1b[2J\x1b[H")
		t.banner()
		fmt.Println()
		return true
	}
	return false
}

// ==================== 帮助 / 技能 / 供应商 ====================

func (t *TUI) help() {
	fmt.Println()
	fmt.Println("  " + t.c(bold, "命令"))
	cmdRows := [][2]string{
		{"/providers", "列出全部 LLM 供应商"},
		{"/add", "添加供应商（交互式向导）"},
		{"/update <id>", "修改供应商字段"},
		{"/use <id>", "切换当前供应商"},
		{"/model <name>", "切换当前供应商的模型"},
		{"/remove <id>", "删除供应商"},
		{"/skills", "技能管理（列表/安装/激活），/skills help 详见"},
		{"/mode <fw>", "切换模式: react|nextjs|vue|tailwind|svelte"},
		{"/theme [d|l]", "切换深色/浅色主题"},
		{"/clear", "重置会话上下文"},
		{"/help", "显示此帮助"},
		{"/quit", "退出"},
	}
	for _, r := range cmdRows {
		fmt.Printf("  %s%s\n", t.c(t.pal.primary, fmt.Sprintf("%-16s", r[0])), t.c(t.pal.muted, r[1]))
	}
	fmt.Println()
	fmt.Println("  " + t.c(bold, "快捷键"))
	keyRows := [][2]string{
		{"Enter", "发送消息"},
		{"Alt+Enter / Shift+Enter", "输入换行"},
		{"↑ / ↓ 或 Ctrl+P/N", "浏览历史记录"},
		{"Tab", "命令补全"},
		{"Ctrl+A / Ctrl+E", "光标到行首 / 行尾"},
		{"Ctrl+W / Ctrl+U", "删除一词 / 删到行首"},
		{"Ctrl+K", "打开技能面板"},
		{"Ctrl+L", "清屏"},
		{"Ctrl+C", "退出（空行时）"},
	}
	for _, r := range keyRows {
		fmt.Printf("  %s%s\n", t.c(t.pal.primary, fmt.Sprintf("%-26s", r[0])), t.c(t.pal.muted, r[1]))
	}
}

func (t *TUI) skills() {
	fmt.Println()
	if len(t.session.Skills) == 0 {
		t.errorf("frontend-skills/ 目录为空")
		return
	}
	fmt.Println("  " + t.c(bold, "前端技能"))
	for _, s := range t.session.Skills {
		fmt.Printf("  %s %s %s\n", t.c(t.pal.accent, "◆"), t.c(bold, s.Title), t.c(dim+t.pal.muted, "— "+s.Description))
	}
}

func (t *TUI) providers() {
	list := t.session.Manager.List()
	fmt.Println()
	if len(list) == 0 {
		t.errorf("暂无供应商，输入 /add 添加")
		return
	}
	fmt.Println("  " + t.c(bold, "LLM 供应商"))
	active := t.session.Manager.ActiveID()
	for _, p := range list {
		mark := t.c(dim+t.pal.muted, "  ")
		if p.ID == active {
			mark = t.c(t.pal.ok, "● ")
		}
		fmt.Printf("  %s%s %s\n", mark, t.c(bold, fmt.Sprintf("%-12s", p.ID)),
			t.c(t.pal.muted, fmt.Sprintf("%s · %s · %s · key %s", p.Protocol, p.Model, p.URL, p.Redacted())))
	}
}

// prompt 交互式读取一个字段（raw mode 下复用 editor，行模式下用 Scanner）。
func (t *TUI) prompt(label, def string) string {
	if t.editor == nil {
		return t.promptLine(label, def)
	}
	savedPrompt, savedStyle := t.editor.Prompt, t.editor.PromptStyle
	if def != "" {
		t.editor.Prompt = fmt.Sprintf("  %s [%s]: ", label, def)
	} else {
		t.editor.Prompt = fmt.Sprintf("  %s: ", label)
	}
	t.editor.PromptStyle = t.pal.muted
	line, err := t.editor.ReadLine()
	t.editor.Prompt, t.editor.PromptStyle = savedPrompt, savedStyle
	if err != nil {
		return def
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return def
	}
	return v
}

func (t *TUI) promptLine(label, def string) string {
	if def != "" {
		fmt.Printf("  %s %s: ", t.c(t.pal.muted, label), t.c(dim+t.pal.muted, "["+def+"]"))
	} else {
		fmt.Printf("  %s: ", t.c(t.pal.muted, label))
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return def
	}
	v := strings.TrimSpace(sc.Text())
	if v == "" {
		return def
	}
	return v
}

func (t *TUI) addProvider() {
	fmt.Println()
	fmt.Println("  " + t.c(bold, "添加供应商") + t.c(dim+t.pal.muted, "  (回车使用默认值)"))
	id := t.prompt("ID", "")
	if id == "" {
		t.errorf("ID 不能为空")
		return
	}
	url := t.prompt("Base URL", "")
	if url == "" {
		t.errorf("Base URL 不能为空")
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	key := t.prompt("API Key", "")
	var protos []string
	for _, p := range llm.SupportedProtocols() {
		protos = append(protos, string(p))
	}
	fmt.Println("  " + t.c(dim+t.pal.muted, "协议可选: "+strings.Join(protos, " / ")))
	proto := t.prompt("Protocol", string(llm.ProtocolOpenAIChat))
	model := t.prompt("Model", "")

	err := t.session.Manager.Add(llm.Provider{
		ID: id, URL: url, APIKey: key, Protocol: llm.Protocol(proto), Model: model,
	})
	if err != nil {
		t.errorf("%v", err)
		return
	}
	t.okf("供应商 %s 已保存 ✨", id)
}

func (t *TUI) updateProvider(args []string) {
	if len(args) != 1 {
		t.errorf("用法: /update <id>")
		return
	}
	p, ok := t.session.Manager.Get(args[0])
	if !ok {
		t.errorf("供应商 %s 不存在", args[0])
		return
	}
	fmt.Println()
	fmt.Println("  " + t.c(bold, "修改供应商 "+args[0]) + t.c(dim+t.pal.muted, "  (回车保留原值)"))
	var protos []string
	for _, pr := range llm.SupportedProtocols() {
		protos = append(protos, string(pr))
	}
	fmt.Println("  " + t.c(dim+t.pal.muted, "协议可选: "+strings.Join(protos, " / ")))
	url := t.prompt("Base URL", p.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	key := t.prompt("API Key（留空保留）", "")
	proto := t.prompt("Protocol", string(p.Protocol))
	model := t.prompt("Model", p.Model)
	if key == "" {
		key = p.APIKey
	}
	err := t.session.Manager.Update(llm.Provider{
		ID: p.ID, URL: url, APIKey: key, Protocol: llm.Protocol(proto), Model: model, Priority: p.Priority,
	})
	if err != nil {
		t.errorf("%v", err)
		return
	}
	t.okf("供应商 %s 已更新", args[0])
}

// ==================== 对话与流式渲染 ====================

// codeState 跨 delta 维护代码块边界状态。
type codeState struct {
	inCode bool
	pend   string // 尾部未决内容（用于 ``` 跨 delta 边界检测）
}

func (t *TUI) chat(input string) {
	if names := t.session.MatchedSkills(input); len(names) > 0 {
		fmt.Print(t.c(t.pal.accent, "  ◆ 激活技能: ") + t.c(t.pal.muted, strings.Join(names, " · ")) + "\n")
	}
	fmt.Println(t.c(bold+t.pal.accent, "  ⚡ Agent"))
	fmt.Print("  ") // 普通文本行首缩进

	st := &codeState{}
	_, err := t.session.Ask(context.Background(), input, func(delta string) {
		t.renderDelta(st, delta)
	})
	fmt.Print(reset)
	fmt.Println()
	if err != nil {
		t.errorf("%v", err)
		fmt.Println(t.c(t.pal.muted, "  提示: 可重新输入消息重试"))
		return
	}
	fmt.Println(t.line("─"))
}

// renderDelta 流式渲染增量文本：正确处理跨 delta 边界的 ``` 代码围栏，
// 代码块内容统一着色，普通文本每行首缩进两个空格。
func (t *TUI) renderDelta(st *codeState, delta string) {
	s := st.pend + delta
	for {
		idx := strings.Index(s, "```")
		if idx < 0 {
			// 保留尾部 2 字符，防止 ``` 被切断在 delta 边界
			keep := 2
			if len(s) <= keep {
				st.pend = s
				return
			}
			t.printChunk(st.inCode, s[:len(s)-keep])
			st.pend = s[len(s)-keep:]
			return
		}
		t.printChunk(st.inCode, s[:idx])
		fmt.Print(t.c(t.pal.muted, "```"))
		st.inCode = !st.inCode
		s = s[idx+3:]
	}
}

func (t *TUI) printChunk(inCode bool, s string) {
	if s == "" {
		return
	}
	if inCode {
		fmt.Print(t.c(t.pal.code, s))
	} else {
		// 普通文本：换行后补行首缩进，保持与 Agent 标题对齐
		fmt.Print(strings.ReplaceAll(s, "\n", "\n  "))
	}
}

// ==================== 反馈 ====================

func (t *TUI) okf(format string, a ...any) {
	fmt.Print(t.c(t.pal.ok, "  ✔ ") + fmt.Sprintf(format, a...) + "\n")
}

func (t *TUI) errorf(format string, a ...any) {
	fmt.Print(t.c(t.pal.err, "  ✖ ") + t.c(t.pal.err, fmt.Sprintf(format, a...)) + "\n")
}
