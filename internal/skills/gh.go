// Package skills 实现 GitHub skills 仓库的浏览、缓存与安装。
//
// 设计要点（见 PRD/skills-management-prd.md）：
//   - 复用系统 gh CLI 的 auth，项目内不存任何凭证
//   - 所有 GitHub API 调用走 `gh api`，自动透传 token
//   - 未装 gh 时优雅降级（匿名 + 提示）
package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// HasGh 检测系统是否安装 gh CLI。
func HasGh() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// AuthStatus 检测 gh 登录状态，返回登录用户名（空串表示未登录或匿名）。
func AuthStatus() (string, error) {
	if !HasGh() {
		return "", errors.New("未安装 GitHub CLI (gh)，请先安装：https://cli.github.com")
	}
	out, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh 未登录，请执行: gh auth login\n%s", firstLine(string(out)))
	}
	// 解析 "Logged in to github.com account <user>"
	s := string(out)
	if i := strings.Index(s, "account "); i >= 0 {
		rest := s[i+len("account "):]
		end := strings.IndexAny(rest, " \r\n")
		if end < 0 {
			end = len(rest)
		}
		return rest[:end], nil
	}
	return "", nil
}

// APIGet 调用 `gh api <path>`，返回 JSON 字节。
func APIGet(path string) ([]byte, error) {
	if !HasGh() {
		return nil, errors.New("未安装 gh")
	}
	out, err := exec.Command("gh", "api", path).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api %s: %s", path, firstLine(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// APIGetJSON 调用 gh api 并解析 JSON 到 v。
func APIGetJSON(path string, v any) error {
	data, err := APIGet(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// APIGetRaw 调用 gh api 取原始文本（用于 SKILL.md 正文）。
func APIGetRaw(path string) (string, error) {
	if !HasGh() {
		return "", errors.New("未安装 gh")
	}
	out, err := exec.Command("gh", "api", "-H", "Accept: application/vnd.github.raw", path).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh api %s: %s", path, firstLine(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// firstLine 取首行，避免长错误信息刷屏。
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
