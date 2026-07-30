// Package skills 的 market.go 协调 gh API、缓存与本地存储，提供 skills 仓库的
// 添加、浏览、安装、卸载、刷新等高层操作。
package skills

import (
	"fmt"
	"strings"
)

// Market skills 市场管理器，协调 cache + store + gh。
type Market struct {
	cache *Cache
	store *Store
}

// NewMarket 创建并初始化 cache 与 store。
func NewMarket() (*Market, error) {
	cache, err := OpenCache()
	if err != nil {
		return nil, err
	}
	store, err := NewStore()
	if err != nil {
		cache.Close()
		return nil, err
	}
	return &Market{cache: cache, store: store}, nil
}

// Close 释放资源。
func (m *Market) Close() error { return m.cache.Close() }

// Store 返回本地存储（供 agent 加载用）。
func (m *Market) Store() *Store { return m.store }

// Auth 检测 gh 登录状态，返回登录用户名。
func (m *Market) Auth() (string, error) { return AuthStatus() }

// AddRepo 添加仓库并拉取元信息 + skills 列表。
// url 支持 owner/repo 或 https://github.com/owner/repo。
func (m *Market) AddRepo(url string) error {
	u, err := normalizeRepoURL(url)
	if err != nil {
		return err
	}
	if err := m.fetchRepoMeta(u); err != nil {
		return err
	}
	return m.fetchSkillsTree(u)
}

func (m *Market) fetchRepoMeta(url string) error {
	var resp struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		Stargazers    int    `json:"stargazers_count"`
		DefaultBranch string `json:"default_branch"`
		Name          string `json:"name"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := APIGetJSON("repos/"+url, &resp); err != nil {
		return err
	}
	return m.cache.SaveRepo(RepoMeta{
		URL:           url,
		Owner:         resp.Owner.Login,
		Name:          resp.Name,
		Description:   resp.Description,
		Stars:         resp.Stargazers,
		DefaultBranch: resp.DefaultBranch,
	})
}

func (m *Market) fetchSkillsTree(url string) error {
	meta, err := m.cache.GetRepo(url)
	if err != nil {
		return err
	}
	branch := meta.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	skillsPath := meta.SkillsPath
	if skillsPath == "" {
		skillsPath = "skills"
	}

	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := APIGetJSON(fmt.Sprintf("repos/%s/git/trees/%s?recursive=1", url, branch), &tree); err != nil {
		return err
	}

	prefix := skillsPath + "/"
	var metas []SkillMeta
	for _, t := range tree.Tree {
		if t.Type != "blob" || !strings.HasPrefix(t.Path, prefix) {
			continue
		}
		rest := t.Path[len(prefix):]
		if !strings.HasSuffix(rest, "/SKILL.md") {
			continue
		}
		// 支持任意深度：skills/<category>/<name>/SKILL.md 或 skills/<name>/SKILL.md
		skillPath := strings.TrimSuffix(rest, "/SKILL.md")
		parts := strings.Split(skillPath, "/")
		metas = append(metas, SkillMeta{RepoURL: url, Name: parts[len(parts)-1], Path: skillPath})
	}

	// 拉取每个 SKILL.md 的 frontmatter 与正文
	for i, s := range metas {
		body, err := APIGetRaw(fmt.Sprintf("repos/%s/contents/%s/%s/SKILL.md", url, skillsPath, s.Path))
		if err != nil {
			continue
		}
		fm := parseFrontmatter(body)
		title := fm["name"]
		if title == "" {
			title = s.Name
		}
		metas[i].Title = title
		metas[i].Description = fm["description"]
		metas[i].Keywords = fm["keywords"]
		m.cache.InsertSkillIfAbsent(url, s.Name)
		m.cache.SaveSkillBody(url, s.Name, body)
	}
	return m.cache.SaveSkills(url, metas)
}

// parseFrontmatter 解析简易 YAML frontmatter（--- 之间）。
func parseFrontmatter(content string) map[string]string {
	m := map[string]string{}
	if !strings.HasPrefix(content, "---") {
		return m
	}
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return m
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return m
}

// normalizeRepoURL 把输入规范化为 owner/repo。
// 支持 owner/repo、https://github.com/owner/repo、github.com/owner/repo，去除 .git 后缀与尾部 /。
func normalizeRepoURL(input string) (string, error) {
	input = strings.TrimSpace(input)
	for _, p := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
		input = strings.TrimPrefix(input, p)
	}
	input = strings.TrimSuffix(input, ".git")
	input = strings.TrimSuffix(input, "/")
	parts := strings.Split(input, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return input, nil
	}
	return "", fmt.Errorf("仓库格式无效，应为 owner/repo 或 https://github.com/owner/repo")
}

// ListRepos 列出已收藏仓库。
func (m *Market) ListRepos() ([]RepoMeta, error) { return m.cache.ListRepos() }

// GetRepo 取仓库元信息（支持完整 URL 或 owner/repo）。
func (m *Market) GetRepo(url string) (RepoMeta, error) {
	u, err := normalizeRepoURL(url)
	if err != nil {
		return RepoMeta{}, err
	}
	return m.cache.GetRepo(u)
}

// RemoveRepo 删除仓库缓存。
func (m *Market) RemoveRepo(url string) error {
	u, err := normalizeRepoURL(url)
	if err != nil {
		return err
	}
	return m.cache.RemoveRepo(u)
}

// BrowseRepo 浏览仓库的 skills（缓存过期自动刷新，刷新失败回退旧缓存）。
func (m *Market) BrowseRepo(url string) ([]SkillMeta, error) {
	u, err := normalizeRepoURL(url)
	if err != nil {
		return nil, err
	}
	if !m.cache.SkillsFresh(u) {
		if err := m.fetchSkillsTree(u); err != nil {
			list, _ := m.cache.ListSkills(u)
			if len(list) > 0 {
				return list, nil
			}
			return nil, err
		}
	}
	return m.cache.ListSkills(u)
}

// GetSkillDetail 取 SKILL.md 正文（缓存优先）。支持深层目录结构。
func (m *Market) GetSkillDetail(url, name string) (string, error) {
	if body, err := m.cache.GetSkillBody(url, name); err == nil {
		return body, nil
	}
	meta, err := m.cache.GetRepo(url)
	if err != nil {
		return "", err
	}
	skillsPath := meta.SkillsPath
	if skillsPath == "" {
		skillsPath = "skills"
	}
	// 查 skill 的相对路径（支持 skills/<category>/<name>/SKILL.md）
	skillPath := name
	if list, err := m.cache.ListSkills(url); err == nil {
		for _, s := range list {
			if s.Name == name && s.Path != "" {
				skillPath = s.Path
				break
			}
		}
	}
	body, err := APIGetRaw(fmt.Sprintf("repos/%s/contents/%s/%s/SKILL.md", url, skillsPath, skillPath))
	if err != nil {
		return "", err
	}
	m.cache.InsertSkillIfAbsent(url, name)
	m.cache.SaveSkillBody(url, name, body)
	return body, nil
}

// IsInstalled 判断 skill 是否已安装。
func (m *Market) IsInstalled(name string) bool { return m.store.IsInstalled(name) }

// ListInstalled 返回已安装 skill 名。
func (m *Market) ListInstalled() []string { return m.store.ListInstalled() }

// Install 安装 skill：拉取正文 → 写入用户级目录 → 记录 installed。
func (m *Market) Install(url, name string) error {
	u, err := normalizeRepoURL(url)
	if err != nil {
		return err
	}
	body, err := m.GetSkillDetail(u, name)
	if err != nil {
		return err
	}
	if err := m.store.Install(name, body); err != nil {
		return err
	}
	return m.cache.RecordInstall(name, u)
}

// InstallResult 批量安装结果。
type InstallResult struct {
	Installed []string
	Failed    map[string]error
}

// InstallBatch 批量安装。
func (m *Market) InstallBatch(url string, names []string) InstallResult {
	r := InstallResult{Failed: map[string]error{}}
	for _, n := range names {
		if err := m.Install(url, n); err != nil {
			r.Failed[n] = err
		} else {
			r.Installed = append(r.Installed, n)
		}
	}
	return r
}

// Uninstall 卸载 skill。
func (m *Market) Uninstall(name string) error {
	if err := m.store.Uninstall(name); err != nil {
		return err
	}
	return m.cache.RecordUninstall(name)
}

// Refresh 强制刷新仓库 skills 缓存。
func (m *Market) Refresh(url string) error {
	u, err := normalizeRepoURL(url)
	if err != nil {
		return err
	}
	return m.fetchSkillsTree(u)
}
