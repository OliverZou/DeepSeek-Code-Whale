package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/pod"
	teamlog "github.com/usewhale/whale/internal/team_engine/log"
	"github.com/usewhale/whale/internal/team_engine"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx      context.Context
	workDir  string
	engine   *team_engine.TeamEngine
	mu       sync.Mutex
	running  bool
}

func NewApp() *App {
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	return &App{workDir: exeDir}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	pod.Log("startup", "whale-pod workDir=%s", a.workDir)
	a.openEngine()
}

func (a *App) openEngine() {
	wbDir := filepath.Join(a.workDir, ".whale", "team_tasks")
	teamlogDir := filepath.Join(a.workDir, ".whale", "team_tasks", "logs")
	os.MkdirAll(teamlogDir, 0755)
	if tl := teamlog.NewTeamLog(a.workDir); tl != nil {
		tl.AddLog(filepath.Join(teamlogDir, "team_engine.log"))
		team_engine.SetLogger(tl)
	}
	spawner := team_engine.NewShellSubagentSpawner()
	eng, err := team_engine.New(wbDir, wbDir, "", spawner)
	if err != nil {
		pod.Log("startup", "open engine: %v", err)
		return
	}
	a.mu.Lock()
	if a.engine != nil { a.engine.Close() }
	a.engine = eng
	a.mu.Unlock()

	// Forward events to frontend.
	eng.OnEvent(func(evt team_engine.TaskEvent) {
		runtime.EventsEmit(a.ctx, "task-event", pod.TaskEvent{
			Type:     pod.TaskEventType(evt.Type),
			TaskID:   evt.TaskID,
			Title:    evt.Title,
			Progress: evt.Progress,
			NewState: evt.NewState,
		})
	})
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	if a.engine != nil { a.engine.Close() }
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Wails-callable: workspace
// ---------------------------------------------------------------------------

// SetWorkDir sets the working directory and re-opens the engine.
func (a *App) SetWorkDir(path string) string {
	path = filepath.Clean(path)
	if path == "" || path == "." { return "" }
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("目录不存在: %s", path)
	}
	a.workDir = path
	a.openEngine()
	pod.Log("workdir", "switched to %s", path)
	return ""
}

// GetWorkDir returns the current working directory.
func (a *App) GetWorkDir() string { return a.workDir }

// ListTeams returns available team names from workDir + exeDir.
func (a *App) ListTeams() []string {
	seen := map[string]bool{}
	var teams []string
	for _, dir := range []string{
		filepath.Join(a.workDir, ".whale", "teams"),
	} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && !seen[e.Name()] {
				seen[e.Name()] = true
				teams = append(teams, e.Name())
			}
		}
	}
	return teams
}

// ---------------------------------------------------------------------------
// Wails-callable: tasks
// ---------------------------------------------------------------------------

// StartTask creates a master task and runs PlanAndRun.
func (a *App) StartTask(goal, teamName string) string {
	if strings.TrimSpace(goal) == "" { return "goal 不能为空" }

	a.mu.Lock()
	if a.running { a.mu.Unlock(); return "已有任务正在执行" }
	a.running = true
	a.mu.Unlock()

	go func() {
		defer func() { a.mu.Lock(); a.running = false; a.mu.Unlock() }()

		a.mu.Lock()
		eng := a.engine
		a.mu.Unlock()
		if eng == nil { a.openEngine(); a.mu.Lock(); eng = a.engine; a.mu.Unlock() }
		if eng == nil { return }

		// Load team config if specified.
		if teamName != "" {
			tc, err := team_engine.FindTeam(filepath.Join(a.workDir, ".whale", "teams"), teamName)
			if err == nil { eng.SetTeam(tc) }
		}

		mt, err := eng.CreateMasterTask(goal, a.workDir)
		if err != nil { pod.Log("task", "create master: %v", err); return }

		ctx := context.Background()
		batches, err := eng.PlanAndRun(ctx, goal, a.workDir, mt.ID)
		pod.Log("task", "done: batches=%d err=%v", len(batches), err)
		eng.CompleteMasterTask(mt.ID)
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}()

	return ""
}

// GetMasterTasks returns master tasks from the working directory.
func (a *App) GetMasterTasks() []pod.MasterTaskJSON {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil { return nil }

	mts, _ := eng.Store.ListMasterTasks()
	var result []pod.MasterTaskJSON
	for _, mt := range mts {
		tasks, _ := eng.Store.ListTasksByMasterTask(mt.ID)
		doneCount := 0
		for _, t := range tasks {
			if t.State == team_engine.TaskStateDone || t.State == team_engine.TaskStateFailed { doneCount++ }
		}
		status := "running"
		if len(tasks) > 0 && doneCount >= len(tasks) { status = "done" }
		result = append(result, pod.MasterTaskJSON{
			ID: mt.ID, Goal: mt.Goal,
			WorkspacePath: a.workDir, WorkspaceLabel: filepath.Base(a.workDir),
			Status: status, CreatedAt: mt.CreatedAt,
			TaskCount: len(tasks), DoneCount: doneCount,
			WorkspaceOnline: true,
		})
	}
	return result
}

// GetSubtasks returns subtasks for a master task.
func (a *App) GetSubtasks(mtID string) []pod.SubtaskJSON {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil { return nil }

	tasks, _ := eng.Store.ListTasksByMasterTask(mtID)
	if len(tasks) == 0 { return nil }

	nodeMap := make(map[string]*pod.SubtaskJSON)
	taskList := make([]*pod.SubtaskJSON, 0, len(tasks))
	for _, t := range tasks {
		sj := &pod.SubtaskJSON{
			ID: t.ID, Title: t.Title, Description: t.Description,
			Output: t.Output, Role: string(t.Role), State: string(t.State),
			Progress: pod.GetProgress(t.State), CreatedAt: t.CreatedAt,
			ParentIDs: t.ParentIDs, BatchID: t.BatchID,
			RetryCount: t.RetryCount, MaxRetries: t.MaxRetries,
		}
		nodeMap[t.ID] = sj; taskList = append(taskList, sj)
	}
	for _, child := range taskList {
		for _, pid := range child.ParentIDs {
			if parent, ok := nodeMap[pid]; ok { parent.Children = append(parent.Children, *child) }
		}
	}
	for _, sj := range taskList {
		if len(sj.Children) > 0 {
			sum := 0; for _, c := range sj.Children { sum += c.Progress }
			sj.Progress = sum / len(sj.Children)
		}
	}
	var roots []pod.SubtaskJSON
	for _, sj := range taskList {
		hasParent := false
		for _, pid := range sj.ParentIDs {
			if _, ok := nodeMap[pid]; ok { hasParent = true; break }
		}
		if !hasParent { roots = append(roots, *sj) }
	}
	if len(tasks) > 0 {
		leader := pod.SubtaskJSON{ID: "__leader__", Title: "📋 任务规划", Role: "teamleader", State: "done", Progress: 100}
		return append([]pod.SubtaskJSON{leader}, roots...)
	}
	return roots
}

// GetAgentDialogue returns worker/verifier round logs.
func (a *App) GetAgentDialogue(taskID string) []pod.AgentDialogueJSON {
	return readDialogue(a.workDir, taskID)
}

// GetLeaderPlan returns leader decompose/review logs.
func (a *App) GetLeaderPlan() []pod.AgentDialogueJSON { return readLeaderPlan(a.workDir) }

// SendFeedback writes a human message to the task's messages/ directory.
func (a *App) SendFeedback(taskID, message string) string {
	msgDir := filepath.Join(a.workDir, ".whale", "team_tasks", taskID, "messages")
	os.MkdirAll(msgDir, 0755)
	msgFile := filepath.Join(msgDir, fmt.Sprintf("%d.md", time.Now().UnixNano()))
	os.WriteFile(msgFile, []byte(message), 0644)
	return ""
}

// RunSubtask resets a task to pending for re-execution.
func (a *App) RunSubtask(taskID string) string { updateMetaState(a.workDir, taskID, "pending"); return "" }

// CancelSubtask suspends a task.
func (a *App) CancelSubtask(taskID string) string { updateMetaState(a.workDir, taskID, "suspended"); return "" }

// OpenTerminal launches whale TUI in the working directory.
func (a *App) OpenTerminal() string { pod.OpenTerminal(a.workDir); return "" }

// WindowMinimize minimizes the window.
func (a *App) WindowMinimize() { runtime.WindowMinimise(a.ctx) }

// WindowMaximize toggles maximized state.
func (a *App) WindowMaximize() { runtime.WindowToggleMaximise(a.ctx) }

// WindowClose closes the window.
func (a *App) WindowClose() { runtime.Quit(a.ctx) }

// GetChatMessages returns human-agent chat history.
func (a *App) GetChatMessages(taskID string) []pod.ChatMessageJSON {
	return readChat(a.workDir, taskID)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func readDialogue(workDir, taskID string) []pod.AgentDialogueJSON {
	tasksDir := filepath.Join(workDir, ".whale", "team_tasks")
	taskDir := filepath.Join(tasksDir, taskID)
	logsDir := filepath.Join(tasksDir, "logs", "tasks", taskID)

	roleName := "worker"
	if data, err := os.ReadFile(filepath.Join(taskDir, "meta.json")); err == nil {
		var meta struct{ Role string `json:"role"` }
		if json.Unmarshal(data, &meta) == nil && meta.Role != "" { roleName = meta.Role }
	}

	var dialogue []pod.AgentDialogueJSON
	if input, err := os.ReadFile(filepath.Join(taskDir, "input.md")); err == nil && len(input) > 0 {
		dialogue = append(dialogue, pod.AgentDialogueJSON{Role: "input", Content: string(input)})
	}
	for round := 1; round <= 99; round++ {
		wc, _ := os.ReadFile(filepath.Join(logsDir, fmt.Sprintf("worker_%03d.md", round)))
		vc, _ := os.ReadFile(filepath.Join(logsDir, fmt.Sprintf("verifier_%03d.md", round)))
		if len(wc) > 0 { dialogue = append(dialogue, pod.AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", roleName, round), Content: string(wc)}) }
		if len(vc) > 0 { dialogue = append(dialogue, pod.AgentDialogueJSON{Role: fmt.Sprintf("审查 (round %d)", round), Content: string(vc)}) }
		if len(wc) == 0 && len(vc) == 0 { break }
	}
	return dialogue
}

func readLeaderPlan(workDir string) []pod.AgentDialogueJSON {
	leaderDir := filepath.Join(workDir, ".whale", "team_tasks", "logs", "leader")
	var dialogue []pod.AgentDialogueJSON
	for round := 1; round <= 9; round++ {
		for _, prefix := range []string{"decompose", "review"} {
			data, _ := os.ReadFile(filepath.Join(leaderDir, fmt.Sprintf("%s_%03d.md", prefix, round)))
			if len(data) > 0 {
				label := "📋 目标分解"; if prefix == "review" { label = "📋 执行审查" }
				dialogue = append(dialogue, pod.AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", label, round), Content: string(data)})
			}
		}
	}
	return dialogue
}

func readChat(workDir, taskID string) []pod.ChatMessageJSON {
	msgDir := filepath.Join(workDir, ".whale", "team_tasks", taskID, "messages")
	entries, _ := os.ReadDir(msgDir)
	var msgs []pod.ChatMessageJSON
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") { continue }
		content, _ := os.ReadFile(filepath.Join(msgDir, e.Name()))
		from := "human"; if strings.Contains(e.Name(), "_agent") { from = "agent" }
		msgs = append(msgs, pod.ChatMessageJSON{Time: strings.TrimSuffix(strings.Split(e.Name(), "_")[0], ".md"), From: from, Content: string(content)})
	}
	return msgs
}

func updateMetaState(workDir, taskID, state string) {
	metaPath := filepath.Join(workDir, ".whale", "team_tasks", taskID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil { return }
	var meta map[string]interface{}
	if json.Unmarshal(data, &meta) != nil { return }
	meta["state"] = state
	newData, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, newData, 0644)
}
