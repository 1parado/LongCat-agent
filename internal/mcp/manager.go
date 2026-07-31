package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"LongCat-frontend/internal/llm"
)

type Manager struct {
	mu        sync.RWMutex
	workspace string
	servers   map[string]MCPServer
	client    *http.Client
}

func NewManager(workspace string) *Manager {
	return &Manager{workspace: workspace, servers: make(map[string]MCPServer), client: &http.Client{Timeout: 8 * time.Second}}
}

func (m *Manager) Workspace() string { m.mu.RLock(); defer m.mu.RUnlock(); return m.workspace }

// Load merges user-level configuration with project configuration, where the
// project entry wins on duplicate IDs. Secrets are read only into memory.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	merged := make(map[string]MCPServer)
	if dir, err := llm.ConfigDir(); err == nil {
		if loadErr := loadFile(filepath.Join(dir, "mcp.json"), merged); loadErr != nil && !os.IsNotExist(loadErr) {
			return loadErr
		}
	}
	if m.workspace != "" {
		if loadErr := loadFile(filepath.Join(m.workspace, ".longcat-frontend", "mcp.json"), merged); loadErr != nil && !os.IsNotExist(loadErr) {
			return loadErr
		}
	}
	m.servers = merged
	return nil
}

func loadFile(path string, into map[string]MCPServer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg configFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("解析 MCP 配置 %s 失败: %w", path, err)
	}
	for _, server := range cfg.Servers {
		if strings.TrimSpace(server.ID) == "" {
			continue
		}
		if server.Protocol == "" {
			server.Protocol = "http"
		}
		if server.Tone == "" {
			server.Tone = HealthUnknown
		}
		into[server.ID] = server
	}
	return nil
}

func (m *Manager) SetWorkspace(workspace string) error {
	m.mu.Lock()
	m.workspace = workspace
	m.mu.Unlock()
	return m.Load()
}

func (m *Manager) List() []MCPServer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MCPServer, 0, len(m.servers))
	for _, server := range m.servers {
		server.Headers = nil
		out = append(out, server)
	}
	return out
}

func (m *Manager) Get(id string) (MCPServer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	server, ok := m.servers[id]
	return server, ok
}

// UpsertProjectServer saves a server in the active project's mcp.json.
func (m *Manager) UpsertProjectServer(server MCPServer) error {
	if strings.TrimSpace(server.ID) == "" || strings.TrimSpace(server.URL) == "" {
		return errors.New("MCP 服务需要 id 和 url")
	}
	if sanitize(server.ID) != server.ID || strings.Contains(server.ID, "__") {
		return errors.New("MCP 服务 id 只能包含字母、数字、下划线和短横线")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workspace == "" {
		return errors.New("尚未打开工作空间")
	}
	if server.Protocol == "" {
		server.Protocol = "http"
	}
	m.servers[server.ID] = server
	return m.saveProjectLocked()
}

func (m *Manager) RemoveProjectServer(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workspace == "" {
		return errors.New("尚未打开工作空间")
	}
	if _, ok := m.servers[id]; !ok {
		return errors.New("MCP 服务不存在")
	}
	delete(m.servers, id)
	return m.saveProjectLocked()
}

func (m *Manager) saveProjectLocked() error {
	path := filepath.Join(m.workspace, ".longcat-frontend", "mcp.json")
	servers := make([]MCPServer, 0, len(m.servers))
	for _, server := range m.servers {
		servers = append(servers, MCPServer{ID: server.ID, Name: server.Name, URL: server.URL, Protocol: server.Protocol, Headers: server.Headers})
	}
	data, err := json.MarshalIndent(configFile{Servers: servers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Definitions converts discovered MCP tools into namespaced native tools.
func (m *Manager) Definitions() []llm.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []llm.Tool
	for _, server := range m.servers {
		for _, tool := range server.Tools {
			name := ToolName(server.ID, tool.Name)
			schema := tool.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, llm.Tool{Type: "function", Function: llm.FunctionDefinition{Name: name, Description: fmt.Sprintf("MCP[%s] %s", server.Name, tool.Description), Parameters: schema}})
		}
	}
	return out
}

func ToolName(serverID, toolName string) string {
	return "mcp_" + sanitize(serverID) + "__" + sanitize(toolName)
}
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func splitToolName(name string) (string, string, bool) {
	const prefix = "mcp_"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	parts := strings.SplitN(rest, "__", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// Execute calls an MCP tools/call endpoint using the server's configured URL.
func (m *Manager) Execute(ctx context.Context, name, raw string) (string, error) {
	serverID, _, ok := splitToolName(name)
	if !ok {
		return "", errors.New("不是 MCP 工具")
	}
	server, ok := m.Get(serverID)
	if !ok {
		return "", fmt.Errorf("MCP 服务 %q 不存在", serverID)
	}
	if server.Protocol == "stdio" {
		return "", errors.New("stdio MCP 服务尚未配置可执行命令")
	}
	toolName := ""
	for _, tool := range server.Tools {
		if ToolName(server.ID, tool.Name) == name {
			toolName = tool.Name
			break
		}
	}
	if toolName == "" {
		return "", fmt.Errorf("MCP 工具 %q 不存在", name)
	}
	var args map[string]any
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return "", err
		}
	}
	body := map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("%d", time.Now().UnixNano()), "method": "tools/call", "params": map[string]any{"name": toolName, "arguments": args}}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range server.Headers {
		req.Header.Set(key, value)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("MCP 服务要求认证 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("MCP 服务返回 HTTP %d", resp.StatusCode)
	}
	var result struct {
		Result any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != nil {
		return "", errors.New(result.Error.Message)
	}
	out, _ := json.Marshal(result.Result)
	return string(out), nil
}

// StartHealthChecks refreshes health and tool metadata on a background ticker.
func (m *Manager) StartHealthChecks(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		_ = m.Refresh(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = m.Refresh(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Refresh probes every configured server and discovers tools for healthy HTTP
// endpoints. A failed optional MCP server never prevents the app from using
// native tools.
func (m *Manager) Refresh(ctx context.Context) []DoctorReport {
	m.mu.RLock()
	ids := make([]string, 0, len(m.servers))
	for id := range m.servers {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	reports := make([]DoctorReport, 0, len(ids))
	for _, id := range ids {
		report := m.refreshOne(ctx, id)
		reports = append(reports, report)
	}
	return reports
}

func (m *Manager) refreshOne(ctx context.Context, id string) DoctorReport {
	m.mu.RLock()
	server, ok := m.servers[id]
	m.mu.RUnlock()
	report := DoctorReport{ServerID: id, At: time.Now()}
	if !ok {
		report.Tone, report.Message = HealthError, "MCP 服务不存在"
		return report
	}
	if server.Protocol == "stdio" {
		report.Tone, report.Message, report.Hint = HealthWarn, "stdio 服务已配置但未启动", "为该服务提供可执行命令后启用"
		m.updateHealth(id, false, report)
		return report
	}
	parsed, err := url.Parse(server.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		report.Tone, report.Message = HealthError, "MCP URL 无效"
		m.updateHealth(id, false, report)
		return report
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		report.Tone, report.Message = HealthError, err.Error()
		m.updateHealth(id, false, report)
		return report
	}
	for key, value := range server.Headers {
		req.Header.Set(key, value)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		report.Tone, report.Message = HealthError, err.Error()
		m.updateHealth(id, false, report)
		return report
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		report.Tone, report.Message, report.Hint = HealthAuthRequired, "MCP 服务需要认证", "检查 mcp.json 中的认证配置"
		m.updateHealth(id, false, report)
		return report
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		report.Tone, report.Message = HealthWarn, fmt.Sprintf("MCP 服务返回 HTTP %d", resp.StatusCode)
		m.updateHealth(id, false, report)
		return report
	}
	report.Tone, report.Message = HealthOK, "连接正常"
	m.updateHealth(id, true, report)
	_ = m.discoverTools(ctx, id)
	return report
}

func (m *Manager) updateHealth(id string, healthy bool, report DoctorReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if server, ok := m.servers[id]; ok {
		server.Healthy, server.Tone, server.Hint, server.LastError, server.LastCheck = healthy, report.Tone, report.Hint, "", report.At
		if report.Tone != HealthOK {
			server.LastError = report.Message
		}
		m.servers[id] = server
	}
}

func (m *Manager) discoverTools(ctx context.Context, id string) error {
	server, ok := m.Get(id)
	if !ok {
		return errors.New("MCP 服务不存在")
	}
	body := `{"jsonrpc":"2.0","id":"tools-list","method":"tools/list","params":{}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range server.Headers {
		req.Header.Set(key, value)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Result struct {
			Tools []MCPTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	m.mu.Lock()
	if current, ok := m.servers[id]; ok {
		current.Tools = envelope.Result.Tools
		m.servers[id] = current
	}
	m.mu.Unlock()
	return nil
}
