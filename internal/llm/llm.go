// Package llm 提供多协议 LLM 供应商管理（CRUD + 持久化）与统一的对话接口。
//
// 设计目标（见 DESIGN.md）：
//   - 自定义供应商模式：URL + API Key + Protocol 即可接入。
//   - 支持多供应商增删改查，JSON 持久化到用户目录。
//   - Streaming 响应优先。
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Protocol 定义供应商使用的通信协议。
type Protocol string

const (
	// ProtocolOpenAIChat OpenAI Chat Completions API (POST {url}/chat/completions)
	ProtocolOpenAIChat Protocol = "openai_chat"
	// ProtocolOpenAIResponses OpenAI Responses API (POST {url}/responses)
	ProtocolOpenAIResponses Protocol = "openai_responses"
	// ProtocolAnthropic Anthropic Messages API (POST {url}/v1/messages)
	ProtocolAnthropic Protocol = "anthropic_messages"
	// ProtocolOllama Ollama 本地推理 (POST {url}/api/chat)
	ProtocolOllama Protocol = "ollama_chat"
)

// SupportedProtocols 列出全部受支持的协议。
func SupportedProtocols() []Protocol {
	return []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropic, ProtocolOllama}
}

// ValidProtocol 校验协议名是否受支持。
func ValidProtocol(p string) bool {
	for _, sp := range SupportedProtocols() {
		if string(sp) == p {
			return true
		}
	}
	return false
}

// Provider 描述一个 LLM 供应商配置。
type Provider struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	URL      string   `json:"url"`
	APIKey   string   `json:"api_key"`
	Protocol Protocol `json:"protocol"`
	Model    string   `json:"model"`
	// Priority 越小优先级越高，用于自动路由。
	Priority int `json:"priority,omitempty"`
}

// Redacted 返回打码后的 APIKey，用于展示。
func (p Provider) Redacted() string {
	if len(p.APIKey) <= 8 {
		return "****"
	}
	return p.APIKey[:4] + "..." + p.APIKey[len(p.APIKey)-4:]
}

// Attachment is a user-supplied file that can be carried through to a
// multimodal provider. Data is normally a data URL; Text is used for text-like
// files so providers that do not support arbitrary files can still read them.
type Attachment struct {
	Name     string `json:"name"`
	MIMEType string `json:"mime_type"`
	Data     string `json:"data,omitempty"`
	Text     string `json:"text,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// Message 单条对话消息。Role: system / user / assistant。
type Message struct {
	Role        string       `json:"role"`
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// ToolCalls is populated on assistant messages that request tools.
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// FunctionDefinition describes a callable function in OpenAI/Anthropic tool format.
type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type Tool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// store 是持久化文件的结构。
type store struct {
	Active    string     `json:"active"`
	Providers []Provider `json:"providers"`
}

// Manager 管理供应商列表，线程安全，自动持久化。
type Manager struct {
	mu        sync.RWMutex
	providers map[string]Provider
	active    string
	path      string
}

// ConfigDir 返回配置目录（~/.longcat-frontend），不存在则创建。
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".longcat-frontend")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// NewManager 创建 Manager 并从磁盘加载已有配置。
func NewManager() (*Manager, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		providers: make(map[string]Provider),
		path:      filepath.Join(dir, "providers.json"),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", m.path, err)
	}
	for _, p := range s.Providers {
		m.providers[p.ID] = p
	}
	m.active = s.Active
	return nil
}

func (m *Manager) save() error {
	s := store{Active: m.active, Providers: m.snapshot()}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0o600)
}

func (m *Manager) snapshot() []Provider {
	list := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Priority != list[j].Priority {
			return list[i].Priority < list[j].Priority
		}
		return list[i].ID < list[j].ID
	})
	return list
}

// Add 新增供应商；ID 已存在时返回错误。
func (m *Manager) Add(p Provider) error {
	if p.ID == "" || p.URL == "" {
		return errors.New("id 与 url 不能为空")
	}
	if !ValidProtocol(string(p.Protocol)) {
		return fmt.Errorf("不支持的协议 %q，可选: %v", p.Protocol, SupportedProtocols())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[p.ID]; ok {
		return fmt.Errorf("供应商 %q 已存在，请使用 update", p.ID)
	}
	m.providers[p.ID] = p
	if m.active == "" {
		m.active = p.ID
	}
	return m.save()
}

// Update 更新已有供应商（整体覆盖）。
func (m *Manager) Update(p Provider) error {
	if !ValidProtocol(string(p.Protocol)) {
		return fmt.Errorf("不支持的协议 %q，可选: %v", p.Protocol, SupportedProtocols())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[p.ID]; !ok {
		return fmt.Errorf("供应商 %q 不存在", p.ID)
	}
	m.providers[p.ID] = p
	return m.save()
}

// Remove 删除供应商。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[id]; !ok {
		return fmt.Errorf("供应商 %q 不存在", id)
	}
	delete(m.providers, id)
	if m.active == id {
		m.active = ""
		for _, p := range m.snapshot() {
			m.active = p.ID
			break
		}
	}
	return m.save()
}

// Get 按 ID 查询供应商。
func (m *Manager) Get(id string) (Provider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.providers[id]
	return p, ok
}

// List 返回按优先级排序的供应商列表。
func (m *Manager) List() []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot()
}

// SetActive 手动切换当前供应商。
func (m *Manager) SetActive(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[id]; !ok {
		return fmt.Errorf("供应商 %q 不存在", id)
	}
	m.active = id
	return m.save()
}

// SetModel 更新指定供应商的模型名并持久化。
func (m *Manager) SetModel(id, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.providers[id]
	if !ok {
		return fmt.Errorf("供应商 %q 不存在", id)
	}
	p.Model = model
	m.providers[id] = p
	return m.save()
}

// Active 返回当前供应商；未设置时按优先级自动路由到第一个。
func (m *Manager) Active() (Provider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.providers[m.active]; ok {
		return p, nil
	}
	list := m.snapshot()
	if len(list) == 0 {
		return Provider{}, errors.New("尚未配置任何供应商，请先执行: LongCat-frontend provider add")
	}
	return list[0], nil
}

// ActiveID 返回当前激活的供应商 ID（可能为空）。
func (m *Manager) ActiveID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}
