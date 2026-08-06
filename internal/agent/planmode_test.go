package agent

import (
	"context"
	"strings"
	"testing"
)

// TestPlanModeFiltersMutatingTools 验证规划模式下 spawn_subagent 与 MCP 工具被剔除，
// 只读/文档工具（list/read/load_skill/preview/write_file）保留。
func TestPlanModeFiltersMutatingTools(t *testing.T) {
	exec := &ToolExecutor{PlanMode: true}
	names := make(map[string]bool)
	for _, tl := range exec.Definitions() {
		names[tl.Function.Name] = true
	}
	if names["spawn_subagent"] {
		t.Error("plan mode must not expose spawn_subagent")
	}
	for n := range names {
		if strings.HasPrefix(n, "mcp_") {
			t.Errorf("plan mode must not expose MCP tool %q", n)
		}
	}
	for _, keep := range []string{"list_directory", "read_file", "load_skill", "preview_file", "write_file"} {
		if !names[keep] {
			t.Errorf("plan mode should keep %q", keep)
		}
	}
}

// TestPlanModeBlocksCodeWrite 验证规划模式下 write_file 拒绝代码文件、允许文档文件。
func TestPlanModeBlocksCodeWrite(t *testing.T) {
	exec := &ToolExecutor{PlanMode: true, Workspace: t.TempDir()}
	if _, err := exec.ExecuteContext(context.Background(), "write_file", `{"path":"main.go","content":"package main"}`); err == nil {
		t.Error("plan mode must reject code file writes")
	}
	out, err := exec.ExecuteContext(context.Background(), "write_file", `{"path":"PLAN.md","content":"# Plan"}`)
	if err != nil {
		t.Fatalf("plan mode should allow doc writes, got %v", err)
	}
	if !strings.Contains(out, "PLAN.md") {
		t.Errorf("unexpected output: %s", out)
	}
}

// TestExecuteModeAllowsEverything 验证执行模式不拦截任何工具。
func TestExecuteModeAllowsEverything(t *testing.T) {
	exec := &ToolExecutor{PlanMode: false, Workspace: t.TempDir()}
	if _, err := exec.ExecuteContext(context.Background(), "write_file", `{"path":"main.go","content":"package main"}`); err != nil {
		t.Fatalf("execute mode should allow code writes, got %v", err)
	}
}
