package memory

// 云同步：记忆仓库由用户手动填写（如 https://github.com/you/longcat-memory.git），
// 通过 UI 上的「云同步」按钮手动触发 push/pull。全部操作走 git CLI，
// 不依赖 gh api；仓库根为 ~/.longcat-frontend/memory（长期记忆 + 工作区镜像）。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SyncStatus 云同步状态（供 UI 展示）。
type SyncStatus struct {
	GitOK      bool   `json:"git_ok"`      // 系统是否安装 git
	IsRepo     bool   `json:"is_repo"`     // 记忆目录是否已是 git 仓库
	HasRemote  bool   `json:"has_remote"`  // 是否配置了 origin 远端
	RepoURL    string `json:"repo_url"`    // 配置的仓库地址
	Branch     string `json:"branch"`      // 当前分支
	Dirty      bool   `json:"dirty"`       // 是否有未提交修改
	LastCommit string `json:"last_commit"` // 最近一次提交短哈希
	Message    string `json:"message"`     // 人类可读提示
}

// SyncStatus 返回当前云同步状态（只读探测，不改动仓库）。
func (s *Store) SyncStatus() SyncStatus {
	st := SyncStatus{RepoURL: s.cfg.Sync.RepoURL, Branch: s.cfg.Sync.Branch}
	if st.Branch == "" {
		st.Branch = "main"
	}
	if !gitOK() {
		st.Message = "未检测到 git，请先安装并配置 git"
		return st
	}
	st.GitOK = true
	if !isGitRepo(s.root) {
		st.Message = "尚未初始化记忆仓库，填写仓库地址后自动初始化"
		return st
	}
	st.IsRepo = true
	if out, err := runGit(s.root, "remote", "get-url", "origin"); err == nil && strings.TrimSpace(out) != "" {
		st.HasRemote = true
	}
	if out, err := runGit(s.root, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
		st.Dirty = true
	}
	if out, err := runGit(s.root, "rev-parse", "--short", "HEAD"); err == nil {
		st.LastCommit = strings.TrimSpace(out)
	}
	if !st.HasRemote {
		st.Message = "未设置仓库地址"
	} else if st.Dirty {
		st.Message = "有未同步的修改"
	} else {
		st.Message = "已同步"
	}
	return st
}

// SetRepo 设置/清除记忆仓库地址。URL 为空表示清除远端（本地仓库保留）。
// 首次设置时自动 git init 并做初始提交，保证后续 push 有基线。
func (s *Store) SetRepo(url string) error {
	url = strings.TrimSpace(url)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !gitOK() {
		return errors.New("未检测到 git，请先安装 git")
	}
	s.cfg.Sync.RepoURL = url
	s.cfg.Sync.Enabled = url != ""
	if url == "" {
		_, _ = runGit(s.root, "remote", "remove", "origin")
		return s.saveLocked()
	}
	branch := s.cfg.Sync.Branch
	if branch == "" {
		branch = "main"
	}
	if !isGitRepo(s.root) {
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			return err
		}
		if _, err := runGit(s.root, "init", "-b", branch); err != nil {
			return err
		}
		// 初始提交：记忆目录可能已存在内容（long-term 等）。
		_, _ = runGit(s.root, "add", "-A")
		_, _ = runGit(s.root, "commit", "-m", "memory: init")
	}
	if _, err := runGit(s.root, "remote", "get-url", "origin"); err != nil {
		if _, err := runGit(s.root, "remote", "add", "origin", url); err != nil {
			return err
		}
	} else {
		_, _ = runGit(s.root, "remote", "set-url", "origin", url)
	}
	return s.saveLocked()
}

// SyncPush 提交本地记忆并推送到远端。
// 兼容两种远端：① 全新空仓库；② 已有内容的仓库（如带 README/初始提交）。
// - push 用 `-u origin <branch>`，首次推送即建立上游，之后无需再指定。
// - pull 用 `--rebase --allow-unrelated-histories`，把远端已有历史与本地
//   init 提交合并（首次同步场景），拉取失败不致命，仍尝试推送。
func (s *Store) SyncPush() (string, error) {
	if !gitOK() {
		return "", errors.New("未检测到 git，请先安装 git")
	}
	if !s.cfg.Sync.Enabled || s.cfg.Sync.RepoURL == "" {
		return "", errors.New("请先设置记忆仓库")
	}
	branch := s.cfg.Sync.Branch
	if branch == "" {
		branch = "main"
	}
	_, _ = runGit(s.root, "add", "-A")
	commitOut, commitErr := runGit(s.root, "commit", "-m", "memory: "+time.Now().Format("2006-01-02 15:04"))
	if commitErr != nil && !nothingToCommit(commitOut, commitErr) {
		return commitOut, commitErr
	}
	// 先拉后推：把远端已有内容合入本地，避免远端领先导致推送被拒。
	if _, pullErr := runGit(s.root, "pull", "--rebase", "--allow-unrelated-histories", "origin", branch); pullErr != nil {
		// 忽略：远端为空或首次推送时 pull 可能失败。
	}
	out, err := runGit(s.root, "push", "-u", "origin", branch)
	if err != nil {
		return out, err
	}
	return strings.TrimSpace(commitOut + "\n" + out), nil
}

// SyncPull 从远端拉取记忆（rebase 方式，保留本地未推送修改）。
// --allow-unrelated-histories 兼容「本地已 init、远端已有内容」的首拉场景。
func (s *Store) SyncPull() (string, error) {
	if !gitOK() {
		return "", errors.New("未检测到 git，请先安装 git")
	}
	if !s.cfg.Sync.Enabled || s.cfg.Sync.RepoURL == "" {
		return "", errors.New("请先设置记忆仓库")
	}
	branch := s.cfg.Sync.Branch
	if branch == "" {
		branch = "main"
	}
	return runGit(s.root, "pull", "--rebase", "--allow-unrelated-histories", "origin", branch)
}

func nothingToCommit(out string, err error) bool {
	msg := strings.ToLower(err.Error() + " " + out)
	return strings.Contains(msg, "nothing to commit")
}

// ---------- git 底层 ----------

// runGit 在指定目录执行 git 命令，带 60s 超时。
func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitOK() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func isGitRepo(dir string) bool {
	_, err := runGit(dir, "rev-parse", "--git-dir")
	return err == nil
}
