package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"LongCat-frontend/internal/llm"
)

type UndoEntry struct {
	ID              string    `json:"id"`
	Workspace       string    `json:"workspace"`
	File            string    `json:"file"`
	PreviousContent string    `json:"previous_content,omitempty"`
	Existed         bool      `json:"existed"`
	Timestamp       time.Time `json:"timestamp"`
	Action          string    `json:"action"`
}

type undoState struct {
	Entries []UndoEntry `json:"entries"`
}

// UndoStore persists the last 100 agent writes per application config.
type UndoStore struct {
	mu      sync.Mutex
	path    string
	entries []UndoEntry
}

func NewUndoStore() (*UndoStore, error) {
	dir, err := llm.ConfigDir()
	if err != nil {
		return nil, err
	}
	return NewUndoStoreAt(filepath.Join(dir, "undo.json"))
}

func NewUndoStoreAt(path string) (*UndoStore, error) {
	u := &UndoStore{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	var saved undoState
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, fmt.Errorf("解析撤销记录失败: %w", err)
	}
	u.entries = saved.Entries
	return u, nil
}

func (u *UndoStore) saveLocked() error {
	data, err := json.MarshalIndent(undoState{Entries: u.entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.path)
}

// Record captures the state immediately before an agent write.
func (u *UndoStore) Record(root, file string, previous []byte, existed bool, action string) error {
	if u == nil {
		return nil
	}
	if _, err := ResolvePath(root, file); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.entries = append(u.entries, UndoEntry{ID: fmt.Sprintf("undo-%d", time.Now().UnixNano()), Workspace: root, File: filepath.ToSlash(file), PreviousContent: string(previous), Existed: existed, Timestamp: time.Now(), Action: action})
	if len(u.entries) > 100 {
		u.entries = u.entries[len(u.entries)-100:]
	}
	return u.saveLocked()
}

func (u *UndoStore) List(root string) []UndoEntry {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UndoEntry, 0)
	for _, entry := range u.entries {
		if samePath(entry.Workspace, root) {
			out = append(out, entry)
		}
	}
	return out
}

// Last returns the latest undo for a workspace without applying it.
func (u *UndoStore) Last(root string) (UndoEntry, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.entries) - 1; i >= 0; i-- {
		if samePath(u.entries[i].Workspace, root) {
			return u.entries[i], true
		}
	}
	return UndoEntry{}, false
}

// UndoLast restores the latest write and removes it from the stack.
func (u *UndoStore) UndoLast(root string) (UndoEntry, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	index := -1
	for i := len(u.entries) - 1; i >= 0; i-- {
		if samePath(u.entries[i].Workspace, root) {
			index = i
			break
		}
	}
	if index < 0 {
		return UndoEntry{}, errors.New("没有可撤销的文件修改")
	}
	entry := u.entries[index]
	target, err := ResolvePath(root, entry.File)
	if err != nil {
		return UndoEntry{}, err
	}
	if entry.Existed {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return UndoEntry{}, err
		}
		if err := os.WriteFile(target, []byte(entry.PreviousContent), 0o644); err != nil {
			return UndoEntry{}, err
		}
	} else if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return UndoEntry{}, err
	}
	u.entries = append(u.entries[:index], u.entries[index+1:]...)
	return entry, u.saveLocked()
}

func (u *UndoStore) Diff(root, file string) (Diff, error) {
	target, err := ResolvePath(root, file)
	if err != nil {
		return Diff{}, err
	}
	before := ""
	kind := ChangeModified
	u.mu.Lock()
	for i := len(u.entries) - 1; i >= 0; i-- {
		entry := u.entries[i]
		if samePath(entry.Workspace, root) && filepath.Clean(entry.File) == filepath.Clean(filepath.ToSlash(file)) {
			before, kind = entry.PreviousContent, ChangeModified
			if !entry.Existed {
				kind = ChangeAdded
			}
			break
		}
	}
	u.mu.Unlock()
	if _, statErr := os.Stat(target); errors.Is(statErr, os.ErrNotExist) {
		kind = ChangeDeleted
	}
	after := fileContent(target)
	return Diff{Path: filepath.ToSlash(file), Kind: kind, Before: before, After: after, Unified: UnifiedDiff(filepath.ToSlash(file), before, after)}, nil
}
