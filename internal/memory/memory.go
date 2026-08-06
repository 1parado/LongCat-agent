// Package memory 实现 LongCat 的 agent 记忆。
//
// 目录布局：
//   - 工作区记忆：<工作空间>/.longcat/memory/YYYY-MM-DD.md（按天归档）
//   - 长期记忆：~/.longcat-frontend/memory/long-term/*.md（全局、跨项目）
//   - 云同步仓库根：~/.longcat-frontend/memory/（git 仓库，包含 long-term/
//     与 workspaces/<id>/ 工作区镜像），同步由用户手动触发，全程使用 git CLI。
//
// 每轮对话结束后 RecordTurn 会把本轮内容写入当日工作区记忆，并把提取出的
// 个人事实追加到长期记忆；LLM 提取失败时降级为原文摘要，保证记忆不丢。
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"LongCat-frontend/internal/llm"
)

// Scope 记忆类型：工作区记忆（随项目）与长期记忆（随用户）。
const (
	ScopeWorkspace = "workspace" // <ws>/.longcat/memory/*.md
	ScopeLongTerm  = "longterm"  // ~/.longcat-frontend/memory/long-term/*.md
)

// 记忆文件防膨胀上限：单文件超过 maxFileSize 时截断为末尾 maxKeepSize 字节。
const (
	maxFileSize = 400 * 1024
	maxKeepSize = 100 * 1024
	// maxContextBytes 注入系统提示时单类记忆的最大字节数。
	maxContextBytes = 4000
)

// Config 记忆配置，持久化到 ~/.longcat-frontend/memory.json。
type Config struct {
	Enabled bool       `json:"enabled"`
	Sync    SyncConfig `json:"sync"`
}

// SyncConfig 云同步配置：仓库由用户手动填写，点击按钮才同步。
type SyncConfig struct {
	Enabled bool   `json:"enabled"`
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
}

// Entry 记忆文件条目（供列表展示）。
type Entry struct {
	Scope   string `json:"scope"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Updated int64  `json:"updated"` // unix 秒
}

// Store 记忆存储：配置 + 长期记忆根目录（云同步仓库根）。
type Store struct {
	mu      sync.Mutex
	cfgPath string
	root    string // ~/.longcat-frontend/memory
	cfg     Config
}

// NewStore 创建记忆存储，配置存于 ~/.longcat-frontend/memory.json。
func NewStore() (*Store, error) {
	dir, err := llm.ConfigDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(filepath.Join(dir, "memory.json"), filepath.Join(dir, "memory"))
}

// NewStoreAt 使用显式路径创建存储（测试用）。
func NewStoreAt(cfgPath, root string) (*Store, error) {
	s := &Store{cfgPath: cfgPath, root: root, cfg: Config{Enabled: true, Sync: SyncConfig{Branch: "main"}}}
	if data, err := os.ReadFile(cfgPath); err == nil {
		var cfg Config
		if json.Unmarshal(data, &cfg) == nil {
			if cfg.Sync.Branch == "" {
				cfg.Sync.Branch = "main"
			}
			s.cfg = cfg
		}
	}
	return s, nil
}

// Enabled 记忆是否开启。
func (s *Store) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Enabled
}

// SetEnabled 开关记忆。
func (s *Store) SetEnabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Enabled = on
	return s.saveLocked()
}

// ---------- 路径 ----------

// longTermDir 长期记忆目录（全局、跨项目）。
func (s *Store) longTermDir() string { return filepath.Join(s.root, "long-term") }

// workspaceDir 工作区记忆目录：<工作空间>/.longcat/memory。
func workspaceDir(ws string) string { return filepath.Join(ws, ".longcat", "memory") }

// mirrorDir 云同步仓库内的工作区记忆镜像目录。
func (s *Store) mirrorDir(ws string) string { return filepath.Join(s.root, "workspaces", stableID(ws)) }

// dirFor 按 scope 返回记忆目录。
func (s *Store) dirFor(scope, workspace string) (string, error) {
	switch scope {
	case ScopeWorkspace:
		if strings.TrimSpace(workspace) == "" {
			return "", errors.New("请先打开工作空间")
		}
		return workspaceDir(workspace), nil
	case ScopeLongTerm:
		return s.longTermDir(), nil
	default:
		return "", fmt.Errorf("未知记忆类型 %q", scope)
	}
}

// sanitizeName 校验并规范化记忆文件名（去掉 .md 后缀、拒绝路径穿越）。
func sanitizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".md")
	name = strings.TrimSuffix(name, ".markdown")
	if name == "" {
		return "", errors.New("文件名不能为空")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", errors.New("文件名不合法")
	}
	return name, nil
}

// todayName 今日记忆文件名：YYYY-MM-DD.md。
func todayName() string { return time.Now().Format("2006-01-02") + ".md" }

// stableID 工作空间路径 → 稳定短 ID（与 workspace.stableID 同算法，避免跨包依赖）。
func stableID(path string) string {
	b := []byte(strings.ToLower(filepath.Clean(path)))
	var h uint64 = 1469598103934665603
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return fmt.Sprintf("ws-%x", h)
}

// ---------- CRUD ----------

// List 列出全部记忆条目（工作区 + 长期），按更新时间倒序。
func (s *Store) List(workspace string) ([]Entry, error) {
	var out []Entry
	if strings.TrimSpace(workspace) != "" {
		out = append(out, listDir(workspaceDir(workspace), ScopeWorkspace)...)
	}
	out = append(out, listDir(s.longTermDir(), ScopeLongTerm)...)
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

func listDir(dir, scope string) []Entry {
	var out []Entry
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Scope:   scope,
			Name:    strings.TrimSuffix(e.Name(), ".md"),
			Size:    info.Size(),
			Updated: info.ModTime().Unix(),
		})
	}
	return out
}

// Read 读取单个记忆文件内容。
func (s *Store) Read(scope, workspace, name string) (string, error) {
	path, err := s.pathFor(scope, workspace, name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", errors.New("记忆不存在")
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Create 新建（或覆盖）一条记忆。工作区记忆同时镜像到用户目录以便云同步。
func (s *Store) Create(scope, workspace, name, content string) (Entry, error) {
	path, err := s.pathFor(scope, workspace, name)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Entry{}, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mirrorLocked(scope, workspace, name)
	return entryFor(path, scope)
}

// Update 更新一条已存在的记忆；不存在时返回错误。
func (s *Store) Update(scope, workspace, name, content string) (Entry, error) {
	path, err := s.pathFor(scope, workspace, name)
	if err != nil {
		return Entry{}, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return Entry{}, errors.New("记忆不存在，无法更新")
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mirrorLocked(scope, workspace, name)
	return entryFor(path, scope)
}

// Delete 删除一条记忆（含工作区镜像）。
func (s *Store) Delete(scope, workspace, name string) error {
	path, err := s.pathFor(scope, workspace, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return errors.New("记忆不存在")
		}
		return err
	}
	if scope == ScopeWorkspace && strings.TrimSpace(workspace) != "" {
		_ = os.Remove(filepath.Join(s.mirrorDir(workspace), filepath.Base(path)))
	}
	return nil
}

// Append 向记忆文件追加内容（不存在则创建），带防膨胀截断。
func (s *Store) Append(scope, workspace, name, content string) (Entry, error) {
	path, err := s.pathFor(scope, workspace, name)
	if err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Entry{}, err
	}
	// 防膨胀：超过上限时先截断为末尾 maxKeepSize 字节。
	if data, err := os.ReadFile(path); err == nil && len(data) > maxFileSize {
		tail := data
		if len(tail) > maxKeepSize {
			tail = tail[len(tail)-maxKeepSize:]
		}
		_ = os.WriteFile(path, tail, 0o644)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Entry{}, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return Entry{}, err
	}
	if err := f.Close(); err != nil {
		return Entry{}, err
	}
	s.mirrorLocked(scope, workspace, name)
	return entryFor(path, scope)
}

// pathFor 解析 scope+name 到绝对路径（含名校验）。
func (s *Store) pathFor(scope, workspace, name string) (string, error) {
	n, err := sanitizeName(name)
	if err != nil {
		return "", err
	}
	dir, err := s.dirFor(scope, workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, n+".md"), nil
}

func entryFor(path, scope string) (Entry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Scope: scope, Name: strings.TrimSuffix(filepath.Base(path), ".md"), Size: info.Size(), Updated: info.ModTime().Unix()}, nil
}

// mirrorLocked 把工作区记忆复制到云同步仓库的 workspaces/<id>/ 镜像目录。
// 调用方需持有 s.mu。失败静默（云同步是可选能力，不应影响主流程）。
func (s *Store) mirrorLocked(scope, workspace, name string) {
	if scope != ScopeWorkspace || strings.TrimSpace(workspace) == "" {
		return
	}
	src := filepath.Join(workspaceDir(workspace), name+".md")
	dst := filepath.Join(s.mirrorDir(workspace), name+".md")
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dst, data, 0o644)
}

// ---------- 对话后写记忆 ----------

// RecordTurn 在每轮对话结束后写入记忆：工作区记忆按天归档，提取出的个人事实
// 追加到长期记忆。应在独立 goroutine 中调用（内部自带 30s 超时），不阻塞对话；
// LLM 提取失败或未配置供应商时降级为原文摘要。
func (s *Store) RecordTurn(ctx context.Context, provider llm.Provider, workspace, userMsg, reply string) {
	if !s.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wsNote, personalNote := s.extract(ctx, provider, userMsg, reply)
	now := time.Now()
	if strings.TrimSpace(wsNote) == "" {
		wsNote = "**用户**: " + strings.TrimSpace(userMsg) + "\n\n**LongCat**: " + strings.TrimSpace(reply)
	}
	section := "\n---\n## " + now.Format("2006-01-02 15:04") + "\n" + strings.TrimSpace(wsNote) + "\n"
	if strings.TrimSpace(workspace) != "" {
		_, _ = s.Append(ScopeWorkspace, workspace, todayName(), section)
	}
	if strings.TrimSpace(personalNote) != "" {
		_, _ = s.Append(ScopeLongTerm, "", "profile", "\n---\n## "+now.Format("2006-01-02 15:04")+"\n"+strings.TrimSpace(personalNote)+"\n")
	}
}

// extract 调用 LLM 从本轮对话中提取工作区记忆与个人记忆；返回空串表示提取失败。
// 未配置供应商（provider.ID 为空）或调用出错时直接返回空，由调用方降级。
func (s *Store) extract(ctx context.Context, provider llm.Provider, userMsg, reply string) (string, string) {
	if provider.ID == "" {
		return "", ""
	}
	const sys = `你是 LongCat 的记忆整理器。根据给定的一轮对话，产出两类记忆：
1. 工作区记忆：本轮的关键决策、任务进度、待办、涉及的文件路径、技术结论与重要代码位置。用简洁的中文 markdown 要点，省略寒暄。
2. 个人记忆：关于用户本人的稳定事实（姓名、职业、偏好、习惯、联系方式等），可用于长期记住用户。若没有则输出空。
严格按以下格式输出，不要输出其它内容：
[WORKSPACE]
<工作区记忆 markdown>
[END]
[PERSONAL]
<个人记忆 markdown，无则留空>
[END]`
	conv := "用户: " + userMsg + "\n\nLongCat: " + reply
	resp, err := llm.Chat(ctx, provider, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: conv},
	}, nil)
	if err != nil {
		return "", ""
	}
	return parseBlocks(resp)
}

// parseBlocks 解析 [WORKSPACE]…[END] 与 [PERSONAL]…[END] 两个块。
func parseBlocks(text string) (workspaceNote, personalNote string) {
	ws := between(text, "[WORKSPACE]", "[END]")
	personal := between(text, "[PERSONAL]", "[END]")
	if ws == "" && personal == "" {
		return "", ""
	}
	return ws, personal
}

func between(text, start, end string) string {
	i := strings.Index(text, start)
	if i < 0 {
		return ""
	}
	rest := text[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		j = len(rest)
	}
	return strings.TrimSpace(rest[:j])
}

// ---------- 记忆注入（读回系统提示） ----------

// LoadContext 返回注入系统提示的记忆片段：今日工作区记忆 + 长期记忆，各截断
// 到 maxContextBytes，避免撑爆上下文。
func (s *Store) LoadContext(workspace string) string {
	var b strings.Builder
	if strings.TrimSpace(workspace) != "" {
		if today := readTail(filepath.Join(workspaceDir(workspace), todayName()), maxContextBytes); today != "" {
			b.WriteString("## 今日工作区记忆\n")
			b.WriteString(today)
			b.WriteString("\n")
		}
	}
	if profile := readTail(filepath.Join(s.longTermDir(), "profile.md"), maxContextBytes); profile != "" {
		b.WriteString("## 长期记忆（关于用户）\n")
		b.WriteString(profile)
		b.WriteString("\n")
	}
	return b.String()
}

// readTail 读取文件末尾最多 max 字节（文件过长时只保留末尾，UTF-8 安全截断）。
func readTail(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) <= max {
		return strings.TrimSpace(s)
	}
	cut := s[len(s)-max:]
	if i := strings.IndexRune(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return strings.TrimSpace(cut)
}

// ---------- 配置持久化 ----------

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		return err
	}
	tmp := s.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfgPath)
}
