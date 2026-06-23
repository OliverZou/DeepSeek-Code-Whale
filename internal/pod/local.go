package pod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/team_engine"
)

// =========================================================================
// Logger
// =========================================================================

var (
	localLogFile *os.File
	localLogMu   sync.Mutex
)

func SetLogFile(path string) {
	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		localLogMu.Lock()
		if localLogFile != nil { localLogFile.Close() }
		localLogFile = f
		localLogMu.Unlock()
	}
}

func Log(cat, format string, args ...interface{}) {
	line := fmt.Sprintf("[%s] [%s] %s", time.Now().Format("2006-01-02T15:04:05Z07:00"), cat, fmt.Sprintf(format, args...))
	localLogMu.Lock()
	if localLogFile != nil {
		fmt.Fprintln(localLogFile, line)
	}
	localLogMu.Unlock()
	fmt.Println(line)
}

// =========================================================================
// EventBus (in-process pub/sub for Wails event loop)
// =========================================================================

const (
	TopicTeamEngine = "team-engine"
	TopicWorkspace  = "workspace"
	EventWSEngineReady = "engine_ready"
)

type Event struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type eventBus struct {
	mu   sync.RWMutex
	subs map[string][]chan Event
}

var globalBus = &eventBus{subs: make(map[string][]chan Event)}

func GlobalBus() *eventBus { return globalBus }

func (b *eventBus) Subscribe(topic string, buf int) chan Event {
	ch := make(chan Event, buf)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch
}

func (b *eventBus) Unsubscribe(topic string, ch chan Event) {
	b.mu.Lock()
	subs := b.subs[topic]
	for i, c := range subs {
		if c == ch {
			b.subs[topic] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	b.mu.Unlock()
}

func (b *eventBus) Publish(topic string, evt Event) {
	b.mu.RLock()
	subs := b.subs[topic]
	b.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- evt:
		default:
		}
	}
}

// =========================================================================
// TaskEvent (for Wails frontend)
// =========================================================================

type TaskEventType int

const (
	EventStateChanged TaskEventType = 0
	EventLeaderLog    TaskEventType = 4
	EventAgentLog     TaskEventType = 5
)

type TaskEvent struct {
	Type     TaskEventType `json:"type"`
	TaskID   string        `json:"task_id"`
	Title    string        `json:"title"`
	Progress int           `json:"progress"`
	NewState string        `json:"new_state,omitempty"`
}

// =========================================================================
// WorkspaceState
// =========================================================================

type WorkspaceState struct {
	ID         string
	Path       string
	Label      string
	Engine     *engine
	Registered time.Time
	LastSeen   time.Time
}

// =========================================================================
// Minimal engine wrapper (replaces old EngineShim)
// =========================================================================

// engine wraps a team_engine.FileTaskStore for read-only access.
type engine struct {
	store *team_engine.FileTaskStore
	dir   string
}

func newEngine(workspacePath string) *engine {
	tasksDir := filepath.Join(workspacePath, ".whale", "team_tasks")
	store, _ := team_engine.NewFileTaskStore(tasksDir)
	return &engine{store: store, dir: tasksDir}
}

func (e *engine) Close() {
	if e.store != nil {
		e.store.Close()
	}
}

func (e *engine) ListMasterTasks() ([]*team_engine.MasterTask, error) {
	if e.store == nil {
		return nil, nil
	}
	return e.store.ListMasterTasks()
}

func (e *engine) ListTasksByMasterTask(id string) ([]*team_engine.Task, error) {
	if e.store == nil {
		return nil, nil
	}
	return e.store.ListTasksByMasterTask(id)
}

func (e *engine) GetMasterTask(id string) (*team_engine.MasterTask, error) {
	if e.store == nil {
		return nil, nil
	}
	return e.store.GetMasterTask(id)
}

func (e *engine) GetTask(id string) (*team_engine.Task, error) {
	if e.store == nil {
		return nil, nil
	}
	return e.store.GetTask(id)
}

func (e *engine) ListTasks() ([]*team_engine.Task, error) {
	if e.store == nil {
		return nil, nil
	}
	return e.store.ListTasks()
}

func (e *engine) DeleteMasterTask(id string) error {
	if e.store == nil {
		return nil
	}
	return e.store.DeleteMasterTask(id)
}

func (e *engine) ListSuspendedMasterTasks() ([]*team_engine.MasterTask, error) {
	all, err := e.store.ListMasterTasks()
	if err != nil {
		return nil, err
	}
	var result []*team_engine.MasterTask
	for _, mt := range all {
		tasks, _ := e.store.ListTasksByMasterTask(mt.ID)
		for _, t := range tasks {
			if t.State == team_engine.TaskStateSuspended {
				result = append(result, mt)
				break
			}
		}
	}
	return result, nil
}

func (e *engine) CancelMasterTaskExecution(id string) bool { return false }
func (e *engine) Kill(ctx context.Context, taskID string) error {
	if e.store == nil {
		return nil
	}
	return e.store.ForceTransitionState(taskID, team_engine.TaskStateSuspended, "pod-kill")
}

// =========================================================================
// Legacy types (for Wails frontend compatibility)
// =========================================================================

type TaskJSON struct {
	WorkspaceID string `json:"workspace_id"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Role        string `json:"role"`
	ElapsedSec  int    `json:"elapsed_sec"`
	RetryCount  int    `json:"retry_count"`
	MaxRetries  int    `json:"max_retries"`
	ProgressPct int    `json:"progress_pct"`
}

type LogFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ListLogFiles returns log files for a task.
func (m *MultiEngineManager) ListLogFiles(wsID, taskID string) ([]LogFileJSON, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return nil, nil }

	tasksDir := filepath.Join(ws.Path, ".whale", "team_tasks")
	taskDir := filepath.Join(tasksDir, taskID)
	var files []LogFileJSON
	for _, name := range []string{"input.md", "output.md", "goal.md"} {
		p := filepath.Join(taskDir, name)
		if fi, err := os.Stat(p); err == nil { files = append(files, LogFileJSON{Name: name, Size: fi.Size()}) }
	}
	return files, nil
}

// ReadLogFile reads a log file for a task.
func (m *MultiEngineManager) ReadLogFile(wsID, taskID, fileID string) (string, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return "", fmt.Errorf("not found") }
	tasksDir := filepath.Join(ws.Path, ".whale", "team_tasks")
	data, err := os.ReadFile(filepath.Join(tasksDir, taskID, fileID))
	if err != nil { return "", err }
	return string(data), nil
}

// =========================================================================
// Misc
// =========================================================================

// IsTerminal reports whether a state is terminal.
func IsTerminal(s team_engine.TaskState) bool {
	return s == team_engine.TaskStateDone || s == team_engine.TaskStateFailed
}

// GetProgress maps a TaskState to a 0-100 integer.
func GetProgress(s team_engine.TaskState) int {
	switch s {
	case team_engine.TaskStateDone, team_engine.TaskStateFailed:
		return 100
	case team_engine.TaskStateVerified:
		return 85
	case team_engine.TaskStateVerifying:
		return 65
	case team_engine.TaskStateProduced:
		return 50
	case team_engine.TaskStateProducing:
		return 25
	case team_engine.TaskStateSuspended:
		return 50
	default:
		return 0
	}
}

// ResetForResume mirrors team_engine.ResetForResume.
func ResetForResume(s team_engine.TaskState) team_engine.TaskState {
	if s == team_engine.TaskStateFailed || s == team_engine.TaskStateSuspended {
		return team_engine.TaskStatePending
	}
	if s == team_engine.TaskStateDone {
		return team_engine.TaskStateDone
	}
	if s == team_engine.TaskStateVerified {
		return team_engine.TaskStateDone
	}
	return team_engine.TaskStateAssigned
}

// updateMetaState writes a new state to a task's meta.json.
func updateMetaState(workspacePath, taskID, newState string) {
	metaPath := filepath.Join(workspacePath, ".whale", "team_tasks", taskID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return
	}
	var meta map[string]interface{}
	if json.Unmarshal(data, &meta) != nil {
		return
	}
	meta["state"] = newState
	newData, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, newData, 0644)
}
