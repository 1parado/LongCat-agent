package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ChangeKind is the normalized kind used by the watcher, diff API, and UI.
type ChangeKind string

const (
	ChangeModified  ChangeKind = "modified"
	ChangeAdded     ChangeKind = "added"
	ChangeDeleted   ChangeKind = "deleted"
	ChangeRenamed   ChangeKind = "renamed"
	ChangeUntracked ChangeKind = "untracked"
	ChangeIgnored   ChangeKind = "ignored"
)

type ChangeEvent struct {
	Workspace string     `json:"workspace"`
	Path      string     `json:"path"`
	Kind      ChangeKind `json:"kind"`
	At        time.Time  `json:"at"`
}

type fileFingerprint struct {
	Size    int64
	ModTime time.Time
	Mode    uint32
}

// ResolvePath applies the same lexical workspace boundary used by agent file
// tools. It is also used by the server's diff/undo endpoints.
func ResolvePath(root, path string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(root, filepath.FromSlash(path)))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("路径必须位于工作空间内")
	}
	if rootInfo, statErr := os.Stat(root); statErr != nil || !rootInfo.IsDir() {
		return "", errors.New("工作空间不是目录")
	}
	check := target
	if _, statErr := os.Lstat(check); os.IsNotExist(statErr) {
		check = filepath.Dir(check)
	}
	if resolved, evalErr := filepath.EvalSymlinks(check); evalErr == nil {
		if resolvedRoot, rootErr := filepath.EvalSymlinks(root); rootErr == nil {
			r, relErr := filepath.Rel(resolvedRoot, resolved)
			if relErr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return "", errors.New("路径不能通过符号链接离开工作空间")
			}
		}
	}
	return target, nil
}
