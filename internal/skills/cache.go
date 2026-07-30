// Package skills 的 cache.go 实现 sqlite 缓存层。
//
// 缓存仓库元信息、skills 列表与 SKILL.md 正文，TTL 24h，避免重复 GitHub API
// 调用。数据库位于 ~/.longcat-frontend/skills.db，使用纯 Go 驱动 modernc.org/sqlite。
package skills

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const cacheTTL = 24 * time.Hour

// Cache sqlite 缓存。
type Cache struct {
	db *sql.DB
}

// OpenCache 打开（必要时创建）~/.longcat-frontend/skills.db 并初始化表结构。
func OpenCache() (*Cache, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".longcat-frontend", "skills.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	c := &Cache{db: db}
	if err := c.init(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cache) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS repos (
  url TEXT PRIMARY KEY,
  owner TEXT, name TEXT, description TEXT,
  stars INTEGER, default_branch TEXT,
  skills_path TEXT DEFAULT 'skills',
  fetched_at INTEGER
)`,
		`CREATE TABLE IF NOT EXISTS skills (
  repo_url TEXT, name TEXT,
  path TEXT DEFAULT '',
  title TEXT, description TEXT, keywords TEXT, body TEXT,
  fetched_at INTEGER,
  PRIMARY KEY (repo_url, name)
)`,
		`CREATE TABLE IF NOT EXISTS installed (
  name TEXT PRIMARY KEY,
  source_repo TEXT, installed_at INTEGER
)`,
	}
	for _, s := range stmts {
		if _, err := c.db.Exec(s); err != nil {
			return fmt.Errorf("init cache: %w", err)
		}
	}
	// 兼容旧库：补充 path 列（已存在则忽略错误）
	c.db.Exec("ALTER TABLE skills ADD COLUMN path TEXT DEFAULT ''")
	return nil
}

// Close 关闭数据库。
func (c *Cache) Close() error { return c.db.Close() }

// ---------- repos ----------

// RepoMeta 仓库元信息。
type RepoMeta struct {
	URL           string `json:"url"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Stars         int    `json:"stars"`
	DefaultBranch string `json:"default_branch"`
	SkillsPath    string `json:"skills_path"`
	FetchedAt     int64  `json:"fetched_at"`
}

// SaveRepo 保存或更新仓库元信息。
func (c *Cache) SaveRepo(r RepoMeta) error {
	sp := r.SkillsPath
	if sp == "" {
		sp = "skills"
	}
	_, err := c.db.Exec(`INSERT OR REPLACE INTO repos
(url, owner, name, description, stars, default_branch, skills_path, fetched_at)
VALUES (?,?,?,?,?,?,?,?)`,
		r.URL, r.Owner, r.Name, r.Description, r.Stars, r.DefaultBranch, sp, time.Now().Unix())
	return err
}

// GetRepo 取仓库元信息。
func (c *Cache) GetRepo(url string) (RepoMeta, error) {
	var r RepoMeta
	var sp sql.NullString
	err := c.db.QueryRow(`SELECT url, owner, name, description, stars, default_branch, skills_path, fetched_at
FROM repos WHERE url=?`, url).
		Scan(&r.URL, &r.Owner, &r.Name, &r.Description, &r.Stars, &r.DefaultBranch, &sp, &r.FetchedAt)
	if err != nil {
		return r, err
	}
	r.SkillsPath = sp.String
	return r, nil
}

// ListRepos 列出所有已收藏仓库。
func (c *Cache) ListRepos() ([]RepoMeta, error) {
	rows, err := c.db.Query(`SELECT url, owner, name, description, stars, default_branch, skills_path, fetched_at
FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []RepoMeta
	for rows.Next() {
		var r RepoMeta
		var sp sql.NullString
		if err := rows.Scan(&r.URL, &r.Owner, &r.Name, &r.Description, &r.Stars,
			&r.DefaultBranch, &sp, &r.FetchedAt); err != nil {
			return nil, err
		}
		r.SkillsPath = sp.String
		list = append(list, r)
	}
	return list, rows.Err()
}

// RemoveRepo 删除仓库及其 skills 缓存。
func (c *Cache) RemoveRepo(url string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	tx.Exec("DELETE FROM skills WHERE repo_url=?", url)
	tx.Exec("DELETE FROM repos WHERE url=?", url)
	return tx.Commit()
}

// RepoFresh 仓库元信息缓存是否未过期。
func (c *Cache) RepoFresh(url string) bool {
	r, err := c.GetRepo(url)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(r.FetchedAt, 0)) < cacheTTL
}

// ---------- skills ----------

// SkillMeta skill 元信息（body 按需获取）。
type SkillMeta struct {
	RepoURL     string `json:"repo_url"`
	Name        string `json:"name"`
	Path        string `json:"path"` // 相对 skills 根的路径，如 "personal/edit-article" 或 "react-component"
	Title       string `json:"title"`
	Description string `json:"description"`
	Keywords    string `json:"keywords"`
	HasBody     bool   `json:"has_body"`
}

// SaveSkills 替换式保存仓库的 skills 列表（不含 body）。
func (c *Cache) SaveSkills(repoURL string, metas []SkillMeta) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	tx.Exec("DELETE FROM skills WHERE repo_url=?", repoURL)
	now := time.Now().Unix()
	for _, s := range metas {
		tx.Exec(`INSERT OR REPLACE INTO skills (repo_url, name, path, title, description, keywords, fetched_at)
VALUES (?,?,?,?,?,?,?)`, s.RepoURL, s.Name, s.Path, s.Title, s.Description, s.Keywords, now)
	}
	return tx.Commit()
}

// ListSkills 列出仓库的 skills（不含 body）。
func (c *Cache) ListSkills(repoURL string) ([]SkillMeta, error) {
	rows, err := c.db.Query(`SELECT repo_url, name, path, title, description, keywords, body IS NOT NULL
FROM skills WHERE repo_url=? ORDER BY name`, repoURL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []SkillMeta
	for rows.Next() {
		var s SkillMeta
		var path sql.NullString
		var bodyNotNull bool
		if err := rows.Scan(&s.RepoURL, &s.Name, &path, &s.Title, &s.Description, &s.Keywords, &bodyNotNull); err != nil {
			return nil, err
		}
		s.Path = path.String
		s.HasBody = bodyNotNull
		s.RepoURL = repoURL
		list = append(list, s)
	}
	return list, rows.Err()
}

// SaveSkillBody 保存 skill 的 SKILL.md 正文。
func (c *Cache) SaveSkillBody(repoURL, name, body string) error {
	_, err := c.db.Exec(`UPDATE skills SET body=?, fetched_at=? WHERE repo_url=? AND name=?`,
		body, time.Now().Unix(), repoURL, name)
	return err
}

// InsertSkillIfAbsent 若 skill 行不存在则插入（用于先建行再存 body）。
func (c *Cache) InsertSkillIfAbsent(repoURL, name string) error {
	_, err := c.db.Exec(`INSERT OR IGNORE INTO skills (repo_url, name, fetched_at) VALUES (?,?,?)`,
		repoURL, name, time.Now().Unix())
	return err
}

// GetSkillBody 取 skill 正文（未缓存返回 sql.ErrNoRows）。
func (c *Cache) GetSkillBody(repoURL, name string) (string, error) {
	var body sql.NullString
	err := c.db.QueryRow(`SELECT body FROM skills WHERE repo_url=? AND name=?`, repoURL, name).Scan(&body)
	if err != nil {
		return "", err
	}
	if !body.Valid || body.String == "" {
		return "", sql.ErrNoRows
	}
	return body.String, nil
}

// SkillsFresh 仓库 skills 缓存是否未过期。
func (c *Cache) SkillsFresh(repoURL string) bool {
	var fetched sql.NullInt64
	err := c.db.QueryRow(`SELECT MIN(fetched_at) FROM skills WHERE repo_url=?`, repoURL).Scan(&fetched)
	if err != nil || !fetched.Valid || fetched.Int64 == 0 {
		return false
	}
	return time.Since(time.Unix(fetched.Int64, 0)) < cacheTTL
}

// ---------- installed ----------

// Installed 已安装记录。
type Installed struct {
	Name        string
	SourceRepo  string
	InstalledAt int64
}

// RecordInstall 记录安装。
func (c *Cache) RecordInstall(name, repo string) error {
	_, err := c.db.Exec(`INSERT OR REPLACE INTO installed (name, source_repo, installed_at) VALUES (?,?,?)`,
		name, repo, time.Now().Unix())
	return err
}

// RecordUninstall 删除安装记录。
func (c *Cache) RecordUninstall(name string) error {
	_, err := c.db.Exec(`DELETE FROM installed WHERE name=?`, name)
	return err
}

// ListInstalledRecords 列出安装记录。
func (c *Cache) ListInstalledRecords() ([]Installed, error) {
	rows, err := c.db.Query(`SELECT name, source_repo, installed_at FROM installed ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Installed
	for rows.Next() {
		var i Installed
		if err := rows.Scan(&i.Name, &i.SourceRepo, &i.InstalledAt); err != nil {
			return nil, err
		}
		list = append(list, i)
	}
	return list, rows.Err()
}
