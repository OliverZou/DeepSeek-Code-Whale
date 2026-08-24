package team_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
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
	worktreeDir     string            // path to the repo for worktree creation
	activeTrees     map[string]string // taskID → branch name

	// activeCancels tracks cancel functions for running subagent spawns.
	activeCancels map[string]context.CancelFunc // taskID → cancel
	// activeAgents tracks the OS process ID for each running agent task.
	activeAgents       map[string]int            // taskID → PID
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

	// totalTokens accumulates real prompt+completion tokens across worker and
	// verifier spawns, so batch cost is reported from usage rather than the
	// rough (RetryCount+1)*10k estimate.
	totalTokens int

	// eventCallbacks — 订阅者列表，状态变化时主动推送
	eventCallbacks []TaskEventCallback

	// Current team configuration (optional).
	team               *TeamConfig
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
	// Resolve role display names and descriptions from the team's agent .md files.
	ResolveTeamRoles(tc)
}

// Team returns the current team configuration, or nil if none is set.
func (e *TeamEngine) Team() *TeamConfig {
	return e.team
}

// addTokens accumulates real prompt+completion tokens from a worker/verifier
// spawn.  Safe for concurrent use from the batch's task goroutines.
func (e *TeamEngine) addTokens(prompt, completion int) {
	if prompt <= 0 && completion <= 0 {
		return
	}
	e.mu.Lock()
	e.totalTokens += prompt + completion
	e.mu.Unlock()
}

// tokenTotal returns the accumulated token total.  Safe for concurrent use.
func (e *TeamEngine) tokenTotal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.totalTokens
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
		Store:              store,
		Whiteboard:         wb,
		Config:             cfg,
		Router:             NewRouter(cfg),
		Runner:             runner,
		Escalation:         NewEscalationManager(),
		Loggers:            loggers,
		timeout:            defaultTimeout,
		activeTrees:        make(map[string]string),
		activeCancels:      make(map[string]context.CancelFunc),
		activeAgents:       make(map[string]int),
		activeStdinWriters: make(map[string]io.WriteCloser),
		masterTaskCancels:  make(map[string]context.CancelFunc),
		shutdownCtx:        shutdownCtx,
		persistentSessions: make(map[string]*PersistentSession),
		shutdownCancel:     shutdownCancel,
	}

	// cleanupInterruptedTasks is called separately by the CLI on startup.
	// Engine instances created for sync/dashboard must never modify state.
	_ = spawner

	if ss, ok := spawner.(*ShellSubagentSpawner); ok {
		eng.shellSpawner = ss
	}
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

	// Close any lingering persistent subprocess sessions (worker/verifier) so
	// their child processes exit instead of idling on an open stdin pipe.
	for key := range e.persistentSessions {
		e.closePersistentSession(key)
	}

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
	escalator := NewEscalator().WithLoggers(e.Loggers)
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
			// Resume runs batches serially — a standalone mutex suffices; the
			// parameter exists so the shared DW path is concurrency-safe.
			e.runDWCycle(ctx, batch, cycleLimit, masterTaskID, completedBatches, passedBatches, cp.CompletedOutputs, batches, escalator, workdir, decomposerTimeout, leaderModel, &sync.Mutex{})
		} else {
			batchStart := time.Now()
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
			BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
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

// propagateTaskOutput copies a completed task's sandboxed out/ output back into
// its declared workdir. Non-worktree tasks run in an out/ sandbox, so their
// results must be copied to the user's workspace to take effect. Relative
// workdirs resolve against the process cwd (where `team execute` was invoked).
// Best-effort: a failure here is logged by the caller, not fatal.
func (e *TeamEngine) propagateTaskOutput(taskID, workdir string) error {
	if workdir == "" {
		workdir = "."
	}
	if !filepath.IsAbs(workdir) {
		abs, err := filepath.Abs(workdir)
		if err != nil {
			return err
		}
		workdir = abs
	}
	return e.ApplyOutput(taskID, workdir)
}

// copyDir recursively copies a directory tree.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip Whale's own state dir — a worker subprocess creates .whale/ in its
		// sandbox (incl. an empty team_engine.log); propagating it would clobber
		// the master's live team_engine.log with the empty sandbox copy.
		if info.IsDir() && info.Name() == ".whale" {
			return filepath.SkipDir
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

	synthOutput := e.Runner.RunVerifier(synthPrompt, task.Workdir, 120*time.Second, 15, 50, 0, "", "")
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
	if passed {
		return "PASS"
	}
	if retry {
		return "RETRY"
	}
	return "FAIL"
}

func extractVerdict(output string, passCount, total int) dwVerdict {
	upper := strings.ToUpper(output)
	if strings.Contains(upper, "VERDICT: PASS") {
		return dwVerdict{passed: true}
	}
	if strings.Contains(upper, "VERDICT: RETRY") {
		return dwVerdict{retry: true}
	}
	// Majority vote fallback.
	if passCount > total/2 {
		return dwVerdict{passed: true}
	}
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
	if e.shellSpawner == nil {
		return
	}
	ws := e.persistentSessions[key]
	if ws == nil {
		return
	}
	e.shellSpawner.CloseSession(ws)
	delete(e.persistentSessions, key)
}

// resolveVerifierAgentName resolves which agent definition to use for
// verifying a task.  Four-level lookup; always returns a non-empty agent
// name (falling back to the builtin "verifier").
//
//	Level 1: task.VerifierRole (Leader explicitly assigned)
//	Level 2: Worker role → Verifier role mapping
//	         (code roles developer/tester/reviewer → "review";
//	          content roles → "verifier")
//	Level 3: Team auto-match — scan team roles for a QA/test/review agent
//	Level 4: Builtin "verifier"
//
// Code-type roles map to the "review" agent, which runs the mechanical
// build/lint/test checks itself via tools — there is no separate
// deterministic Checker phase.
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
//
// Falls back to the legacy markdown file when no structured JSON memories
// exist yet (e.g. after upgrading from a version that only wrote .md files).
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
		// Fallback: read legacy markdown file for backward compatibility.
		return e.buildMemoryContextFromMarkdown(role)
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

// buildMemoryContextFromMarkdown reads the legacy markdown memory file.
// This is a fallback for deployments that have .md files from before the
// JSON memory store was introduced.
func (e *TeamEngine) buildMemoryContextFromMarkdown(role AgentRole) (string, error) {
	path := filepath.Join(e.Store.baseDir, "memory", string(role)+".md")
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "", nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	var b strings.Builder
	b.WriteString("\n\n## 🧠 历史经验（同角色）\n")
	b.WriteString("以下是本角色在过去任务中的关键教训：\n\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
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
