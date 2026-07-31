package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type AgentScope string

const (
	ScopeProject AgentScope = "project"
	ScopeUser    AgentScope = "user"
	ScopeBundled AgentScope = "bundled"
)

type AgentDefinition struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Scope       AgentScope `json:"scope"`
	Tools       []string   `json:"tools,omitempty"`
	Body        string     `json:"body"`
	Path        string     `json:"path"`
}

// ParseAgentDefinition reads a markdown agent definition with simple YAML
// frontmatter. The parser accepts scalar strings and [a, b] lists without a
// YAML dependency, which is enough for the documented agent format.
func ParseAgentDefinition(path string, scope AgentScope) (AgentDefinition, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return AgentDefinition{}, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return AgentDefinition{}, errors.New("agent definition 缺少 frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return AgentDefinition{}, errors.New("agent definition frontmatter 未闭合")
	}
	end += 4
	d := AgentDefinition{Scope: scope, Path: path}
	scanner := bufio.NewScanner(strings.NewReader(text[4:end]))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), "\"'")
		switch key {
		case "name":
			d.Name = value
		case "description":
			d.Description = value
		case "scope":
			if value != "" {
				d.Scope = AgentScope(value)
			}
		case "tools":
			d.Tools = parseList(value)
		}
	}
	d.Body = strings.TrimSpace(text[end+4:])
	if d.Name == "" {
		d.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if d.Description == "" {
		d.Description = d.Name
	}
	if d.Body == "" {
		return AgentDefinition{}, errors.New("agent definition 正文不能为空")
	}
	return d, nil
}

func parseList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), "\"'")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DiscoverAgents applies project > user > bundled precedence by name.
func DiscoverAgents(workspace, userDir, bundledDir string) ([]AgentDefinition, error) {
	byName := make(map[string]AgentDefinition)
	load := func(dir string, scope AgentScope) error {
		if strings.TrimSpace(dir) == "" {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
				continue
			}
			d, err := ParseAgentDefinition(filepath.Join(dir, entry.Name()), scope)
			if err != nil {
				return fmt.Errorf("加载 agent %s: %w", entry.Name(), err)
			}
			byName[d.Name] = d
		}
		return nil
	}
	// Lowest precedence first so later scopes override earlier definitions.
	if err := load(bundledDir, ScopeBundled); err != nil {
		return nil, err
	}
	if err := load(userDir, ScopeUser); err != nil {
		return nil, err
	}
	if workspace != "" {
		if err := load(filepath.Join(workspace, ".longcat-frontend", "agents"), ScopeProject); err != nil {
			return nil, err
		}
	}
	out := make([]AgentDefinition, 0, len(byName))
	for _, d := range byName {
		out = append(out, d)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name < out[i].Name {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}
