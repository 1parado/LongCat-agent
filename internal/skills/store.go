// Package skills 的 store.go 管理用户级 skills 目录 ~/.longcat-frontend/skills/。
//
// 已安装的 skill 以 <dir>/<name>/SKILL.md 形式落盘，agent.NewSession 加载该目录
// 即可让已安装 skill 参与关键词匹配。
package skills

import (
	"os"
	"path/filepath"
	"sort"
)

// Store 管理用户级 skills 目录。
type Store struct {
	dir string
}

// NewStore 创建 Store，dir 为 ~/.longcat-frontend/skills/（不存在则创建）。
func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".longcat-frontend", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir 返回 skills 目录路径。
func (s *Store) Dir() string { return s.dir }

// Install 将 SKILL.md 正文写入 <dir>/<name>/SKILL.md，覆盖已有。
func (s *Store) Install(name, body string) error {
	d := filepath.Join(s.dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644)
}

// Uninstall 删除某个 skill 目录。
func (s *Store) Uninstall(name string) error {
	return os.RemoveAll(filepath.Join(s.dir, name))
}

// IsInstalled 判断是否已安装。
func (s *Store) IsInstalled(name string) bool {
	info, err := os.Stat(filepath.Join(s.dir, name, "SKILL.md"))
	return err == nil && !info.IsDir()
}

// ListInstalled 返回已安装 skill 的目录名（排序）。
func (s *Store) ListInstalled() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.dir, e.Name(), "SKILL.md")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}
