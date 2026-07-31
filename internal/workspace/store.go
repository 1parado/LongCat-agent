// Package workspace persists opened folders and their chat sessions.
package workspace

import (
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

type Workspace struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	OpenedAt time.Time `json:"opened_at"`
}

type SessionRecord struct {
	ID          string        `json:"id"`
	WorkspaceID string        `json:"workspace_id"`
	Title       string        `json:"title"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	Messages    []llm.Message `json:"messages"`
	Mode        string        `json:"mode,omitempty"`
	ActiveSkill string        `json:"active_skill,omitempty"`
}

type state struct {
	Workspaces []Workspace     `json:"workspaces"`
	Sessions   []SessionRecord `json:"sessions"`
}

type Store struct {
	mu         sync.Mutex
	path       string
	workspaces map[string]Workspace
	sessions   map[string]SessionRecord
}

func NewStore() (*Store, error) {
	dir, err := llm.ConfigDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(filepath.Join(dir, "workspaces.json"))
}

func NewStoreAt(path string) (*Store, error) {
	s := &Store{path: path, workspaces: map[string]Workspace{}, sessions: map[string]SessionRecord{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var saved state
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("解析工作空间数据失败: %w", err)
	}
	for _, w := range saved.Workspaces {
		s.workspaces[w.ID] = w
	}
	for _, r := range saved.Sessions {
		s.sessions[r.ID] = r
	}
	return s, nil
}

func canonicalFolder(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("文件夹路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("无法打开文件夹: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("选择的路径不是文件夹")
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs), nil
}

// OpenFolder registers a folder and returns its stable workspace record.
func (s *Store) OpenFolder(path string) (Workspace, error) {
	abs, err := canonicalFolder(path)
	if err != nil {
		return Workspace{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, w := range s.workspaces {
		if samePath(w.Path, abs) {
			w.OpenedAt = now
			s.workspaces[id] = w
			return w, s.saveLocked()
		}
	}
	id := stableID(abs)
	name := filepath.Base(abs)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = abs
	}
	w := Workspace{ID: id, Name: name, Path: abs, OpenedAt: now}
	s.workspaces[id] = w
	return w, s.saveLocked()
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

func stableID(path string) string {
	b := []byte(strings.ToLower(filepath.Clean(path)))
	var h uint64 = 1469598103934665603
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return fmt.Sprintf("ws-%x", h)
}
func newID(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

func (s *Store) ListWorkspaces() []Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Workspace, 0, len(s.workspaces))
	for _, w := range s.workspaces {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.After(out[j].OpenedAt) })
	return out
}
func (s *Store) GetWorkspace(id string) (Workspace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[id]
	return w, ok
}

func (s *Store) CreateSession(workspaceID, title string) (SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workspaces[workspaceID]; !ok {
		return SessionRecord{}, errors.New("工作空间不存在")
	}
	now := time.Now()
	title = strings.TrimSpace(title)
	if title == "" {
		title = "新会话"
	}
	r := SessionRecord{ID: newID("session"), WorkspaceID: workspaceID, Title: title, CreatedAt: now, UpdatedAt: now, Messages: []llm.Message{}}
	s.sessions[r.ID] = r
	return r, s.saveLocked()
}
func (s *Store) ListSessions(workspaceID string) []SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SessionRecord{}
	for _, r := range s.sessions {
		if r.WorkspaceID == workspaceID {
			r.Messages = nil
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}
func (s *Store) GetSession(id string) (SessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sessions[id]
	return r, ok
}

func (s *Store) UpdateSessionTitle(id, title string) (SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sessions[id]
	if !ok {
		return SessionRecord{}, errors.New("会话不存在")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return SessionRecord{}, errors.New("会话名称不能为空")
	}
	r.Title = title
	r.UpdatedAt = time.Now()
	s.sessions[id] = r
	if err := s.saveLocked(); err != nil {
		return SessionRecord{}, err
	}
	return r, nil
}
func (s *Store) SaveSession(r SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now()
	s.sessions[r.ID] = r
	return s.saveLocked()
}
func (s *Store) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return errors.New("会话不存在")
	}
	delete(s.sessions, id)
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	out := state{Workspaces: make([]Workspace, 0, len(s.workspaces)), Sessions: make([]SessionRecord, 0, len(s.sessions))}
	for _, w := range s.workspaces {
		out.Workspaces = append(out.Workspaces, w)
	}
	for _, r := range s.sessions {
		out.Sessions = append(out.Sessions, r)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
