// Package ui 实现现代简洁风格的 ANSI TUI。
//
// 纯 stdlib 实现：ANSI 256 色 + 盒线字符 + emoji，键盘优先，
// 在 cmd.exe / PowerShell / Windows Terminal / Unix 终端下均可运行
// （Windows 侧由 utils.EnableConsole 开启 VT 处理）。
package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/llm"
)

// ---------- 配色（现代简洁：靛蓝主色 + 中性灰阶） ----------

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	italic = "\x1b[3m"

	cPrimary = "\x1b[38;5;111m" // 靛蓝
	cAccent  = "\x1b[38;5;183m" // 淡紫
	cOK      = "\x1b[38;5;114m" // 绿
	cWarn    = "\x1b[38;5;215m" // 橙
	cErr     = "\x1b[38;5;210m" // 红
	cMuted   = "\x1b[38;5;245m" // 灰
	cCode    = "\x1b[38;5;222m" // 代码块
	cUser    = "\x1b[38;5;117m" // 用户输入
)

const width = 78

// TUI 终端交互界面。
type TUI struct {
	session *agent.Session
	in      *bufio.Scanner
	ansi    bool
}

// New 创建 TUI。ansi=false 时自动降级为纯文本。
func New(s *agent.Session, ansi bool) *TUI {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &TUI{session: s, in: sc, ansi: ansi}
}

func (t *TUI) c(code, s string) string {
	if !t.ansi {
		return s
	}
	return code + s + reset
}

func (t *TUI) line(ch string) string {
	return t.c(dim+cMuted, strings.Repeat(ch, width))
}

// Run 启动主循环。
func (t *TUI) Run() error {
	t.banner()
	for {
		fmt.Print(t.c(bold+cPrimary, "\n❯ "))
		if !t.in.Scan() {
			fmt.Println()
			return t.in.Err()
		}
		input := strings.TrimSpace(t.in.Text())
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if quit := t.command(input); quit {
				fmt.Println(t.c(cMuted, "  再见 👋"))
				return nil
			}
			continue
		}
		t.chat(input)
	}
}

func (t *TUI) banner() {
	title := "⚡ LongCat-frontend"
	sub := "React · Next.js · Vue 3 · Svelte · Tailwind"
	fmt.Println()
	fmt.Println("  " + t.c(bold+cPrimary, title) + "  " + t.c(dim+cMuted, "v0.2.0"))
	fmt.Println("  " + t.c(cMuted, sub))
	fmt.Println("  " + t.line("─")[:len(t.line("─"))])

	if p, err := t.session.Manager.Active(); err == nil {
		fmt.Printf("  %s %s %s\n",
			t.c(cOK, "●"),
			t.c(cMuted, "供应商:"),
			t.c(bold, p.ID)+t.c(dim+cMuted, fmt.Sprintf("  (%s · %s)", p.Protocol, p.Model)))
	} else {
		fmt.Printf("  %s %s\n", t.c(cWarn, "○"), t.c(cWarn, "尚未配置供应商 — 输入 /add 添加"))
	}
	fmt.Printf("  %s %s\n",
		t.c(cAccent, "◆"),
		t.c(cMuted, fmt.Sprintf("已加载 %d 个前端技能 · 输入 /help 查看命令", len(t.session.Skills))))
}

// ---------- 斜杠命令 ----------

func (t *TUI) command(input string) (quit bool) {
	fields := strings.Fields(input)
	cmd, args := fields[0], fields[1:]
	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help", "/h":
		t.help()
	case "/skills":
		t.skills()
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
	case "/clear", "/reset":
		t.session.Reset()
		t.okf("会话已重置")
	default:
		t.errorf("未知命令 %s，输入 /help 查看", cmd)
	}
	return false
}

func (t *TUI) help() {
	rows := [][2]string{
		{"/providers", "列出全部 LLM 供应商"},
		{"/add", "添加供应商（交互式向导）"},
		{"/use <id>", "切换当前供应商"},
		{"/remove <id>", "删除供应商"},
		{"/skills", "查看已加载的前端技能"},
		{"/clear", "重置会话上下文"},
		{"/quit", "退出"},
	}
	fmt.Println()
	fmt.Println("  " + t.c(bold, "命令"))
	for _, r := range rows {
		fmt.Printf("  %s%s\n", t.c(cPrimary, fmt.Sprintf("%-14s", r[0])), t.c(cMuted, r[1]))
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
		fmt.Printf("  %s %s %s\n", t.c(cAccent, "◆"), t.c(bold, s.Title), t.c(dim+cMuted, "— "+s.Description))
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
		mark := t.c(dim+cMuted, "  ")
		if p.ID == active {
			mark = t.c(cOK, "● ")
		}
		fmt.Printf("  %s%s %s\n", mark, t.c(bold, fmt.Sprintf("%-12s", p.ID)),
			t.c(cMuted, fmt.Sprintf("%s · %s · %s · key %s", p.Protocol, p.Model, p.URL, p.Redacted())))
	}
}

func (t *TUI) prompt(label, def string) string {
	if def != "" {
		fmt.Printf("  %s %s: ", t.c(cMuted, label), t.c(dim+cMuted, "["+def+"]"))
	} else {
		fmt.Printf("  %s: ", t.c(cMuted, label))
	}
	if !t.in.Scan() {
		return def
	}
	v := strings.TrimSpace(t.in.Text())
	if v == "" {
		return def
	}
	return v
}

func (t *TUI) addProvider() {
	fmt.Println()
	fmt.Println("  " + t.c(bold, "添加供应商") + t.c(dim+cMuted, "  (回车使用默认值)"))
	id := t.prompt("ID", "")
	if id == "" {
		t.errorf("ID 不能为空")
		return
	}
	protos := make([]string, 0)
	for _, p := range llm.SupportedProtocols() {
		protos = append(protos, string(p))
	}
	fmt.Println("  " + t.c(dim+cMuted, "协议可选: "+strings.Join(protos, " / ")))
	proto := t.prompt("Protocol", string(llm.ProtocolOpenAIChat))
	url := t.prompt("Base URL", "")
	key := t.prompt("API Key", "")
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

// ---------- 对话渲染 ----------

func (t *TUI) chat(input string) {
	if names := t.session.MatchedSkills(input); len(names) > 0 {
		fmt.Printf("  %s %s\n", t.c(cAccent, "◆"), t.c(dim+cMuted, "激活技能: "+strings.Join(names, " · ")))
	}
	fmt.Println()
	fmt.Println("  " + t.c(bold+cAccent, "⚡ Agent"))

	inCode := false
	lineStart := true
	render := func(delta string) {
		var out strings.Builder
		for _, r := range delta {
			if lineStart {
				out.WriteString("  ")
				lineStart = false
			}
			if r == '\n' {
				out.WriteRune(r)
				lineStart = true
				continue
			}
			out.WriteRune(r)
		}
		s := out.String()
		// 代码块着色：按 ``` 交替切换。
		for {
			idx := strings.Index(s, "```")
			if idx < 0 {
				break
			}
			if inCode {
				fmt.Print(t.c(cCode, s[:idx+3]))
			} else {
				fmt.Print(s[:idx+3])
			}
			inCode = !inCode
			s = s[idx+3:]
		}
		if inCode {
			fmt.Print(t.c(cCode, s))
		} else {
			fmt.Print(s)
		}
	}

	_, err := t.session.Ask(context.Background(), input, render)
	fmt.Println()
	if err != nil {
		t.errorf("%v", err)
		return
	}
	fmt.Println("  " + t.line("─"))
}

func (t *TUI) okf(format string, a ...any) {
	fmt.Printf("  %s %s\n", t.c(cOK, "✔"), fmt.Sprintf(format, a...))
}

func (t *TUI) errorf(format string, a ...any) {
	fmt.Printf("  %s %s\n", t.c(cErr, "✖"), t.c(cErr, fmt.Sprintf(format, a...)))
}
