package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/team_engine"
	teampglog "github.com/usewhale/whale/internal/team_engine/log"
)

// TeamEngineTools returns the team engine tools (team_execute/team_run/
// team_status/…). The subagent tool registries must carry them so a Leader
// child agent whose definition selects team_run/team_status/… by name actually
// receives the tools (addToolSelectors errors on unknown names) and can drive
// the team state machine from its session.
func (b *Toolset) TeamEngineTools() []core.Tool {
	return b.teamEngineTools()
}

func (b *Toolset) teamEngineTools() []core.Tool {
	return []core.Tool{
		b.teamExecuteTool(),
		b.teamComposeTool(),
		b.teamCreateTool(),
		b.teamRunTool(),
		b.teamRunPlanTool(),
		b.teamStatusTool(),
		b.teamListTool(),
		b.teamRosterTool(),
		b.teamFeedbackTool(),
		b.teamHistoryTool(),
		b.teamExportTool(),
		b.teamResultTool(),
		b.teamOutputTool(),
		b.teamDeleteTool(),
		b.teamPromptTool(),
		b.teamSpawnTool(),
		b.teamAbortTool(),
		b.teamKillTool(),
		b.teamSummarizeTool(),
		b.teamForkTool(),
		b.agentDefineTool(),
		b.teamDefineTool(),
	}
}

func (b *Toolset) teamEnginePaths() (dbPath, wbDir string) {
	return filepath.Join(b.root, ".whale", "team_engine.db"),
		filepath.Join(b.root, ".whale", "team_tasks")
}

func (b *Toolset) newTeamEngine() (*team_engine.TeamEngine, error) {
	dbPath, wbDir := b.teamEnginePaths()
	// Prefer the native subagent adapter wired by the app runtime via
	// SetDefaultSpawnFunc; fall back to the shell spawner in standalone
	// contexts that have no app runtime.
	var spawner team_engine.SubagentSpawner = team_engine.NewShellSubagentSpawner()
	if fn := team_engine.DefaultSpawnFunc(); fn != nil {
		spawner = team_engine.NewFuncSpawner(fn)
		team_engine.LogSpawnerType("default", "adapter", "", 0)
	} else {
		team_engine.LogSpawnerType("default", "shell", "", 0)
	}
	eng, err := team_engine.New(dbPath, wbDir, "", spawner)
	if err != nil {
		return nil, err
	}
	// Tool-created engines execute team_run/team_run_plan asynchronously; without
	// a logger the engine logs silently vanish (v45: team_run re-dispatch turned
	// black-box — no [task]/[worker] lines while it ran).
	// 日志归属 .whale/team_tasks/logs/（baseDir=team_tasks）——用 b.root 会把
	// logs/ 与 leader_*.md 写到工作区根目录（v46 污染用户工作区）。
	if lg, lerr := teampglog.New(filepath.Join(b.root, ".whale", "team_tasks")); lerr == nil {
		eng.Loggers = lg
	}
	// Inject the app-layer SessionOps so the six primitives (prompt/spawn/
	// abort/kill/summarize/fork) can address member sessions.
	if ops := team_engine.DefaultSessionOps(); ops != nil {
		eng.SetSessionOps(ops)
	}
	return eng, nil
}

func toolResult(text string) core.ToolResult {
	return core.ToolResult{ModelText: text}
}

func toolError(format string, args ...interface{}) core.ToolResult {
	return core.ToolResult{ModelText: fmt.Sprintf(format, args...), Outcome: core.OutcomeFailure}
}

// --- team_execute ---

// AutoExecuteMasterTask resumes a previously suspended master task.
// a resume command via heartbeat.  It loads the master task and executes
// it directly without waiting for a user-initiated team_execute call.
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

		batches, err := eng.ResumeMasterTask(ctx, masterTaskID, mt.Goal, workdir)
		if eng.Loggers != nil {
			eng.Loggers.Engine("dashboard-resume: masterTask=%s goal=%q batches=%d err=%v",
				masterTaskID, mt.Goal, len(batches), err)
		}
	}()
}

// RunSingleTask runs a single subtask independently (per-subtask Run button).
// RunSingleTask executes a single task by ID.
func (b *Toolset) RunSingleTask(taskID string) {
	go func() {
		eng, err := b.newTeamEngine()
		if err != nil {
			logToFile(filepath.Join(b.root, ".whale", "team_tasks", "logs", "engine.log"),
				"run-single-task: newTeamEngine failed: %v", err)
			return
		}
		defer eng.Close()

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

		ok, err := eng.RunTask(ctx, taskID)
		if eng.Loggers != nil {
			eng.Loggers.Engine("run-single-task: taskID=%s ok=%v err=%v", taskID, ok, err)
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

// --- team_execute ---

func (b *Toolset) teamExecuteTool() toolFn {
	return toolFn{
		name:        "team_execute",
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

	// Load team configuration if specified.
	if args.Team != "" {
		teamRoots := team_engine.DefaultTeamRoots(b.root)
		tc, err := team_engine.FindTeamInRoots(teamRoots, args.Team)
		if err != nil {
			return toolError("load team %q: %v", args.Team, err), nil
		}
		eng.SetTeam(tc)
	}

	// Register progress callback if we have one.
	// Throttled to at most 1 update per 500ms to avoid TUI flickering.
	if progress != nil {
		var lastProgress time.Time
		eng.OnEvent(func(event team_engine.TaskEvent) {
			defer func() {
				if r := recover(); r != nil && true {
					team_engine.Log("sync", "event callback panic: %v", r)
				}
			}()
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

	// --- Phase 1: Create master task immediately ---
	var masterTask *team_engine.MasterTask
	var mtErr error
	existing, _ := eng.Store.ListMasterTasks()
	for _, mt := range existing {
		if mt.Goal == args.Goal {
			masterTask = mt
			break
		}
	}
	// team_execute is scoped to this goal — never DeleteMasterTask on other
	// masters: they may belong to other teams/runs. Reuse the goal match only.
	if masterTask == nil {
		masterTask, mtErr = eng.CreateMasterTask(args.Goal, b.root, "")
		if mtErr != nil {
			return toolError("create master task: %v", mtErr), nil
		}
	}
	if mtErr != nil {
		return toolError("create master task: %v", mtErr), nil
	}

	// Async mode: return immediately so the agent can review later.
	// The master task already exists.
	if args.Async {
		return core.ToolResult{
			ModelText: fmt.Sprintf("📋 Master task created: `%s`\nRun `team_execute goal=\"...\"` (without async) to execute.", masterTask.ID),
			Metadata:  map[string]any{"master_task_id": masterTask.ID, "mode": "async"},
		}, nil
	}

	// Synchronous mode: TeamCycle handles decompose + execution in one step.
	var batches []*team_engine.Batch
	existingTasks, _ := eng.Store.ListTasksByMasterTask(masterTask.ID)
	if len(existingTasks) > 0 {
		batches, err = eng.ResumeMasterTask(ctx, masterTask.ID, args.Goal, b.root)
	} else {
		batches, err = eng.TeamCycle(ctx, args.Goal, b.root, masterTask.ID)
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
		Title   string `json:"title"`
		Role    string `json:"role"`
		State   string `json:"state"`
		Workdir string `json:"workdir"`
		Output  string `json:"output,omitempty"`
		Files   string `json:"files,omitempty"`
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
	return core.ToolResult{ModelText: fullResult, Metadata: metadata}, nil
}

// --- team_compose ---

// teamComposeTool decomposes a goal into subtasks and batches, writing the plan
// (plan.md / plan.json) but does NOT execute anything. Use this when you want
// to review the decomposition before kicking off team_execute or manually
// running individual tasks. Equivalent to the deprecated team_execute async=true.
func (b *Toolset) teamComposeTool() toolFn {
	return toolFn{
		name:        "team_compose",
		description: "Decompose a goal into subtasks and batches, write plan.md/plan.json, but do NOT execute. Use when you want to review the decomposition before running, or when you need the plan for debugging/documentation.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"goal":    map[string]any{"type": "string", "description": "The goal to decompose"},
				"workdir": map[string]any{"type": "string", "description": "Working directory for subtasks (default: workspace root)"},
				"team":    map[string]any{"type": "string", "description": "Team name from .whale/teams/{team}.yaml (optional)"},
			},
			"required": []string{"goal"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			return b.runTeamPlan(ctx, call, nil)
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
			var args struct {
				TaskID string `json:"task_id"`
			}
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
			if task != nil {
				eng.AppendReviewAction(task.MasterTaskID, "team_run", args.TaskID, ok, "leader re-dispatch")
			}
			mark := tick(ok)
			return toolResult(fmt.Sprintf("%s %s %s -> %s", mark, args.TaskID, task.Title, task.State)), nil
		},
	}
}

// teamRunPlanTool hands the master's plan to the TeamEngine for execution.
// The TE owns scheduling (parallel-ready batches run concurrently, dependencies
// run serially, every task runs produce→verify→mechanical-gate→propagate);
// the Leader polls team_list/team_status to observe. This is the "TE executes,
// Leader waits asynchronously" primitive — do NOT drive tasks one by one.
func (b *Toolset) teamRunPlanTool() toolFn {
	return toolFn{
		name:        "team_run_plan",
		description: "Submit the master's decomposed plan to the TeamEngine for execution. The TE owns scheduling: parallel-ready batches run concurrently, dependencies run serially, every task runs produce -> verify -> mechanical gate -> propagate. Returns an execution ticket immediately (do not wait on it); observe progress with team_list/team_status. Use this INSTEAD of calling team_run for each task.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"master_task_id": map[string]any{"type": "string", "description": "Master task whose plan/plan.json to execute"},
			},
			"required": []string{"master_task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				MasterTaskID string `json:"master_task_id"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			// v46 异步化：StartPlanRun 后台完成「分解（如缺 plan.json）→ 执行」
			// 全流程并立即返回票据；完成/失败通过引擎事件通知（进展桥注入 leader
			// 叙述），leader turn 不再被分解同步阻塞。
			msg, err := eng.StartPlanRun(ctx, args.MasterTaskID)
			if err != nil {
				eng.Close()
				return toolError("start plan run: %v", err), nil
			}
			// Success path: the run owns the engine's lifecycle (closed when it
			// finishes) — never Close here.
			return toolResult(msg), nil
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
			var args struct {
				TaskID string `json:"task_id"`
			}
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
				if mt, _ := eng.GetMasterTask(args.TaskID); mt != nil {
					return toolResult(fmt.Sprintf("%q 是 master（不是子任务）。请用 team_run_plan(master_task_id=%q) 提交计划，用 team_list 观察进度。", args.TaskID, args.TaskID)), nil
				}
				return toolError("task %q not found（team_status 只接受子任务 ID，来自 team_list 输出；不要传 master_task_id）", args.TaskID), nil
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
				masters, _ := eng.ListMasterTasks()
				if len(masters) > 0 {
					var lines []string
					for _, m := range masters {
						// v48 计划生成期提示：StartPlanRun 已把 run 注册进 planRuns（进程内）；
						// 此时无子任务是正常的（后台分解中），明确告诉调用者「正在生成」，
						// 避免误导其再次提交 plan（team_run_plan 被幂等拒绝）。
						if v, ok := team_engine.PlanRunStatus(m.ID); ok && v.Running {
							lines = append(lines, fmt.Sprintf("%s：计划已提交，正在后台生成任务（无需重复提交；完成后我会汇报）", m.ID))
						} else {
							lines = append(lines, fmt.Sprintf("%s（尚无子任务）", m.ID))
						}
					}
					if len(lines) > 0 {
						return toolResult("已创建 master(s)：" + strings.Join(lines, "；")), nil
					}
					return toolResult("团队引擎还没有任何任务。请先通过 /team 创建 master 或提交计划。"), nil
				}
				return toolResult("团队引擎还没有任何任务。请先通过 /team 创建 master 或提交计划。"), nil
			}
			var s string
			for _, t := range tasks {
				s += fmt.Sprintf("%s %s [%s] %d%% %s\n", tick(t.State == team_engine.TaskStateDone), t.ID, t.State, team_engine.GetProgress(t.State), t.Title)
			}
			return toolResult(s), nil
		},
	}
}

// --- team_roster ---

// teamRosterTool lets whale/expert discover which teams and experts EXIST
// (distinct from team_list, which lists running tasks). Use before team_execute
// to pick a matching team to delegate to.
func (b *Toolset) teamRosterTool() toolFn {
	return toolFn{
		name:        "team_roster",
		description: "List available expert TEAMS and single EXPERTS (from workspace .whale/teams and ~/.whale/{teams,experts}). Use this BEFORE team_execute to find a matching team to delegate to. Returns team names (pass as team_execute's `team` param), labels, capabilities, roles; and expert names/domains. NOTE: distinct from team_list, which lists running tasks.",
		readOnly:    true,
		parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var sb strings.Builder

			// Teams — walk roots so we keep the folder/file name (team_execute needs it).
			sb.WriteString("## 可用团队（team_execute 传 team=<name>）\n")
			roots := team_engine.DefaultTeamRoots(b.root)
			seen := map[string]bool{}
			teamCount := 0
			for i := len(roots) - 1; i >= 0; i-- {
				entries, err := os.ReadDir(roots[i])
				if err != nil {
					continue
				}
				for _, e := range entries {
					var name, cfgPath string
					if e.IsDir() {
						name = e.Name()
						cfgPath = filepath.Join(roots[i], name, "team.yaml")
					} else if strings.HasSuffix(e.Name(), ".yaml") {
						name = strings.TrimSuffix(e.Name(), ".yaml")
						cfgPath = filepath.Join(roots[i], e.Name())
					} else {
						continue
					}
					if seen[name] {
						continue
					}
					tc, err := team_engine.LoadTeamConfig(cfgPath)
					if err != nil || tc == nil {
						continue
					}
					seen[name] = true
					teamCount++
					label := tc.Label
					if label == "" {
						label = name
					}
					sb.WriteString(fmt.Sprintf("- **%s** — %s", name, label))
					if len(tc.Capabilities) > 0 {
						sb.WriteString(" | 能力: " + strings.Join(tc.Capabilities, "、"))
					}
					if len(tc.Roles) > 0 {
						sb.WriteString(" | 角色: " + strings.Join(tc.Roles, "、"))
					}
					sb.WriteString("\n")
				}
			}
			if teamCount == 0 {
				sb.WriteString("（无现成团队）\n")
			}

			// Experts.
			sb.WriteString("\n## 可用专家\n")
			expCount := 0
			if home, err := os.UserHomeDir(); err == nil {
				if reg, err := team_engine.LoadAllExperts(filepath.Join(home, ".whale", "experts")); err == nil {
					for _, exp := range reg.AllExperts() {
						expCount++
						sb.WriteString(fmt.Sprintf("- **%s**", exp.Name))
						if exp.Agent != "" {
							sb.WriteString(fmt.Sprintf(" (%s)", exp.Agent))
						}
						if exp.Description != "" {
							sb.WriteString(" — " + exp.Description)
						}
						if len(exp.Domains) > 0 {
							sb.WriteString(" | 领域: " + strings.Join(exp.Domains, "、"))
						}
						sb.WriteString("\n")
					}
				}
			}
			if expCount == 0 {
				sb.WriteString("（无专家）\n")
			}

			return toolResult(sb.String()), nil
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
			if task, _ := eng.GetTask(args.TaskID); task != nil {
				eng.AppendReviewAction(task.MasterTaskID, "team_feedback", args.TaskID, true, truncateNote(args.Message))
			}
			return toolResult("Feedback sent to " + args.TaskID), nil
		},
	}
}

// truncateNote caps a review note (feedback message) to keep the review
// trail readable; long verifier feedback belongs in the task dir, not here.
func truncateNote(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
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
			var args struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			entries, err := eng.Store.GetTaskHistory(args.TaskID)
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
			var args struct {
				TaskID string `json:"task_id"`
			}
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
			var args struct {
				TaskID string `json:"task_id"`
			}
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
				if mt, _ := eng.GetMasterTask(args.TaskID); mt != nil {
					return toolResult(fmt.Sprintf("%q 是 master（不是子任务）。请用 team_run_plan(master_task_id=%q) 提交计划，用 team_list 观察进度。", args.TaskID, args.TaskID)), nil
				}
				return toolError("task %q not found（team_status 只接受子任务 ID，来自 team_list 输出；不要传 master_task_id）", args.TaskID), nil
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

			// Output（完整输出,不截断 —— pod UI 展开需要看全文）
			if output, err := eng.Whiteboard.ReadOutput(args.TaskID); err == nil && output != "" {
				md += "### 工作产出\n\n" + output + "\n"
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
				"task_id":  task.ID,
				"title":    task.Title,
				"state":    string(task.State),
				"progress": team_engine.GetProgress(task.State),
				"workdir":  task.Workdir,
			}
			return core.ToolResult{ModelText: md, Metadata: metadata}, nil
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
				"master_task_id": map[string]any{"type": "string", "description": "Master task ID from team_execute result"},
			},
			"required": []string{"master_task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				MasterTaskID string `json:"master_task_id"`
			}
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
				ID       string   `json:"id"`
				Title    string   `json:"title"`
				Role     string   `json:"role"`
				State    string   `json:"state"`
				Progress int      `json:"progress"`
				Workdir  string   `json:"workdir"`
				Output   string   `json:"output,omitempty"`
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
			return core.ToolResult{ModelText: mdBuilder, Metadata: metadata}, nil
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
				masterTasks, err := eng.Store.ListMasterTasks()
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
				tasks, err := eng.Store.ListTasks()
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

// --- team_prompt ---

func (b *Toolset) teamPromptTool() toolFn {
	return toolFn{
		name:        "team_prompt",
		description: "Send a message to a team member's session and wait for its reply (prompt primitive). Works on running or already-finished members; appends a turn to the member's existing session.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Member task ID to prompt"},
				"message": map[string]any{"type": "string", "description": "Message to send"},
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

			reply, err := eng.Prompt(ctx, team_engine.PromptRequest{
				ToTaskID: args.TaskID,
				From:     "agent",
				Content:  args.Message,
				Sync:     true,
			})
			if err != nil {
				return toolError("prompt: %v", err), nil
			}
			if reply == nil {
				return toolResult("Message delivered (no reply)."), nil
			}
			return toolResult(reply.Content), nil
		},
	}
}

// --- team_spawn ---

func (b *Toolset) teamSpawnTool() toolFn {
	return toolFn{
		name:        "team_spawn",
		description: "Spawn a new team member task (spawn primitive).",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "Task title"},
				"description": map[string]any{"type": "string", "description": "Task prompt for the member"},
				"role":        map[string]any{"type": "string", "description": "Member role", "default": "worker"},
				"workdir":     map[string]any{"type": "string", "description": "Working directory"},
				"max_retries": map[string]any{"type": "integer", "description": "Max retries", "default": 3},
			},
			"required": []string{"title", "description"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Role        string `json:"role"`
				Workdir     string `json:"workdir"`
				MaxRetries  int    `json:"max_retries"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			if args.Role == "" {
				args.Role = "worker"
			}
			if args.MaxRetries <= 0 {
				args.MaxRetries = 3
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			task, err := eng.Spawn(ctx, team_engine.SpawnRequest{
				Title:       args.Title,
				Description: args.Description,
				Role:        team_engine.AgentRole(args.Role),
				MaxRetries:  args.MaxRetries,
				Workdir:     args.Workdir,
				From:        "agent",
			})
			if err != nil {
				return toolError("spawn: %v", err), nil
			}
			return toolResult(fmt.Sprintf("Spawned task %s: %s [%s]", task.ID, task.Title, task.State)), nil
		},
	}
}

// --- team_abort ---

func (b *Toolset) teamAbortTool() toolFn {
	return toolFn{
		name:        "team_abort",
		description: "Gracefully stop a running team member task (abort primitive).",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":        map[string]any{"type": "string", "description": "Task ID to abort（中止单个任务）"},
				"master_task_id": map[string]any{"type": "string", "description": "Master ID 传入时：立即停止整个 run（全部任务）——用户说\u201c停止/取消/别做了\u201d时用这个"},
			},
			"required": []string{"task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				TaskID       string `json:"task_id"`
				MasterTaskID string `json:"master_task_id"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			// master 级停止（用户喊停）：中止整个 run。
			if strings.TrimSpace(args.MasterTaskID) != "" {
				stopped := eng.StopRun(args.MasterTaskID)
				if !stopped {
					return toolResult("没有发现运行中的任务（可能已结束）——当前状态以 team_status/team_list 为准。"), nil
				}
				return toolResult("已停止全部任务（master " + args.MasterTaskID + "）。已执行产出保留在工作区。"), nil
			}

			if err := eng.Abort(ctx, args.TaskID); err != nil {
				return toolError("abort: %v", err), nil
			}
			return toolResult("Aborted " + args.TaskID), nil
		},
	}
}

// --- team_kill ---

func (b *Toolset) teamKillTool() toolFn {
	return toolFn{
		name:        "team_kill",
		description: "Forcefully terminate a team member task (kill primitive).",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Task ID to kill"},
			},
			"required": []string{"task_id"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			if err := eng.Kill(ctx, args.TaskID); err != nil {
				return toolError("kill: %v", err), nil
			}
			return toolResult("Killed " + args.TaskID), nil
		},
	}
}

// --- team_summarize ---

func (b *Toolset) teamSummarizeTool() toolFn {
	return toolFn{
		name:        "team_summarize",
		description: "Get a team member's last report/summary (summarize primitive).",
		readOnly:    true,
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":    map[string]any{"type": "string", "description": "Member task ID"},
				"session_id": map[string]any{"type": "string", "description": "Member session ID (alternative to task_id)"},
			},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				TaskID    string `json:"task_id"`
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			sessionID := args.SessionID
			if sessionID == "" {
				sessionID = eng.Store.SessionID(args.TaskID)
			}
			if sessionID == "" {
				return toolError("no member session for task %q", args.TaskID), nil
			}
			summary, err := eng.Summarize(ctx, sessionID)
			if err != nil {
				return toolError("summarize: %v", err), nil
			}
			if summary == "" {
				return toolResult("(no report yet for session " + sessionID + ")"), nil
			}
			return toolResult(summary), nil
		},
	}
}

// --- team_fork ---

func (b *Toolset) teamForkTool() toolFn {
	return toolFn{
		name:        "team_fork",
		description: "Clone a team member's session and return the new session ID (fork primitive). The clone continues from the same transcript under a different instruction.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":    map[string]any{"type": "string", "description": "Member task ID"},
				"session_id": map[string]any{"type": "string", "description": "Member session ID (alternative to task_id)"},
			},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var args struct {
				TaskID    string `json:"task_id"`
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			eng, err := b.newTeamEngine()
			if err != nil {
				return toolError("init: %v", err), nil
			}
			defer eng.Close()

			sessionID := args.SessionID
			if sessionID == "" {
				sessionID = eng.Store.SessionID(args.TaskID)
			}
			if sessionID == "" {
				return toolError("no member session for task %q", args.TaskID), nil
			}
			newID, err := eng.Fork(ctx, sessionID)
			if err != nil {
				return toolError("fork: %v", err), nil
			}
			return toolResult("Forked session " + sessionID + " -> " + newID), nil
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
