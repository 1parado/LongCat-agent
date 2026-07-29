// Package frontend 提供前端专属技能（SKILL.md）的加载与匹配。
package frontend

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill 表示一个 frontend-skills/<name>/SKILL.md 技能。
type Skill struct {
	Name        string // 目录名，如 react-component
	Title       string // frontmatter name 或目录名
	Description string // frontmatter description
	Keywords    []string
	Body        string // SKILL.md 正文（不含 frontmatter）
	Path        string
}

// LoadSkills 从 dir（通常是 frontend-skills/）加载全部技能。
// 目录不存在时返回空列表而非错误。
func LoadSkills(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s := parseSkill(e.Name(), string(data))
		s.Path = path
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// parseSkill 解析简易 YAML frontmatter（name / description / keywords）。
func parseSkill(dirName, content string) Skill {
	s := Skill{Name: dirName, Title: dirName, Body: content}
	if !strings.HasPrefix(content, "---") {
		return s
	}
	rest := content[3:]
	end := strings.Index(rest, "---")
	if end < 0 {
		return s
	}
	front := rest[:end]
	s.Body = strings.TrimSpace(rest[end+3:])
	for _, line := range strings.Split(front, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.Trim(strings.TrimSpace(v), `"'`)
		switch key {
		case "name":
			s.Title = val
		case "description":
			s.Description = val
		case "keywords":
			for _, kw := range strings.Split(val, ",") {
				if kw = strings.TrimSpace(kw); kw != "" {
					s.Keywords = append(s.Keywords, strings.ToLower(kw))
				}
			}
		}
	}
	return s
}

// Match 根据用户输入返回相关技能（关键词命中），最多 max 个。
func Match(skills []Skill, query string, max int) []Skill {
	q := strings.ToLower(query)
	var hit []Skill
	for _, s := range skills {
		for _, kw := range s.Keywords {
			if strings.Contains(q, kw) {
				hit = append(hit, s)
				break
			}
		}
		if len(hit) >= max {
			break
		}
	}
	return hit
}
