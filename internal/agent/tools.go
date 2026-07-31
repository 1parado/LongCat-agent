package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/mcp"
	"LongCat-frontend/internal/workspace"
)

// ToolExecutor exposes the tools available to one agent session.
// All filesystem operations are confined to Workspace.
type ToolExecutor struct {
	Workspace          string
	Skills             []frontend.Skill
	MCP                *mcp.Manager
	Undo               *workspace.UndoStore
	Agents             []AgentDefinition
	Manager            *llm.Manager
	Activity           *ActivityTracker
	OrchestrationDepth int
}

// ValidateWorkspace checks that path is an existing directory and returns its absolute form.
func ValidateWorkspace(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("工作空间不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("工作空间不是目录")
	}
	return abs, nil
}

func (e *ToolExecutor) Definitions() []llm.Tool {
	tools := []llm.Tool{
		{Type: "function", Function: llm.FunctionDefinition{Name: "list_directory", Description: "列出工作空间目录内容。path 为空表示工作空间根目录。", Parameters: objectSchema(map[string]any{
			"path": map[string]any{"type": "string", "description": "相对工作空间路径"},
		})}},
		{Type: "function", Function: llm.FunctionDefinition{Name: "read_file", Description: "读取工作空间中的文本文件。", Parameters: objectSchema(map[string]any{
			"path": map[string]any{"type": "string", "description": "相对工作空间文件路径"},
		})}},
		{Type: "function", Function: llm.FunctionDefinition{Name: "write_file", Description: "写入工作空间中的文本文件；父目录不存在时自动创建。", Parameters: objectSchema(map[string]any{
			"path":    map[string]any{"type": "string", "description": "相对工作空间文件路径"},
			"content": map[string]any{"type": "string", "description": "要写入的完整文本内容"},
		})}},
		{Type: "function", Function: llm.FunctionDefinition{Name: "preview_file", Description: "在内置浏览器中打开工作空间内的 HTML 文件进行预览。用户要求打开、预览或查看项目页面时必须调用此工具，而不是只给出手动操作说明。", Parameters: objectSchema(map[string]any{
			"path": map[string]any{"type": "string", "description": "工作空间内 HTML 文件的相对路径，例如 index.html 或 src/index.html"},
		})}},
		{Type: "function", Function: llm.FunctionDefinition{Name: "load_skill", Description: "按需加载一个技能的完整 SKILL.md 正文。先查看可用技能名称，再按需调用。", Parameters: objectSchema(map[string]any{
			"name": map[string]any{"type": "string", "description": "技能名称"},
		})}},
	}
	if e.MCP != nil {
		tools = append(tools, e.MCP.Definitions()...)
	}
	if len(e.Agents) > 0 && e.Manager != nil {
		tools = append(tools, llm.Tool{Type: "function", Function: llm.FunctionDefinition{Name: "spawn_subagent", Description: "委派一个专门的子 Agent 完成独立任务并返回结果。", Parameters: objectSchema(map[string]any{
			"agent": map[string]any{"type": "string", "description": "Agent 名称"},
			"task":  map[string]any{"type": "string", "description": "交给子 Agent 的清晰任务"},
		})}})
	}
	return tools
}

func objectSchema(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": requiredKeys(properties), "additionalProperties": false}
}

func requiredKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (e *ToolExecutor) Execute(name, raw string) (string, error) {
	return e.ExecuteContext(context.Background(), name, raw)
}

func (e *ToolExecutor) ExecuteContext(ctx context.Context, name, raw string) (string, error) {
	var args map[string]any
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", fmt.Errorf("工具参数无效: %w", err)
	}
	switch name {
	case "spawn_subagent":
		var input struct {
			Agent string `json:"agent"`
			Task  string `json:"task"`
		}
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return "", err
		}
		return SpawnSubagent(ctx, e.Manager, e.Workspace, e.Skills, e.Agents, input.Agent, input.Task, e.OrchestrationDepth, e.Activity, e.MCP, e.Undo)
	case "list_directory":
		path, _ := args["path"].(string)
		return e.list(path)
	case "read_file":
		path, _ := args["path"].(string)
		return e.read(path)
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		return e.write(path, content)
	case "preview_file":
		path, _ := args["path"].(string)
		return e.preview(path)
	case "load_skill":
		name, _ := args["name"].(string)
		for _, s := range e.Skills {
			if s.Name == name || s.Title == name {
				return s.Body, nil
			}
		}
		return "", fmt.Errorf("技能 %q 不存在", name)
	default:
		if e.MCP != nil && strings.HasPrefix(name, "mcp_") {
			return e.MCP.Execute(ctx, name, raw)
		}
		return "", fmt.Errorf("未知工具 %q", name)
	}
}

func (e *ToolExecutor) resolve(path string) (string, error) {
	if strings.TrimSpace(e.Workspace) == "" {
		return "", errors.New("尚未设置工作空间")
	}
	root, err := filepath.Abs(e.Workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("工作空间不可用: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("工作空间不是目录")
	}
	clean := filepath.Clean(filepath.Join(root, filepath.FromSlash(path)))
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("路径必须位于工作空间内")
	}
	// Reject symlink escapes as well as lexical ../ escapes. For a new file,
	// validate the nearest existing parent directory.
	check := clean
	if _, statErr := os.Lstat(clean); os.IsNotExist(statErr) {
		check = filepath.Dir(clean)
	}
	if resolved, evalErr := filepath.EvalSymlinks(check); evalErr == nil {
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr == nil {
			r, relErr := filepath.Rel(resolvedRoot, resolved)
			if relErr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return "", errors.New("路径不能通过符号链接离开工作空间")
			}
		}
	}
	return clean, nil
}

func (e *ToolExecutor) list(path string) (string, error) {
	dir, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	type item struct {
		Name      string `json:"name"`
		Directory bool   `json:"directory"`
	}
	out := make([]item, 0, len(entries))
	for _, x := range entries {
		out = append(out, item{Name: x.Name(), Directory: x.IsDir()})
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func (e *ToolExecutor) read(path string) (string, error) {
	file, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (e *ToolExecutor) write(path, content string) (string, error) {
	file, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	previous, readErr := os.ReadFile(file)
	existed := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return "", err
	}
	if e.Undo != nil {
		if err := e.Undo.Record(e.Workspace, path, previous, existed, "agent write_file"); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("已写入 %d 字节到 %s", len(content), filepath.ToSlash(path)), nil
}

func (e *ToolExecutor) preview(path string) (string, error) {
	file, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("预览目标不是文件")
	}
	ext := strings.ToLower(filepath.Ext(file))
	if ext != ".html" && ext != ".htm" {
		return "", errors.New("preview_file 只支持 HTML 文件")
	}
	return fmt.Sprintf("已在内置浏览器中打开 %s", filepath.ToSlash(path)), nil
}
