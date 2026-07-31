package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/i18n"
	"LongCat-frontend/internal/llm"
	"LongCat-frontend/internal/mcp"
	"LongCat-frontend/internal/workspace"
)

type SubagentActivity struct {
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	Task        string    `json:"task"`
	Status      string    `json:"status"`
	CurrentTool string    `json:"current_tool,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type ActivityTracker struct {
	mu    sync.Mutex
	items map[string]SubagentActivity
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{items: make(map[string]SubagentActivity)}
}
func (t *ActivityTracker) List() []SubagentActivity {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SubagentActivity, 0, len(t.items))
	for _, item := range t.items {
		out = append(out, item)
	}
	return out
}
func (t *ActivityTracker) set(item SubagentActivity) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.items[item.ID] = item
	t.mu.Unlock()
}

// SpawnSubagent runs a definition in the same workspace with an isolated
// message history. It is synchronous from the parent tool loop, which keeps
// cancellation and error propagation deterministic.
func SpawnSubagent(ctx context.Context, manager *llm.Manager, workspace string, skills []frontend.Skill, defs []AgentDefinition, definitionName, task string, depth int, tracker *ActivityTracker, mcpManager *mcp.Manager, undoStore *workspace.UndoStore) (string, error) {
	if depth >= 2 {
		return "", errors.New(i18n.T(i18n.LocaleZH, i18n.MsgSubagentDepthExceeded, nil))
	}
	var definition AgentDefinition
	for _, candidate := range defs {
		if candidate.Name == definitionName {
			definition = candidate
			break
		}
	}
	if definition.Name == "" {
		return "", errors.New(i18n.T(i18n.LocaleZH, i18n.MsgSubagentNotFound, map[string]string{"name": definitionName}))
	}
	id := fmt.Sprintf("subagent-%d", time.Now().UnixNano())
	activity := SubagentActivity{ID: id, Agent: definition.Name, Task: task, Status: "running", StartedAt: time.Now()}
	if tracker != nil {
		tracker.set(activity)
	}
	child := &Session{Manager: manager, Skills: skills, Workspace: workspace, Agents: defs, Activity: tracker, OrchestrationDepth: depth + 1, MCP: mcpManager, Undo: undoStore}
	childPrompt := definition.Body + "\n\n## 当前子任务\n" + task
	child.Skills = append([]frontend.Skill(nil), skills...)
	// Keep the definition as an explicit system instruction without modifying
	// the parent's history or active skill.
	child.DefinitionOverride = childPrompt
	result, err := child.AskWithEvents(ctx, task, nil, func(event ToolEvent) {
		if tracker != nil {
			current := activity
			current.CurrentTool, current.Status = event.Name, "running"
			tracker.set(current)
		}
	})
	activity.FinishedAt = time.Now()
	activity.Status = "success"
	if err != nil {
		activity.Status, activity.Error = "error", err.Error()
	}
	if tracker != nil {
		tracker.set(activity)
	}
	return result, err
}
