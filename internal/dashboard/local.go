package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// =========================================================================
// Local logger — replaces team_engine.Log
// =========================================================================

var (
	localLogFile *os.File
	localLogMu   sync.Mutex
)

// SetLogFile sets the local log output file.
func SetLogFile(path string) {
	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		localLogMu.Lock()
		if localLogFile != nil {
			localLogFile.Close()
		}
		localLogFile = f
		localLogMu.Unlock()
	}
}

// Log writes a diagnostic entry.  No-op if no log file is set.
func Log(cat, format string, args ...interface{}) {
	localLogMu.Lock()
	defer localLogMu.Unlock()
	if localLogFile == nil {
		return
	}
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(localLogFile, "[%s] [%s] %s\n", ts, cat, msg)
}

// =========================================================================
// Local TaskEvent — replaces team_engine.TaskEvent
// =========================================================================

// TaskEventType mirrors the team_engine event types the dashboard needs.
type TaskEventType int

const (
	EventStateChanged TaskEventType = 0
	EventLeaderLog    TaskEventType = 4
	EventAgentLog     TaskEventType = 5
)

// TaskEvent is a lightweight event sent from the whale CLI via WebSocket.
type TaskEvent struct {
	Type     TaskEventType `json:"type"`
	TaskID   string        `json:"task_id"`
	Title    string        `json:"title"`
	Progress int           `json:"progress"`
	NewState string        `json:"new_state,omitempty"`
}

// =========================================================================
// Local EventBus — replaces eventbus.Global
// =========================================================================

type localEventBus struct {
	mu   sync.RWMutex
	subs map[string][]chan Event
}

// Event is a simple topic+type+payload envelope.
type Event struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// BridgedEvent is sent over WebSocket between CLI and dashboard.
type BridgedEvent struct {
	Topic string `json:"topic"`
	Event Event  `json:"event"`
}

var defaultBus = &localEventBus{subs: make(map[string][]chan Event)}

// Subscribe returns a channel that receives events for the given topic.
func Subscribe(topic string, bufSize int) <-chan Event {
	if bufSize <= 0 {
		bufSize = 64
	}
	ch := make(chan Event, bufSize)
	defaultBus.mu.Lock()
	defaultBus.subs[topic] = append(defaultBus.subs[topic], ch)
	defaultBus.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscription.
func Unsubscribe(topic string, ch <-chan Event) {
	defaultBus.mu.Lock()
	defer defaultBus.mu.Unlock()
	subs := defaultBus.subs[topic]
	for i, s := range subs {
		if s == ch {
			defaultBus.subs[topic] = append(subs[:i], subs[i+1:]...)
			close(s)
			return
		}
	}
}

// Publish sends an event to all subscribers. Non-blocking.
func Publish(topic string, ev Event) {
	defaultBus.mu.RLock()
	subs := make([]chan Event, len(defaultBus.subs[topic]))
	copy(subs, defaultBus.subs[topic])
	defaultBus.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// PublishBridged publishes a BridgedEvent (for WebSocket bridge fan-out).
func PublishBridged(be BridgedEvent) {
	// bridged events go to subscribers of the topic
	Publish(be.Topic, be.Event)
}

// =========================================================================
// Topic constants
// =========================================================================

const (
	TopicTeamEngine = "team_engine"
	TopicWorkspace  = "workspace"
)

// =========================================================================
// State helper
// =========================================================================

// ResetForResume transitions stuck tasks back to runnable.
func ResetForResume(state string) string {
	switch state {
	case "failed", "suspended":
		return "pending"
	case "done":
		return "done"
	case "verified":
		return "done"
	default:
		return "assigned"
	}
}

// IsTerminal reports whether a state is terminal.
func IsTerminal(state string) bool {
	return state == "done" || state == "failed"
}

// GetProgress maps task state to a progress percentage.
func GetProgress(state string) int {
	switch state {
	case "pending", "assigned":
		return 0
	case "producing":
		return 25
	case "produced":
		return 50
	case "verifying":
		return 65
	case "verified":
		return 85
	case "done", "failed":
		return 100
	case "suspended":
		return 50
	default:
		return 0
	}
}

// =========================================================================
// Lightweight local types — replace team_engine.Task and MasterTask
// =========================================================================

// TaskLocal is a lightweight task struct for the dashboard cache.
type TaskLocal struct {
	ID               string
	Title            string
	Description      string
	Output           string
	Role             string
	State            string
	RetryCount       int
	MaxRetries       int
	ParentIDs        []string
	BatchID          string
	MasterTaskID     string
	CreatedAt        string
}

// MasterTaskLocal is a lightweight master task for the dashboard cache.
type MasterTaskLocal struct {
	ID            string
	Goal          string
	Status        string
	CreatedAt     string
	WorkspacePath string
}

// =========================================================================
// Engine shim — the dashboard no longer opens SQLite.
// WorkspaceState.Engine becomes a local shim that only holds cache refs.
// =========================================================================

// EngineShim replaces *team_engine.TeamEngine in WorkspaceState.
// It provides only the methods the dashboard actually calls.
type EngineShim struct {
	DB        *DBShim
	Whiteboard *WhiteboardShim
}


// WhiteboardShim replaces team_engine.Whiteboard.
type WhiteboardShim struct{}
func (w *WhiteboardShim) BaseDir() string { return "" }

// DBShim provides stub DB methods.  The dashboard no longer reads SQLite —
// all task data comes via WebSocket sync messages.
type DBShim struct{}

func (d *DBShim) ListMasterTasks() ([]*MasterTaskLocal, error)      { return nil, nil }
func (d *DBShim) ListTasksByMasterTask(id string) ([]*TaskLocal, error) { return nil, nil }
func (d *DBShim) GetMasterTask(id string) (*MasterTaskLocal, error)    { return nil, nil }
func (d *DBShim) UpdateMasterTaskStatus(id, status string) error       { return nil }
func (d *DBShim) Checkpoint() error                                     { return nil }
func (d *DBShim) ListTasksByParent(pid string) ([]*TaskLocal, error)   { return nil, nil }
func (d *DBShim) ForceTransitionState(id, state, reason string) error  { return nil }
func (d *DBShim) Kill(ctx interface{}, id string) error                { return nil }
func (d *DBShim) Close() error                                          { return nil }

func NewEngineShim() *EngineShim {
	return &EngineShim{DB: &DBShim{}, Whiteboard: &WhiteboardShim{}}
}

func (e *EngineShim) CancelMasterTaskExecution(id string) bool { return false }
func (e *EngineShim) Close() error                               { return nil }
func (e *EngineShim) ListMasterTasks() ([]*MasterTaskLocal, error) { return e.DB.ListMasterTasks() }
func (e *EngineShim) ListTasksByMasterTask(id string) ([]*TaskLocal, error) { return e.DB.ListTasksByMasterTask(id) }
func (e *EngineShim) GetMasterTask(id string) (*MasterTaskLocal, error)    { return e.DB.GetMasterTask(id) }
func (e *EngineShim) ListTasks() ([]*TaskLocal, error)                     { return nil, nil }
func (e *EngineShim) DeleteMasterTask(id string) error                     { return nil }
func (e *EngineShim) Kill(ctx interface{}, id string) error                { return nil }
func (e *EngineShim) GetTask(id string) (*TaskLocal, error)                { return nil, nil }
func (e *EngineShim) ListSuspendedMasterTasks() ([]string, error)          { return nil, nil }


// SetLoggerLocal is a no-op stub — dashboard uses its own logger.
func SetLoggerLocal(interface{}) {}

// AddLogFile is a no-op stub.
func AddLogFile(string) {}

// EnableBridge is a no-op — the dashboard's WebSocket handler manages bridge.
func EnableBridge() (chan BridgedEvent, chan<- BridgedEvent) {
	return nil, nil
}

// =========================================================================
// EventBus event type constants (used in bridge handler)
// =========================================================================

const (
	EventWSEngineReady = "engine_ready"
)

// =========================================================================
// Type aliases — let existing dashboard code compile without team_engine import
// =========================================================================

// TeamEngine shim — replaces *team_engine.TeamEngine
type TeamEngine = EngineShim
type Task = TaskLocal
type MasterTask = MasterTaskLocal
type AgentRole = string
type ToolProfile = string

// TaskState shim
type TaskState = string
const (
	TaskStatePending   TaskState = "pending"
	TaskStateAssigned  TaskState = "assigned"
	TaskStateProducing TaskState = "producing"
	TaskStateProduced  TaskState = "produced"
	TaskStateVerifying TaskState = "verifying"
	TaskStateVerified  TaskState = "verified"
	TaskStateDone      TaskState = "done"
	TaskStateFailed    TaskState = "failed"
	TaskStateSuspended TaskState = "suspended"
)

// PlanTask shim (for escalator code)
type PlanTask struct {
	Title           string
	Description     string
	Output          string
	Role            string
	BatchID         string
	BatchLabel      string
	DependsOnBatch  []string
	DependsOnIndex  int
	Profile         string
	VerifierFocus   string
	UseDW           bool
	MaxCycles       int
}

// Batch/BatchStatus shim
type BatchStatus = string
const (
	BatchStatusPending BatchStatus = "pending"
	BatchStatusRunning BatchStatus = "running"
	BatchStatusPassed  BatchStatus = "passed"
	BatchStatusFailed  BatchStatus = "failed"
)

type Batch struct {
	ID          string
	Label       string
	Tasks       []*Task
	DependsOn   []string
	Status      BatchStatus
	Concurrency int
	MaxCycles   int
	CycleCount  int
	UseDW       bool
}

func (b *Batch) LabelOrID() string {
	if b.Label != "" { return b.Label }
	return b.ID
}

// =========================================================================
// EventBus shim — replaces eventbus.Global()
// =========================================================================

type BusShim struct{}
func (b *BusShim) Subscribe(topic string, bufSize int) <-chan Event { return Subscribe(topic, bufSize) }
func (b *BusShim) Unsubscribe(topic string, ch <-chan Event) { Unsubscribe(topic, ch) }
func (b *BusShim) Publish(topic string, ev Event) { Publish(topic, ev) }
func (b *BusShim) PublishLocal(topic string, ev Event) { Publish(topic, ev) }

var globalBus = &BusShim{}
func GlobalBus() *BusShim { return globalBus }

// EnableGlobalBridge returns a read channel and write channel for the bridge.
func EnableGlobalBridge() (<-chan BridgedEvent, chan<- BridgedEvent) {
	ch := make(chan BridgedEvent, 128)
	return ch, ch
}

// =========================================================================
// Log shims — typed wrappers that the old code expects
// =========================================================================

func SpawnerType(role, kind, model string, maxTokens int) {
	Log("spawner", "role=%s type=%s model=%s maxTokens=%d", role, kind, model, maxTokens)
}
func WorkerDone(taskID string, dur float64, exitCode int, outputLen int, success bool) {
	Log("worker", "%s done dur=%.1fs exit=%d out=%d chars success=%v", taskID[:8], dur, exitCode, outputLen, success)
}
func WorkerStart(taskID, role, model string, attempt, maxRetries int) {
	Log("worker", "%s start role=%s model=%s attempt=%d/%d", taskID[:8], role, model, attempt, maxRetries)
}
func VerifierStart(taskID string) { Log("verifier", "%s start", taskID[:8]) }
func VerifierDone(taskID string, passed bool, dur float64) {
	v := "FAIL"
	if passed { v = "PASS" }
	Log("verifier", "%s done verdict=%s dur=%.1fs", taskID[:8], v, dur)
}
func BatchStart(batchID, label string, taskCount, cycle, maxCycles int) {
	Log("batch", "%s start label=%q tasks=%d cycle=%d/%d", batchID, label, taskCount, cycle, maxCycles)
}
func BatchDone(batchID string, status string, dur float64) {
	Log("batch", "%s done status=%s dur=%.1fs", batchID, status, dur)
}
func LeaderDecompose(goal, model string, attempt, maxTokens, promptTok, compTok int, dur float64, outputLen int, truncated, success bool) {
	Log("leader", "decompose goal=%q model=%s attempt=%d maxTok=%d prompt=%d comp=%d dur=%.1fs out=%d chars truncated=%v success=%v",
		goal[:min(60, len(goal))], model, attempt, maxTokens, promptTok, compTok, dur, outputLen, truncated, success)
}
func LeaderRetry(attempt int, reason string) { Log("leader", "retry attempt=%d: %s", attempt, reason) }
func EngineResumeTask(taskID, newState string) { Log("engine", "resume-task %s → %s", taskID[:8], newState) }
func EngineAutoResume(masterTaskID string, err error) {
	if err != nil { Log("engine", "auto-resume %s FAILED: %v", masterTaskID[:8], err) } else { Log("engine", "auto-resume %s started", masterTaskID[:8]) }
}
func EngineResume(masterTaskID, goal string, batchCount int, err error) {
	if err != nil { Log("engine", "resume %s FAILED: %v", masterTaskID[:8], err) } else { Log("engine", "resume %s ok", masterTaskID[:8]) }
}
func DashboardRegister(path, wsID string, err error) { Log("dashboard", "register path=%s → %s", path, wsID) }
func DashboardWSConnect(wsID string, ok bool, err error) { Log("dashboard", "ws connect %s", wsID) }
func DashboardWSDisconnect(wsID string) { Log("dashboard", "ws disconnect %s", wsID) }
func DashboardResumeMaster(wsID, taskID string, err error) {
	if err != nil { Log("dashboard", "resume master %s task=%s FAILED: %v", wsID, taskID, err) } else { Log("dashboard", "resume master %s task=%s OK", wsID, taskID) }
}
func CLIHeartbeat(wsID string, registered bool) { Log("cli", "heartbeat wsID=%s registered=%v", wsID, registered) }
func CLIWSConnect(wsID string, err error) { Log("cli", "ws connect %s ok", wsID) }
func CLIWSDisconnect(wsID string) { Log("cli", "ws disconnect %s", wsID) }
func CLIReceiveResume(masterTaskID string) { Log("cli", "receive resume %s", masterTaskID[:8]) }

func min(a, b int) int { if a < b { return a }; return b }

// =========================================================================
// Import shim — functions expected from team_engine package
// =========================================================================

// SetLogger / SetDefaultTeamLog — these set the global logger.
// The dashboard now uses its own local logger.
func SetLogger(interface{}) {}
func SetDefaultTeamLog(interface{}) {}
func AddLogWriter(string) {}
func NewTeamLogAt(string) interface{} { return nil }

// NewEngine opens a team engine — the dashboard uses EngineShim instead.
func NewEngine(dbPath, wbDir, configPath string, spawner interface{}) (*EngineShim, error) {
	return NewEngineShim(), nil
}
func CleanupInterruptedTasks(string) {}

// Escalator/Leader shims
type LeaderShim struct{}
func NewLeaderShim() *LeaderShim { return &LeaderShim{} }
type EscalatorShim struct{}
func NewEscalatorShim() *EscalatorShim { return &EscalatorShim{} }
func (e *EscalatorShim) ProcessBatch(batch *Batch, masterTaskID, workdir string, timeout time.Duration, model string, getTask func(string) (*Task, error), createTask func(PlanTask, string, string, []string) (*Task, error), transitionState func(string, TaskState, string) error) ([]*Task, int) {
	return nil, 0
}
