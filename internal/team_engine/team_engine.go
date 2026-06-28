package team_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/usewhale/whale/internal/eventbus"
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
	Store      *FileTaskStore
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
	// activeAgents tracks the OS process ID for each running agent task.
	activeAgents  map[string]int // taskID → PID
	activeStdinWriters map[string]io.WriteCloser // taskID → stdin pipe

	// masterTaskCancels stores cancel functions for active PlanAndRun /
	// ResumeMasterTask executions.  When the user clicks "stop" in the
	// dashboard, the corresponding cancel is called to abort the batch loop.
	masterTaskCancels map[string]context.CancelFunc // masterTaskID → cancel

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
		shellSpawner       *ShellSubagentSpawner
		persistentSessions map[string]*PersistentSession
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
	e.Runner.WithTeam(tc)
}

// Team returns the current team configuration, or nil if none is set.
func (e *TeamEngine) Team() *TeamConfig {
	return e.team
}

// New creates a TeamEngine with the given dependencies.
//
// Parameters:
//   - _:              Deprecated (was SQLite dbPath, now unused — state is file-based)
//   - whiteboardDir:  Directory for inter-agent file communication and task storage
//   - configPath:     Path to team_engine.yaml (empty = use defaults)
//   - spawner:        Whale SubagentSpawner implementation
func New(_, whiteboardDir, configPath string, spawner SubagentSpawner) (*TeamEngine, error) {
	wb, err := NewWhiteboard(whiteboardDir)
	if err != nil {
		return nil, fmt.Errorf("init whiteboard: %w", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	runner := NewRunner(spawner)
	store, _ := NewFileTaskStore(whiteboardDir)
	loggers, err := log.New(whiteboardDir)
	if err != nil {
		return nil, fmt.Errorf("init loggers: %w", err)
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	eng := &TeamEngine{
		Store:      store,
		Whiteboard: wb,
		Config:     cfg,
		Router:     NewRouter(cfg),
		Runner:     runner,
		Escalation: NewEscalationManager(),
		Loggers:    loggers,
		timeout:    defaultTimeout,
		activeTrees:    make(map[string]string),
		activeCancels:  make(map[string]context.CancelFunc),
		activeAgents:   make(map[string]int),
		activeStdinWriters: make(map[string]io.WriteCloser),
	masterTaskCancels: make(map[string]context.CancelFunc),
		shutdownCtx:    shutdownCtx,
		persistentSessions: make(map[string]*PersistentSession),
		shutdownCancel: shutdownCancel,
	}

	// cleanupInterruptedTasks is called separately by the CLI on startup.
	// Engine instances created for sync/dashboard must never modify state.
	_ = spawner

	if ss, ok := spawner.(*ShellSubagentSpawner); ok { eng.shellSpawner = ss }
	return eng, nil
}

// Close releases resources held by the engine.
func (e *TeamEngine) Close() error {
	e.shutdownCancel()

	e.mu.Lock()
	// Cancel all running agent contexts (graceful shutdown signal).
	for taskID, cancel := range e.activeCancels {
		cancel()
		delete(e.activeCancels, taskID)
	}
	// Force-kill any agents that haven't exited yet.
	for taskID, pid := range e.activeAgents {
		if pid > 0 {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
		delete(e.activeAgents, taskID)
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

	return e.Store.Close()
}

// CancelMasterTaskExecution cancels a running PlanAndRun / ResumeMasterTask
// for the given masterTaskID.  It is called from the dashboard stop button.
// Returns true if a running execution was found and cancelled.
func (e *TeamEngine) CancelMasterTaskExecution(masterTaskID string) bool {
	e.mu.Lock()
	cancel, ok := e.masterTaskCancels[masterTaskID]
	if ok {
		delete(e.masterTaskCancels, masterTaskID)
	}
	e.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// CleanupInterruptedTasks resets tasks in transient states to suspended.
// With file-based state, this is rarely needed — deriveState handles crash
// recovery naturally. Kept for explicit cleanup scenarios.
func CleanupInterruptedTasks(storeDir string) {
	store, err := NewFileTaskStore(storeDir)
	if err != nil {
		return
	}
	tasks, _ := store.ListTasks()
	for _, t := range tasks {
		switch t.State {
		case TaskStateProducing, TaskStateChecking, TaskStateChecked, TaskStateVerifying, TaskStateAssigned:
			_ = store.ForceTransitionState(t.ID, TaskStateSuspended, "interrupted-restart")
		}
	}
}

// cleanupInterruptedTasks resets tasks in transient states (producing,
// verifying, assigned) to suspended.  Called on engine open to handle
// the case where a previous whale process was killed mid-execution.
func (e *TeamEngine) cleanupInterruptedTasks() {
	tasks, err := e.Store.ListTasks()
	if err != nil {
		return
	}
	for _, t := range tasks {
		switch t.State {
		case TaskStateProducing, TaskStateChecking, TaskStateChecked, TaskStateVerifying, TaskStateAssigned:
			_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "interrupted-restart")
			if defaultTeamLog != nil {
				defaultTeamLog.EngineResumeTask(t.ID, string(TaskStateSuspended))
			}
		}
	}
	// Also reset any master task status from "running" so the dashboard
	// doesn't show stale active states.
	mts, _ := e.Store.ListMasterTasks()
	for _, mt := range mts {
		if mt.Status == "running" {
			_ = e.Store.UpdateMasterTaskStatus(mt.ID, "")
		}
	}
}

// FireEvent broadcasts an event to all subscribers.
func (e *TeamEngine) FireEvent(event TaskEvent) {
	e.fireEvent(event)
}

// fireEvent 向所有订阅者广播事件，同时发布到全局 EventBus。
func (e *TeamEngine) fireEvent(event TaskEvent) {
	// 1. 本地订阅者（向后兼容）
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

	// 2. 发布到全局 EventBus，供 Dashboard 等跨模块消费者
	eventType := taskEventToBusType(event.Type)
	if eventType != "" {
		eventbus.Global().Publish(eventbus.TopicTeamEngine, eventbus.Event{
			Type:    eventType,
			Payload: event,
		})
	}
}

// taskEventToBusType maps internal event types to EventBus event type strings.
func taskEventToBusType(t TaskEventType) string {
	switch t {
	case EventStateChanged:
		return eventbus.EventStateChanged
	case EventWorkerOutput:
		return eventbus.EventWorkerOutput
	case EventVerifierResult:
		return eventbus.EventVerifierResult
	case EventTaskDone:
		return eventbus.EventTaskDone
	case EventLeaderLog:
		return eventbus.EventLeaderLog
	case EventAgentLog:
		return eventbus.EventAgentLog
	default:
		return ""
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
func (e *TeamEngine) CreateMasterTask(goal, workspacePath, sessionID string) (*MasterTask, error) {
	agent := ""
	if e.team != nil {
		if len(e.team.Roles) == 1 {
			agent = "expert:" + e.team.Roles[0]
		} else if e.team.Label != "" {
			agent = "team:" + e.team.Label
		}
	}
	mt := &MasterTask{
		ID:            uuid.New().String(),
		Goal:          goal,
		Agent:         agent,
		SessionID:     sessionID,
		WorkspacePath: workspacePath,
		Status:        "running",
	}
	if err := e.Store.InsertMasterTask(mt); err != nil {
		return nil, fmt.Errorf("insert master task: %w", err)
	}

	// Dual-write to file store.
	if e.Store != nil {
		_ = e.Store.InsertMasterTask(mt)
	}

	// Notify dashboard that this workspace now has an engine.
	eventbus.Global().Publish(eventbus.TopicWorkspace, eventbus.Event{
		Type: eventbus.EventWSEngineReady,
		Payload: map[string]string{
			"path": workspacePath,
		},
	})
	return mt, nil
}

// ListMasterTasks returns all master tasks.
func (e *TeamEngine) ListMasterTasks() ([]*MasterTask, error) {
	return e.Store.ListMasterTasks()
}

// ListMasterTasksBySession returns master tasks for a given session.
func (e *TeamEngine) ListMasterTasksBySession(sessionID string) ([]*MasterTask, error) {
	return e.Store.ListMasterTasksBySession(sessionID)
}

// DeleteMasterTaskAndChildren removes a master task and all its subtasks.
func (e *TeamEngine) DeleteMasterTaskAndChildren(masterTaskID string) error {
	tasks, _ := e.Store.ListTasksByMasterTask(masterTaskID)
	for _, t := range tasks {
		e.Store.DeleteTask(t.ID)
	}
	return e.Store.DeleteMasterTask(masterTaskID)
}

// ListTasksByMasterTask returns subtasks for a master task.
func (e *TeamEngine) ListTasksByMasterTask(masterTaskID string) ([]*Task, error) {
	return e.Store.ListTasksByMasterTask(masterTaskID)
}

// GetMasterTask retrieves a master task by ID.
func (e *TeamEngine) GetMasterTask(id string) (*MasterTask, error) {
	return e.Store.GetMasterTask(id)
}

// CompleteMasterTask marks a master task as done.
func (e *TeamEngine) CompleteMasterTask(id string) error {
	return e.Store.UpdateMasterTaskStatus(id, "done")
}

// ListSuspendedMasterTasks returns master tasks with suspended subtasks.
func (e *TeamEngine) ListSuspendedMasterTasks() ([]*MasterTask, error) {
	all, err := e.Store.ListMasterTasks()
	if err != nil {
		return nil, err
	}
	var result []*MasterTask
	for _, mt := range all {
		tasks, err := e.Store.ListTasksByMasterTask(mt.ID)
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
func (e *TeamEngine) saveCheckpoint(masterTaskID string, completed map[string]bool, passed map[string]bool, outputs map[string]string, batches []*Batch) {
	type checkpoint struct {
		CompletedBatches []string            `json:"completed_batches"`
		PassedBatches    []string            `json:"passed_batches,omitempty"`
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
		PassedBatches:    make([]string, 0),
		BatchCycles:      make(map[string]int),
		CompletedOutputs: outputs,
		BatchDependsOn:   deps,
	}
	for id := range completed {
		cp.CompletedBatches = append(cp.CompletedBatches, id)
	}
	for id := range passed {
		cp.PassedBatches = append(cp.PassedBatches, id)
	}
	for _, b := range batches {
		cp.BatchCycles[b.ID] = b.CycleCount
	}
	data, _ := json.Marshal(cp)
	_ = e.Store.SaveMasterTaskProgress(masterTaskID, string(data))
}

// ResumeMasterTask resumes a previously suspended master task.
func (e *TeamEngine) ResumeMasterTask(ctx context.Context, masterTaskID, goal, workdir string) ([]*Batch, error) {
	// Mark as running so the dashboard shows "Stop" button.
	_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "running")
	// Register cancel so dashboard stop button can interrupt resume.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	e.mu.Lock()
	e.masterTaskCancels[masterTaskID] = execCancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.masterTaskCancels, masterTaskID)
		e.mu.Unlock()
	}()

	progressJSON, err := e.Store.GetMasterTaskProgress(masterTaskID)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	type checkpoint struct {
		CompletedBatches []string            `json:"completed_batches"`
		PassedBatches    []string            `json:"passed_batches,omitempty"`
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
	passedBatches := make(map[string]bool)
	for _, id := range cp.PassedBatches {
		passedBatches[id] = true
	}

	allTasks, err := e.Store.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		return nil, fmt.Errorf("list subtasks: %w", err)
	}

	// Build set of task IDs that are parents (have been re-decomposed
	// into children).  These are virtual management nodes —
	// they should not be executed, only track child progress.
	isParent := make(map[string]bool)
	for _, t := range allTasks {
		for _, pid := range t.ParentIDs {
			isParent[pid] = true
		}
	}

	// Cross-validate completedBatches from checkpoint: for legacy checkpoints
	// or batches that were marked completed without a passedBatches entry,
	// verify that all non-management tasks in the batch are actually Done.
	// If they are, promote to passedBatches; otherwise remove from completedBatches
	// so the batch will be re-executed instead of being incorrectly skipped.
	if len(passedBatches) == 0 {
		// Legacy checkpoint — derive passedBatches from actual task states.
		for id := range completedBatches {
			allDone := true
			for _, t := range allTasks {
				if t.BatchID == id && !isParent[t.ID] {
					if t.State != TaskStateDone {
						allDone = false
						break
					}
				}
			}
			if allDone {
				passedBatches[id] = true
			}
		}
	}

	for _, t := range allTasks {
		// Skip management nodes: they were decomposed into children
		// and should not be retried.
		if isParent[t.ID] {
			if t.State != TaskStateDone {
				_ = e.Store.ForceTransitionState(t.ID, TaskStateDone, "management node")
			}
			continue
		}

		// File-based state: for done/verified tasks, verify output files
		// still exist.  If files were deleted (e.g. user cleaned up), the
		// task must be re-executed.
		if t.State == TaskStateDone || t.State == TaskStateVerified {
			if t.Output != "" {
				if _, oErr := os.Stat(t.Output); oErr == nil {
					if _, vErr := os.Stat(filepath.Join(filepath.Dir(t.Output), "verify.md")); vErr == nil {
						// Both files exist — genuinely done, don't reset.
						continue
					}
				}
			}
			// Files missing — reset to pending so the engine re-executes.
			if defaultTeamLog != nil {
				Log("resume", "task %s was %s but output files missing, resetting to pending", t.ID[:8], t.State)
			}
			_ = e.Store.UpdateTask(t.ID, map[string]interface{}{"state": string(TaskStatePending)})
			_ = e.Whiteboard.WriteStatus(t.ID, string(TaskStatePending))
			_ = e.Whiteboard.AppendOutput(t.ID, "\n[RESUMED — output files missing, re-executing]\n")
			continue
		}

		newState := ResetForResume(t.State)
		if newState != t.State {
			_ = e.Store.UpdateTask(t.ID, map[string]interface{}{"state": string(newState)})
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
	escalator := NewEscalator(leader.planner).WithLoggers(e.Loggers)
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")

	for _, batch := range batches {
		if passedBatches[batch.ID] {
			continue // already passed — skip
		}
		select {
		case <-execCtx.Done():
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches)
			return batches, fmt.Errorf("resume cancelled: %w", ctx.Err())
		default:
		}

		cycleLimit := batch.MaxCycles
		if cycleLimit <= 0 {
			cycleLimit = 1
		}

		if batch.UseDW {
			// DW pipeline execution: multi-verifier per task, no Leader review.
			e.runDWCycle(ctx, batch, cycleLimit, masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches, escalator, workdir, decomposerTimeout, leaderModel)
		} else {
			for cycle := 0; cycle < cycleLimit; cycle++ {
				batch.CycleCount = cycle + 1
				if err := e.RunBatch(execCtx, batch); err != nil {
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches)
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
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches)
					break
				}
				if e.Loggers != nil && reviewOutput != "" {
					e.Loggers.LogLeader("review", fmt.Sprintf("Batch: %s\nDecision: %s", batch.LabelOrID(), review.Decision), reviewOutput, decomposerTimeout, nil)
					e.fireEvent(TaskEvent{Type: EventLeaderLog})
				}
				switch review.Decision {
				case CycleAccept:
					completedBatches[batch.ID] = true
					passedBatches[batch.ID] = true
					batch.Status = BatchStatusPassed
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches)
					cp.CompletedOutputs[batch.ID] = e.collectBatchOutputs(batch)
					break
				case CycleReject:
					batch.Status = BatchStatusPending
					for _, t := range batch.Tasks {
						e.SendFeedback(t.ID, review.Feedback)
						if !t.State.IsTerminal() && t.State != TaskStateSuspended {
							_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
						}
					}
					continue
				case CycleEscalated:
					batch.Status = BatchStatusPending
					for _, t := range batch.Tasks {
						e.SendFeedback(t.ID, review.Feedback)
						if t.State == TaskStateFailed || t.State == TaskStateSuspended {
							_ = e.Store.TransitionState(t.ID, TaskStatePending, "escalated retry", "")
						} else if !t.State.IsTerminal() {
							_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
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
}

if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
	_ = e.Whiteboard.WriteDeliverable(delContent)
}
if e.Loggers != nil {
	summary := e.buildExecutionSummary(batches, goal, workdir)
	e.Loggers.LogLeader("summary", goal, summary, 0, nil)
	e.fireEvent(TaskEvent{Type: EventLeaderLog})
}
e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches)
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
			tasks, err := e.Store.ListTasksByMasterTask(masterTaskID)
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

// CreateTask creates a new task and persists it to the store.
func (e *TeamEngine) CreateTask(title, description string, role AgentRole, profile ToolProfile, parentIDs []string, maxRetries int, workdir, verifierFocus, batchID, masterTaskID string) (*Task, error) {
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

	task := NewTask(id, title, description, role, profile, maxRetries, workdir, parentIDs, batchID, masterTaskID)
	task.VerifierFocus = verifierFocus

	if err := e.Store.InsertTask(task); err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}
	// Dual-write to file store.
	if e.Store != nil {
		_ = e.Store.InsertTask(task)
	}
	return task, nil
}

// GetTask retrieves a task by ID.
func (e *TeamEngine) GetTask(taskID string) (*Task, error) {
	return e.Store.GetTask(taskID)
}

// ListTasks returns all tasks.
func (e *TeamEngine) ListTasks() ([]*Task, error) {
	return e.Store.ListTasks()
}

// ListTasksByState returns tasks in a given state.
func (e *TeamEngine) ListTasksByState(state TaskState) ([]*Task, error) {
	return e.Store.ListTasksByState(state)
}

// ---------------------------------------------------------------------------
// State helpers
// ---------------------------------------------------------------------------

// AssignTask transitions a task from pending → assigned.
func (e *TeamEngine) AssignTask(taskID string) error {
	return e.Store.TransitionState(taskID, TaskStateAssigned, "", "")
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
	if defaultTeamLog != nil {
		Log("task", "task: %s start", taskID[:8])
	}
	e.mu.Lock()
	task, err := e.Store.GetTask(taskID)
	if err != nil {
		e.mu.Unlock()
		return false, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q not found", taskID)
	}

	// File-based completion: task-dir output + verifier → skip execution.
	// State is derived from files, not read from meta.json.
	dir := e.Whiteboard.TaskDir(taskID)
	state := e.Store.DeriveState(dir)
	if state == TaskStateDone {
		e.mu.Unlock()
		Log("task", "task: %s skipped (done: output+verify exist)", taskID[:8])
		return true, nil
	}

	// Guard: only start from PENDING, ASSIGNED, or PRODUCED.
	if task.State != TaskStatePending && task.State != TaskStateAssigned && task.State != TaskStateProduced {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q is in state %s; can only run from pending/assigned/produced", taskID, task.State)
	}

	// When resuming with existing output, skip produce and go straight to verification.
	skipProduce := task.State == TaskStateProduced

	// Step 1: Assign (only for pending tasks).
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
			_ = e.Store.ForceTransitionState(taskID, TaskStateSuspended, "cancelled-by-user")
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return false, ctx.Err()
		default:
		}
		// ---- Phase 1: Producing ----------------------------------------
		// ---- Phase 1: Producing ----------------------------------------
		// Build inbox.md with file-path references, not inline content.
		// Agent reads inbox.md → does work → writes output file.

		// Collect upstream output file references from parent tasks.
		var upstreamRefs []UpstreamRef
		for _, pid := range task.ParentIDs {
			if pt, ptErr := e.Store.GetTask(pid); ptErr == nil && pt != nil && pt.Output != "" {
				upstreamRefs = append(upstreamRefs, UpstreamRef{Name: pt.Title, Path: pt.Output})
			}
		}
		// Also include done same-batch task outputs.
		if task.BatchID != "" && task.MasterTaskID != "" {
			batchTasks, _ := e.Store.ListTasksByMasterTask(task.MasterTaskID)
			for _, bt := range batchTasks {
				if bt.BatchID == task.BatchID && bt.ID != task.ID && bt.State == TaskStateDone && bt.Output != "" {
					upstreamRefs = append(upstreamRefs, UpstreamRef{Name: bt.Title, Path: bt.Output})
				}
			}
		}

		// ---- Phase 1: Producing (skip if resuming with existing output) --
		workerKey := "worker:" + taskID
		if !skipProduce {
			// Persistent session retry: send Verifier feedback to existing
			// Worker session instead of re-spawning.
			if ws := e.persistentSessions[workerKey]; ws != nil && e.shellSpawner != nil {
				fbPrompt := verifierFeedbackForWorker(task.VerifierFeedback)
				if e.Loggers != nil { e.Loggers.Engine("task %s worker CONTINUE retry=%d", taskID[:8], attempt) }
				workerStart := time.Now()
				resp := e.shellSpawner.ContinueSession(ws, fbPrompt)
				contResult := &RunResult{
					ExitCode:        resp.ExitCode,
					Stdout:          resp.Output,
					Stderr:          resp.Diagnostic,
					DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
					Success:         resp.Success,
					PID:             resp.PID,
				}
				Log("timing", "task %s worker continue done in %.1fs", taskID[:8], time.Since(workerStart).Seconds())
				if e.Loggers != nil { e.Loggers.Engine("task %s worker CONTINUE DONE in %.1fs (success=%v)", taskID[:8], time.Since(workerStart).Seconds(), resp.Success) }
				if contResult.Stdout != "" {
					e.Whiteboard.WriteOutput(task.ID, contResult.Stdout)
				}
				goto verifyPhase
			}

			// Read team template and memory.
			template := e.readTeamTemplate(task.Output)
		memory, _ := e.BuildMemoryContext(task.Role, task.Title)

		// Write structured inbox.md.
		inboxParams := InboxParams{
			Title:           task.Title,
			Role:            string(task.Role),
			Description:     task.Description,
			Output:          task.Output,
			UpstreamOutputs: upstreamRefs,
			Template:        template,
			Memory:          memory,
			AllowSelfSplit:  len(task.ParentIDs) == 0,
			RetryFeedback:   task.VerifierFeedback,
		}
		if err := e.Whiteboard.WriteInboxFile(task.ID, inboxParams); err != nil {
			return false, fmt.Errorf("write inbox: %w", err)
		}

		// Agent prompt: reference inbox.md, working dir is out/.
		inboxPath := filepath.Join(e.Whiteboard.TaskDir(task.ID), "input.md")
		outDir := filepath.Join(e.Whiteboard.TaskDir(task.ID), "out")
		prompt := fmt.Sprintf("[Role: %s]\n\n工作目录: %s\n任务文件: %s\n产出: %s\n\n将产出文件写在你的工作目录下。只汇报实际完成的内容，不虚构数字。",
			task.Role, outDir, inboxPath, task.Output)
		if len(task.ParentIDs) == 0 {
			prompt += "\n\n如果任务过大无法一次完成，在产出开头输出 [SPLIT_PLAN] 拆分。"
		}

		// State: assigned → producing (under lock).
		e.mu.Lock()
		if err := e.Store.TransitionState(taskID, TaskStateProducing, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to producing: %w", err)
		}
		e.mu.Unlock()
		e.fireEvent(TaskEvent{Type: EventStateChanged, TaskID: taskID, NewState: string(TaskStateProducing)})

		workdir := task.Workdir
		if workdir == "" {
			workdir = "."
		}
		// Agent runs in a sandboxed out/ directory.
		// task.Workdir keeps the original workspace for checker/build reference.
		agentWorkdir := filepath.Join(e.Whiteboard.TaskDir(taskID), "out")
		os.MkdirAll(agentWorkdir, 0755)

		// Coding Harness (场景2): create isolated git worktree for coding tasks.
		var hasWorktree bool
		if e.worktreeEnabled && isCodingRole(task.Role) {
			wtPath, branch, err := e.createWorktree(task.ID)
			if err == nil {
				agentWorkdir = wtPath
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
		// Register a per-task cancel so Close() / Kill() can immediately
		// abort running subagents instead of waiting for them to finish.
		taskCtx, taskCancel := context.WithCancel(ctx)
		e.mu.Lock()
		e.activeCancels[taskID] = taskCancel
		e.mu.Unlock()
		// onPID callback: track the OS process ID as soon as it starts,
		// so the stop button can kill it during execution.
		onPID := func(pid int) {
			if pid > 0 {
				e.mu.Lock()
				e.activeAgents[taskID] = pid
				e.mu.Unlock()
			}
		}
		onStdin := func(w io.WriteCloser) {
			e.mu.Lock()
			e.activeStdinWriters[taskID] = w
			e.mu.Unlock()
		}
		workerKey := "worker:" + taskID
		workerStart := time.Now()
		var result *RunResult

		// Persistent session: on retry, send Verifier feedback to the
		// existing Worker session instead of restarting from scratch.
		if ws := e.persistentSessions[workerKey]; ws != nil && e.shellSpawner != nil {
			fbPrompt := verifierFeedbackForWorker(task.VerifierFeedback)
			if e.Loggers != nil { e.Loggers.Engine("task %s worker CONTINUE session (retry=%d)", taskID[:8], attempt) }
			resp := e.shellSpawner.ContinueSession(ws, fbPrompt)
			result = &RunResult{
				ExitCode:        resp.ExitCode,
				Stdout:          resp.Output,
				Stderr:          resp.Diagnostic,
				DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
				Success:         resp.Success,
				PID:             resp.PID,
			}
		} else if e.shellSpawner != nil {
			// First attempt: spawn a persistent Worker session.
			if e.Loggers != nil { e.Loggers.Engine("task %s worker SPAWN persistent role=%s", taskID[:8], task.Role) }
			req := SubagentRequest{
				Task:     prompt,
				Role:     string(task.Role),
				Model:    "",
				Tools:    toolNames,
				Workdir:  agentWorkdir,
				Timeout:  taskTimeout,
				MaxIters: 80,
				MaxCalls: 200,
				MaxTokens: effectiveMaxTokens(0, ""),
				OnPID:    onPID,
			}
			ws, resp := e.shellSpawner.SpawnPersistent(context.Background(), req)
			if ws != nil {
				e.persistentSessions[workerKey] = ws
			}
			result = &RunResult{
				SessionID:       resp.SessionID,
				ExitCode:        resp.ExitCode,
				Stdout:          resp.Output,
				Stderr:          resp.Diagnostic,
				DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
				Success:         resp.Success,
				PID:             resp.PID,
			}
		} else {
			// Fallback: normal spawn via Runner.
			if e.Loggers != nil { e.Loggers.Engine("task %s worker START role=%s", taskID[:8], task.Role) }
			result = e.Runner.RunWithContext(taskCtx, prompt, agentWorkdir, toolsStr, taskTimeout, liveOutput, onPID, onStdin)
		}
		Log("timing", "task %s worker done in %.1fs (success=%v)", taskID[:8], time.Since(workerStart).Seconds(), result.Success)
		if e.Loggers != nil { e.Loggers.Engine("task %s worker DONE in %.1fs (success=%v exit=%d)", taskID[:8], time.Since(workerStart).Seconds(), result.Success, result.ExitCode) }
		// Clean up nested .whale created by whale exec in the agent sandbox.
		_ = os.RemoveAll(filepath.Join(agentWorkdir, ".whale"))
		e.mu.Lock()
		delete(e.activeCancels, taskID)
		delete(e.activeAgents, taskID)
		delete(e.activeStdinWriters, taskID)
		e.mu.Unlock()
		taskCancel()

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
		if defaultTeamLog != nil { defaultTeamLog.WorkerDone(task.ID, result.DurationSeconds, result.ExitCode, len(result.Stdout), result.Success) }

		// Persist subagent session ID for traceability.
		if result.SessionID != "" {
			_ = e.Store.UpdateTask(task.ID, map[string]interface{}{"session_id": result.SessionID})
		}

		// Self-split: only top-level tasks (no parents) can split.
		// Children must complete without further splitting.
		const splitMarker = "[SPLIT_PLAN]"
		if len(task.ParentIDs) == 0 && result.Success && strings.Contains(result.Stdout, splitMarker) {
			idx := strings.Index(result.Stdout, splitMarker)
			splitJSON := result.Stdout[idx+len(splitMarker):]
			childPlan, err := ParsePlanTasks(splitJSON)
			if err == nil && len(childPlan) > 0 {
				if defaultTeamLog != nil {
					Log("task", "task: %s self-split into %d children", task.ID[:8], len(childPlan))
				}
				var createdChildren []*Task
				for _, pt := range childPlan {
					child, err := e.CreateTask(pt.Title, pt.Description, task.Role, task.Profile, []string{task.ID}, 0, workdir, pt.VerifierFocus, task.BatchID, task.MasterTaskID)
					if err != nil {
						if defaultTeamLog != nil {
							Log("task", "task: %s child create failed: %v", task.ID[:8], err)
						}
						continue
					}
					child.Output = pt.Output
					child.BatchID = task.BatchID
					child.MasterTaskID = task.MasterTaskID
					_ = e.Store.UpdateTask(child.ID, map[string]interface{}{"batch_id": task.BatchID, "master_task_id": task.MasterTaskID})
					createdChildren = append(createdChildren, child)
					if defaultTeamLog != nil { Log("task", "task: %s child %s created", task.ID[:8], child.ID[:8]) }
				}
				e.writeTaskPlanJSON(task, createdChildren)
				// Mark parent as done (children carry the work forward).
				_ = e.Store.TransitionState(taskID, TaskStateDone, "", "self-split into children")
				e.fireEvent(TaskEvent{Type: EventStateChanged})
				return true, nil
			}
		}

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
		if err := e.Store.TransitionState(taskID, TaskStateProduced, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to produced: %w", err)
		}
		e.mu.Unlock()
		} // end if !skipProduce

		// Reset for subsequent retry iterations.
		skipProduce = false

	verifyPhase:
		// ---- Phase 2: Verifying ----------------------------------------
		var passed bool
		var feedback string
		var verifyDur time.Duration
		var v *Verifier

		// Transition to verifying (skip the separate checking phase —
		// the Verifier agent performs mechanical checks itself).
		e.mu.Lock()
		if err := e.Store.TransitionState(taskID, TaskStateVerifying, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to verifying: %w", err)
		}
		e.mu.Unlock()


		if task.UseDW {
			// Dynamic Workflow mode: N verifiers in parallel + Synthesizer.
			passed, _, feedback = e.runDWVerification(task)
		} else {
			verifierAgentName := e.resolveVerifierAgentName(task)
			verifierModel := ""
			if e.team != nil && e.team.Config != nil {
				verifierModel = e.team.Config.Model.VerifierDefault
			}
			verifyStart := time.Now()
		vKey := "verifier:" + taskID

		// Persistent Verifier session: reuse process across retries.
		if ws := e.persistentSessions[vKey]; ws != nil && e.shellSpawner != nil {
			if e.Loggers != nil { e.Loggers.Engine("task %s verifier CONTINUE session", taskID[:8]) }
			v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName)
			prompt := v.BuildPrompt(task)
			resp := e.shellSpawner.ContinueSession(ws, prompt)
			verifyDur = time.Since(verifyStart)
			passed, _ = parseVerdict(resp.Output)
			feedback = resp.Output
		} else if e.shellSpawner != nil {
			if e.Loggers != nil { e.Loggers.Engine("task %s verifier SPAWN persistent agent=%s", taskID[:8], verifierAgentName) }
			v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName)
			prompt := v.BuildPrompt(task)
			req := SubagentRequest{
				Task:      prompt,
				Role:      "verifier",
				AgentName: verifierAgentName,
				Workdir:   task.Workdir,
				Timeout:   time.Duration(e.Router.ResolveTimeout(task.Role, true)) * time.Second,
				MaxIters:  15,
				MaxCalls:  50,
				MaxTokens: effectiveMaxTokens(0, verifierModel),
			}
			if verifierModel != "" {
				req.Model = verifierModel
			}
			ws, resp := e.shellSpawner.SpawnPersistent(context.Background(), req)
			if ws != nil {
				e.persistentSessions[vKey] = ws
			}
			verifyDur = time.Since(verifyStart)
			passed, _ = parseVerdict(resp.Output)
			feedback = resp.Output
		} else {
			// Fallback: normal spawn via Runner.
			v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName)
			if e.Loggers != nil { e.Loggers.Engine("task %s verifier START agent=%s", taskID[:8], verifierAgentName) }
			passed, _, feedback, err = v.Verify(task)
			verifyDur = time.Since(verifyStart)
			if err != nil {
				return false, fmt.Errorf("verifier error: %w", err)
			}
		}
		if e.Loggers != nil { e.Loggers.Engine("task %s verifier DONE in %.1fs (pass=%v)", taskID[:8], verifyDur.Seconds(), passed) }
		}

		// Log verifier output for dashboard dialogue.
		if e.Loggers != nil {
			verdict := "PASS"
			if !passed {
				verdict = "FAIL"
			}
			var verifierPrompt string
			if v != nil { verifierPrompt = v.LastPrompt } else { verifierPrompt = "mechanical" }
			e.Loggers.LogAgent("verifier", taskID, attempt+1, verifierPrompt, fmt.Sprintf("[%s] %s", verdict, feedback), 0, verifyDur, nil)
			e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: taskID})
		}

		if passed {
			e.mu.Lock()
			if err := e.Store.TransitionState(taskID, TaskStateVerified, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to verified: %w", err)
			}
			if err := e.Store.TransitionState(taskID, TaskStateDone, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to done: %w", err)
			}
			if err := e.Store.UpdateTask(taskID, map[string]interface{}{
				"retry_count": attempt,
			}); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("update retry count: %w", err)
			}
			e.mu.Unlock()
			// Write verify file — file-based completion proof.
			if task.Output != "" {
				os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(taskID), "verify.md"), []byte(feedback), 0644)
			}
			// Record lesson for future agents with the same role.
			e.recordLesson(task.Role, task.Title, truncateLesson(feedback, 80))
			return true, nil
		}


		// Verification failed — prepare retry.
		e.mu.Lock()
		task, err = e.Store.GetTask(taskID)
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
		// Guard: empty feedback from a lazy verifier wastes Worker time.
			// Generate a meaningful default so the Worker knows what to fix.
			if len(strings.TrimSpace(feedback)) < 30 {
				feedback = "Previous attempt did not pass verification. " +
					"Please re-read the original task requirements carefully and " +
					"ensure ALL requirements are met. Verify your output is " +
					"complete — no truncated code, all functions implemented, " +
					"and the deliverable compiles or passes basic checks."
			}
		task.Description += fmt.Sprintf(
			"\n\n[VERIFIER FEEDBACK - Attempt %d]\n%s",
			task.RetryCount, feedback,
		)

		if err := e.Store.UpdateTask(taskID, map[string]interface{}{
			"retry_count":        task.RetryCount,
			"description":        task.Description,
			"verifier_feedback":  feedback,
		}); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("update task for retry: %w", err)
		}

		// 0 issues → pass immediately.
		currCount := len(ParseFindings(feedback))
		if currCount == 0 {
			e.mu.Unlock()
			e.closePersistentSession("worker:" + taskID)
			if task.Output != "" { os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(taskID), "verify.md"), []byte(feedback), 0644) }
			e.recordLesson(task.Role, task.Title, truncateLesson(feedback, 80))
			return true, nil
		}
		// Stagnation: same findings 2 rounds → suspend.
		if attempt >= 2 {
			prevFindings := ParseFindings(task.VerifierFeedback)
			currFindings := ParseFindings(feedback)
			if findingsAreSame(prevFindings, currFindings) {
				if err := e.Store.TransitionState(taskID, TaskStateSuspended, "same findings 2 rounds — early re-decomposition", feedback); err != nil {
					e.mu.Unlock()
					return false, fmt.Errorf("transition to suspended: %w", err)
				}
				e.mu.Unlock()
				return false, nil
			}
		}

		if task.RetryCount >= task.MaxRetries {
			// Don't fail — the leader should re-decompose just this task.
			if err := e.Store.TransitionState(taskID, TaskStateSuspended, "retries exhausted — needs re-decomposition", feedback); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to suspended: %w", err)
			}
			e.mu.Unlock()
			return false, nil
		}

		// Reset state back to assigned for retry.
		if err := e.Store.TransitionState(taskID, TaskStateAssigned, "", ""); err != nil {
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
// PlanAndRun accepts optional pre-decomposed planTasks.  When provided, the
// internal decompose step is skipped and the pre-computed plan is used directly.
func (e *TeamEngine) PlanAndRun(ctx context.Context, goal, workdir, masterTaskID string, preDecomposed ...PlanTask) ([]*Batch, error) {
	// Register a cancel for this execution so the dashboard stop button works.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	e.mu.Lock()
	e.masterTaskCancels[masterTaskID] = execCancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.masterTaskCancels, masterTaskID)
		e.mu.Unlock()
	}()

	// Scope files and logs under the master task directory.
	e.Whiteboard.SetMaster(masterTaskID)
	if e.Loggers != nil {
		masterDir := filepath.Join(e.Whiteboard.BaseDir(), masterTaskID)
		if err := e.Loggers.SetBaseDir(masterDir); err != nil {
			return nil, fmt.Errorf("set log dir: %w", err)
		}
	}

	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")

	var planTasks []PlanTask
	if len(preDecomposed) > 0 {
		planTasks = preDecomposed
		if defaultTeamLog != nil {
			Log("plan", "plan: using pre-decomposed plan: %d tasks in %d batches", len(planTasks), countBatches(planTasks))
		}
	} else {
		// Phase 0: Goal Elaboration — expand fuzzy goals into concrete specs.
		if defaultTeamLog != nil {
			Log("plan", "plan: elaborate START model=%s", leaderModel)
		}
		elaboratedGoal, elabErr := leader.Elaborate(goal, workdir, time.Duration(e.Router.ResolveDecomposerTimeout())*time.Second, leaderModel)
		if elabErr != nil {
			if defaultTeamLog != nil {
				Log("plan", "plan: elaborate FAIL: %v, continuing with raw goal", elabErr)
			}
			elaboratedGoal = goal
		}
		if elaboratedGoal != goal {
			if defaultTeamLog != nil {
				Log("plan", "plan: elaborate OK — %d chars", len(elaboratedGoal))
			}
		}

		if defaultTeamLog != nil {
			Log("plan", "plan: decompose START model=%s", leaderModel)
		}
		var err error
		planTasks, err = leader.Decompose(elaboratedGoal, workdir, decomposerTimeout, leaderModel)
		if err != nil {
			if defaultTeamLog != nil {
				Log("plan", "plan: decompose FAIL: %v", err)
			}
			return nil, fmt.Errorf("decompose goal: %w", err)
		}
		if len(planTasks) == 0 {
			if defaultTeamLog != nil {
				Log("plan", "plan: decompose EMPTY")
			}
			return nil, fmt.Errorf("plan is empty")
		}
		if defaultTeamLog != nil {
			Log("plan", "plan: decompose OK: %d tasks in %d batches", len(planTasks), countBatches(planTasks))
		}
		e.writePlanJSON(masterTaskID, planTasks)
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
			concurrency = e.Config.Batch.MaxAgents
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
				pt.VerifierFocus, bid, masterTaskID,
			)
			if err != nil {
				return nil, fmt.Errorf("create subtask %s: %w", pt.Title, err)
			}
			task.Output = pt.Output
			task.BatchID = bid
			task.UseDW = pt.UseDW
			task.VerifierRole = pt.VerifierRole
			task.MasterTaskID = masterTaskID
			e.Store.UpdateTask(task.ID, map[string]interface{}{
				"batch_id":       bid,
				"master_task_id": masterTaskID,
			})
			batch.Tasks = append(batch.Tasks, task)
		}
			// Propagate UseDW from tasks to batch: if any task uses DW,
			// the entire batch gets DW pipeline execution (multi-verifier per task).
			for _, t := range batch.Tasks {
				if t.UseDW {
					batch.UseDW = true
					break
				}
			}
		batches = append(batches, batch)
	}

	if defaultTeamLog != nil {
		for _, batch := range batches {
			Log("plan", "plan: batch %s created — %d tasks", batch.LabelOrID(), len(batch.Tasks))
		}
		Log("plan", "plan: %d batches ready, starting execution", len(batches))
	}

	// Write plan.md — structured overview of the goal and all batches/tasks.
	e.writePlanMarkdown(masterTaskID, goal, batches)

	// Notify dashboard that tasks have been created.
	e.fireEvent(TaskEvent{Type: EventStateChanged})

	// Step 3: Execute batches in order with dependency gating + CycleReport
	//         + cross-stage artifact passing (场景4).
	leader = NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	escalator := NewEscalator(leader.planner).WithLoggers(e.Loggers)
	completedBatches := make(map[string]bool)
	passedBatches := make(map[string]bool) // only CycleAccept; controls dep gating
	// Track completed batch outputs for cross-stage injection.
	completedBatchOutputs := make(map[string]string)

	for _, batch := range batches {
		// Check batch dependencies — use passedBatches so a failed batch
		// does NOT unblock downstream batches (Bug 1 fix).
		allDepsPassed := true
		for _, depID := range batch.DependsOn {
			if !passedBatches[depID] {
				allDepsPassed = false
				break
			}
		}
		if !allDepsPassed {
			batch.Status = BatchStatusFailed
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			return batches, fmt.Errorf("batch %s: dependency %v not satisfied", batch.ID, batch.DependsOn)
		}

		// Check for cancellation between batches.
		select {
		case <-execCtx.Done():
			// Suspend all non-terminal tasks so the dashboard shows them as
			// stopped rather than still running.
			for _, b := range batches {
				for _, t := range b.Tasks {
					if !t.State.IsTerminal() {
						_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
					}
				}
			}
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return batches, fmt.Errorf("cancelled: %w", ctx.Err())
		default:
		}

		// Run this batch with cycle support.
		cycleLimit := batch.MaxCycles
		if cycleLimit <= 0 {
			cycleLimit = 1 // at minimum one cycle
		}

		if batch.UseDW {
			// DW pipeline execution: multi-verifier per task, no Leader review.
			e.runDWCycle(ctx, batch, cycleLimit, masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches, escalator, workdir, decomposerTimeout, leaderModel)
			// Cross-stage artifact passing for completed DW batch.
			if completedBatches[batch.ID] {
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				if depArtifacts := e.buildStageArtifactContext(completedBatchOutputs, batch.DependsOn); depArtifacts != "" {
					for _, t := range batch.Tasks {
						updated := t.Description + depArtifacts
						e.Store.UpdateTask(t.ID, map[string]interface{}{"description": updated})
					}
				}
			}
		} else {
			// Normal execution with Leader review cycle.
			var prevFindings *CycleFindingsSet
			dryCount := 0

		batchCycleLoop:
			for cycle := 0; cycle < cycleLimit; cycle++ {
				batch.CycleCount = cycle + 1

				// Execute all tasks in this batch (parallel if concurrency > 0).
				if err := e.RunBatch(execCtx, batch); err != nil {
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
					return batches, fmt.Errorf("run batch %s cycle %d: %w", batch.ID, cycle, err)
				}

				// Pick up self-split children for the next cycle.
				for _, t := range batch.Tasks {
					children, err := e.Store.ListTasksByParent(t.ID)
					if err == nil && len(children) > 0 {
						for _, child := range children {
							if child.State == TaskStatePending || child.State == TaskStateAssigned {
								_ = e.Store.UpdateTask(child.ID, map[string]interface{}{"batch_id": batch.ID, "master_task_id": t.MasterTaskID})
								batch.Tasks = append(batch.Tasks, child)
							}
						}
					}
				}

				// Loop-until-dry: collect findings, exit when no new ones for 2 cycles.
				currentFindings := e.collectCycleFindings(batch, cycle+1)
				if prevFindings != nil && !currentFindings.HasNewFindings(prevFindings) {
					dryCount++
					if dryCount >= 2 {
						if e.Loggers != nil {
							e.Loggers.Engine("batch %s: dry after %d cycles", batch.ID, cycle+1)
						}
						completedBatches[batch.ID] = true
						completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
						e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
						break batchCycleLoop
					}
				} else {
					dryCount = 0
				}
				prevFindings = currentFindings

				// Check for tasks needing re-decomposition (retries exhausted).
				newTasks, recount := escalator.ProcessBatch(batch, masterTaskID, workdir, decomposerTimeout, leaderModel,
					func(id string) (*Task, error) { return e.Store.GetTask(id) },
					func(pt PlanTask, batchID, mtID string, parentIDs []string) (*Task, error) {
						profile := ToolProfile(pt.Profile)
						task, err := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, parentIDs, 0, workdir, pt.VerifierFocus, batchID, mtID)
						if err != nil {
							return nil, err
						}
						task.BatchID = batchID
						task.UseDW = pt.UseDW
						task.VerifierRole = pt.VerifierRole
						_ = e.Store.UpdateTask(task.ID, map[string]interface{}{"batch_id": batchID, "master_task_id": mtID})
						return task, nil
					},
					func(taskID string, state TaskState, reason string) error {
						return e.Store.ForceTransitionState(taskID, state, reason)
					},
				)
				// Pick up new re-decomposed tasks for the dashboard.
				if len(newTasks) > 0 {
					_ = e.Store.Checkpoint()
				}
				// Add new child tasks to the batch so RunBatch picks them up.
				batch.Tasks = append(batch.Tasks, newTasks...)
				// If new child tasks were created by the escalator, extend the
				// cycle limit and skip Leader review — child tasks must run
				// first before the Leader can judge the batch.
				if recount > 0 {
					if cycle == cycleLimit-1 {
						cycleLimit++
					}
					continue // skip Leader review, run child tasks next cycle
				}

				// Write board.md after each batch cycle.
				if boardContent := e.Whiteboard.BuildBoardContent(batches); boardContent != "" {
					_ = e.Whiteboard.WriteBoard(boardContent)
				}

				// Check for self-split children before declaring batch done.
				anyNew := false
				for _, t := range batch.Tasks {
					if children, _ := e.Store.ListTasksByParent(t.ID); len(children) > 0 {
						for _, c := range children {
							if c.State == TaskStatePending || c.State == TaskStateAssigned {
								c.BatchID = batch.ID
								_ = e.Store.UpdateTask(c.ID, map[string]interface{}{"batch_id": batch.ID, "master_task_id": t.MasterTaskID})
								batch.Tasks = append(batch.Tasks, c)
								anyNew = true
							}
						}
					}
				}
				if anyNew {
					if cycle == cycleLimit-1 {
						cycleLimit++ // extend limit for children
					}
					continue // run children in next cycle
				}

				// Batch done. Verifier already judged every task — skip Leader review.
				completedBatches[batch.ID] = true
				passedBatches[batch.ID] = true
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				break batchCycleLoop
				// Build and send CycleReport to Leader for review.
				report := e.buildCycleReport(batch, cycle+1)
				review, err := leader.ReviewCycle(goal, report, workdir, decomposerTimeout, leaderModel)
				if err != nil {
					// If review fails, accept by default (don't block pipeline).
					completedBatches[batch.ID] = true
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
					break batchCycleLoop
				}

				switch review.Decision {
				case CycleAccept:
					completedBatches[batch.ID] = true
						passedBatches[batch.ID] = true
						batch.Status = BatchStatusPassed
					// Cross-stage artifact passing:
					// Collect outputs from completed batch tasks so the
					// next batch can reference them.
					completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
					if depArtifacts := e.buildStageArtifactContext(completedBatchOutputs, batch.DependsOn); depArtifacts != "" {
						// Inject dependency artifacts into this batch's tasks.
						for _, t := range batch.Tasks {
							updated := t.Description + depArtifacts
							e.Store.UpdateTask(t.ID, map[string]interface{}{"description": updated})
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
							_ = e.Store.UpdateTask(t.ID, map[string]interface{}{
								"retry_count":       0,
								"max_retries":       t.MaxRetries + 3,
								"description":       t.Description + fmt.Sprintf("\n\n[Leader Feedback - Retry]\n%s", review.Feedback),
								"verifier_feedback": "",
							})
							_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "leader retry", "")
						} else {
							_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
						}
					}
					// Continue the cycle loop to retry.
					continue

				case CycleEscalate:
					// Send escalation request to user via inbox and wait.
					e.handleEscalation(batch, review)
					// After escalation, continue based on user decision.
					completedBatches[batch.ID] = true
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
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
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				} else {
					switch review.Decision {
					case CycleAccept:
						passedBatches[batch.ID] = true
						batch.Status = BatchStatusPassed
						completedBatches[batch.ID] = true
						completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
					default:
						// Leader still not satisfied — mark batch as failed but continue pipeline.
						batch.Status = BatchStatusFailed
						completedBatches[batch.ID] = true
					}
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
					if e.Loggers != nil {
							e.Loggers.LogLeader("review", fmt.Sprintf("Batch: %s\nDecision: %s (final)", batch.LabelOrID(), review.Decision), fmt.Sprintf("Final review: %s\nFeedback: %s", review.Decision, review.Feedback), decomposerTimeout, nil)
						e.fireEvent(TaskEvent{Type: EventLeaderLog})
					}
				}
			}
		}
		}

	// Step 4: Write final deliverable.md.
	if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
		_ = e.Whiteboard.WriteDeliverable(delContent)
	}

	// Step 5: Leader final summary — collect all outputs and produce
	// a user-facing summary of what was accomplished.
	e.assembleParentOutputs(batches)
	e.writeMasterOutput(goal, batches, workdir)

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

// runDWCycle executes a batch using DW pipeline mode: each task gets
// multi-verifier DW verification, there is no batch-level Leader review,
// and tasks complete independently (no barrier).  Only failed tasks are
// retried — passed tasks stay done.
func (e *TeamEngine) runDWCycle(
	ctx context.Context,
	batch *Batch,
	cycleLimit int,
	masterTaskID string,
	completedBatches map[string]bool,
	passedBatches map[string]bool,
	completedBatchOutputs map[string]string,
	batches []*Batch,
	escalator *Escalator,
	workdir string,
	decomposerTimeout time.Duration,
	leaderModel string,
) {
	var prevFindings *CycleFindingsSet
	dryCount := 0

	for cycle := 0; cycle < cycleLimit; cycle++ {
		batch.CycleCount = cycle + 1

		// Check cancellation between cycles.
		select {
		case <-ctx.Done():
			for _, t := range batch.Tasks {
				if !t.State.IsTerminal() {
					_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
				}
			}
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return
		default:
		}

		// Execute all tasks in parallel (each with DW verification if task.UseDW).
		if err := e.RunBatch(ctx, batch); err != nil {
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			return
		}

		if e.Loggers != nil {
			e.Loggers.Engine("DW batch %s cycle %d/%d: status=%s", batch.LabelOrID(), cycle+1, cycleLimit, batch.Status)
		}

		// Loop-until-dry: collect findings, exit when no new ones for 2 cycles.
		currentFindings := e.collectCycleFindings(batch, cycle+1)
		if prevFindings != nil && !currentFindings.HasNewFindings(prevFindings) {
			dryCount++
			if dryCount >= 2 {
				if e.Loggers != nil {
					e.Loggers.Engine("DW batch %s: dry after %d cycles", batch.ID, cycle+1)
				}
				completedBatches[batch.ID] = true
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				return
			}
		} else {
			dryCount = 0
		}
		prevFindings = currentFindings

		// Escalator: re-decompose stuck tasks (retries exhausted).
		newTasks, recount := escalator.ProcessBatch(batch, masterTaskID, workdir, decomposerTimeout, leaderModel,
			func(id string) (*Task, error) { return e.Store.GetTask(id) },
			func(pt PlanTask, batchID, mtID string, parentIDs []string) (*Task, error) {
				profile := ToolProfile(pt.Profile)
				task, err := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, parentIDs, 0, workdir, pt.VerifierFocus, batchID, mtID)
				if err != nil {
					return nil, err
				}
				task.BatchID = batchID
				task.UseDW = pt.UseDW
				task.VerifierRole = pt.VerifierRole
				_ = e.Store.UpdateTask(task.ID, map[string]interface{}{"batch_id": batchID, "master_task_id": mtID})
				return task, nil
			},
			func(taskID string, state TaskState, reason string) error {
				return e.Store.ForceTransitionState(taskID, state, reason)
			},
		)
		batch.Tasks = append(batch.Tasks, newTasks...)
		// Flush WAL so dashboard sees new re-decomposed tasks.
		if len(newTasks) > 0 {
			_ = e.Store.Checkpoint()
		}
		if recount > 0 {
			if cycle == cycleLimit-1 {
				cycleLimit++
			}
			continue // skip further processing, run child tasks next cycle
		}

		// Write board after each cycle.
		if boardContent := e.Whiteboard.BuildBoardContent(batches); boardContent != "" {
			_ = e.Whiteboard.WriteBoard(boardContent)
		}

		// Check if all tasks passed.  Only retry failed tasks (not suspended —
		// those are handled by Escalator).
		allPassed := true
		for _, t := range batch.Tasks {
			current, err := e.Store.GetTask(t.ID)
			if err != nil {
				continue
			}
			if current == nil {
				continue
			}
			if current.State == TaskStateFailed {
				allPassed = false
				_ = e.Store.ForceTransitionState(t.ID, TaskStateAssigned, "dw-retry")
			} else if current.State != TaskStateDone && current.State != TaskStateSuspended {
				allPassed = false
			}
		}

		if allPassed {
			completedBatches[batch.ID] = true
			completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			return
		}
	}

	// Max cycles reached — auto-accept whatever we have.
	completedBatches[batch.ID] = true
	completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
	e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
	if e.Loggers != nil {
		e.Loggers.Engine("DW batch %s: max cycles (%d) reached, auto-accepting", batch.ID, cycleLimit)
	}
}

// RunBatch executes all tasks in a batch in parallel, respecting the
// configured concurrency limit.// RunBatch executes all tasks in a batch in parallel, respecting the
// configured concurrency limit.
func (e *TeamEngine) RunBatch(ctx context.Context, batch *Batch) error {
	batch.Status = BatchStatusRunning
	if defaultTeamLog != nil { defaultTeamLog.BatchStart(batch.ID, batch.Label, len(batch.Tasks), batch.CycleCount, batch.MaxCycles) }

	tasks := batch.Tasks
	if len(tasks) == 0 {
		return nil // empty batch is a no-op; Leader review decides outcome
	}

	concurrency := batch.Concurrency
	if concurrency <= 0 {
		concurrency = e.Config.Batch.MaxAgents
	}
	if concurrency <= 0 || concurrency > len(tasks) {
		concurrency = len(tasks) // cap at task count; 0 = unlimited in config
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
					_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
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

		// Drain result channel to unblock goroutines.
		close(resultCh)
		for range resultCh {
		}


	// Estimate batch cost: worker + verifier tokens per task × rounds.
	for _, t := range batch.Tasks {
		batch.TotalTokens += (t.RetryCount + 1) * 10000 // ~10K tokens per worker+verifier round
	}
	// Batch status is determined by Leader review in the cycle loop above;
	// RunBatch only reports whether all tasks ran without errors.
	if defaultTeamLog != nil { defaultTeamLog.BatchDone(batch.ID, string(batch.Status), 0) }
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
	if defaultTeamLog != nil {
		Log("plan", "plan: decompose START model=%s", leaderModel)
	}
	planTasks, err := leader.Decompose(goal, workdir, decomposerTimeout, leaderModel)
	if err != nil {
		if defaultTeamLog != nil {
			Log("plan", "plan: decompose FAIL: %v", err)
		}
		return nil, fmt.Errorf("decompose goal: %w", err)
	}
	if len(planTasks) == 0 {
		if defaultTeamLog != nil {
			Log("plan", "plan: decompose EMPTY")
		}
		return nil, fmt.Errorf("plan is empty")
	}
	if defaultTeamLog != nil {
		Log("plan", "plan: decompose OK: %d tasks in %d batches", len(planTasks), countBatches(planTasks))
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
		task, err := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, parentIDs, 0, workdir, pt.VerifierFocus, "", "")
		if err != nil {
			return nil, fmt.Errorf("create subtask %d: %w", i, err)
		}
		task.VerifierRole = pt.VerifierRole
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
			task, err := e.Store.GetTask(tid)
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

// CancelTask cancels a running task: first signals the agent to stop via
// context cancellation, then force-kills the OS process after a grace period.
func (e *TeamEngine) CancelTask(taskID string) error {
	task, err := e.Store.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	if task.State.IsTerminal() {
		return fmt.Errorf("task %q is already in terminal state %s", taskID, task.State)
	}

	// Phase 1: signal agent to exit gracefully via context cancellation.
	e.mu.Lock()
	if cancel, ok := e.activeCancels[taskID]; ok {
		cancel()
		delete(e.activeCancels, taskID)
	}
	e.mu.Unlock()

	// Phase 2: after a grace period, force-kill the process if still alive.
	e.forceKillAfterGrace(taskID, 5*time.Second)

	// PENDING → ASSIGNED → FAILED (PENDING → FAILED is not valid alone).
	if task.State == TaskStatePending {
		if err := e.Store.TransitionState(taskID, TaskStateAssigned, "", ""); err != nil {
			return fmt.Errorf("transition to assigned: %w", err)
		}
	}

	return e.Store.TransitionState(taskID, TaskStateSuspended, "cancelled by user", "")
}

// forceKillAfterGrace waits for grace period then kills the agent process
// by PID if it hasn't exited yet.  Safe to call multiple times.
func (e *TeamEngine) forceKillAfterGrace(taskID string, grace time.Duration) {
	e.mu.Lock()
	pid, ok := e.activeAgents[taskID]
	e.mu.Unlock()
	if !ok || pid <= 0 {
		return
	}
	go func() {
		time.Sleep(grace)
		e.mu.Lock()
		currentPID, stillActive := e.activeAgents[taskID]
		if stillActive && currentPID == pid {
			delete(e.activeAgents, taskID)
		}
		e.mu.Unlock()
		if !stillActive || currentPID != pid {
			return // already cleaned up or replaced
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
			if defaultTeamLog != nil {
				Log("task", "task: %s force-killed PID=%d after grace period", taskID[:8], pid)
			}
		}
	}()
}

// KillAgentProcess immediately kills the agent process for a task.
// Prefer CancelTask for graceful shutdown; use this for cleanup.
func (e *TeamEngine) KillAgentProcess(taskID string) {
	e.mu.Lock()
	pid, ok := e.activeAgents[taskID]
	if ok {
		delete(e.activeAgents, taskID)
	}
	e.mu.Unlock()
	if !ok || pid <= 0 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
		if defaultTeamLog != nil {
			Log("task", "task: %s killed PID=%d", taskID[:8], pid)
		}
	}
}

// DeleteTask permanently removes a single task and its history from the database.
func (e *TeamEngine) DeleteTask(taskID string) error {
	return e.Store.DeleteTask(taskID)
}

// DeleteMasterTask deletes a master task and all its subtasks.
// Actively running subtasks are transitioned to failed before deletion.
func (e *TeamEngine) DeleteMasterTask(masterTaskID string) error {
	subtasks, err := e.Store.ListTasksByMasterTask(masterTaskID)
	if err != nil {
	return fmt.Errorf("list subtasks: %w", err)
	}
	for _, t := range subtasks {
		if t.State == TaskStateProducing || t.State == TaskStateChecking || t.State == TaskStateChecked || t.State == TaskStateVerifying {
			_ = e.Store.TransitionState(t.ID, TaskStateFailed, "deleted by user", "")
		}
		_ = e.Store.DeleteTask(t.ID)
	}
	if err := e.Store.DeleteMasterTask(masterTaskID); err != nil {
		return err
	}
	// Clean up whiteboard files — task directories, board, deliverable.
	var ids []string
	for _, t := range subtasks {
	ids = append(ids, t.ID)
	}
	e.Whiteboard.CleanupMasterTask(masterTaskID, ids)
	return nil
	}

// ApplyOutput copies the task's out/ directory contents to targetDir.
// This is the user-facing "apply to workspace" action. The original
// output files in the task directory are never modified or removed.
func (e *TeamEngine) ApplyOutput(taskID, targetDir string) error {
	srcDir := filepath.Join(e.Whiteboard.TaskDir(taskID), "out")
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("task %s has no output directory", taskID)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	return copyDir(srcDir, targetDir)
}

// copyDir recursively copies a directory tree.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

// runDWVerification executes Dynamic Workflow verification for a task:
// N parallel verifiers with different perspectives + Synthesizer merge.
func (e *TeamEngine) runDWVerification(task *Task) (passed bool, retry bool, feedback string) {
	workerOutput, _ := e.Whiteboard.ReadOutput(task.ID)
	if workerOutput == "" {
		return false, false, "worker output is empty"
	}

	// Perspectives: default set for code tasks, can be overridden by task.VerifierFocus.
	perspectives := []string{"correctness", "completeness"}
	if task.VerifierFocus != "" {
		perspectives = strings.Split(task.VerifierFocus, ",")
	}
	// Ensure we have at least 2 perspectives for DW verification.
	if len(perspectives) < 2 {
		perspectives = []string{"correctness", "completeness"}
	}

	// Run verifiers in parallel using goroutine + semaphore pattern.
	type verifierResult struct {
		perspective string
		output      string
		passed      bool
		isRetry     bool
		feedback    string
	}
	sem := make(chan struct{}, len(perspectives))
	results := make([]verifierResult, len(perspectives))
	var wg sync.WaitGroup

	for i, p := range perspectives {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, perspective string) {
			defer func() { <-sem; wg.Done() }()
			agentName := e.resolveVerifierAgentName(task)
			v := NewVerifier(e.Whiteboard, e.Runner, e.Router, 0).WithAgentName(agentName)
			// Set verifier focus for this perspective.
			t := *task // shallow copy
			t.VerifierFocus = strings.TrimSpace(perspective)
			r := verifierResult{perspective: perspective}
			r.passed, r.isRetry, r.output, _ = v.Verify(&t)
			// Parse findings for feedback synthesis.
			findings := ParseFindings(r.output)
			for j, f := range findings {
				if j > 0 {
					r.feedback += "; "
				}
				r.feedback += fmt.Sprintf("[%s] %s", f.Severity, f.Title)
			}
			if r.feedback == "" {
				r.feedback = r.output
			}
			results[idx] = r
		}(i, p)
	}
	wg.Wait()

	// Synthesizer: analyze all verifier outputs and produce a final verdict.
	synthPrompt := fmt.Sprintf(`You are a Verification Synthesizer. Multiple verifiers examined the same worker output from different perspectives. Merge their findings into a single verdict.

WORKER TASK: %s

VERIFIER REPORTS:
`, task.Title)
	passCount := 0
	for _, r := range results {
		synthPrompt += fmt.Sprintf("\n## %s\nVERDICT: %s\n%s\n", r.perspective, verdictLabel(r.passed, r.isRetry), r.feedback)
		if r.passed {
			passCount++
		}
	}
	synthPrompt += fmt.Sprintf(`
OUTPUT: VERDICT: PASS|FAIL|RETRY
SYNTHESIS: brief explanation
FINDINGS: key issues consolidated from all verifiers
`)

	synthOutput := e.Runner.RunVerifier(synthPrompt, task.Workdir, 120*time.Second, "", "")
	synthVerdict := extractVerdict(synthOutput.Stdout, passCount, len(results))

	// Write DW verification results to whiteboard.
	var dwLog strings.Builder
	dwLog.WriteString(fmt.Sprintf("# DW Verification — %s\n\n", task.Title))
	for _, r := range results {
		dwLog.WriteString(fmt.Sprintf("## Verifier: %s\nVERDICT: %s\n%s\n\n", r.perspective, verdictLabel(r.passed, r.isRetry), r.feedback))
	}
	dwLog.WriteString(fmt.Sprintf("## Synthesizer\n%s\n", synthOutput.Stdout))
	_ = e.Whiteboard.WriteVerifier(task.ID, dwLog.String())

	return synthVerdict.passed, synthVerdict.retry, synthOutput.Stdout
}

type dwVerdict struct{ passed, retry bool }

func verdictLabel(passed, retry bool) string {
	if passed { return "PASS" }
	if retry { return "RETRY" }
	return "FAIL"
}

func extractVerdict(output string, passCount, total int) dwVerdict {
	upper := strings.ToUpper(output)
	if strings.Contains(upper, "VERDICT: PASS") { return dwVerdict{passed: true} }
	if strings.Contains(upper, "VERDICT: RETRY") { return dwVerdict{retry: true} }
	// Majority vote fallback.
	if passCount > total/2 { return dwVerdict{passed: true} }
	return dwVerdict{}
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
		Sync:     false,
	})
	if err != nil {
		return fmt.Errorf("send feedback via prompt channel: %w", err)
	}

	// Write directly to the agent's stdin if it's running.
	e.mu.Lock()
	w, hasStdin := e.activeStdinWriters[taskID]
	e.mu.Unlock()
	if hasStdin {
		w.Write([]byte("\n\n[人类消息] " + message + "\n"))
	} else {
		// Fallback: write to messages/ directory.
		// Fallback: write to messages/ directory.
		msgDir := filepath.Join(e.Whiteboard.TaskDir(taskID), "messages")
		os.MkdirAll(msgDir, 0755)
		msgFile := filepath.Join(msgDir, fmt.Sprintf("%d.md", time.Now().UnixNano()))
		os.WriteFile(msgFile, []byte(message), 0644)
	}

	// Also update the verifier_feedback field for state tracking.
	return e.Store.UpdateTask(taskID, map[string]interface{}{
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
	case TaskStateChecking:
		return 55
	case TaskStateChecked:
		return 60
	case TaskStateVerifying:
		return 65
	case TaskStateVerified:
		return 85
	case TaskStateDone:
		return 100
	case TaskStateFailed:
		return 100
	case TaskStateSuspended:
		return 50 // interrupted mid-execution, actual progress unclear
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Agent Memory — 跨 session 经验复用
// ---------------------------------------------------------------------------

// recordLesson appends a one-line lesson to the role's memory file.
// Called after Verifier PASS so the next agent with the same role benefits.

// truncateLesson returns the first n runes of s.
func truncateLesson(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return s
}
// closePersistentSession closes and removes a persistent subprocess session.
func (e *TeamEngine) closePersistentSession(key string) {
	if e.shellSpawner == nil { return }
	ws := e.persistentSessions[key]
	if ws == nil { return }
	e.shellSpawner.CloseSession(ws)
	delete(e.persistentSessions, key)
}

// resolveVerifierAgentName resolves which agent definition to use for
// verifying a task.  Two-level lookup; returns "" when no Verifier is needed.
//
//	Level 1: task.VerifierRole (Leader explicitly assigned)
//	Level 2: Worker role → Verifier role mapping
//
// Returns "" for deterministic roles (formatter, evaluator, synthesizer)
// where the Checker's mechanical checks are sufficient.
func (e *TeamEngine) resolveVerifierAgentName(task *Task) string {
	// Level 1: Leader explicitly set VerifierRole.
	// An explicitly empty string means "no Verifier needed".
	if task.VerifierRole != "" {
		return task.VerifierRole
	}
	// If Leader explicitly set VerifierRole to "" on the PlanTask,
	// the task.VerifierRole will be "" and we skip the fallback.
	// (PlanTask always initialises the field; Leader must set it.)

	// Level 2: Worker role → Verifier role mapping.
	roleMap := map[AgentRole]string{
		RoleDeveloper:   "review",
		RoleTester:      "review",
		RoleReviewer:    "review",
		RoleResearcher:  "verifier",
		RoleWriter:      "verifier",
		RoleFormatter:   "verifier",
		RoleEvaluator:   "verifier",
		RoleSynthesizer: "verifier",
	}
	if agentName := roleMap[task.Role]; agentName != "" {
		return agentName
	}

	// Level 3: Team auto-match — scan team roles for a QA/test/review agent.
	if e.team != nil {
		for _, name := range e.team.Roles {
			lower := strings.ToLower(name)
			for _, kw := range []string{"qa", "test", "review", "verifier"} {
				if strings.Contains(lower, kw) {
					return name
				}
			}
		}
	}

	// Level 4: Builtin verifier (always available).
	return "verifier"
}

// verifierFeedbackForWorker strips tool details from the Verifier's full
// output, leaving only the verdict and issues — what the Worker needs to fix.
// The Worker's session should not see the Verifier's internal conversation.
func verifierFeedbackForWorker(fullOutput string) string {
	// Extract VERDICT line.
	verdict := ""
	if idx := strings.Index(fullOutput, "VERDICT:"); idx >= 0 {
		end := strings.Index(fullOutput[idx:], "\n")
		if end < 0 {
			end = len(fullOutput[idx:])
		}
		verdict = strings.TrimSpace(fullOutput[idx : idx+end])
	}
	// Extract ISSUES block.
	issues := ""
	if idx := strings.Index(fullOutput, "ISSUES:"); idx >= 0 {
		rest := fullOutput[idx:]
		// Stop at next section heading or FINDINGS.
		end := len(rest)
		for _, marker := range []string{"\n## ", "\nEVIDENCE:", "\nFINDINGS"} {
			if i := strings.Index(rest, marker); i >= 0 && i < end {
				end = i
			}
		}
		issues = strings.TrimSpace(rest[:end])
	}
	if verdict == "" && issues == "" {
		return "Verifier: see verify.md for details"
	}
	return fmt.Sprintf("[Verifier]: %s\n%s", verdict, issues)
}

func (e *TeamEngine) recordLesson(role AgentRole, title, lesson string) {
	if e.Store == nil {
		return
	}
	// Keep markdown for backward compatibility.
	line := fmt.Sprintf("- %s: %s — %s\n", time.Now().UTC().Format("2006-01-02"), title, lesson)
	memDir := filepath.Join(e.Store.baseDir, "memory")
	os.MkdirAll(memDir, 0755)
	path := filepath.Join(memDir, string(role)+".md")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)

	// Also write structured JSON with dedup.
	lesson = truncateLesson(lesson, 200)
	if !e.Store.HasMemory(role, title, lesson) {
		mem := &MemoryEntry{
			ID:         uuid.New().String(),
			AgentRole:  string(role),
			Key:        title,
			Content:    lesson,
			SourceTask: "",
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		}
		_ = e.Store.SaveMemory(mem)
	}
}

// BuildMemoryContext reads the role's historical lessons and injects them
// into the agent's inbox so it learns from past successes and failures.
// When key is non-empty, returns memories matching that key.  Otherwise
// returns the 10 most recent memories for the role.
func (e *TeamEngine) BuildMemoryContext(role AgentRole, key string) (string, error) {
	if e.Store == nil {
		return "", nil
	}
	var memories []MemoryEntry
	var err error
	if key != "" {
		memories, err = e.Store.GetMemories(role, key)
	} else {
		memories, err = e.Store.GetRecentMemories(role, 10)
	}
	if err != nil || len(memories) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("\n\n## 🧠 历史经验（同角色）\n")
	b.WriteString("以下是本角色在过去任务中的关键教训：\n\n")
	for _, m := range memories {
		b.WriteString(fmt.Sprintf("- %s: %s\n", m.CreatedAt[:10], m.Content))
	}
	b.WriteString("\n参考这些经验，避免重复错误。\n")
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
	task, err := e.Store.GetTask(taskID)
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
	history, _ := e.Store.GetTaskHistory(taskID)
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

	// Session ID — read from meta.json (persistence-only field).
	if meta, err := e.Store.readMeta(e.Store.taskDir(taskID)); err == nil && meta.SessionID != "" {
		result["session_id"] = meta.SessionID
	}

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
	tasks, err := e.Store.ListTasks()
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

// findingsAreSame returns true if two finding sets share the same IDs and
// severities, indicating the verifier reported the same issues again —
// the Worker did not fix anything new between rounds.
func findingsAreSame(prev, curr []Finding) bool {
	if len(prev) == 0 || len(curr) == 0 {
		return false
	}
	prevKeys := make(map[string]string, len(prev))
	for _, f := range prev {
		prevKeys[f.ID] = f.Severity
	}
	for _, f := range curr {
		if sev, ok := prevKeys[f.ID]; !ok || sev != f.Severity {
			return false
		}
	}
	// Must have at least one critical/major overlap
	for _, f := range curr {
		if f.Severity == "critical" || f.Severity == "major" {
			if _, ok := prevKeys[f.ID]; ok {
				return true
			}
		}
	}
	return false
}

func countBatches(tasks []PlanTask) int {
	seen := make(map[string]bool)
	for _, t := range tasks {
		bid := t.BatchID
		if bid == "" {
			bid = "default"
		}
		seen[bid] = true
	}
	return len(seen)
}

// writePlanMarkdown writes plan.md under the master task directory.
func (e *TeamEngine) writePlanMarkdown(masterTaskID, goal string, batches []*Batch) {
	path := filepath.Join(e.Whiteboard.BaseDir(), masterTaskID, "plan.md")
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	f.WriteString("# 项目计划\n\n")
	f.WriteString(fmt.Sprintf("## 目标\n\n%s\n\n", goal))
	f.WriteString(fmt.Sprintf("## 概览\n\n**批次:** %d | **任务:** %d\n\n", len(batches), countTasks(batches)))
	f.WriteString("---\n\n")

	for i, batch := range batches {
		label := batch.LabelOrID()
		if label == "" {
			label = batch.ID
		}
		f.WriteString(fmt.Sprintf("## Batch %d: %s\n\n", i+1, label))
		for _, t := range batch.Tasks {
			f.WriteString(fmt.Sprintf("- **%s** (%s)\n", t.Title, t.Role))
		}
		f.WriteString("\n")
	}
}

func countTasks(batches []*Batch) int {
	n := 0
	for _, b := range batches {
		n += len(b.Tasks)
	}
	return n
}

	// writeMasterOutput writes the master task's output.md by listing all
	// subtask outputs with file references.
	func (e *TeamEngine) writeMasterOutput(goal string, batches []*Batch, workdir string) {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("# %s\n\n", goal))
		b.WriteString("---\n\n")

		for _, batch := range batches {
			b.WriteString(fmt.Sprintf("## %s\n\n", batch.LabelOrID()))
			for _, t := range batch.Tasks {
				if t.Output == "" {
					b.WriteString(fmt.Sprintf("- **%s** (%s): 无产出路径\n", t.Title, t.Role))
					continue
				}
				b.WriteString(fmt.Sprintf("- **%s** (%s) → [%s](%s)\n", t.Title, t.Role, filepath.Base(t.Output), t.Output))
			}
			b.WriteString("\n")
		}

		path := filepath.Join(e.Whiteboard.BaseDir(), "output.md")
		os.WriteFile(path, []byte(b.String()), 0644)
		if defaultTeamLog != nil {
			Log("output", "wrote output.md (%d bytes)", b.Len())
		}
	}

// readTeamTemplate reads a template matching the task's output basename.
// E.g. output "docs/requirements.md" → templates/requirements.md
func (e *TeamEngine) readTeamTemplate(output string) string {
	if e.team == nil || e.team.TemplatesDir == "" || output == "" {
		return ""
	}
	name := filepath.Base(output)
	path := filepath.Join(e.team.TemplatesDir, name)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n## 📄 产出模板 (%s)\n按以下模板填写产出内容：\n\n%s\n", name, string(data))
}

// readTeamMemory reads role-specific memory from the team's memory directory.
func (e *TeamEngine) readTeamMemory(role AgentRole) string {
	if e.team == nil || e.team.MemoryDir == "" {
		return ""
	}
	path := filepath.Join(e.team.MemoryDir, string(role)+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n## 🧠 Team Memory (%s)\n%s\n", role, string(data))
}

// appendTeamMemory appends new lessons to the role's memory file.
func (e *TeamEngine) appendTeamMemory(role AgentRole, lesson string) {
	if e.team == nil || e.team.MemoryDir == "" || lesson == "" {
		return
	}
	os.MkdirAll(e.team.MemoryDir, 0755)
	path := filepath.Join(e.team.MemoryDir, string(role)+".md")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(fmt.Sprintf("\n## %s\n%s\n", time.Now().Format("2006-01-02 15:04"), lesson))
}

	// assembleParentOutputs finds tasks that were decomposed into children
	// (management nodes) and whose children are all done.  For each such
	// parent, it reads all child output files and concatenates them into the

	// writePlanJSON writes the decomposition plan as plan.json.
	// plan.json is the authoritative record of how a task was decomposed.
	func (e *TeamEngine) writePlanJSON(masterTaskID string, planTasks []PlanTask) {
		type planEntry struct {
			ID          string   `json:"id"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Role        string   `json:"role"`
			Output      string   `json:"output"`
			BatchID     string   `json:"batch_id"`
			DependsOn   []string `json:"depends_on,omitempty"`
		}
		entries := make([]planEntry, len(planTasks))
		for i, pt := range planTasks {
			entries[i] = planEntry{
				Title:       pt.Title,
				Description: pt.Description,
				Role:        pt.Role,
				Output:      pt.Output,
				BatchID:     pt.BatchID,
				DependsOn:   pt.DependsOnBatch,
			}
		}
		plan := map[string]interface{}{
			"generated": time.Now().UTC().Format(time.RFC3339),
			"tasks":     entries,
		}
		data, _ := json.MarshalIndent(plan, "", "  ")
		path := filepath.Join(e.Whiteboard.BaseDir(), masterTaskID, "plan.json")
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, data, 0644)
		if defaultTeamLog != nil {
			Log("plan", "wrote plan.json (%d tasks) → %s", len(planTasks), path)
		}
	}
	// writeTaskPlanJSON writes a per-task plan.json for a self-split task.
	func (e *TeamEngine) writeTaskPlanJSON(task *Task, children []*Task) {
		type childEntry struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Output      string `json:"output"`
		}
		entries := make([]childEntry, len(children))
		for i, c := range children {
			entries[i] = childEntry{
				ID:          c.ID,
				Title:       c.Title,
				Description: c.Description,
				Output:      c.Output,
			}
		}
		plan := map[string]interface{}{
			"parent_id": task.ID,
			"generated": time.Now().UTC().Format(time.RFC3339),
			"children":  entries,
		}
		data, _ := json.MarshalIndent(plan, "", "  ")
		taskDir := e.Whiteboard.TaskDir(task.ID)
		os.MkdirAll(taskDir, 0755)
		os.WriteFile(filepath.Join(taskDir, "plan.json"), data, 0644)
		if defaultTeamLog != nil {
			Log("task", "task: %s wrote plan.json (%d children)", task.ID[:8], len(children))
		}
	}
	// readPlanJSON reads a plan.json file and returns the list of PlanTasks.
	// Returns nil if the file doesn't exist or can't be parsed.
	func (e *TeamEngine) readPlanJSON(workdir string) []PlanTask {
		path := filepath.Join(workdir, ".whale", "plan.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var plan struct {
			Tasks []struct {
				Title       string   `json:"title"`
				Description string   `json:"description"`
				Role        string   `json:"role"`
				Output      string   `json:"output"`
				BatchID     string   `json:"batch_id"`
				DependsOn   []string `json:"depends_on"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(data, &plan); err != nil {
			return nil
		}
		planTasks := make([]PlanTask, len(plan.Tasks))
		for i, t := range plan.Tasks {
			planTasks[i] = PlanTask{
				Title:          t.Title,
				Description:    t.Description,
				Role:           t.Role,
				Output:         t.Output,
				BatchID:        t.BatchID,
				DependsOnBatch: t.DependsOn,
			}
		}
		return planTasks
	}

	// parent's output file, then writes the .verify file.
	func (e *TeamEngine) assembleParentOutputs(batches []*Batch) {
	for _, batch := range batches {
		for _, t := range batch.Tasks {
			children, err := e.Store.ListTasksByParent(t.ID)
			if err != nil || len(children) == 0 {
				continue
			}
			// Check if all children are done.
			allDone := true
			for _, c := range children {
				if c.State != TaskStateDone {
					allDone = false
					break
				}
			}
			if !allDone {
				continue
			}
			// Assemble parent output from children.
			if t.Output == "" {
				continue
			}
			var assembled strings.Builder
			assembled.WriteString(fmt.Sprintf("# %s\n\n", t.Title))
			assembled.WriteString(fmt.Sprintf("> 以下内容由 %d 个子任务产出自动拼接生成。\n\n", len(children)))
			for _, c := range children {
				out, err := e.Whiteboard.ReadOutput(c.ID)
				if err != nil || out == "" {
					// Try reading from file system directly.
					if c.Output != "" {
						data, fErr := os.ReadFile(c.Output)
						if fErr == nil {
							out = string(data)
						}
					}
				}
				if out == "" {
					assembled.WriteString(fmt.Sprintf("## %s\n\n*(无产出)*\n\n", c.Title))
					continue
				}
				assembled.WriteString(fmt.Sprintf("## %s\n\n", c.Title))
				assembled.WriteString(out)
				assembled.WriteString("\n\n")
			}
			// Write parent output + verify file.  Resolve relative paths
			// against the task workdir so files do not land in CWD.
			outputPath := t.Output
			if !filepath.IsAbs(outputPath) {
				outputPath = filepath.Join(t.Workdir, outputPath)
			}
			os.MkdirAll(filepath.Dir(outputPath), 0755)
			os.WriteFile(outputPath, []byte(assembled.String()), 0644)
			os.WriteFile(filepath.Join(filepath.Dir(outputPath), "verify.md"), []byte("auto-assembled from children"), 0644)
			if defaultTeamLog != nil {
				Log("task", "task: %s parent assembled from %d children → %s", t.ID[:8], len(children), t.Output)
			}
		}
	}
	}
