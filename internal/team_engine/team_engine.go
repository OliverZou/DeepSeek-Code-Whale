package team_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/usewhale/whale/internal/team_engine/log"
)

const defaultTimeout = 300 * time.Second

// isSubstantive performs a lightweight pre-check on agent output for content
// roles.  Returns false when the output is too short or consists mostly of
// intent phrases ("I will...", "准备...") without actual substance.
func isSubstantive(output string) bool {
	trimmed := strings.TrimSpace(output)

	// Too short.
	if len(trimmed) < 100 {
		return false
	}

	// Remove common "planning to do" patterns and re-check length.
	intentPatterns := []string{"I will", "I plan", "plan to", "准备", "计划", "将", "I'll"}
	cleaned := trimmed
	for _, p := range intentPatterns {
		cleaned = strings.ReplaceAll(cleaned, p, "")
	}
	if len(strings.TrimSpace(cleaned)) < 50 {
		return false
	}

	return true
}

// TeamEngine is the core state machine that orchestrates multi-agent task
// execution using Whale subagents.
//
// It drives tasks through the Leader-Worker-Verifier lifecycle:
//
//	pending → assigned → producing → produced → verifying → verified → done
//
// On verification failure the task loops back to producing (retry).  When
// the retry budget is exhausted the task transitions to failed.
type TeamEngine struct {
	DB         *TaskDB
	Whiteboard *Whiteboard
	Config     *Config
	Router     *Router
	Runner     *AgentRunner
	Escalation *EscalationManager
	Loggers    *log.Loggers
	timeout    time.Duration

	// Worktree integration for Coding Harness (场景2).
	worktreeEnabled bool
	worktreeDir     string // path to the repo for worktree creation
	activeTrees     map[string]string // taskID → branch name

	// activeCancels tracks cancel functions for running subagent spawns.
	activeCancels map[string]context.CancelFunc // taskID → cancel

	// shutdownCtx / shutdownCancel define the engine's lifecycle.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	// wg tracks goroutines spawned by RunBatch so Close() can drain them.
	wg sync.WaitGroup

	mu sync.Mutex

	// eventCallbacks — 订阅者列表，状态变化时主动推送
	eventCallbacks []TaskEventCallback

	// Current team configuration (optional).
	team *TeamConfig
}

// OnEvent 注册一个事件回调函数。
func (e *TeamEngine) OnEvent(cb TaskEventCallback) func() {
	e.mu.Lock()
	e.eventCallbacks = append(e.eventCallbacks, cb)
	idx := len(e.eventCallbacks) - 1
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.eventCallbacks = append(e.eventCallbacks[:idx], e.eventCallbacks[idx+1:]...)
		e.mu.Unlock()
	}
}

// SetTeam configures a team for the next PlanAndRun execution.
func (e *TeamEngine) SetTeam(tc *TeamConfig) {
	e.team = tc
}

// Team returns the current team configuration, or nil if none is set.
func (e *TeamEngine) Team() *TeamConfig {
	return e.team
}

// New creates a TeamEngine with the given dependencies.
//
// Parameters:
//   - dbPath:         Path to SQLite database (use ":memory:" for testing)
//   - whiteboardDir:  Directory for inter-agent file communication
//   - configPath:     Path to team_engine.yaml (empty = use defaults)
//   - spawner:        Whale SubagentSpawner implementation
func New(dbPath, whiteboardDir, configPath string, spawner SubagentSpawner) (*TeamEngine, error) {
	database, err := NewDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("init db: %w", err)
	}

	wb, err := NewWhiteboard(whiteboardDir)
	if err != nil {
		database.Close()
		return nil, fmt.Errorf("init whiteboard: %w", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		database.Close()
		return nil, fmt.Errorf("load config: %w", err)
	}

	runner := NewRunner(spawner)
	loggers, err := log.New(whiteboardDir)
	if err != nil {
		database.Close()
		return nil, fmt.Errorf("init loggers: %w", err)
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	eng := &TeamEngine{
		DB:         database,
		Whiteboard: wb,
		Config:     cfg,
		Router:     NewRouter(cfg),
		Runner:     runner,
		Escalation: NewEscalationManager(),
		Loggers:    loggers,
		timeout:    defaultTimeout,
		activeTrees:    make(map[string]string),
		activeCancels:  make(map[string]context.CancelFunc),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}

	// Clean up tasks interrupted by a previous crash/exit — only when
	// we have a spawner (i.e. this engine can actually execute tasks).
	// Read-only engines (dashboard) must not modify task state.
	if spawner != nil {
		eng.cleanupInterruptedTasks()
	}

	return eng, nil
}

// Close releases resources held by the engine.
func (e *TeamEngine) Close() error {
	e.shutdownCancel()

	e.mu.Lock()
	for taskID, cancel := range e.activeCancels {
		cancel()
		delete(e.activeCancels, taskID)
	}
	activeTrees := make(map[string]string, len(e.activeTrees))
	for k, v := range e.activeTrees {
		activeTrees[k] = v
	}
	e.mu.Unlock()

	e.wg.Wait()

	for taskID := range activeTrees {
		e.cleanupWorktree(taskID)
	}

	if e.Loggers != nil {
		_ = e.Loggers.Close()
	}

	return e.DB.Close()
}

// CleanupInterruptedTasks opens the database at dbPath, resets tasks
// in transient states (producing, verifying, assigned) to suspended,
// and clears stale "running" status on master tasks.  Safe to call
// during startup or when a whale process reconnects.
func CleanupInterruptedTasks(dbPath string) {
	tdb, err := NewDB(dbPath)
	if err != nil {
		return
	}
	defer tdb.Close()
	tasks, err := tdb.ListTasks()
	if err != nil {
		return
	}
	for _, t := range tasks {
		switch t.State {
		case TaskStateProducing, TaskStateVerifying, TaskStateAssigned:
			_ = tdb.ForceTransitionState(t.ID, TaskStateSuspended, "interrupted-restart")
			if DefaultTeamLog != nil {
				DefaultTeamLog.EngineResumeTask(t.ID, string(TaskStateSuspended))
			}
		}
	}
	mts, _ := tdb.ListMasterTasks()
	for _, mt := range mts {
		if mt.Status == "running" {
			_ = tdb.UpdateMasterTaskStatus(mt.ID, "")
		}
	}
}

// cleanupInterruptedTasks resets tasks in transient states (producing,
// verifying, assigned) to suspended.  Called on engine open to handle
// the case where a previous whale process was killed mid-execution.
func (e *TeamEngine) cleanupInterruptedTasks() {
	tasks, err := e.DB.ListTasks()
	if err != nil {
		return
	}
	for _, t := range tasks {
		switch t.State {
		case TaskStateProducing, TaskStateVerifying, TaskStateAssigned:
			_ = e.DB.ForceTransitionState(t.ID, TaskStateSuspended, "interrupted-restart")
			if DefaultTeamLog != nil {
				DefaultTeamLog.EngineResumeTask(t.ID, string(TaskStateSuspended))
			}
		}
	}
	// Also reset any master task status from "running" so the dashboard
	// doesn't show stale active states.
	mts, _ := e.DB.ListMasterTasks()
	for _, mt := range mts {
		if mt.Status == "running" {
			_ = e.DB.UpdateMasterTaskStatus(mt.ID, "")
		}
	}
}

// FireEvent broadcasts an event to all subscribers.
func (e *TeamEngine) FireEvent(event TaskEvent) {
	e.fireEvent(event)
}

// fireEvent 向所有订阅者广播事件。
func (e *TeamEngine) fireEvent(event TaskEvent) {
	e.mu.Lock()
	cbs := make([]TaskEventCallback, len(e.eventCallbacks))
	copy(cbs, e.eventCallbacks)
	e.mu.Unlock()
	for _, cb := range cbs {
		func() {
			defer func() { recover() }()
			cb(event)
		}()
	}
}

// fireStateEvent 是状态转换的便捷触发方法。
func (e *TeamEngine) fireStateEvent(taskID, title, oldState, newState string) {
	e.fireEvent(TaskEvent{
		Type:     EventStateChanged,
		TaskID:   taskID,
		Title:    title,
		OldState: oldState,
		NewState: newState,
		Progress: GetProgress(TaskState(newState)),
	})
}

// EnableWorktree activates git worktree-based isolation for coding tasks.
// repoPath should be the root of the git repository.
func (e *TeamEngine) EnableWorktree(repoPath string) {
	e.worktreeEnabled = true
	e.worktreeDir = repoPath
}

// createWorktree creates a git worktree for a coding task.
// Returns the worktree path and branch name.
func (e *TeamEngine) createWorktree(taskID string) (string, string, error) {
	if !e.worktreeEnabled || e.worktreeDir == "" {
		return "", "", fmt.Errorf("worktree not enabled")
	}
	shortID := taskID[:8]
	branchName := "team-" + shortID
	treePath := filepathJoin(e.worktreeDir, ".whale", "worktrees", branchName)

	// Use `git worktree add` directly.
	cmd := exec.Command("git", "worktree", "add", treePath, "-b", branchName)
	cmd.Dir = e.worktreeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Try without -b (branch may already exist).
		cmd2 := exec.Command("git", "worktree", "add", treePath, branchName)
		cmd2.Dir = e.worktreeDir
		out, err = cmd2.CombinedOutput()
		if err != nil {
			return "", "", fmt.Errorf("git worktree add: %s: %w", string(out), err)
		}
	}

	e.activeTrees[taskID] = branchName
	return treePath, branchName, nil
}

// collectWorktreeDiff generates a git diff for a coding task's worktree.
func (e *TeamEngine) collectWorktreeDiff(taskID string) string {
	e.mu.Lock()
	branch, ok := e.activeTrees[taskID]
	delete(e.activeTrees, taskID)
	e.mu.Unlock()

	if !ok || branch == "" || e.worktreeDir == "" {
		return ""
	}

	// git diff of the worktree branch vs its base.
	cmd := exec.Command("git", "diff", branch+"^.."+branch)
	cmd.Dir = e.worktreeDir
	out, _ := cmd.Output()
	diff := string(out)

	if strings.TrimSpace(diff) == "" {
		return "(no diff — no changes detected)"
	}
	return diff
}

// cleanupWorktree removes a worktree directory and branch.
func (e *TeamEngine) cleanupWorktree(taskID string) {
	e.mu.Lock()
	branch, ok := e.activeTrees[taskID]
	delete(e.activeTrees, taskID)
	e.mu.Unlock()

	if !ok || branch == "" || e.worktreeDir == "" {
		return
	}

	treePath := filepathJoin(e.worktreeDir, ".whale", "worktrees", branch)
	// Remove the worktree.
	cmd := exec.Command("git", "worktree", "remove", treePath, "--force")
	cmd.Dir = e.worktreeDir
	cmd.Run()
	// Remove the branch.
	exec.Command("git", "branch", "-D", branch).Dir = e.worktreeDir
}

// isCodingRole returns true if the role modifies code and benefits from worktree isolation.
func isCodingRole(role AgentRole) bool {
	switch role {
	case RoleDeveloper, RoleTester:
		return true
	default:
		return false
	}
}

// filepathJoin is a local helper to avoid importing path/filepath in this file.
func filepathJoin(elem ...string) string {
	return strings.Join(elem, string(os.PathSeparator))
}

// ---------------------------------------------------------------------------
// Task creation & lifecycle
// ---------------------------------------------------------------------------

// CreateMasterTask creates a new master task record.
func (e *TeamEngine) CreateMasterTask(goal, workspacePath string) (*MasterTask, error) {
	mt := &MasterTask{
		ID:            uuid.New().String(),
		Goal:          goal,
		WorkspacePath: workspacePath,
		Status:        "running",
	}
	if err := e.DB.InsertMasterTask(mt); err != nil {
		return nil, fmt.Errorf("insert master task: %w", err)
	}
	return mt, nil
}

// ListMasterTasks returns all master tasks.
func (e *TeamEngine) ListMasterTasks() ([]*MasterTask, error) {
	return e.DB.ListMasterTasks()
}

// ListTasksByMasterTask returns subtasks for a master task.
func (e *TeamEngine) ListTasksByMasterTask(masterTaskID string) ([]*Task, error) {
	return e.DB.ListTasksByMasterTask(masterTaskID)
}

// GetMasterTask retrieves a master task by ID.
func (e *TeamEngine) GetMasterTask(id string) (*MasterTask, error) {
	return e.DB.GetMasterTask(id)
}

// CompleteMasterTask marks a master task as done.
func (e *TeamEngine) CompleteMasterTask(id string) error {
	return e.DB.UpdateMasterTaskStatus(id, "done")
}

// ListSuspendedMasterTasks returns master tasks with suspended subtasks.
func (e *TeamEngine) ListSuspendedMasterTasks() ([]*MasterTask, error) {
	all, err := e.DB.ListMasterTasks()
	if err != nil {
		return nil, err
	}
	var result []*MasterTask
	for _, mt := range all {
		tasks, err := e.DB.ListTasksByMasterTask(mt.ID)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.State == TaskStateSuspended {
				result = append(result, mt)
				break
			}
		}
	}
	return result, nil
}

// saveCheckpoint persists PlanAndRun progress for later Resume.
func (e *TeamEngine) saveCheckpoint(masterTaskID string, completed map[string]bool, outputs map[string]string, batches []*Batch) {
	type checkpoint struct {
		CompletedBatches []string            `json:"completed_batches"`
		BatchCycles      map[string]int      `json:"batch_cycles"`
		CompletedOutputs map[string]string   `json:"completed_outputs,omitempty"`
		BatchDependsOn   map[string][]string `json:"batch_depends_on,omitempty"`
	}
	deps := make(map[string][]string)
	for _, b := range batches {
		if len(b.DependsOn) > 0 {
			deps[b.ID] = b.DependsOn
		}
	}
	cp := checkpoint{
		CompletedBatches: make([]string, 0),
		BatchCycles:      make(map[string]int),
		CompletedOutputs: outputs,
		BatchDependsOn:   deps,
	}
	for id := range completed {
		cp.CompletedBatches = append(cp.CompletedBatches, id)
	}
	for _, b := range batches {
		cp.BatchCycles[b.ID] = b.CycleCount
	}
	data, _ := json.Marshal(cp)
	_ = e.DB.SaveMasterTaskProgress(masterTaskID, string(data))
}

// ResumeMasterTask resumes a previously suspended master task.
func (e *TeamEngine) ResumeMasterTask(ctx context.Context, masterTaskID, goal, workdir string) ([]*Batch, error) {
	progressJSON, err := e.DB.GetMasterTaskProgress(masterTaskID)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	type checkpoint struct {
		CompletedBatches []string            `json:"completed_batches"`
		BatchCycles      map[string]int      `json:"batch_cycles"`
		CompletedOutputs map[string]string   `json:"completed_outputs,omitempty"`
		BatchDependsOn   map[string][]string `json:"batch_depends_on,omitempty"`
	}
	var cp checkpoint
	if progressJSON != "" {
		if err := json.Unmarshal([]byte(progressJSON), &cp); err != nil {
			return nil, fmt.Errorf("parse checkpoint: %w", err)
		}
	}
	if cp.CompletedOutputs == nil {
		cp.CompletedOutputs = make(map[string]string)
	}
	if cp.BatchCycles == nil {
		cp.BatchCycles = make(map[string]int)
	}
	if cp.BatchDependsOn == nil {
		cp.BatchDependsOn = make(map[string][]string)
	}
	completedBatches := make(map[string]bool)
	for _, id := range cp.CompletedBatches {
		completedBatches[id] = true
	}

	allTasks, err := e.DB.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		return nil, fmt.Errorf("list subtasks: %w", err)
	}
	for _, t := range allTasks {
		newState := ResetForResume(t.State)
		if newState != t.State {
			_ = e.DB.UpdateTask(t.ID, map[string]interface{}{"state": string(newState)})
			_ = e.Whiteboard.WriteStatus(t.ID, string(newState))
			_ = e.Whiteboard.AppendOutput(t.ID, "\n[RESUMED]\n")
		}
	}

	batchMap := make(map[string]*Batch)
	batchOrder := make([]string, 0)
	for _, t := range allTasks {
		bid := t.BatchID
		if bid == "" {
			bid = "default"
		}
		if _, ok := batchMap[bid]; !ok {
			batchMap[bid] = &Batch{
				ID:        bid,
				Label:     bid,
				Status:    BatchStatusPending,
				Tasks:     make([]*Task, 0),
				DependsOn: cp.BatchDependsOn[bid],
			}
			batchOrder = append(batchOrder, bid)
		}
		batchMap[bid].Tasks = append(batchMap[bid].Tasks, t)
	}
	for _, b := range batchMap {
		if cycles, ok := cp.BatchCycles[b.ID]; ok {
			b.CycleCount = cycles
		}
	}

	batches := make([]*Batch, 0, len(batchOrder))
	for _, bid := range batchOrder {
		batches = append(batches, batchMap[bid])
	}

	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")

	for _, batch := range batches {
		if completedBatches[batch.ID] {
			continue
		}
		select {
		case <-ctx.Done():
			e.saveCheckpoint(masterTaskID, completedBatches, cp.CompletedOutputs, batches)
			return batches, fmt.Errorf("resume cancelled: %w", ctx.Err())
		default:
		}

		cycleLimit := batch.MaxCycles
		if cycleLimit <= 0 {
			cycleLimit = 1
		}
		for cycle := 0; cycle < cycleLimit; cycle++ {
			batch.CycleCount = cycle + 1
			if err := e.RunBatch(ctx, batch); err != nil {
				e.saveCheckpoint(masterTaskID, completedBatches, cp.CompletedOutputs, batches)
				return batches, fmt.Errorf("run batch %s cycle %d: %w", batch.ID, cycle, err)
			}
			if e.Loggers != nil {
				e.Loggers.Engine("Resume: batch %s cycle %d/%d: status=%s", batch.LabelOrID(), cycle+1, cycleLimit, batch.Status)
			}
			if boardContent := e.Whiteboard.BuildBoardContent(batches); boardContent != "" {
				_ = e.Whiteboard.WriteBoard(boardContent)
			}
			report := e.buildCycleReport(batch, cycle+1)
			review, reviewOutput, err := leader.ReviewCycleFull(goal, report, workdir, decomposerTimeout, leaderModel)
			if err != nil {
				completedBatches[batch.ID] = true
				e.saveCheckpoint(masterTaskID, completedBatches, cp.CompletedOutputs, batches)
				break
			}
			if e.Loggers != nil && reviewOutput != "" {
				e.Loggers.LogLeader("review", fmt.Sprintf("Batch: %s\nDecision: %s", batch.LabelOrID(), review.Decision), reviewOutput, decomposerTimeout, nil)
				e.fireEvent(TaskEvent{Type: EventLeaderLog})
			}
			switch review.Decision {
			case CycleAccept:
				completedBatches[batch.ID] = true
				e.saveCheckpoint(masterTaskID, completedBatches, cp.CompletedOutputs, batches)
				cp.CompletedOutputs[batch.ID] = e.collectBatchOutputs(batch)
				break
			case CycleReject:
				batch.Status = BatchStatusPending
				for _, t := range batch.Tasks {
					e.SendFeedback(t.ID, review.Feedback)
					if !t.State.IsTerminal() && t.State != TaskStateSuspended {
						_ = e.DB.TransitionState(t.ID, TaskStateAssigned, "", "")
					}
				}
				continue
			case CycleEscalated:
				batch.Status = BatchStatusPending
				for _, t := range batch.Tasks {
					e.SendFeedback(t.ID, review.Feedback)
					if t.State == TaskStateFailed || t.State == TaskStateSuspended {
						_ = e.DB.TransitionState(t.ID, TaskStatePending, "escalated retry", "")
					} else if !t.State.IsTerminal() {
						_ = e.DB.TransitionState(t.ID, TaskStateAssigned, "", "")
					}
				}
				continue
			case CycleEscalate:
				e.handleEscalation(batch, review)
				completedBatches[batch.ID] = true
				break
			}
		}
	}

	if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
		_ = e.Whiteboard.WriteDeliverable(delContent)
	}
	if e.Loggers != nil {
		summary := e.buildExecutionSummary(batches, goal, workdir)
		e.Loggers.LogLeader("summary", goal, summary, 0, nil)
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	}
	e.saveCheckpoint(masterTaskID, completedBatches, cp.CompletedOutputs, batches)
	return batches, nil
}

// leaderWatchLoop gives the Leader real-time oversight during execution.
func (e *TeamEngine) leaderWatchLoop(ctx context.Context, leader *Leader, masterTaskID, goal, workdir string) {
	eventCh := make(chan TaskEvent, 256)
	cancel := e.OnEvent(func(event TaskEvent) {
		select {
		case eventCh <- event:
		default:
		}
	})
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			tasks, err := e.DB.ListTasksByMasterTask(masterTaskID)
			if err != nil || len(tasks) == 0 {
				continue
			}
			for _, t := range tasks {
				if t.State.IsTerminal() || t.State == TaskStateSuspended || t.RetryCount <= 1 {
					continue
				}
				output, _ := e.Whiteboard.ReadOutput(t.ID)
				feedback, err := leader.ReviewProgress(goal, t.Title, string(t.Role), string(t.State), t.RetryCount, output, workdir, 30*time.Second)
				if err != nil || feedback == "" {
					continue
				}
				_ = e.SendFeedback(t.ID, feedback)
			}
		}
	}
}

// buildExecutionSummary constructs a human-readable summary of batch execution results.
func (e *TeamEngine) buildExecutionSummary(batches []*Batch, goal, workdir string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## ✅ 任务执行完成\n\n目标: %s\n\n", goal))
	b.WriteString("### 执行结果\n\n")
	b.WriteString("| 任务 | 角色 | 状态 | 工作目录 |\n")
	b.WriteString("|------|------|------|----------|\n")
	for _, batch := range batches {
		for _, t := range batch.Tasks {
			stateIcon := "✅"
			if t.State == TaskStateFailed {
				stateIcon = "❌"
			}
			dir := t.Workdir
			if dir == "" {
				dir = workdir
			}
			b.WriteString(fmt.Sprintf("| %s %s | %s | %s | `%s` |\n", stateIcon, t.Title, t.Role, t.State, dir))
			artifacts, _ := e.Whiteboard.ListArtifacts(t.ID)
			if len(artifacts) > 0 {
				b.WriteString(fmt.Sprintf("  📎 产出文件: %v\n", artifacts))
			}
		}
	}
	b.WriteString(fmt.Sprintf("\n📁 工作目录: `%s`\n", workdir))
	return b.String()
}

// CreateTask creates a new task and persists it to the database.
func (e *TeamEngine) CreateTask(title, description string, role AgentRole, profile ToolProfile, parentIDs []string, maxRetries int, workdir, verifierFocus string) (*Task, error) {
	id := uuid.New().String()
	if profile == "" {
		// Resolve profile from config based on role.
		profile = e.Router.ResolveProfile(role, description, false)
	}

	// Warn on excessively high retry counts — each retry costs tokens and time.
	// The warning is informational only; the task is still created.
	const maxRetriesWarn = 10
	if maxRetries > maxRetriesWarn {
		fmt.Fprintf(os.Stderr, "⚠️  Task %q: max-retries=%d is high (>%d). "+
			"Each retry spawns a fresh Worker+Verifier round. "+
			"Consider a lower value to control token/time cost.\n",
			title, maxRetries, maxRetriesWarn)
	}

	task := NewTask(id, title, description, role, profile, maxRetries, workdir, parentIDs, "", "")
	task.VerifierFocus = verifierFocus

	if err := e.DB.InsertTask(task); err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}
	return task, nil
}

// GetTask retrieves a task by ID.
func (e *TeamEngine) GetTask(taskID string) (*Task, error) {
	return e.DB.GetTask(taskID)
}

// ListTasks returns all tasks.
func (e *TeamEngine) ListTasks() ([]*Task, error) {
	return e.DB.ListTasks()
}

// ListTasksByState returns tasks in a given state.
func (e *TeamEngine) ListTasksByState(state TaskState) ([]*Task, error) {
	return e.DB.ListTasksByState(state)
}

// ---------------------------------------------------------------------------
// State helpers
// ---------------------------------------------------------------------------

// AssignTask transitions a task from pending → assigned.
func (e *TeamEngine) AssignTask(taskID string) error {
	return e.DB.TransitionState(taskID, TaskStateAssigned, "", "")
}

// ---------------------------------------------------------------------------
// Core lifecycle
// ---------------------------------------------------------------------------

// RunTask runs a single task through the full produce→verify→done lifecycle.
//
// Steps:
//  1. pending → assigned
//  2. Write whiteboard input
//  3. assigned → producing, spawn Worker via SubagentSpawner
//  4. Worker done → produced (write output to whiteboard)
//  5. produced → verifying, spawn Verifier
//  6. Verifier PASS → verified → done
//  7. Verifier FAIL → increment retry, append feedback, loop to step 3
//  8. Retry budget exhausted → failed
//
// Returns true if the task reached done, false otherwise.
func (e *TeamEngine) RunTask(ctx context.Context, taskID string) (bool, error) {
	// Phase 0: Initial state check and assignment (under lock).
	e.mu.Lock()
	task, err := e.DB.GetTask(taskID)
	if err != nil {
		e.mu.Unlock()
		return false, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q not found", taskID)
	}

	// Guard: only start from PENDING or ASSIGNED.
	if task.State != TaskStatePending && task.State != TaskStateAssigned {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q is in state %s; can only run from pending or assigned", taskID, task.State)
	}

	// Step 1: Assign.
	if task.State == TaskStatePending {
		if err := e.AssignTask(taskID); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("assign task: %w", err)
		}
		task.State = TaskStateAssigned
	}
	e.mu.Unlock()

	// Main retry loop.
	for attempt := 0; attempt < task.MaxRetries; attempt++ {
		// Check for cancellation before each retry.
		select {
		case <-ctx.Done():
			_ = e.DB.ForceTransitionState(taskID, TaskStateSuspended, "cancelled-by-user")
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return false, ctx.Err()
		default:
		}
		// ---- Phase 1: Producing ----------------------------------------
		// Build the full prompt including inbox messages + agent memory.
		prompt := task.Description
		if inboxCtx, err := e.Whiteboard.BuildInboxContext(task.ID); err == nil && inboxCtx != "" {
			prompt += inboxCtx
		}
		// Inject relevant agent memories so the worker can learn from past tasks.
		if memCtx, err := e.BuildMemoryContext(task.Role, task.Title); err == nil && memCtx != "" {
			prompt += memCtx
		}
		if err := e.Whiteboard.InitTask(task.ID, prompt); err != nil {
			return false, fmt.Errorf("init whiteboard: %w", err)
		}

		// State: assigned → producing (under lock).
		e.mu.Lock()
		if err := e.DB.TransitionState(taskID, TaskStateProducing, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to producing: %w", err)
		}
		e.mu.Unlock()

		workdir := task.Workdir
		if workdir == "" {
			workdir = "."
		}

		// Coding Harness (场景2): create isolated git worktree for coding tasks.
		var hasWorktree bool
		if e.worktreeEnabled && isCodingRole(task.Role) {
			wtPath, branch, err := e.createWorktree(task.ID)
			if err == nil {
				workdir = wtPath
				task.Workdir = wtPath
				task.ArtifactPath = branch
				hasWorktree = true
				// Append worktree info to prompt so the Worker knows where it is.
				prompt += fmt.Sprintf("\n\n## Git Worktree: %s\nYou are working in an isolated git branch `%s`. All changes are safe.\nWhen finished, describe what you changed.",
					wtPath, branch)
			}
		}

		toolNames := ProfileToToolNames(task.Profile)
		toolsStr := strings.Join(toolNames, ",")

		// Live streaming: write each agent output line to whiteboard in real-time.
		liveOutput := func(status, summary, toolName string) {
			if summary != "" {
				if err := e.Whiteboard.AppendTaskOutput(task.ID,
					fmt.Sprintf("[%s] %s: %s", status, toolName, summary)); err != nil {
					// Best-effort streaming; don't fail on write error.
				}
			}
		}

		// Resolve per-role timeout from config instead of using the
		// hardcoded default.  Research/deep-analysis roles need more
		// time (900-1800s) than the default 300s.
		taskTimeout := time.Duration(e.Router.ResolveTimeout(task.Role, false)) * time.Second
		result := e.Runner.RunWithContext(ctx, prompt, workdir, toolsStr, taskTimeout, liveOutput)

		if !result.Success {
			errorContent := fmt.Sprintf("\nERROR (exit %d):\n%s", result.ExitCode, result.Stderr)
			if err := e.Whiteboard.AppendOutput(task.ID, errorContent); err != nil {
				return false, fmt.Errorf("write error output: %w", err)
			}
			// Write the partial stdout anyway.
			if result.Stdout != "" {
				if err := e.Whiteboard.WriteOutput(task.ID, result.Stdout); err != nil {
					return false, fmt.Errorf("write partial stdout: %w", err)
				}
			}
		} else {
			if err := e.Whiteboard.WriteOutput(task.ID, result.Stdout); err != nil {
				return false, fmt.Errorf("write worker output: %w", err)
			}
		}
		if DefaultTeamLog != nil { DefaultTeamLog.WorkerDone(task.ID, result.DurationSeconds, result.ExitCode, len(result.Stdout), result.Success) }

		// Coding Harness: collect git diff after worker completes.
		if hasWorktree {
			diff := e.collectWorktreeDiff(task.ID)
			diffContent := fmt.Sprintf("\n\n## git diff\n```diff\n%s\n```", diff)
			if err := e.Whiteboard.AppendOutput(task.ID, diffContent); err != nil {
				// Best-effort; don't fail on diff write error.
			}
			// Save diff as a deliverable artifact.
			_ = e.Whiteboard.CopyArtifact(task.ID, task.ID[:8]+".diff", diffContent)
		}

		// Log worker output for dashboard dialogue.
		if e.Loggers != nil {
			e.Loggers.LogAgent("worker", taskID, attempt+1, prompt, result.Stdout, result.ExitCode, time.Duration(result.DurationSeconds)*time.Second, nil)
			e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: taskID})
		}

		// State: producing → produced (under lock).
		e.mu.Lock()
		if err := e.DB.TransitionState(taskID, TaskStateProduced, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to produced: %w", err)
		}
		e.mu.Unlock()

		// ---- Phase 2: Verifying ----------------------------------------
		e.mu.Lock()
		if err := e.DB.TransitionState(taskID, TaskStateVerifying, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to verifying: %w", err)
		}
		e.mu.Unlock()

		v := NewVerifier(e.Whiteboard, e.Runner, 0)
		passed, isRetry, feedback, err := v.Verify(task)
		if err != nil {
			return false, fmt.Errorf("verifier error: %w", err)
		}

		// Fallback for content roles: if the Verifier couldn't produce a
		// verdict (empty feedback) but the Worker output is clearly substantive
		// (> 500 chars), accept it.
		if !passed && !isRetry && feedback == "" && task.Role.IsContentRole() {
			out, _ := e.Whiteboard.ReadOutput(task.ID)
			if len(out) > 500 {
				passed = true
				feedback = "AUTO-PASS: output is substantive (" + fmt.Sprintf("%d", len(out)) + " chars)"
			}
		}

		// Log verifier output for dashboard dialogue.
		if e.Loggers != nil {
			verdict := "PASS"
			if !passed {
				verdict = "FAIL"
			}
			verifierPrompt := fmt.Sprintf("Verify output of task %q (role: %s)", task.Title, task.Role)
			e.Loggers.LogAgent("verifier", taskID, attempt+1, verifierPrompt, fmt.Sprintf("[%s] %s", verdict, feedback), 0, 0, nil)
			e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: taskID})
		}

		if passed {
			e.mu.Lock()
			if err := e.DB.TransitionState(taskID, TaskStateVerified, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to verified: %w", err)
			}
			if err := e.DB.TransitionState(taskID, TaskStateDone, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to done: %w", err)
			}
			if err := e.DB.UpdateTask(taskID, map[string]interface{}{
				"retry_count": attempt,
			}); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("update retry count: %w", err)
			}
			e.mu.Unlock()
			return true, nil
		}

		// Verification failed — prepare retry.
		e.mu.Lock()
		task, err = e.DB.GetTask(taskID)
		if err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("re-get task for retry: %w", err)
		}
		if task == nil {
			e.mu.Unlock()
			return false, fmt.Errorf("task %q disappeared", taskID)
		}

		task.RetryCount = attempt + 1
		task.VerifierFeedback = feedback
		// Strip any previous verifier feedback blocks to prevent
		// prompt bloat across retries (context grows unboundedly).
		if idx := strings.Index(task.Description, "\n\n[VERIFIER FEEDBACK"); idx >= 0 {
			task.Description = task.Description[:idx]
		}
		task.Description += fmt.Sprintf(
			"\n\n[VERIFIER FEEDBACK - Attempt %d]\n%s",
			task.RetryCount, feedback,
		)

		if err := e.DB.UpdateTask(taskID, map[string]interface{}{
			"retry_count":        task.RetryCount,
			"description":        task.Description,
			"verifier_feedback":  feedback,
		}); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("update task for retry: %w", err)
		}

		if task.RetryCount >= task.MaxRetries {
			// Don't fail — the leader should re-decompose just this task.
			if err := e.DB.TransitionState(taskID, TaskStateSuspended, "retries exhausted — needs re-decomposition", feedback); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to suspended: %w", err)
			}
			e.mu.Unlock()
			return false, nil
		}

		// Reset state back to assigned for retry.
		if err := e.DB.TransitionState(taskID, TaskStateAssigned, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to assigned for retry: %w", err)
		}
		e.mu.Unlock()

		task.State = TaskStateAssigned
	}

	return false, nil
}

// ---------------------------------------------------------------------------
// Pipeline: Plan → Batches → Tasks (with parallelism)
// ---------------------------------------------------------------------------

// PlanAndRun uses the Leader to decompose a goal into batches of subtasks
// and runs them respecting batch-level dependencies with parallelism.
//
// Architecture:
//   Leader     → plan ([]PlanTask with batch_id)
//   Engine     → group by batch_id → []Batch
//              → for each Batch (in dependency order):
//                  → spawn all tasks in batch in parallel (goroutine pool)
//                  → wait for all tasks to finish (sync.WaitGroup)
//                  → gate: all PASS → next batch, any FAIL → abort
//              → write board.md + deliverable.md
func (e *TeamEngine) PlanAndRun(ctx context.Context, goal, workdir, masterTaskID string) ([]*Batch, error) {
	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")
	planTasks, err := leader.Decompose(goal, workdir, decomposerTimeout, leaderModel)
	if err != nil {
		return nil, fmt.Errorf("decompose goal: %w", err)
	}
	if len(planTasks) == 0 {
		return nil, fmt.Errorf("plan is empty")
	}

	// Step 1: Group PlanTasks into batches by batch_id.
	type batchGroup struct {
		label       string
		dependsOn   []string
		planTasks   []PlanTask
		concurrency int
		maxCycles   int
	}
	batchMap := make(map[string]*batchGroup)
	batchOrder := make([]string, 0) // preserve insertion order

	for _, pt := range planTasks {
		batchID := pt.BatchID
		if batchID == "" {
			batchID = "default"
		}
		if _, ok := batchMap[batchID]; !ok {
			batchMap[batchID] = &batchGroup{
				label:       pt.BatchLabel,
				dependsOn:   pt.DependsOnBatch,
				concurrency: pt.Concurrency,
				maxCycles:   pt.MaxCycles,
			}
			batchOrder = append(batchOrder, batchID)
		}
		batchMap[batchID].planTasks = append(batchMap[batchID].planTasks, pt)
	}

	// Step 2: Create Batch objects.
	var batches []*Batch
	for _, bid := range batchOrder {
		bg := batchMap[bid]
		concurrency := bg.concurrency
		if concurrency <= 0 {
			concurrency = e.Config.Batch.DefaultConcurrency
		}
		maxCycles := bg.maxCycles
		if maxCycles <= 0 {
			maxCycles = e.Config.Batch.DefaultMaxCycles
		}
		batch := &Batch{
			ID:          bid,
			Label:       bg.label,
			DependsOn:   bg.dependsOn,
			Status:      BatchStatusPending,
			Concurrency: concurrency,
			MaxCycles:   maxCycles,
		}

		// Create tasks for this batch.
		for _, pt := range bg.planTasks {
			profile := ToolProfile(pt.Profile)
			task, err := e.CreateTask(
				pt.Title,
				pt.Description,
				AgentRole(pt.Role),
				profile,
				nil, // parent IDs now handled at batch level
				0, // 0 = use NewTask default (9)
				workdir,
				pt.VerifierFocus,
			)
			if err != nil {
				return nil, fmt.Errorf("create subtask %s: %w", pt.Title, err)
			}
			task.BatchID = bid
			e.DB.UpdateTask(task.ID, map[string]interface{}{
				"batch_id":       bid,
				"master_task_id": masterTaskID,
			})
			batch.Tasks = append(batch.Tasks, task)
		}
		batches = append(batches, batch)
	}

	// Step 3: Execute batches in order with dependency gating + CycleReport
	//         + cross-stage artifact passing (场景4).
	leader = NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	completedBatches := make(map[string]bool)
	// Track completed batch outputs for cross-stage injection.
	completedBatchOutputs := make(map[string]string)

	for _, batch := range batches {
		// Check batch dependencies.
		allDepsPassed := true
		for _, depID := range batch.DependsOn {
			if !completedBatches[depID] {
				allDepsPassed = false
				break
			}
		}
		if !allDepsPassed {
			batch.Status = BatchStatusFailed
			e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
			return batches, fmt.Errorf("batch %s: dependency %v not satisfied", batch.ID, batch.DependsOn)
		}

		// Check for cancellation between batches.
		select {
		case <-ctx.Done():
			// Suspend all non-terminal tasks so the dashboard shows them as
			// stopped rather than still running.
			for _, b := range batches {
				for _, t := range b.Tasks {
					if !t.State.IsTerminal() {
						_ = e.DB.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
					}
				}
			}
			_ = e.DB.UpdateMasterTaskStatus(masterTaskID, "")
			e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return batches, fmt.Errorf("cancelled: %w", ctx.Err())
		default:
		}

		// Run this batch with cycle support.
		cycleLimit := batch.MaxCycles
		if cycleLimit <= 0 {
			cycleLimit = 1 // at minimum one cycle
		}

	batchCycleLoop:
		for cycle := 0; cycle < cycleLimit; cycle++ {
			batch.CycleCount = cycle + 1

			// Execute all tasks in this batch (parallel if concurrency > 0).
			if err := e.RunBatch(ctx, batch); err != nil {
				e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
				return batches, fmt.Errorf("run batch %s cycle %d: %w", batch.ID, cycle, err)
			}

			// Check for tasks needing re-decomposition (retries exhausted).
			if reTasks := e.collectReDecomposeTasks(batch); len(reTasks) > 0 {
				for _, t := range reTasks {
					if e.Loggers != nil {
						e.Loggers.Engine("leader: re-decompose task %s (%s) after %d retries", t.ID[:8], t.Title, t.MaxRetries)
					}
					smaller, err := leader.DecomposeTask(t, workdir, decomposerTimeout, leaderModel)
					if err == nil && len(smaller) > 1 {
						// Replace the original task with smaller subtasks.
						e.replaceTaskWithSubtasks(t, smaller, masterTaskID)
						if e.Loggers != nil {
							e.Loggers.Engine("leader: re-decomposed %s into %d smaller tasks", t.ID[:8], len(smaller))
						}
					}
				}
			}

			// Write board.md after each batch cycle.
			if boardContent := e.Whiteboard.BuildBoardContent(batches); boardContent != "" {
				_ = e.Whiteboard.WriteBoard(boardContent)
			}

			// Build and send CycleReport to Leader for review.
			report := e.buildCycleReport(batch, cycle+1)
			review, err := leader.ReviewCycle(goal, report, workdir, decomposerTimeout, leaderModel)
			if err != nil {
				// If review fails, accept by default (don't block pipeline).
				completedBatches[batch.ID] = true
				e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
				break batchCycleLoop
			}

			switch review.Decision {
			case CycleAccept:
				completedBatches[batch.ID] = true
				// Cross-stage artifact passing (场景4):
				// Collect outputs from completed batch tasks so the
				// next batch can reference them.
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
				if depArtifacts := e.buildStageArtifactContext(completedBatchOutputs, batch.DependsOn); depArtifacts != "" {
					// Inject dependency artifacts into this batch's tasks.
					for _, t := range batch.Tasks {
						updated := t.Description + depArtifacts
						e.DB.UpdateTask(t.ID, map[string]interface{}{"description": updated})
					}
				}
				break batchCycleLoop

			case CycleReject:
				// Leader rejected: reset all tasks (including failed ones) for retry.
				batch.Status = BatchStatusPending
				for _, t := range batch.Tasks {
					e.SendFeedback(t.ID, review.Feedback)
					// Log leader feedback into task dialogue for dashboard visibility.
					if e.Loggers != nil {
						e.Loggers.LogTaskFeedback(t.ID, "leader", t.RetryCount+1, review.Feedback)
						e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: t.ID})
					}
					if t.State.IsTerminal() {
						// Reset failed/done tasks: clear retries, bump max, re-assign.
						_ = e.DB.UpdateTask(t.ID, map[string]interface{}{
							"retry_count":       0,
							"max_retries":       t.MaxRetries + 3,
							"description":       t.Description + fmt.Sprintf("\n\n[Leader Feedback - Retry]\n%s", review.Feedback),
							"verifier_feedback": "",
						})
						_ = e.DB.TransitionState(t.ID, TaskStateAssigned, "leader retry", "")
					} else {
						_ = e.DB.TransitionState(t.ID, TaskStateAssigned, "", "")
					}
				}
				// Continue the cycle loop to retry.
				continue

			case CycleEscalate:
				// Send escalation request to user via inbox and wait.
				e.handleEscalation(batch, review)
				// After escalation, continue based on user decision.
				completedBatches[batch.ID] = true
				e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
				break batchCycleLoop
			}
		}

		// All cycles exhausted without acceptance — leader makes final call.
		if !completedBatches[batch.ID] {
			report := e.buildCycleReport(batch, cycleLimit)
			review, err := leader.ReviewCycle(goal, report, workdir, decomposerTimeout, leaderModel)
			if err != nil {
				// Leader unavailable: accept whatever we have (don't block).
				completedBatches[batch.ID] = true
				e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
			} else {
				switch review.Decision {
				case CycleAccept:
					completedBatches[batch.ID] = true
					completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				default:
					// Leader still not satisfied — mark batch as failed but continue pipeline.
					batch.Status = BatchStatusFailed
					completedBatches[batch.ID] = true
				}
				e.saveCheckpoint(masterTaskID, completedBatches, completedBatchOutputs, batches)
				if e.Loggers != nil {
					e.Loggers.LogLeader("review", fmt.Sprintf("Batch: %s\nDecision: %s (final)", batch.LabelOrID(), review.Decision), fmt.Sprintf("Final review: %s\nFeedback: %s", review.Decision, review.Feedback), decomposerTimeout, nil)
					e.fireEvent(TaskEvent{Type: EventLeaderLog})
				}
			}
		}
	}

	// Step 4: Write final deliverable.md.
	if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
		_ = e.Whiteboard.WriteDeliverable(delContent)
	}

	return batches, nil
}

// collectBatchOutputs gathers output summaries from all completed tasks in a batch.
// Used for cross-stage artifact passing (场景4).
func (e *TeamEngine) collectBatchOutputs(batch *Batch) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n\n## Cross-Stage Context: Batch %s Outputs\n", batch.LabelOrID()))
	for _, t := range batch.Tasks {
		output, _ := e.Whiteboard.ReadOutput(t.ID)
		if len(output) == 0 {
			continue
		}
		preview := output
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		b.WriteString(fmt.Sprintf("\n### %s (%s)\n%s\n", t.Title, t.State, preview))
	}
	artifacts, _ := e.Whiteboard.ListArtifacts(batch.Tasks[0].ID)
	if len(artifacts) > 0 {
		b.WriteString(fmt.Sprintf("\nArtifacts: %v\n", artifacts))
	}
	return b.String()
}

// buildStageArtifactContext generates context that injects previous batch
// outputs into the next batch's tasks, enabling true cross-stage pipeline
// flow where Stage 2's workers see Stage 1's deliverables.
func (e *TeamEngine) buildStageArtifactContext(cache map[string]string, dependsOn []string) string {
	if len(dependsOn) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## 📦 Previous Stage Deliverables\n")
	b.WriteString("The following outputs were produced by earlier stages. Reference them in your work:\n")
	for _, depID := range dependsOn {
		if output, ok := cache[depID]; ok && output != "" {
			b.WriteString(output)
		}
	}
	return b.String()
}

// buildCycleReport creates a CycleReport from a batch's current state.
func (e *TeamEngine) buildCycleReport(batch *Batch, cycleNumber int) *CycleReport {
	report := &CycleReport{
		BatchID:     batch.ID,
		BatchLabel:  batch.Label,
		CycleNumber: cycleNumber,
		Status:      batch.Status,
		BoardPath:   e.Whiteboard.BoardPath(),
		Deliverable: e.Whiteboard.DeliverablePath(),
	}

	for _, t := range batch.Tasks {
		summary := TaskSummary{
			ID:         t.ID,
			Title:      t.Title,
			Role:       string(t.Role),
			State:      t.State,
			RetryCount: t.RetryCount,
		}
		// Add a brief output preview if available.
		if output, err := e.Whiteboard.ReadOutput(t.ID); err == nil && len(output) > 0 {
			if len(output) > 100 {
				summary.OutputBrief = output[:100] + "..."
			} else {
				summary.OutputBrief = output
			}
		}
		report.Tasks = append(report.Tasks, summary)
	}

	return report
}

// handleEscalation suspends pipeline execution and blocks until the user
// (or another agent via AgentChannel.ResolveEscalation) provides a decision.
//
// The engine writes a detailed escalation to board.md and the user's inbox,
// then blocks on EscalationManager.Escalate with a 30-minute timeout.
// If the timeout expires, the pipeline aborts.
func (e *TeamEngine) handleEscalation(batch *Batch, review *CycleReview) {
	req := EscalationRequest{
		BatchID:    batch.ID,
		BatchLabel: batch.Label,
		Reason:     review.Reason,
		Feedback:   review.Feedback,
		Cycle:      batch.CycleCount,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		TimeoutMin: 30,
	}

	// Write to board.md.
	boardEntry := fmt.Sprintf("\n## 🚨 ESCALATION — Batch %s\n\n"+
		"- **Reason**: %s\n- **Feedback**: %s\n- **Cycle**: %d\n- **Time**: %s\n"+
		"- **Status**: ⏳ Waiting for decision...\n"+
		"- **Resolve**: `whale team resolve %s continue|retry|abort|modify`\n\n",
		batch.LabelOrID(), review.Reason, review.Feedback, batch.CycleCount, req.CreatedAt, batch.ID)

	existing, _ := e.Whiteboard.ReadBoard()
	_ = e.Whiteboard.WriteBoard(existing + boardEntry)

	// Block until resolved (or timeout).
	decision, err := e.Escalation.Escalate(req)

	// Update board with result.
	resultEntry := fmt.Sprintf("  → **Resolved**: %s", decision)
	if err != nil {
		resultEntry = fmt.Sprintf("  → **Timeout**: %v", err)
	}
	existing, _ = e.Whiteboard.ReadBoard()
	_ = e.Whiteboard.WriteBoard(existing + resultEntry + "\n")
}

// RunBatch executes all tasks in a batch in parallel, respecting the
// configured concurrency limit.
func (e *TeamEngine) RunBatch(ctx context.Context, batch *Batch) error {
	batch.Status = BatchStatusRunning
	if DefaultTeamLog != nil { DefaultTeamLog.BatchStart(batch.ID, batch.Label, len(batch.Tasks), batch.CycleCount, batch.MaxCycles) }

	tasks := batch.Tasks
	if len(tasks) == 0 {
		batch.Status = BatchStatusPassed
		return nil
	}

	concurrency := batch.Concurrency
	if concurrency <= 0 {
		concurrency = len(tasks) // unlimited
	}

	// Use a semaphore channel to limit concurrency.
	sem := make(chan struct{}, concurrency)
	type taskResult struct {
		taskID string
		ok     bool
		err    error
	}
	resultCh := make(chan taskResult, len(tasks))

	for _, task := range tasks {
		select {
		case <-ctx.Done():
			// Suspend all not-yet-launched tasks.
			for _, t := range tasks {
				if !t.State.IsTerminal() {
					_ = e.DB.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
				}
			}
			batch.Status = BatchStatusFailed
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return ctx.Err()
		default:
		}
		sem <- struct{}{} // acquire semaphore (blocks if at limit)
		go func(t *Task) {
			defer func() { <-sem }() // release semaphore
			ok, err := e.RunTask(ctx, t.ID)
			resultCh <- taskResult{taskID: t.ID, ok: ok, err: err}
		}(task)
	}

	// Drain the semaphore (wait for all goroutines to finish).
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}

	// Collect results.
	close(resultCh)
	allPassed := true
	for res := range resultCh {
		if res.err != nil || !res.ok {
			allPassed = false
		}
	}

	if allPassed {
		batch.Status = BatchStatusPassed
	} else {
		batch.Status = BatchStatusFailed
	}
	if DefaultTeamLog != nil { DefaultTeamLog.BatchDone(batch.ID, string(batch.Status), 0) }
	return nil
}

// PlanAndRunLegacy runs the plan sequentially (original behavior, no batches).
// Kept for backward compatibility.
func (e *TeamEngine) PlanAndRunLegacy(goal, workdir string) ([]*Task, error) {
	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")
	planTasks, err := leader.Decompose(goal, workdir, decomposerTimeout, leaderModel)
	if err != nil {
		return nil, fmt.Errorf("decompose goal: %w", err)
	}
	if len(planTasks) == 0 {
		return nil, fmt.Errorf("plan is empty")
	}

	var tasks []*Task
	taskIndex := make(map[int]string)
	for i, pt := range planTasks {
		var parentIDs []string
		if pt.DependsOnIndex >= 0 {
			if pid, ok := taskIndex[pt.DependsOnIndex]; ok {
				parentIDs = []string{pid}
			}
		}
		for _, depIdx := range pt.DependsOnIndices {
			if pid, ok := taskIndex[depIdx]; ok {
				parentIDs = append(parentIDs, pid)
			}
		}
		profile := ToolProfile(pt.Profile)
		task, err := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, parentIDs, 0, workdir, pt.VerifierFocus)
		if err != nil {
			return nil, fmt.Errorf("create subtask %d: %w", i, err)
		}
		tasks = append(tasks, task)
		taskIndex[i] = task.ID
	}

	completed := make(map[string]bool)
	for _, task := range tasks {
		allDepsDone := true
		for _, pid := range task.ParentIDs {
			if !completed[pid] {
				allDepsDone = false
				break
			}
		}
		if !allDepsDone {
			continue
		}
		if _, err := e.RunTask(e.shutdownCtx, task.ID); err != nil {
			return tasks, fmt.Errorf("run subtask %s (%s): %w", task.ID, task.Title, err)
		}
		completed[task.ID] = true
	}
	return tasks, nil
}

// RunPipeline runs multiple tasks in dependency order (without batch grouping).
func (e *TeamEngine) RunPipeline(taskIDs []string) (map[string]bool, error) {
	results := make(map[string]bool)
	completed := make(map[string]bool)

	maxPasses := len(taskIDs)
	for pass := 0; pass < maxPasses; pass++ {
		var ranAny bool
		for _, tid := range taskIDs {
			if completed[tid] {
				continue
			}
			task, err := e.DB.GetTask(tid)
			if err != nil || task == nil {
				results[tid] = false
				completed[tid] = true
				continue
			}
			depsMet := true
			for _, pid := range task.ParentIDs {
				if !completed[pid] {
					depsMet = false
					break
				}
			}
			if !depsMet {
				continue
			}
			ok, err := e.RunTask(e.shutdownCtx, tid)
			if err != nil {
				return results, fmt.Errorf("run task %s: %w", tid, err)
			}
			results[tid] = ok
			completed[tid] = true
			ranAny = true
		}
		if !ranAny {
			break
		}
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

// CancelTask cancels a running task by transitioning it to failed.
func (e *TeamEngine) CancelTask(taskID string) error {
	task, err := e.DB.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	if task.State.IsTerminal() {
		return fmt.Errorf("task %q is already in terminal state %s", taskID, task.State)
	}

	// PENDING → ASSIGNED → FAILED (PENDING → FAILED is not valid alone).
	if task.State == TaskStatePending {
		if err := e.DB.TransitionState(taskID, TaskStateAssigned, "", ""); err != nil {
			return fmt.Errorf("transition to assigned: %w", err)
		}
	}

	return e.DB.TransitionState(taskID, TaskStateSuspended, "cancelled by user", "")
}

// DeleteTask permanently removes a single task and its history from the database.
func (e *TeamEngine) DeleteTask(taskID string) error {
	return e.DB.DeleteTask(taskID)
}

// DeleteMasterTask deletes a master task and all its subtasks.
// Actively running subtasks are transitioned to failed before deletion.
func (e *TeamEngine) DeleteMasterTask(masterTaskID string) error {
	subtasks, err := e.DB.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		return fmt.Errorf("list subtasks: %w", err)
	}
	for _, t := range subtasks {
		if t.State == TaskStateProducing || t.State == TaskStateVerifying {
			_ = e.DB.TransitionState(t.ID, TaskStateFailed, "deleted by user", "")
		}
	}
	return e.DB.DeleteMasterTask(masterTaskID)
}

// collectReDecomposeTasks returns tasks that exhausted retries and need the
// leader to break them into smaller subtasks.  Re-reads from DB because
// batch.Tasks pointers may be stale after goroutine execution.
func (e *TeamEngine) collectReDecomposeTasks(batch *Batch) []*Task {
	var out []*Task
	for _, t := range batch.Tasks {
		current, _ := e.DB.GetTask(t.ID)
		if current != nil && current.State == TaskStateSuspended && current.RetryCount >= current.MaxRetries {
			out = append(out, current)
		}
	}
	return out
}

// replaceTaskWithSubtasks marks the original task as done (replaced) and
// inserts the new smaller subtasks into the same batch.
func (e *TeamEngine) replaceTaskWithSubtasks(original *Task, smaller []PlanTask, masterTaskID string) {
	_ = e.DB.ForceTransitionState(original.ID, TaskStateDone, "re-decomposed into smaller tasks")
	for _, pt := range smaller {
		profile := ToolProfile(pt.Profile)
		task, _ := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, nil, 0, original.Workdir, pt.VerifierFocus)
		task.BatchID = original.BatchID
		task.MasterTaskID = masterTaskID
		_ = e.DB.UpdateTask(task.ID, map[string]interface{}{
			"batch_id":       original.BatchID,
			"master_task_id": masterTaskID,
		})
		_ = e.DB.TransitionState(task.ID, TaskStateAssigned, "re-decomposed from "+original.ID[:8], "")
	}
}

// collectCycleFindings reads the latest verifier output for each task in
// the batch and returns a unified findings set for loop-until-dry comparison.
func (e *TeamEngine) collectCycleFindings(batch *Batch, cycle int) *CycleFindingsSet {
	set := &CycleFindingsSet{Cycle: cycle}
	seen := make(map[string]bool)
	for _, t := range batch.Tasks {
		output, err := e.Whiteboard.ReadVerifier(t.ID)
		if err != nil {
			continue
		}
		for _, f := range ParseFindings(output) {
			if !seen[f.ID] {
				seen[f.ID] = true
				set.Findings = append(set.Findings, f)
			}
		}
	}
	return set
}

// SendFeedback sends human feedback to a task via the AgentChannel.Prompt
// interface.  The message is written to the task's inbox so the agent sees
// it on its next turn — same mechanism used by agent-to-agent messaging.
//
// This implements the "Agent 与人类同权" principle: human feedback and
// agent-to-agent messages go through the exact same channel.
func (e *TeamEngine) SendFeedback(taskID, message string) error {
	_, err := e.Prompt(context.Background(), PromptRequest{
		ToTaskID: taskID,
		From:     "human",
		Content:  message,
		Sync:     false, // fire-and-forget; agent sees it on next run
	})
	if err != nil {
		return fmt.Errorf("send feedback via prompt channel: %w", err)
	}

	// Also update the verifier_feedback field for state tracking.
	return e.DB.UpdateTask(taskID, map[string]interface{}{
		"verifier_feedback": message,
	})
}

// GetProgress returns an approximate completion percentage for a task.
func GetProgress(state TaskState) int {
	switch state {
	case TaskStatePending:
		return 0
	case TaskStateAssigned:
		return 0
	case TaskStateProducing:
		return 25
	case TaskStateProduced:
		return 50
	case TaskStateVerifying:
		return 65
	case TaskStateVerified:
		return 85
	case TaskStateDone:
		return 100
	case TaskStateFailed:
		return 100
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Agent Memory — 跨 session 经验复用
// ---------------------------------------------------------------------------

// RecordMemory persists an agent experience/learning for future reuse.
// Agents can call this after task completion to save lessons learned.
func (e *TeamEngine) RecordMemory(role AgentRole, key, content, sourceTaskID string) error {
	memory := &MemoryEntry{
		AgentRole:  string(role),
		Key:        key,
		Content:    content,
		SourceTask: sourceTaskID,
	}
	return e.DB.SaveMemory(memory)
}

// GetMemories retrieves memories for an agent role, optionally filtered by key.
func (e *TeamEngine) GetMemories(role AgentRole, key string) ([]MemoryEntry, error) {
	return e.DB.GetMemories(role, key)
}

// BuildMemoryContext generates a memory prompt for an agent role.
// This is injected into the agent's task description so it can leverage
// past experiences.
func (e *TeamEngine) BuildMemoryContext(role AgentRole, key string) (string, error) {
	memories, err := e.DB.GetMemories(role, key)
	if err != nil || len(memories) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("\n\n## 🧠 Agent Memory — Past Experiences\n")
	b.WriteString("You have the following relevant memories from past tasks:\n\n")
	for _, m := range memories {
		b.WriteString(fmt.Sprintf("### Memory (%s)\n", m.CreatedAt[:10]))
		b.WriteString(fmt.Sprintf("Key: %s\n", m.Key))
		b.WriteString(fmt.Sprintf("%s\n\n", m.Content))
	}
	b.WriteString("Use these experiences to avoid repeating past mistakes and to apply proven strategies.\n")
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Session log export — 完整 session 导出（场景1 缺失项）
// ---------------------------------------------------------------------------

// ExportTaskLog exports a task's full state as a JSON-serializable map.
// Includes: task metadata, state history, whiteboard output, verifier result,
// artifacts list, and inbox messages. This is the "可恢复对象" required by
// Scenario 1 (IM 异步 — 外部 session log).
func (e *TeamEngine) ExportTaskLog(taskID string) (map[string]interface{}, error) {
	task, err := e.DB.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	result := map[string]interface{}{
		"task_id":    task.ID,
		"title":      task.Title,
		"role":       string(task.Role),
		"state":      string(task.State),
		"retries":    fmt.Sprintf("%d/%d", task.RetryCount, task.MaxRetries),
		"batch_id":   task.BatchID,
		"created_at": task.CreatedAt,
		"updated_at": task.UpdatedAt,
	}

	// State history.
	history, _ := e.DB.GetTaskHistory(taskID)
	var histEntries []map[string]string
	for _, h := range history {
		histEntries = append(histEntries, map[string]string{
			"old_state":  h.OldState,
			"new_state":  h.NewState,
			"error_msg":  h.ErrorMsg,
			"changed_at": h.ChangedAt,
		})
	}
	result["state_history"] = histEntries

	// Whiteboard output.
	output, _ := e.Whiteboard.ReadOutput(taskID)
	result["output"] = output

	// Verifier result.
	verifier, _ := e.Whiteboard.ReadVerifier(taskID)
	result["verifier_result"] = verifier

	// Artifacts.
	artifacts, _ := e.Whiteboard.ListArtifacts(taskID)
	result["artifacts"] = artifacts

	// Inbox messages.
	messages, _ := e.Whiteboard.ReadInbox(taskID)
	result["inbox_messages"] = messages

	// Board + deliverable paths.
	result["board_path"] = e.Whiteboard.BoardPath()
	result["deliverable_path"] = e.Whiteboard.DeliverablePath()

	return result, nil
}

// ExportTaskLogJSON is like ExportTaskLog but returns a JSON string.
// Stats captures aggregate statistics across all tasks for the dashboard.
type Stats struct {
	Total          int            `json:"total"`
	ByState        map[string]int `json:"by_state"`
	SuccessRate    float64        `json:"success_rate"`
	AvgDurationSec float64        `json:"avg_duration_sec"`
}

// GetStats computes aggregate statistics across all tasks.
func (e *TeamEngine) GetStats() (*Stats, error) {
	tasks, err := e.DB.ListTasks()
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	stats := &Stats{
		Total:   len(tasks),
		ByState: make(map[string]int),
	}
	for _, t := range tasks {
		stats.ByState[string(t.State)]++
	}
	done := stats.ByState[string(TaskStateDone)]
	failed := stats.ByState[string(TaskStateFailed)]
	completed := done + failed
	if completed > 0 {
		stats.SuccessRate = float64(done) / float64(completed) * 100
	}
	return stats, nil
}

func (e *TeamEngine) ExportTaskLogJSON(taskID string) (string, error) {
	log, err := e.ExportTaskLog(taskID)
	if err != nil {
		return "", err
	}
	payload, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal log: %w", err)
	}
	return string(payload), nil
}

// GetRetryDelay returns the delay in seconds before the next retry,
// using exponential backoff.
func GetRetryDelay(retryCount int) int {
	if retryCount <= 0 {
		return 0
	}
	// Exponential backoff: 5s, 10s, 20s, 40s, ...
	delay := int(math.Pow(2, float64(retryCount-1)) * 5)
	if delay > 120 {
		return 120
	}
	return delay
}
