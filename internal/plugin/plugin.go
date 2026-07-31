// Package plugin 提供轻量、可移植的插件 / 扩展机制。
//
// 设计目标（见 ISSUES.md #5）：
//   - 不依赖 cgo / 动态链接（Go plugin 在 Windows 不可用，且 Tauri 打包为单一二进制）。
//   - 用声明式 manifest（plugin.json）统一挂载现有扩展点：技能、子 Agent、MCP 服务。
//   - 具备完整的发现（discover）→ 注册（register）→ 启用/禁用（enable/disable）
//     → 热重载（reload）生命周期。
//
// 插件目录布局（用户级与项目级各一个）：
//
//	<root>/<plugin-id>/
//	  plugin.json        # 必填：清单
//	  skills/<name>/SKILL.md   # 可选：前端技能
//	  agents/<name>.md         # 可选：子 Agent 定义
//
// plugin.json 字段：
//
//	{
//	  "id": "hello",
//	  "name": "Hello Plugin",
//	  "version": "1.0.0",
//	  "description": "示例插件",
//	  "author": "you",
//	  "disabled": false,        // 可选，默认启用
//	  "skills": ["skills"],     // 可选：相对插件根目录，含 SKILL.md 子目录
//	  "agents": ["agents"],     // 可选：相对插件根目录，含 *.md 定义
//	  "mcp": [                  // 可选：内联 MCP 服务（仅内存，不写项目配置）
//	    {"id": "x", "name": "X", "url": "http://...", "protocol": "http"}
//	  ]
//	}
package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"LongCat-frontend/internal/agent"
	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/mcp"
)

// Manifest 对应 plugin.json。
type Manifest struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Author      string          `json:"author"`
	Disabled    bool            `json:"disabled"` // 默认 false（启用）
	Skills      []string        `json:"skills"`   // 相对插件根目录，含 SKILL.md 子目录
	Agents      []string        `json:"agents"`   // 相对插件根目录，含 *.md Agent 定义
	MCP         []mcp.MCPServer `json:"mcp"`      // 内联 MCP 服务配置
}

// Plugin 是已发现的插件实例。
type Plugin struct {
	Manifest
	Root   string   `json:"root"`   // 插件绝对目录
	Active bool     `json:"active"` // 实际是否生效（未禁用）
	Errors []string `json:"errors,omitempty"`
}

// Manager 负责发现与管理插件。
type Manager struct {
	userDir  string
	projDir  string
	plugins  []*Plugin
	disabled map[string]bool
}

const disabledFile = "disabled.json"

// NewManager 创建管理器并载入禁用列表（位于用户级插件目录）。
func NewManager(userDir, projDir string) *Manager {
	m := &Manager{userDir: userDir, projDir: projDir, disabled: map[string]bool{}}
	m.loadDisabled()
	return m
}

func (m *Manager) loadDisabled() {
	if m.userDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(m.userDir, disabledFile))
	if err != nil {
		return
	}
	var doc struct{ Disabled []string `json:"disabled"` }
	if json.Unmarshal(b, &doc) == nil {
		for _, id := range doc.Disabled {
			m.disabled[id] = true
		}
	}
}

func (m *Manager) saveDisabled() {
	if m.userDir == "" {
		return
	}
	ids := make([]string, 0, len(m.disabled))
	for id := range m.disabled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data, _ := json.MarshalIndent(struct {
		Disabled []string `json:"disabled"`
	}{ids}, "", "  ")
	_ = os.MkdirAll(m.userDir, 0o755)
	_ = os.WriteFile(filepath.Join(m.userDir, disabledFile), data, 0o600)
}

// Discover 扫描用户级与项目级插件目录，载入所有 plugin.json。
func (m *Manager) Discover() error {
	m.plugins = nil
	dirs := make([]string, 0, 2)
	if m.userDir != "" {
		dirs = append(dirs, m.userDir)
	}
	if m.projDir != "" {
		dirs = append(dirs, m.projDir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if p := m.loadPlugin(filepath.Join(dir, e.Name())); p != nil {
				m.plugins = append(m.plugins, p)
			}
		}
	}
	sort.Slice(m.plugins, func(i, j int) bool { return m.plugins[i].ID < m.plugins[j].ID })
	return nil
}

func (m *Manager) loadPlugin(root string) *Plugin {
	path := filepath.Join(root, "plugin.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var man Manifest
	if err := json.Unmarshal(b, &man); err != nil {
		return &Plugin{Manifest: Manifest{ID: filepath.Base(root)}, Root: root, Errors: []string{"解析 plugin.json 失败: " + err.Error()}}
	}
	if man.ID == "" {
		man.ID = filepath.Base(root)
	}
	if man.Name == "" {
		man.Name = man.ID
	}
	p := &Plugin{Manifest: man, Root: root}
	p.Active = !man.Disabled && !m.disabled[man.ID]
	if man.ID == "" {
		p.Errors = append(p.Errors, "插件缺少 id")
	}
	return p
}

// List 返回已发现的插件（含状态）。
func (m *Manager) List() []*Plugin { return m.plugins }

// Enable 启用插件（从禁用列表移除并持久化）。
func (m *Manager) Enable(id string) error {
	delete(m.disabled, id)
	m.saveDisabled()
	m.recompute()
	return nil
}

// Disable 禁用插件（加入禁用列表并持久化）。
func (m *Manager) Disable(id string) error {
	m.disabled[id] = true
	m.saveDisabled()
	m.recompute()
	return nil
}

func (m *Manager) recompute() {
	for _, p := range m.plugins {
		p.Active = !p.Manifest.Disabled && !m.disabled[p.Manifest.ID]
	}
}

// Reload 重新发现并刷新插件状态。
func (m *Manager) Reload() error {
	if err := m.Discover(); err != nil {
		return err
	}
	for _, p := range m.plugins {
		p.Active = !p.Manifest.Disabled && !m.disabled[p.Manifest.ID]
	}
	return nil
}

// LoadInto 将生效插件贡献的能力注册进会话：技能 / 子 Agent / MCP 服务。
func (m *Manager) LoadInto(s *agent.Session) error {
	if s == nil {
		return nil
	}
	for _, p := range m.plugins {
		if !p.Active {
			continue
		}
		for _, rel := range p.Manifest.Skills {
			sk, err := frontend.LoadSkills(filepath.Join(p.Root, rel))
			if err != nil {
				p.Errors = append(p.Errors, fmt.Sprintf("技能目录 %q: %v", rel, err))
				continue
			}
			s.Skills = append(s.Skills, sk...)
		}
		for _, rel := range p.Manifest.Agents {
			defs, err := agent.DiscoverAgents("", "", filepath.Join(p.Root, rel))
			if err != nil {
				p.Errors = append(p.Errors, fmt.Sprintf("agent 目录 %q: %v", rel, err))
				continue
			}
			s.Agents = append(s.Agents, defs...)
		}
		if s.MCP != nil {
			for _, srv := range p.Manifest.MCP {
				if err := s.MCP.AddEphemeral(srv); err != nil {
					p.Errors = append(p.Errors, fmt.Sprintf("MCP %q: %v", srv.ID, err))
				}
			}
		}
	}
	return nil
}

// LoadInto 便捷函数：构造管理器、发现插件并注册能力进会话。
func LoadInto(s *agent.Session, userDir, projDir string) error {
	if userDir == "" && projDir == "" {
		return nil
	}
	m := NewManager(userDir, projDir)
	if err := m.Discover(); err != nil {
		return err
	}
	return m.LoadInto(s)
}
