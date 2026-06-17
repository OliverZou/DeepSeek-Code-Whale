package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/dashboard"
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
		b.teamResultTool(),
		b.teamOutputTool(),
		b.teamDeleteTool(),
	}
}

func (b *Toolset) teamEnginePaths() (dbPath, wbDir string) {
	return filepath.Join(b.root, ".whale", "team_engine.db"),
		filepath.Join(b.root, ".whale", "team_tasks")
}

func (b *Toolset) newTeamEngine() (*team_engine.TeamEngine, error) {
	dbPath, wbDir := b.teamEnginePaths()
	// Prefer per-instance spawn func, then package-level default, then shell.
	var spawner team_engine.SubagentSpawner
	if b.teamEngineSpawnFunc != nil {
		spawner = team_engine.NewFuncSpawner(b.teamEngineSpawnFunc)
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.SpawnerType("default", "adapter", "", 0) }
	} else if df := team_engine.DefaultSpawnFunc(); df != nil {
		spawner = team_engine.NewFuncSpawner(df)
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.SpawnerType("default", "adapter", "", 0) }
	} else {
		spawner = team_engine.NewShellSubagentSpawner()
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.SpawnerType("default", "shell", "", 0) }
	}
	return team_engine.New(dbPath, wbDir, "", spawner)
}

func toolResult(text string) core.ToolResult {
	return core.ToolResult{Content: text}
}

func toolError(format string, args ...interface{}) core.ToolResult {
	return core.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// --- team_plan ---

// AutoExecuteMasterTask is called by the dashboard client when it receives
// a resume command via heartbeat.  It loads the master task and executes
// it directly without waiting for a user-initiated team_plan call.
func (b *Toolset) AutoExecuteMasterTask(masterTaskID string) {
	go func() {
		eng, err := b.newTeamEngine()
		if err != nil {
			logToFile(filepath.Join(b.root, ".whale", "team_tasks", "logs", "engine.log"),
				"dashboard-resume: newTeamEngine failed: %v", err)
			return
		}
		defer eng.Close()

		mt, err := eng.GetMasterTask(masterTaskID)
		if err != nil || mt == nil {
			logToFile(filepath.Join(b.root, ".whale", "team_tasks", "logs", "engine.log"),
				"dashboard-resume: GetMasterTask %s failed: err=%v", masterTaskID, err)
			return
		}

		workdir := mt.WorkspacePath
		if workdir == "" {
			workdir = b.root
		}

		ctx, cancel := context.WithCancel(context.Background())
		b.autoExecCancelMu.Lock()
		b.autoExecCancel = cancel
		b.autoExecCancelMu.Unlock()
		defer func() {
			cancel()
			b.autoExecCancelMu.Lock()
			b.autoExecCancel = nil
			b.autoExecCancelMu.Unlock()
		}()

			// Forward events and sync state to dashboard during auto-resume.
			if b.dashboardClient != nil {
				var lastSync time.Time
				eng.OnEvent(func(event team_engine.TaskEvent) { defer func() { if r := recover(); r != nil && team_engine.DefaultTeamLog != nil { team_engine.Log("sync", "event callback panic: %v", r) } }()
					b.dashboardClient.SendTaskEvent(event)
					if (event.Type == team_engine.EventStateChanged || event.Type == team_engine.EventAgentLog) && time.Since(lastSync) > 2*time.Second {
						lastSync = time.Now()
						pushSyncState(eng, b.dashboardClient, b.root)
					}
				})
			}

		batches, err := eng.ResumeMasterTask(ctx, masterTaskID, mt.Goal, workdir)
		if eng.Loggers != nil {
			eng.Loggers.Engine("dashboard-resume: masterTask=%s goal=%q batches=%d err=%v",
				masterTaskID, mt.Goal, len(batches), err)
		}
	}()
}

// CancelAutoExecute cancels a running AutoExecuteMasterTask.
func (b *Toolset) CancelAutoExecute() {
	b.autoExecCancelMu.Lock()
	defer b.autoExecCancelMu.Unlock()
	if b.autoExecCancel != nil {
		b.autoExecCancel()
		b.autoExecCancel = nil
	}
}

func logToFile(path, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().UTC().Format(time.RFC3339)
	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", ts, msg)
}

// --- team_plan ---

func (b *Toolset) teamPlanTool() toolFn {
	return toolFn{
		name:        "team_plan",
		description: "Decompose a complex goal into batches of subtasks and execute them in parallel with Leader-Worker-Verifier orchestration.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"goal":    map[string]any{"type": "string", "description": "The goal to decompose and execute"},
				"workdir": map[string]any{"type": "string", "description": "Working directory for subtasks (default: workspace root)"},
				"team":    map[string]any{"type": "string", "description": "Team name from .whale/teams/{team}.yaml (optional)"},
				"async":   map[string]any{"type": "boolean", "description": "If true, decompose only and return immediately. Use team_run/team_result to track progress."},
			},
			"required": []string{"goal"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			return b.runTeamPlan(ctx, call, nil)
		},
		fnWithProgress: func(ctx context.Context, call core.ToolCall, progress func(core.ToolProgress)) (core.ToolResult, error) {
			return b.runTeamPlan(ctx, call, progress)
		},
	}
}

func (b *Toolset) runTeamPlan(ctx context.Context, call core.ToolCall, progress func(core.ToolProgress)) (core.ToolResult, error) {
	var args struct {
		Goal    string `json:"goal"`
		Workdir string `json:"workdir,omitempty"`
		Team    string `json:"team,omitempty"`
		Async   bool   `json:"async,omitempty"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return toolError("invalid args: %v", err), nil
	}
	workdir := args.Workdir
	if workdir == "" {
		workdir = b.root
	}
	eng, err := b.newTeamEngine()
	if err != nil {
		return toolError("init: %v", err), nil
	}
	defer eng.Close()

	// Check for a pending dashboard resume command (auto-triggered by dashboard).
	if args.Goal == "" && b.dashboardClient != nil {
		if mtID := b.dashboardClient.PendingResume(); mtID != "" {
			mt, err := eng.GetMasterTask(mtID)
			if err != nil {
				return toolError("dashboard resume: master task %s: %v", mtID, err), nil
			}
			if mt == nil {
				return toolError("dashboard resume: master task %s not found", mtID), nil
			}
			args.Goal = mt.Goal
			workdir = mt.WorkspacePath
			if workdir == "" {
				workdir = b.root
			}
		}
	}

	// Load team configuration if specified.
	if args.Team != "" {
		teamRoots := team_engine.DefaultTeamRoots(b.root)
		tc, err := team_engine.FindTeamInRoots(teamRoots, args.Team)
		if err != nil {
			return toolError("load team %q: %v", args.Team, err), nil
		}
		eng.SetTeam(tc)
	}

	// Forward engine events to dashboard for real-time UI updates.
	if b.dashboardClient != nil {
		var lastSync time.Time
		eng.OnEvent(func(event team_engine.TaskEvent) { defer func() { if r := recover(); r != nil && team_engine.DefaultTeamLog != nil { team_engine.Log("sync", "event callback panic: %v", r) } }()
			b.dashboardClient.SendTaskEvent(event)
			// Throttled full sync (max 1 per 2s) after state changes.
			if (event.Type == team_engine.EventStateChanged || event.Type == team_engine.EventAgentLog) && time.Since(lastSync) > 2*time.Second {
				lastSync = time.Now()
				if team_engine.DefaultTeamLog != nil {
					team_engine.Log("sync", "triggering sync push")
				}
				pushSyncState(eng, b.dashboardClient, b.root)
			}
		})
	}

	// Register progress callback if we have one.
	// Throttled to at most 1 update per 500ms to avoid TUI flickering.
	if progress != nil {
		var lastProgress time.Time
		eng.OnEvent(func(event team_engine.TaskEvent) { defer func() { if r := recover(); r != nil && team_engine.DefaultTeamLog != nil { team_engine.Log("sync", "event callback panic: %v", r) } }()
			summary := ""
			switch event.Type {
			case team_engine.EventStateChanged:
				summary = fmt.Sprintf("📋 %s → %s", event.Title, event.NewState)
			case team_engine.EventWorkerOutput:
				summary = fmt.Sprintf("🔨 %s 产出完成", event.Title)
			case team_engine.EventVerifierResult:
				summary = fmt.Sprintf("🔍 %s 审查完成", event.Title)
			case team_engine.EventTaskDone:
				summary = fmt.Sprintf("✅ %s 完成", event.Title)
			}
			if summary != "" {
				// Throttle: skip if last update was less than 500ms ago.
				now := time.Now()
				if now.Sub(lastProgress) < 500*time.Millisecond {
					return
				}
				lastProgress = now
				progress(core.ToolProgress{
					Status:  "running",
					Summary: summary,
				})
			}
		})
	}

	// --- Phase 1: Create master task immediately so dashboard sees it ---
			var masterTask *team_engine.MasterTask
			var mtErr error
			existing, _ := eng.DB.ListMasterTasks()
			for _, mt := range existing {
				if mt.Goal == args.Goal { masterTask = mt; break }
			}
				for _, mt := range existing { if mt != masterTask { _ = eng.DeleteMasterTask(mt.ID) } }
			if masterTask == nil {
				masterTask, mtErr = eng.CreateMasterTask(args.Goal, b.root)
				if mtErr != nil {
					return toolError("create master task: %v", mtErr), nil
				}
			}
			if mtErr != nil {
				return toolError("create master task: %v", mtErr), nil
			}

			// Async mode: return immediately so the agent can review later.
		// The master task is already visible in the dashboard.
		if args.Async {
			return core.ToolResult{
				Content: fmt.Sprintf("📋 Master task created: `%s`\nRun `team_plan goal=\"...\"` (without async) to execute.", masterTask.ID),
				Metadata: map[string]any{"master_task_id": masterTask.ID, "mode": "async"},
			}, nil
		}

		// Synchronous mode: PlanAndRun handles decompose + execution in one step.
			var batches []*team_engine.Batch
			existingTasks, _ := eng.DB.ListTasksByMasterTask(masterTask.ID)
			if len(existingTasks) > 0 {
				batches, err = eng.ResumeMasterTask(ctx, masterTask.ID, args.Goal, b.root)
			} else {
				batches, err = eng.PlanAndRun(ctx, args.Goal, b.root, masterTask.ID)
			}
			if err != nil {
				var s string
				for _, batch := range batches {
					icon := tick(batch.Status == team_engine.BatchStatusPassed)
					s += fmt.Sprintf("%s Batch %s [%s]\n", icon, batch.LabelOrID(), batch.Status)
					for _, t := range batch.Tasks {
 					s += fmt.Sprintf("  %s %s [%s] %s\n", tick(t.State == team_engine.TaskStateDone), t.ID, t.State, t.Title)
					}
				}
				if s != "" {
					s = "\n---\n## Results (partial)\n\n" + s + fmt.Sprintf("\nCancelled: %v", err)
					return toolResult(s), nil
				}
				return toolError("plan: %v", err), nil
			}

			// Build rich result with task details, file paths, and output previews.
			var mdResults string
			type taskSummary struct {
				Title    string `json:"title"`
				Role     string `json:"role"`
				State    string `json:"state"`
				Workdir  string `json:"workdir"`
				Output   string `json:"output,omitempty"`
				Files    string `json:"files,omitempty"`
			}
			allTasks := make([]taskSummary, 0)
			doneCount := 0

			for _, batch := range batches {
				icon := tick(batch.Status == team_engine.BatchStatusPassed)
				mdResults += fmt.Sprintf("\n### %s Batch: %s [%s]\n", icon, batch.LabelOrID(), batch.Status)
				for _, t := range batch.Tasks {
					stateIcon := tick(t.State == team_engine.TaskStateDone)
					if t.State == team_engine.TaskStateFailed {
						stateIcon = "❌"
					} else if t.State == team_engine.TaskStateSuspended {
						stateIcon = "⏸"
					}
					if t.State == team_engine.TaskStateDone {
						doneCount++
					}
					mdResults += fmt.Sprintf("\n  **%s** `%s`\n", stateIcon, t.Title)
					mdResults += fmt.Sprintf("  - 角色: %s | 状态: %s | 进度: %d%%\n", t.Role, t.State, team_engine.GetProgress(t.State))
					if t.Workdir != "" {
						mdResults += fmt.Sprintf("  - 工作目录: `%s`\n", t.Workdir)
					}

					ts := taskSummary{
						Title:   t.Title,
						Role:    string(t.Role),
						State:   string(t.State),
						Workdir: t.Workdir,
					}

					// Read output preview.
					if output, err := eng.Whiteboard.ReadOutput(t.ID); err == nil && len(output) > 0 {
						preview := output
						if len(preview) > 200 {
							preview = preview[:200] + "..."
						}
						mdResults += fmt.Sprintf("  - 输出摘要: %s\n", preview)
						ts.Output = preview
					}

					// List artifacts.
					if artifacts, err := eng.Whiteboard.ListArtifacts(t.ID); err == nil && len(artifacts) > 0 {
						files := ""
						for _, a := range artifacts {
							files += a + ", "
							mdResults += fmt.Sprintf("  - 📎 产出文件: `%s`\n", a)
						}
						ts.Files = files
					}
					allTasks = append(allTasks, ts)
				}
			}

			overallStatus := "✅ 全部完成"
			if doneCount < len(allTasks) {
				overallStatus = fmt.Sprintf("⏳ 部分完成 (%d/%d)", doneCount, len(allTasks))
			}

			fullResult := fmt.Sprintf("## 执行结果: %s\n%s", overallStatus, mdResults)

			// Also return structured metadata for programmatic consumption.
			metadata := map[string]any{
				"status":      overallStatus,
				"total_tasks": len(allTasks),
				"done_count":  doneCount,
				"tasks":       allTasks,
			}
			return core.ToolResult{Content: fullResult, Metadata: metadata}, nil
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

			task, err := eng.CreateTask(args.Title, args.Description, team_engine.AgentRole(args.Role), "", nil, 3, b.root, "", "", "")
			if err != nil {
				return toolError("create: %v", err), nil
			}
 			return toolResult(fmt.Sprintf("Created task %s: %s [%s]", task.ID, task.Title, task.State)), nil
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

			ok, err := eng.RunTask(ctx, args.TaskID)
			if err != nil {
				return toolError("run: %v", err), nil
			}
			task, _ := eng.GetTask(args.TaskID)
			mark := tick(ok)
 			return toolResult(fmt.Sprintf("%s %s %s -> %s", mark, args.TaskID, task.Title, task.State)), nil
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
 				task.ID, task.Title, task.Role, task.State,
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
 				s += fmt.Sprintf("%s %s [%s] %d%% %s\n", tick(t.State == team_engine.TaskStateDone), t.ID, t.State, team_engine.GetProgress(t.State), t.Title)
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
 			return toolResult("Feedback sent to " + args.TaskID), nil
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
 				return toolResult("No history for " + args.TaskID), nil
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

// --- team_output ---

func (b *Toolset) teamOutputTool() toolFn {
	return toolFn{
		name:        "team_output",
		description: "Get a task's output, messages, and artifacts. Use to check worker progress during execution.",
		readOnly:    true,
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID to inspect"},
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

			var md string
			md += fmt.Sprintf("## %s\n\n", task.Title)
			md += fmt.Sprintf("**角色**: %s | **状态**: %s | **进度**: %d%%\n", task.Role, task.State, team_engine.GetProgress(task.State))
			if task.Workdir != "" {
				md += fmt.Sprintf("**工作目录**: `%s`\n", task.Workdir)
			}
			if task.VerifierFeedback != "" {
				md += fmt.Sprintf("**审查反馈**: %s\n", task.VerifierFeedback)
			}
			md += "\n"

			// Output
			if output, err := eng.Whiteboard.ReadOutput(args.TaskID); err == nil && output != "" {
				if len(output) > 2000 {
					md += fmt.Sprintf("### 工作产出（前2000字符）\n\n%s\n\n*(共%d字符，剩余内容已截断)*\n", output[:2000], len(output))
				} else {
					md += "### 工作产出\n\n" + output + "\n"
				}
			}

			// Inbox messages (feedback from Leader or others)
			if msgs, err := eng.Whiteboard.ReadInbox(args.TaskID); err == nil && len(msgs) > 0 {
				md += "\n### 未读消息\n\n"
				for _, msg := range msgs {
					md += fmt.Sprintf("- **来自 %s**: %s\n", msg.From, msg.Content)
				}
			}

			// Artifacts
			if artifacts, err := eng.Whiteboard.ListArtifacts(args.TaskID); err == nil && len(artifacts) > 0 {
				md += "\n### 产出文件\n\n"
				for _, a := range artifacts {
					md += fmt.Sprintf("- `%s`\n", a)
				}
			}

			metadata := map[string]any{
				"task_id":    task.ID,
				"title":      task.Title,
				"state":      string(task.State),
				"progress":   team_engine.GetProgress(task.State),
				"workdir":    task.Workdir,
			}
			return core.ToolResult{Content: md, Metadata: metadata}, nil
		},
	}
}

// --- team_result ---

func (b *Toolset) teamResultTool() toolFn {
	return toolFn{
		name:        "team_result",
		description: "Get structured execution results for a master task (completed or suspended). Returns task details, file paths, and output summaries.",
		readOnly:    true,
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"master_task_id": map[string]any{"type": "string", "description": "Master task ID from team_plan result"},
			},
			"required": []string{"master_task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct{ MasterTaskID string `json:"master_task_id"` }
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			mt, err := eng.GetMasterTask(args.MasterTaskID)
			if err != nil || mt == nil {
				return toolError("master task %q not found", args.MasterTaskID), nil
			}

			tasks, err := eng.ListTasksByMasterTask(args.MasterTaskID)
			if err != nil {
				return toolError("list tasks: %v", err), nil
			}

			type taskInfo struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Role     string `json:"role"`
				State    string `json:"state"`
				Progress int    `json:"progress"`
				Workdir  string `json:"workdir"`
				Output   string `json:"output,omitempty"`
				Files    []string `json:"files,omitempty"`
			}

			doneCount := 0
			taskList := make([]taskInfo, 0)
			var mdBuilder string
			mdBuilder += fmt.Sprintf("## 执行结果: %s\n\n", mt.Goal)
			mdBuilder += fmt.Sprintf("| 任务 | 角色 | 状态 | 进度 |\n")
			mdBuilder += fmt.Sprintf("|------|------|------|------|\n")

			for _, t := range tasks {
				if t.State == team_engine.TaskStateDone {
					doneCount++
				}
				ti := taskInfo{
					ID:       t.ID,
					Title:    t.Title,
					Role:     string(t.Role),
					State:    string(t.State),
					Progress: team_engine.GetProgress(t.State),
					Workdir:  t.Workdir,
				}
				stateIcon := tick(t.State == team_engine.TaskStateDone)

				output, _ := eng.Whiteboard.ReadOutput(t.ID)
				if len(output) > 0 {
					preview := output
					if len(preview) > 300 {
						preview = preview[:300] + "..."
					}
					ti.Output = preview
				}

				artifacts, _ := eng.Whiteboard.ListArtifacts(t.ID)
				if len(artifacts) > 0 {
					ti.Files = artifacts
				}

				mdBuilder += fmt.Sprintf("| %s %s | %s | %s | %d%% |\n",
					stateIcon, t.Title, t.Role, t.State, ti.Progress)
				if t.Workdir != "" {
					mdBuilder += fmt.Sprintf("  📁 工作目录: `%s`\n", t.Workdir)
				}
				if len(ti.Output) > 0 {
					mdBuilder += fmt.Sprintf("  📄 输出: %s\n", ti.Output)
				}
				for _, f := range artifacts {
					mdBuilder += fmt.Sprintf("  📎 文件: `%s`\n", f)
				}
				taskList = append(taskList, ti)
			}

			overall := fmt.Sprintf("%d/%d tasks done", doneCount, len(tasks))

			metadata := map[string]any{
				"goal":       mt.Goal,
				"status":     overall,
				"total":      len(tasks),
				"done_count": doneCount,
				"tasks":      taskList,
			}
			return core.ToolResult{Content: mdBuilder, Metadata: metadata}, nil
		},
	}
}

// --- team_delete ---

func (b *Toolset) teamDeleteTool() toolFn {
	return toolFn{
		name:        "team_delete",
		description: "Delete a team task by ID. Use all=true to delete all tasks at once (both standalone and master tasks). Running tasks are transitioned to failed before deletion.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID to delete. Optional if all=true."},
				"all":     map[string]any{"type": "boolean", "description": "Delete all tasks when true."},
			},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				TaskID string `json:"task_id"`
				All    bool   `json:"all"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}

			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			if args.All {
				// Delete all master tasks first (each cascades to subtasks).
				masterTasks, err := eng.DB.ListMasterTasks()
				if err != nil {
					return toolError("list master: %v", err), nil
				}
				masterCount := 0
				for _, mt := range masterTasks {
					if err := eng.DeleteMasterTask(mt.ID); err != nil {
						return toolError("delete master %s: %v", mt.ID, err), nil
					}
					masterCount++
				}

				// Delete remaining standalone tasks.
				tasks, err := eng.DB.ListTasks()
				if err != nil {
					return toolError("list: %v", err), nil
				}
				standaloneCount := 0
				for _, t := range tasks {
					if err := eng.DeleteTask(t.ID); err != nil {
						return toolError("delete task %s: %v", t.ID, err), nil
					}
					standaloneCount++
				}
				return toolResult(fmt.Sprintf("Deleted all tasks: %d master + %d standalone.", masterCount, standaloneCount)), nil
			}

			if args.TaskID == "" {
				return toolError("task_id is required when all is not set"), nil
			}

			// Try as a regular task first, then as a master task.
			task, err := eng.GetTask(args.TaskID)
			if err == nil && task != nil {
				if err := eng.DeleteTask(args.TaskID); err != nil {
					return toolError("delete: %v", err), nil
				}
				return toolResult(fmt.Sprintf("Deleted task %s (%s).", args.TaskID, task.Title)), nil
			}

			// Try as master task.
			if err := eng.DeleteMasterTask(args.TaskID); err != nil {
				return toolError("delete master: %v", err), nil
			}
			return toolResult(fmt.Sprintf("Deleted master task %s and all subtasks.", args.TaskID)), nil
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

// pushSyncState builds full master-task + subtask state from the engine
// and pushes it to the dashboard via WebSocket for cache update.
func pushSyncState(eng *team_engine.TeamEngine, client interface {
	SyncState(mts []dashboard.MasterTaskJSON, sts map[string][]dashboard.SubtaskJSON, wsLabel string)
}, workspacePath string) {
	defer func() {
		if r := recover(); r != nil {
			if team_engine.DefaultTeamLog != nil {
				team_engine.Log("sync", "pushSyncState panic: %v", r)
			}
		}
	}()
	mts, err := eng.DB.ListMasterTasks()
	if team_engine.DefaultTeamLog != nil {
		team_engine.Log("sync", "pushSyncState: mts=%d err=%v", len(mts), err)
	}
	if err != nil || len(mts) == 0 {
		return
	}
	var masterTasks []dashboard.MasterTaskJSON
	subtaskMap := make(map[string][]dashboard.SubtaskJSON)
	for _, mt := range mts {
		tasks, _ := eng.DB.ListTasksByMasterTask(mt.ID)
		mj := dashboard.BuildMasterTaskJSON(mt, tasks, true)
		mj.WorkspacePath = workspacePath
		masterTasks = append(masterTasks, mj)
		subtaskMap[mt.ID] = dashboard.BuildSubtaskJSON(tasks)
	}
	client.SyncState(masterTasks, subtaskMap, filepath.Base(workspacePath))
}
