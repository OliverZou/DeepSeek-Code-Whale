package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/team_engine"
)

func (b *Toolset) teamEngineTools() []core.Tool {
	return []core.Tool{
		b.teamPlanTool(),
		b.teamCreateTool(),
		b.teamRunTool(),
		b.teamStatusTool(),
		b.teamListTool(),
		b.teamFeedbackTool(),
		b.teamHistoryTool(),
		b.teamExportTool(),
	}
}

func (b *Toolset) teamEnginePaths() (dbPath, wbDir string) {
	return filepath.Join(b.root, "team_engine.db"),
		filepath.Join(b.root, "team_tasks")
}

func (b *Toolset) newTeamEngine() (*team_engine.TeamEngine, error) {
	dbPath, wbDir := b.teamEnginePaths()
	spawner := team_engine.NewShellSubagentSpawner()
	return team_engine.New(dbPath, wbDir, "", spawner)
}

func toolResult(text string) core.ToolResult {
	return core.ToolResult{Content: text}
}

func toolError(format string, args ...interface{}) core.ToolResult {
	return core.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// --- team_plan ---

func (b *Toolset) teamPlanTool() toolFn {
	return toolFn{
		name:        "team_plan",
		description: "Decompose a complex goal into batches of subtasks and execute them in parallel with Leader-Worker-Verifier orchestration.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"goal": map[string]any{"type": "string", "description": "The goal to decompose and execute"},
			},
			"required": []string{"goal"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct{ Goal string `json:"goal"` }
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			batches, err := eng.PlanAndRun(args.Goal, b.root)
			if err != nil {
				return toolError("plan: %v", err), nil
			}
			var s string
			for _, batch := range batches {
				icon := tick(batch.Status == team_engine.BatchStatusPassed)
				s += fmt.Sprintf("%s Batch %s [%s]\n", icon, batch.LabelOrID(), batch.Status)
				for _, t := range batch.Tasks {
					s += fmt.Sprintf("  %s %s [%s] %s\n", tick(t.State == team_engine.TaskStateDone), t.ID[:8], t.State, t.Title)
				}
			}
			return toolResult(s), nil
		},
	}
}

// --- team_create ---

func (b *Toolset) teamCreateTool() toolFn {
	return toolFn{
		name:        "team_create",
		description: "Create a single team task with a specific role.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "Task title"},
				"description": map[string]any{"type": "string", "description": "Task prompt for the agent"},
				"role":        map[string]any{"type": "string", "description": "developer/tester/reviewer/researcher/writer/formatter/evaluator/synthesizer", "default": "developer"},
			},
			"required": []string{"title", "description"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Role        string `json:"role"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			task, err := eng.CreateTask(args.Title, args.Description, team_engine.AgentRole(args.Role), "", nil, 3, b.root, "")
			if err != nil {
				return toolError("create: %v", err), nil
			}
			return toolResult(fmt.Sprintf("Created task %s: %s [%s]", task.ID[:8], task.Title, task.State)), nil
		},
	}
}

// --- team_run ---

func (b *Toolset) teamRunTool() toolFn {
	return toolFn{
		name:        "team_run",
		description: "Execute a task through produce -> verify -> done lifecycle.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID to execute"},
			},
			"required": []string{"task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct{ TaskID string `json:"task_id"` }
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			ok, err := eng.RunTask(args.TaskID)
			if err != nil {
				return toolError("run: %v", err), nil
			}
			task, _ := eng.GetTask(args.TaskID)
			mark := tick(ok)
			return toolResult(fmt.Sprintf("%s %s %s -> %s", mark, args.TaskID[:8], task.Title, task.State)), nil
		},
	}
}

// --- team_status ---

func (b *Toolset) teamStatusTool() toolFn {
	return toolFn{
		name:        "team_status",
		description: "Get detailed status of a team task.",
		readOnly:    true,
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID"},
			},
			"required": []string{"task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct{ TaskID string `json:"task_id"` }
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			task, err := eng.GetTask(args.TaskID)
			if err != nil || task == nil {
				return toolError("task %q not found", args.TaskID), nil
			}
			s := fmt.Sprintf("Task: %s\nTitle: %s\nRole: %s\nState: %s (%d%%)\nRetries: %d/%d\nBatch: %s\nCreated: %s",
				task.ID[:8], task.Title, task.Role, task.State,
				team_engine.GetProgress(task.State), task.RetryCount, task.MaxRetries,
				task.BatchID, task.CreatedAt)
			if task.VerifierFeedback != "" {
				short := task.VerifierFeedback
				if len(short) > 200 {
					short = short[:200]
				}
				s += "\nFeedback: " + short
			}
			return toolResult(s), nil
		},
	}
}

// --- team_list ---

func (b *Toolset) teamListTool() toolFn {
	return toolFn{
		name:        "team_list",
		description: "List all team tasks with state and progress.",
		readOnly:    true,
		parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			tasks, err := eng.ListTasks()
			if err != nil {
				return toolError("list: %v", err), nil
			}
			if len(tasks) == 0 {
				return toolResult("No tasks. Use team_plan or team_create."), nil
			}
			var s string
			for _, t := range tasks {
				s += fmt.Sprintf("%s %s [%s] %d%% %s\n", tick(t.State == team_engine.TaskStateDone), t.ID[:8], t.State, team_engine.GetProgress(t.State), t.Title)
			}
			return toolResult(s), nil
		},
	}
}

// --- team_feedback ---

func (b *Toolset) teamFeedbackTool() toolFn {
	return toolFn{
		name:        "team_feedback",
		description: "Send human feedback to a running task.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID"},
				"message": map[string]any{"type": "string", "description": "Feedback message"},
			},
			"required": []string{"task_id", "message"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				TaskID  string `json:"task_id"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			if err := eng.SendFeedback(args.TaskID, args.Message); err != nil {
				return toolError("feedback: %v", err), nil
			}
			return toolResult("Feedback sent to " + args.TaskID[:8]), nil
		},
	}
}

// --- team_history ---

func (b *Toolset) teamHistoryTool() toolFn {
	return toolFn{
		name:        "team_history",
		description: "Show state transition history for a task.",
		readOnly:    true,
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID"},
			},
			"required": []string{"task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct{ TaskID string `json:"task_id"` }
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			entries, err := eng.DB.GetTaskHistory(args.TaskID)
			if err != nil {
				return toolError("history: %v", err), nil
			}
			if len(entries) == 0 {
				return toolResult("No history for " + args.TaskID[:8]), nil
			}
			var s string
			for _, e := range entries {
				if e.OldState == "" {
					s += fmt.Sprintf("  new -> %s (%s)\n", e.NewState, e.ChangedAt[:19])
				} else {
					s += fmt.Sprintf("  %s -> %s (%s)\n", e.OldState, e.NewState, e.ChangedAt[:19])
				}
				if e.ErrorMsg != "" {
					s += "    err: " + e.ErrorMsg + "\n"
				}
			}
			return toolResult(s), nil
		},
	}
}

// --- team_export ---

func (b *Toolset) teamExportTool() toolFn {
	return toolFn{
		name:        "team_export",
		description: "Export complete task session log as JSON.",
		readOnly:    true,
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID"},
			},
			"required": []string{"task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct{ TaskID string `json:"task_id"` }
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			jsonStr, err := eng.ExportTaskLogJSON(args.TaskID)
			if err != nil {
				return toolError("export: %v", err), nil
			}
			return toolResult(jsonStr), nil
		},
	}
}

// --- helpers ---

func tick(ok bool) string {
	if ok {
		return "+"
	}
	return "-"
}
