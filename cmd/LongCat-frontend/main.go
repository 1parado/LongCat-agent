// LongCat-frontend 入口。
//
// 子命令:
//
//	tui               启动终端 TUI（默认）
//	serve             启动桌面后端（HTTP API + Web UI，供 Tauri/浏览器使用）
//	provider ...      供应商 CRUD 管理
//	version           打印版本
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/server"
	"LongCat-frontend/internal/ui"
	"LongCat-frontend/internal/utils"
)

const version = "0.2.0"

func main() {
	ansi := utils.EnableConsole()

	args := os.Args[1:]
	cmd := "tui"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	manager, err := llm.NewManager()
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "tui":
		session, err := agent.NewSession(manager, skillsDir())
		if err != nil {
			fatal(err)
		}
		if err := ui.New(session, ansi).Run(); err != nil {
			fatal(err)
		}
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:5510", "监听地址")
		_ = fs.Parse(args)
		session, err := agent.NewSession(manager, skillsDir())
		if err != nil {
			fatal(err)
		}
		fmt.Printf("⚡ LongCat 桌面后端已启动: http://%s\n", *addr)
		if err := server.Run(*addr, manager, session); err != nil {
			fatal(err)
		}
	case "provider":
		providerCmd(manager, args)
	case "version", "-v", "--version":
		fmt.Println("LongCat v" + version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// skillsDir 优先使用可执行文件旁的 frontend-skills/，其次当前目录。
func skillsDir() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Join(filepath.Dir(exe), "..", "frontend-skills")
		if _, err := os.Stat(d); err == nil {
			return d
		}
		d = filepath.Join(filepath.Dir(exe), "frontend-skills")
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return "frontend-skills"
}

func providerCmd(m *llm.Manager, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: LongCat-frontend provider <add|list|update|remove|use>")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		list := m.List()
		if len(list) == 0 {
			fmt.Println("暂无供应商。添加示例:")
			fmt.Println(`  LongCat-frontend provider add -id my -url https://api.openai.com/v1 -key sk-xxx -protocol openai_chat -model gpt-4o-mini`)
			return
		}
		active := m.ActiveID()
		for _, p := range list {
			mark := "  "
			if p.ID == active {
				mark = "* "
			}
			fmt.Printf("%s%-12s %-18s %-24s %s (key %s)\n", mark, p.ID, p.Protocol, p.Model, p.URL, p.Redacted())
		}
	case "add", "update":
		fs := flag.NewFlagSet("provider "+sub, flag.ExitOnError)
		id := fs.String("id", "", "供应商 ID（必填）")
		url := fs.String("url", "", "Base URL（必填）")
		key := fs.String("key", "", "API Key")
		protocol := fs.String("protocol", string(llm.ProtocolOpenAIChat),
			fmt.Sprintf("协议 %v", llm.SupportedProtocols()))
		model := fs.String("model", "", "模型名")
		priority := fs.Int("priority", 0, "优先级（越小越优先）")
		_ = fs.Parse(rest)
		p := llm.Provider{
			ID: *id, URL: *url, APIKey: *key,
			Protocol: llm.Protocol(*protocol), Model: *model, Priority: *priority,
		}
		var err error
		if sub == "add" {
			err = m.Add(p)
		} else {
			err = m.Update(p)
		}
		if err != nil {
			fatal(err)
		}
		fmt.Printf("✔ 供应商 %s 已保存\n", *id)
	case "remove", "rm":
		if len(rest) != 1 {
			fatal(fmt.Errorf("用法: LongCat-frontend provider remove <id>"))
		}
		if err := m.Remove(rest[0]); err != nil {
			fatal(err)
		}
		fmt.Printf("✔ 供应商 %s 已删除\n", rest[0])
	case "use":
		if len(rest) != 1 {
			fatal(fmt.Errorf("用法: LongCat-frontend provider use <id>"))
		}
		if err := m.SetActive(rest[0]); err != nil {
			fatal(err)
		}
		fmt.Printf("✔ 已切换到供应商 %s\n", rest[0])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: provider %s\n", sub)
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`LongCat v` + version + ` — 智能通用 AI 助手

用法:
  LongCat [tui]                 启动终端 TUI（默认）
  LongCat serve [-addr host:port]  启动桌面后端 (默认 127.0.0.1:5510)
  LongCat provider list         列出供应商
  LongCat provider add -id <id> -url <url> -key <key> -protocol <p> -model <m>
  LongCat provider update ...   更新供应商（参数同 add）
  LongCat provider remove <id>  删除供应商
  LongCat provider use <id>     切换当前供应商
  LongCat version               打印版本

协议: openai_chat | openai_responses | anthropic_messages | ollama_chat
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "✖ "+err.Error())
	os.Exit(1)
}
