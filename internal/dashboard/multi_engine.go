// Package dashboard provides a multi-workspace team-engine dashboard server
// that aggregates tasks from all registered Whale instances on the machine.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/usewhale/whale/internal/eventbus"
	"github.com/usewhale/whale/internal/team_engine"
	teampglog "github.com/usewhale/whale/internal/team_engine/log"
	"golang.org/x/sys/windows"
)
    
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WorkspaceState tracks one registered workspace and its TeamEngine.
type WorkspaceState struct {
	ID         string
	Path       string
	Label      string
	Engine     *team_engine.TeamEngine
	Registered time.Time
	LastSeen   time.Time
}

// WorkspaceJSON is the JSON representation sent to the frontend.
type WorkspaceJSON struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Label      string `json:"label"`
	Registered string `json:"registered"`
	LastSeen   string `json:"last_seen"`
	Online     bool   `json:"online"`
	TaskCount  int    `json:"task_count"`
	ActiveCount int   `json:"active_count"`
}

// MasterTaskJSON is the JSON representation of a master task (总任务).
type MasterTaskJSON struct {
	ID             string `json:"id"`
	Goal           string `json:"goal"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspacePath  string `json:"workspace_path"`
	WorkspaceLabel string `json:"workspace_label"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	TaskCount      int    `json:"task_count"`
	DoneCount      int    `json:"done_count"`
	ActiveCount    int    `json:"active_count"`
	SuspendedCount int    `json:"suspended_count"`
	WorkspaceOnline bool  `json:"workspace_online"`
}

// SubtaskJSON is a subtask in a master task's plan.
type SubtaskJSON struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Role        string        `json:"role"`
	State       string        `json:"state"`
	Progress    int           `json:"progress"`
	CreatedAt   string        `json:"created_at"`
	ParentIDs   []string      `json:"parent_ids"`
	BatchID     string        `json:"batch_id"`
	RetryCount  int           `json:"retry_count"`
	MaxRetries  int           `json:"max_retries"`
	Children    []SubtaskJSON `json:"children,omitempty"`
}

// AgentDialogueJSON is one message in the agent's conversation.
type AgentDialogueJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TaskJSON is a per-workspace task summary sent to the frontend.
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

// LogFileJSON describes one log file available for a task.
type LogFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// MultiEngineManager manages a map of workspace ID → WorkspaceState.
// It is safe for concurrent use.
type MultiEngineManager struct {
	mu           sync.RWMutex
	states       map[string]*WorkspaceState
	seq          int64
	eventHandler func(event team_engine.TaskEvent) // 可选的事件回调

	// pendingResume maps wsID → masterTaskID for commands waiting to be
	// picked up by the main Whale CLI via heartbeat response.
	pendingResume map[string]string
    
	// wsConns tracks active WebSocket connections keyed by workspace ID.
	wsConns map[string]*websocket.Conn

	// EventBus bridge fan-out: one goroutine reads from the global
	// bridgeOut and fans out to all connected whale CLI processes.
	bridgeFanOut    sync.Once
	bridgeWriters   map[string]chan<- []byte // wsID → write channel
	bridgeWritersMu sync.Mutex

	// dashboardDir is the directory containing the dashboard executable,
	// used for persisting workspace discovery data (workspaces.json).
	dashboardDir string
}

// OnEngineEvent 注册一个事件回调，当 engine 有任务状态变化时触发。
// 由 Wails App 调用，用于向前端推送实时更新。
func (m *MultiEngineManager) OnEngineEvent(handler func(event team_engine.TaskEvent)) {
	m.mu.Lock()
	m.eventHandler = handler
	m.mu.Unlock()
}

// QueueResume pushes a resume command to the workspace's WebSocket
// connection immediately.  Falls back to heartbeat-based delivery
// if the client is not connected via WebSocket.
func (m *MultiEngineManager) QueueResume(wsID, masterTaskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.states[wsID]

	msg, _ := json.Marshal(map[string]string{
		"command":         "resume",
		"master_task_id": masterTaskID,
	})

	// Push via WebSocket if connected (millisecond delivery).
	if conn, ok := m.wsConns[wsID]; ok {
		werr := conn.WriteMessage(websocket.TextMessage, msg)
			if werr == nil {
			m.logResumeDiag(ws, "resume queued via ws for %s task=%s", wsID, masterTaskID)
			return
		}
		m.logResumeDiag(ws, "ws write failed for %s: %v", wsID, werr)
		// Connection dead, remove it.
		delete(m.wsConns, wsID)
	} else {
		m.logResumeDiag(ws, "no ws conn for %s (have %d conns)", wsID, len(m.wsConns))
		for k := range m.wsConns {
			log.Printf("dashboard:   conn: %s", k)
		}
	}

	// Fallback: store for heartbeat-based delivery.
	m.logResumeDiag(ws, "resume stored in pendingResume for %s task=%s", wsID, masterTaskID)
	if m.pendingResume == nil {
		m.pendingResume = make(map[string]string)
	}
	m.pendingResume[wsID] = masterTaskID
}

// DequeueResume returns and clears any pending resume command for wsID.
func (m *MultiEngineManager) DequeueResume(wsID string) (masterTaskID string, ok bool) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.pendingResume == nil {
        return "", false
    }
    mtID, exists := m.pendingResume[wsID]
    if !exists {
        return "", false
    }
    delete(m.pendingResume, wsID)
    return mtID, true
}

// RegisterWSConn registers a WebSocket connection for a workspace.
func (m *MultiEngineManager) RegisterWSConn(wsID string, conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wsConns == nil {
		m.wsConns = make(map[string]*websocket.Conn)
	}
	// Close any existing connection for this workspace.
	if old, ok := m.wsConns[wsID]; ok {
		old.Close()
	}
	m.wsConns[wsID] = conn
}

// UnregisterWSConn removes a WebSocket connection for a workspace.
func (m *MultiEngineManager) UnregisterWSConn(wsID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.wsConns, wsID)
}

// startBridgeFanOut ensures a single goroutine reads from the global
// EventBus bridgeOut and fans events out to all connected WebSocket
// clients.  Safe to call multiple times (runs once).
func (m *MultiEngineManager) startBridgeFanOut() {
	m.bridgeFanOut.Do(func() {
		bridgeOut, _ := eventbus.EnableGlobalBridge()
		log.Printf("dashboard: bridge fan-out started")
		go func() {
			for be := range bridgeOut {
				data, err := json.Marshal(be)
				if err != nil {
					continue
				}
				if team_engine.DefaultTeamLog != nil {
					team_engine.DefaultTeamLog.Log("bridge", "dashboard → ws: topic=%s type=%s writers=%d", be.Topic, be.Event.Type, len(m.bridgeWriters))
				}
				m.bridgeWritersMu.Lock()
				for wsID, ch := range m.bridgeWriters {
					select {
					case ch <- data:
					default:
						log.Printf("dashboard: bridge fan-out drop for %s (slow consumer)", wsID)
					}
				}
				m.bridgeWritersMu.Unlock()
			}
		}()
	})
}

// registerBridgeWriter registers a WebSocket write channel for EventBus bridge fan-out.
func (m *MultiEngineManager) registerBridgeWriter(wsID string) chan []byte {
	ch := make(chan []byte, 64)
	m.bridgeWritersMu.Lock()
	m.bridgeWriters[wsID] = ch
	m.bridgeWritersMu.Unlock()
	m.startBridgeFanOut()
	return ch
}

// unregisterBridgeWriter removes a WebSocket write channel.
func (m *MultiEngineManager) unregisterBridgeWriter(wsID string) {
	m.bridgeWritersMu.Lock()
	delete(m.bridgeWriters, wsID)
	m.bridgeWritersMu.Unlock()
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and registers
// it for the workspace identified by ?wsid= query parameter.
func (m *MultiEngineManager) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("wsid")
	if wsID == "" {
		http.Error(w, "missing wsid", http.StatusBadRequest)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("dashboard: ws upgrade for %s: %v", wsID, err)
		return
	}

	m.RegisterWSConn(wsID, conn)
	if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardWSConnect(wsID, true, nil) }
	log.Printf("dashboard: ws connected for workspace %s", wsID)

	// Notify eventLoop so the frontend sees WorkspaceOnline=true immediately.
	eventbus.Global().Publish(eventbus.TopicWorkspace, eventbus.Event{Type: "ws_connected", Payload: map[string]string{"id": wsID}})

	// Enable cross-process EventBus bridge.
	_, bridgeIn := eventbus.EnableGlobalBridge()

	// Register for bridge fan-out: dashboard-side EventBus events
	// are broadcast to all connected whale CLI processes.
	bridgeCh := m.registerBridgeWriter(wsID)
	defer m.unregisterBridgeWriter(wsID)

	// Writer goroutine: forward bridge events to this whale CLI.
	go func() {
		for data := range bridgeCh {
			if werr := conn.WriteMessage(websocket.TextMessage, data); werr != nil {
				return
			}
		}
	}()

	// Read loop — receive events from whale CLI and inject them into
	// the dashboard's EventBus.  Also handles legacy TaskEvent format.
	go func() {
		defer func() {
			conn.Close()
			m.UnregisterWSConn(wsID)
			if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardWSDisconnect(wsID) }
			log.Printf("dashboard: ws disconnected for workspace %s", wsID)
			eventbus.Global().Publish(eventbus.TopicWorkspace, eventbus.Event{Type: "ws_disconnected", Payload: map[string]string{"id": wsID}})
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// Try BridgedEvent format first (cross-process EventBus).
			var be eventbus.BridgedEvent
			if err := json.Unmarshal(msg, &be); err == nil && be.Topic != "" {
				if team_engine.DefaultTeamLog != nil {
					team_engine.DefaultTeamLog.Log("bridge", "dashboard ← ws(%s): topic=%s type=%s", wsID, be.Topic, be.Event.Type)
				}
				select {
				case bridgeIn <- be:
				default:
					log.Printf("dashboard: bridgeIn full, dropped topic=%s type=%s from ws=%s", be.Topic, be.Event.Type, wsID)
				}
				continue
			}
			// Fallback: legacy TaskEvent format.
			var event team_engine.TaskEvent
			if err := json.Unmarshal(msg, &event); err == nil && event.Type > 0 {
				m.fireEngineEvent(event)
			}
		}
	}()
}

// wireEngine 将 m.eventHandler 注册到 engine 上。
func (m *MultiEngineManager) wireEngine(eng *team_engine.TeamEngine) {
	if eng == nil {
		return
	}
	// Make a local copy of the manager pointer for the closure.
	mgr := m
	eng.OnEvent(func(event team_engine.TaskEvent) {
		mgr.fireEngineEvent(event)
	})
}

// fireEngineEvent 向已注册的事件处理函数发送事件。
func (m *MultiEngineManager) fireEngineEvent(event team_engine.TaskEvent) {
	m.mu.RLock()
	h := m.eventHandler
	m.mu.RUnlock()
	if h != nil {
		h(event)
	}
}

// NewMultiEngineManager creates an empty manager.
func NewMultiEngineManager(dashboardDir string) *MultiEngineManager {
	return &MultiEngineManager{
		states:        make(map[string]*WorkspaceState),
		dashboardDir:  dashboardDir,
		bridgeWriters: make(map[string]chan<- []byte),
	}
}

// Register adds a workspace.  Returns the assigned ID and any error.
// If the engine cannot be opened (db doesn't exist yet), it still registers
// but engine will be nil — it gets lazy-loaded on next heartbeat.
func (m *MultiEngineManager) Register(workspacePath string) (*WorkspaceState, error) {
	workspacePath = filepath.Clean(workspacePath)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Deduplicate by path.
			for _, ws := range m.states {
		if pathsEqual(ws.Path, workspacePath) {
			ws.LastSeen = time.Now()
			// Try lazy-load engine if the db was created after first registration.
			if ws.Engine == nil {
				dbPath := filepath.Join(workspacePath, ".whale", "team_engine.db")
				wbDir := filepath.Join(workspacePath, ".whale", "team_tasks")
				if _, err := os.Stat(dbPath); err == nil {
					eng, err := team_engine.New(dbPath, wbDir, "", nil)
					if err != nil {
						log.Printf("dashboard: lazy open engine for %s: %v", workspacePath, err)
					} else {
						m.wireEngine(eng)
						ws.Engine = eng
					}
				}
			}
			return ws, nil
		}
	}

	m.seq++
	id := fmt.Sprintf("ws_%04d", m.seq)
	dbPath := filepath.Join(workspacePath, ".whale", "team_engine.db")
	wbDir := filepath.Join(workspacePath, ".whale", "team_tasks")

	var eng *team_engine.TeamEngine
	if _, err := os.Stat(dbPath); err == nil {
		eng, err = team_engine.New(dbPath, wbDir, "", nil)
		if err != nil {
			log.Printf("dashboard: open engine for %s: %v", workspacePath, err)
		}
	}
	m.wireEngine(eng)

	if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardRegister(workspacePath, id, nil) }
	if team_engine.DefaultTeamLog != nil {
		// Wire workspace log as aux so dashboard-global events also
		// appear in the workspace log.
		wsLog := filepath.Join(workspacePath, ".whale", "team_tasks", "logs", "team_engine.log")
		team_engine.DefaultTeamLog.AddLog(wsLog)
	} else {
		team_engine.SetDefaultTeamLog(teampglog.NewTeamLog(workspacePath))
	}
	ws := &WorkspaceState{
		ID:         id,
		Path:       workspacePath,
		Label:      filepath.Base(workspacePath),
		Engine:     eng,
		Registered: time.Now(),
		LastSeen:   time.Now(),
	}
	m.states[id] = ws
	m.saveWorkspacePath(workspacePath)
	log.Printf("dashboard: registered workspace %s → %s", id, workspacePath)
	return ws, nil
}

// Heartbeat updates LastSeen and tries to open the engine if it was nil.
func (m *MultiEngineManager) Heartbeat(wsID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ws, ok := m.states[wsID]
	if !ok {
		return fmt.Errorf("workspace %s not found", wsID)
	}
	ws.LastSeen = time.Now()

	// Lazy-load engine if the db was created after registration.
	if ws.Engine == nil {
		dbPath := filepath.Join(ws.Path, ".whale", "team_engine.db")
		wbDir := filepath.Join(ws.Path, ".whale", "team_tasks")
		if _, err := os.Stat(dbPath); err == nil {
			eng, err := team_engine.New(dbPath, wbDir, "", nil)
			if err != nil {
				log.Printf("dashboard: lazy open engine for %s: %v", ws.Path, err)
			} else {
				m.wireEngine(eng)
				ws.Engine = eng
			}
		}
	}
	return nil
}

// Deregister removes a workspace and closes its engine.
func (m *MultiEngineManager) Deregister(wsID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ws, ok := m.states[wsID]
	if !ok {
		return fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine != nil {
		_ = ws.Engine.Close()
	}
	delete(m.states, wsID)
	log.Printf("dashboard: deregistered workspace %s (%s)", wsID, ws.Label)
	return nil
}

// LoadEngineByPath finds a workspace by its filesystem path and (re)loads the
// engine.  Called in response to engine_ready events from the EventBus.
func (m *MultiEngineManager) LoadEngineByPath(workspacePath string) error {
	workspacePath = filepath.Clean(workspacePath)
	dbPath := filepath.Join(workspacePath, ".whale", "team_engine.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no db at %s", dbPath)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ws := range m.states {
		if pathsEqual(ws.Path, workspacePath) {
			wbDir := filepath.Join(workspacePath, ".whale", "team_tasks")
			eng, err := team_engine.New(dbPath, wbDir, "", nil)
			if err != nil {
				return fmt.Errorf("load engine: %w", err)
			}
			if ws.Engine != nil {
				_ = ws.Engine.Close()
			}
			m.wireEngine(eng)
			ws.Engine = eng
			log.Printf("dashboard: LoadEngineByPath (re)loaded %s", workspacePath)
			return nil
		}
	}
	return fmt.Errorf("workspace %s not registered", workspacePath)
}

// PruneStale is a no-op: workspaces are never removed from the dashboard.
// When a whale process exits and heartbeat stops, the workspace simply becomes
// offline (Online=false via ListWorkspaces/GetMasterTasks).  The engine stays
// open so historical master task data remains available for reading from the
// .whale/team_engine.db bolt store.
func (m *MultiEngineManager) PruneStale(timeout time.Duration) {
	// Workspaces are persisted in workspaces.json and never pruned.
}

// ---------------------------------------------------------------------------
// Workspace discovery persistence (workspaces.json)
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) workspacesFilePath() string {
	if m.dashboardDir == "" {
		return filepath.Join(".", "workspaces.json")
	}
	return filepath.Join(m.dashboardDir, "workspaces.json")
}

// saveWorkspacePath appends a workspace path to workspaces.json if it's not
// already recorded there.  Callers must hold m.mu.
func (m *MultiEngineManager) saveWorkspacePath(workspacePath string) {
	paths, _ := m.readWorkspacePaths()

	// Deduplicate.
	for _, p := range paths {
		if p == workspacePath {
			return
		}
	}

	paths = append(paths, workspacePath)
	m.writeWorkspacePaths(paths)
}

// LoadWorkspacePaths reads workspaces.json and registers each path that is
// not already tracked.  Historical paths whose whale process is not running
// will be loaded without an engine (offline).
func (m *MultiEngineManager) LoadWorkspacePaths() []string {
	paths, _ := m.readWorkspacePaths()

	// Normalize and deduplicate (historical file may have stale paths).
	seen := make(map[string]bool)
	var clean []string
	for _, p := range paths {
		p = filepath.Clean(p)
		if seen[p] || p == "" || p == "." {
			continue
		}
		seen[p] = true
		clean = append(clean, p)

		// Register is idempotent and will try to open the engine db.
		if _, err := m.Register(p); err != nil {
			log.Printf("dashboard: load historical workspace %s: %v", p, err)
		}
	}
	// Rewrite cleaned list back.
	if len(clean) != len(paths) {
		_ = m.writeWorkspacePaths(clean)
	}
	return clean
}

func (m *MultiEngineManager) readWorkspacePaths() ([]string, error) {
	data, err := os.ReadFile(m.workspacesFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("parse workspaces.json: %w", err)
	}
	return paths, nil
}

func (m *MultiEngineManager) writeWorkspacePaths(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.workspacesFilePath(), data, 0644)
}

// ListWorkspaces returns all registered workspaces as JSON.
func (m *MultiEngineManager) ListWorkspaces() []WorkspaceJSON {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]WorkspaceJSON, 0, len(m.states))
	for _, ws := range m.states {
		wj := WorkspaceJSON{
			ID:         ws.ID,
			Path:       ws.Path,
			Label:      ws.Label,
			Registered: ws.Registered.Format(time.RFC3339),
			LastSeen:   ws.LastSeen.Format(time.RFC3339),
			Online:     m.isOnline(ws),
		}
		if ws.Engine != nil {
			tasks, _ := ws.Engine.ListTasks()
			wj.TaskCount = len(tasks)
			for _, t := range tasks {
				if !t.State.IsTerminal() {
					wj.ActiveCount++
				}
			}
		}
		result = append(result, wj)
	}
	return result
}

// ListTasks returns all tasks for a workspace.
func (m *MultiEngineManager) ListTasks(wsID string) ([]TaskJSON, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		return nil, nil
	}

	fullTasks, err := ws.Engine.ListTasks()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	result := make([]TaskJSON, len(fullTasks))
	for i, t := range fullTasks {
		elapsed := 0
		if createdAt, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil {
			elapsed = int(now.Sub(createdAt).Seconds())
		}
		result[i] = TaskJSON{
			WorkspaceID: wsID,
			ID:          t.ID,
			Title:       t.Title,
			State:       string(t.State),
			Role:        string(t.Role),
			ElapsedSec:  elapsed,
			RetryCount:  t.RetryCount,
			MaxRetries:  t.MaxRetries,
			ProgressPct: team_engine.GetProgress(t.State),
		}
	}
	return result, nil
}

// ListLogFiles returns available log files for a task.
func (m *MultiEngineManager) ListLogFiles(wsID, taskID string) ([]LogFileJSON, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		return nil, nil
	}

	wbDir := ws.Engine.Whiteboard.BaseDir()
	logsDir := filepath.Join(wbDir, "logs")
	taskLogsDir := filepath.Join(logsDir, "tasks", taskID)

	var files []LogFileJSON

	// input.md / output.md / verifier.md from whiteboard
	taskDir := filepath.Join(wbDir, taskID)
	for _, name := range []string{"input.md", "output.md", "verifier.md"} {
		p := filepath.Join(taskDir, name)
		if fi, err := os.Stat(p); err == nil {
			files = append(files, LogFileJSON{Name: name, Size: fi.Size()})
		}
	}

	// Leader logs (workspace-level, relevant to all tasks).
	leaderDir := filepath.Join(logsDir, "leader")
	if entries, err := os.ReadDir(leaderDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, _ := e.Info()
			sz := int64(0)
			if fi != nil {
				sz = fi.Size()
			}
			files = append(files, LogFileJSON{Name: "leader/" + e.Name(), Size: sz})
		}
	}

	// Worker / Verifier round logs from logs/tasks/<taskID>/
	if entries, err := os.ReadDir(taskLogsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, _ := e.Info()
			sz := int64(0)
			if fi != nil {
				sz = fi.Size()
			}
			files = append(files, LogFileJSON{Name: e.Name(), Size: sz})
		}
	}

	return files, nil
}

// ReadLogFile returns the content of a log file for a task.
// fileID is like "input.md", "worker_001.md", "engine.md", etc.
func (m *MultiEngineManager) ReadLogFile(wsID, taskID, fileID string) (string, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		return "", fmt.Errorf("engine not open for %s", wsID)
	}

	wbDir := ws.Engine.Whiteboard.BaseDir()

	// Whiteboard files: input.md, output.md, verifier.md
	switch fileID {
	case "input.md", "output.md", "verifier.md":
		return readFileString(filepath.Join(wbDir, taskID, fileID))
	}

	// Leader logs: leader/decompose_001.md
	if strings.HasPrefix(fileID, "leader/") {
		return readFileString(filepath.Join(wbDir, "logs", fileID))
	}

	// Log files: worker_001.md, verifier_001.md, engine.md
	logsDir := filepath.Join(wbDir, "logs", "tasks", taskID)
	return readFileString(filepath.Join(logsDir, fileID))
}

// GetMasterTasks returns all master tasks across all workspaces,
// deduplicated by workspace path + master task ID.
func (m *MultiEngineManager) GetMasterTasks() []MasterTaskJSON {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.Log("dashboard", "GetMasterTasks ENTRY: %d workspaces", len(m.states)) }
	var out []MasterTaskJSON
	seen := make(map[string]bool) // "path:mtID" dedup key
	for _, ws := range m.states {
		log.Printf("dashboard:   ws=%s path=%s engine=%v", ws.ID, ws.Path, ws.Engine != nil)
		if ws.Engine == nil {
			continue // no DB -> nothing to show
		}
		mts, err := ws.Engine.ListMasterTasks()
		if err != nil {
			log.Printf("dashboard: list master tasks for %s: %v", ws.ID, err)
			continue
		}
		if len(mts) == 0 {
			continue // no master tasks -> nothing to show
		}
		for _, mt := range mts {
			// Dedup by master task ID (UUID — globally unique).
			if seen[mt.ID] {
				continue
			}
			seen[mt.ID] = true

			subtasks, _ := ws.Engine.ListTasksByMasterTask(mt.ID)
			taskCount := len(subtasks)
			doneCount := 0
			activeCount := 0
			suspendedCount := 0
			online := m.isOnline(ws)
			for _, t := range subtasks {
				if t.State == team_engine.TaskStateDone || t.State == team_engine.TaskStateFailed {
					doneCount++
				}
				if t.State == team_engine.TaskStateSuspended {
					suspendedCount++
				}
				if online && (t.State == team_engine.TaskStateProducing || t.State == team_engine.TaskStateVerifying) {
					activeCount++
				}
			}
			out = append(out, MasterTaskJSON{
				ID:             mt.ID,
				Goal:           mt.Goal,
				WorkspaceID:    ws.ID,
				WorkspacePath:  ws.Path,
				WorkspaceLabel: ws.Label,
				Status:         mt.Status,
				CreatedAt:      mt.CreatedAt,
				TaskCount:      taskCount,
				DoneCount:      doneCount,
				ActiveCount:    activeCount,
				SuspendedCount: suspendedCount,
				WorkspaceOnline: m.isOnline(ws),
			})
		}
	}
	return out
}

// GetSubtasks returns first-level subtasks for a master task.
func (m *MultiEngineManager) GetSubtasks(wsID, masterTaskID string) []SubtaskJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil {
		return []SubtaskJSON{}
	}

	tasks, err := ws.Engine.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		log.Printf("dashboard: list subtasks for %s/%s: %v", wsID, masterTaskID, err)
		return []SubtaskJSON{}
	}
	// If the master task itself is gone, return empty.
	if mt, _ := ws.Engine.GetMasterTask(masterTaskID); mt == nil {
		return []SubtaskJSON{}
	}

	// Build a map from task ID to its JSON node, then assemble a tree
	// using ParentIDs so the frontend can render parent-child indentation.
	nodeMap := make(map[string]*SubtaskJSON, len(tasks))
	taskList := make([]*SubtaskJSON, 0, len(tasks))
	for _, t := range tasks {
		sj := &SubtaskJSON{
			ID:          t.ID,
			Title:       t.Title,
			Description: t.Description,
			Role:        string(t.Role),
			State:       string(t.State),
			Progress:    team_engine.GetProgress(t.State),
			CreatedAt:   t.CreatedAt,
			ParentIDs:   t.ParentIDs,
			RetryCount:  t.RetryCount,
			MaxRetries:  t.MaxRetries,
			BatchID:     t.BatchID,
		}
		nodeMap[t.ID] = sj
		taskList = append(taskList, sj)
	}
	// Attach children to their parents.
	roots := make([]SubtaskJSON, 0, 1+len(tasks))
	for _, sj := range taskList {
		hasParent := false
		for _, pid := range sj.ParentIDs {
			if parent, ok := nodeMap[pid]; ok {
				parent.Children = append(parent.Children, *sj)
				hasParent = true
			}
		}
		if !hasParent {
			roots = append(roots, *sj)
		}
	}
	// Prepend synthetic 任务规划 entry — only when tasks exist.
	if len(tasks) > 0 {
		leader := SubtaskJSON{
			ID:          "__leader__",
			Title:       "📋 任务规划",
			Description: masterTaskID,
			Role:        "teamleader",
			State:       "done",
			Progress:    100,
			Children:    roots,
		}
		return []SubtaskJSON{leader}
	}
	return roots
}

// GetAgentDialogue returns the multi-round Worker ⟷ Verifier conversation.
// Order: input.md → worker_001.md → verifier_001.md → worker_002.md → ...
func (m *MultiEngineManager) GetAgentDialogue(wsID, taskID string) []AgentDialogueJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil {
		return []AgentDialogueJSON{}
	}

	wbDir := ws.Engine.Whiteboard.BaseDir()
	taskDir := filepath.Join(wbDir, taskID)
	logsDir := filepath.Join(wbDir, "logs", "tasks", taskID)
	var dialogue []AgentDialogueJSON

	// Resolve task role name for dialogue display.
	roleName := "worker"
	verifierName := "verifier"
	if task, _ := ws.Engine.GetTask(taskID); task != nil {
		roleName = string(task.Role)
		verifierName = "审查: " + roleName
	}

	// 1. Task input (prompt from the system)
	if input, err := readFileString(filepath.Join(taskDir, "input.md")); err == nil && input != "" {
		dialogue = append(dialogue, AgentDialogueJSON{Role: "input", Content: input})
	}

	// 2. Multi-round Worker ⟷ Verifier logs (with interleaved leader feedback)
	for round := 1; round <= 99; round++ {
		workerFile := fmt.Sprintf("worker_%03d.md", round)
		verifierFile := fmt.Sprintf("verifier_%03d.md", round)
		leaderFbFile := fmt.Sprintf("leader_feedback_%03d.md", round)
		workerPath := filepath.Join(logsDir, workerFile)
		verifierPath := filepath.Join(logsDir, verifierFile)
		leaderFbPath := filepath.Join(logsDir, leaderFbFile)

		// Leader feedback (before re-attempt) — appears between rounds.
		if fb, ferr := readFileString(leaderFbPath); ferr == nil && fb != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("📋 任务主管反馈 (round %d)", round),
				Content: fb,
			})
		}

		workerContent, werr := readFileString(workerPath)
		if werr == nil && workerContent != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("%s (round %d)", roleName, round),
				Content: workerContent,
			})
		}

		verifierContent, verr := readFileString(verifierPath)
		if verr == nil && verifierContent != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("%s (round %d)", verifierName, round),
				Content: verifierContent,
			})
		}

		// No more rounds
		if werr != nil && verr != nil {
			break
		}
	}

	// 3. Final output / verifier summary (if not already captured in rounds)
	if len(dialogue) == 1 { // only input.md exists, no rounds
		if output, err := readFileString(filepath.Join(taskDir, "output.md")); err == nil && output != "" {
			dialogue = append(dialogue, AgentDialogueJSON{Role: "worker", Content: output})
		}
		if vf, err := readFileString(filepath.Join(taskDir, "verifier.md")); err == nil && vf != "" {
			dialogue = append(dialogue, AgentDialogueJSON{Role: "verifier", Content: vf})
		}
	}

	if len(dialogue) == 0 {
		return []AgentDialogueJSON{}
	}
	return dialogue
}

// GetLeaderPlan returns the full Leader conversation:
//   1. Initial decomposition plan  (decompose_*.md)
//   2. Batch review decisions      (review_*.md)
func (m *MultiEngineManager) GetLeaderPlan(wsID string) []AgentDialogueJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil {
		return []AgentDialogueJSON{}
	}

	wbDir := ws.Engine.Whiteboard.BaseDir()
	leaderDir := filepath.Join(wbDir, "logs", "leader")

	var dialogue []AgentDialogueJSON

	// Read decomposition plans.
	for round := 1; round <= 9; round++ {
		path := filepath.Join(leaderDir, fmt.Sprintf("decompose_%03d.md", round))
		content, err := readFileString(path)
		if err != nil {
			continue // missing round
		}
		if content != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("📋 任务主管 — 目标分解 (round %d)", round),
				Content: content,
			})
		}
	}

	// Read batch review decisions.
	for round := 1; round <= 9; round++ {
		path := filepath.Join(leaderDir, fmt.Sprintf("review_%03d.md", round))
		content, err := readFileString(path)
		if err != nil {
			continue // missing round
		}
		if content != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("📋 任务主管 — 执行审查 (round %d)", round),
				Content: content,
			})
		}
	}

	// Read execution summaries.
	for round := 1; round <= 3; round++ {
		path := filepath.Join(leaderDir, fmt.Sprintf("summary_%03d.md", round))
		content, err := readFileString(path)
		if err != nil {
			continue // missing round
		}
		if content != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("📋 任务主管 — 执行总结 (round %d)", round),
				Content: content,
			})
		}
	}

	if len(dialogue) == 0 {
		return []AgentDialogueJSON{}
	}
	return dialogue
}

// safeTruncate truncates a string to maxRunes runes without breaking UTF-8.
func safeTruncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// GetLeaderFlowchart generates an SVG flowchart showing the plan structure.
func (m *MultiEngineManager) GetLeaderFlowchart(wsID, masterTaskID string) string {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil {
		return ""
	}

	tasks, err := ws.Engine.ListTasksByMasterTask(masterTaskID)
	if err != nil || len(tasks) == 0 {
		return ""
	}

	// Group tasks by batch
	type batchInfo struct {
		Label string
		Tasks []*team_engine.Task
	}
	batches := make(map[string]*batchInfo)
	batchOrder := []string{}
	for _, t := range tasks {
		bid := t.BatchID
		if bid == "" {
			bid = "default"
		}
		if _, ok := batches[bid]; !ok {
			batches[bid] = &batchInfo{Label: bid}
			batchOrder = append(batchOrder, bid)
		}
		batches[bid].Tasks = append(batches[bid].Tasks, t)
	}

	if len(batchOrder) == 0 {
		return ""
	}

	// Build SVG flowchart
	// Layout: batches as vertical swimlanes, each containing task boxes
	boxW := 220
	boxH := 65
	padX := 60
	padY := 55
	swimH := 60
	maxTasks := 1
		for _, bg := range batches {
			if len(bg.Tasks) > maxTasks {
				maxTasks = len(bg.Tasks)
			}
		}

	colW := boxW + padX*2
	totalW := len(batchOrder)*colW + padX
	if totalW < 600 {
		totalW = 600
	}
	totalH := swimH + maxTasks*(boxH+padY) + padY*2

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" style="background:#181b22;font-family:system-ui,sans-serif;">`, totalW, totalH)
	svg += `<defs><marker id="arrow" viewBox="0 0 10 10" refX="10" refY="5" markerWidth="6" markerHeight="6" orient="auto"><path d="M0,0 L10,5 L0,10 Z" fill="#58a6ff"/></marker></defs>`

	// Draw batch labels as column headers
	for ci, bid := range batchOrder {
		bg := batches[bid]
		x := ci*colW + padX
		label := safeTruncate(bg.Label, 16)
		// Batch header
		svg += fmt.Sprintf(`<rect x="%d" y="10" width="%d" height="40" rx="6" fill="#1f6feb33" stroke="#58a6ff" stroke-width="1"/>`, x-10, colW-20)
		svg += fmt.Sprintf(`<text x="%d" y="35" fill="#c9d1d9" font-size="14" font-weight="600" text-anchor="middle">%s</text><title>%s</title>`, x+boxW/2, escSVG(label), escSVG(bg.Label))
	}

	// Draw task boxes and arrows
	type node struct{ x, y int }
	placed := map[string]node{}
	prevBatchTasks := []string{}

	for ci, bid := range batchOrder {
		bg := batches[bid]
		x := ci*colW + padX

		for ti, t := range bg.Tasks {
			y := swimH + ti*(boxH+padY) + padY/2
			color := "#4299e1"
			switch t.State {
			case "done", "passed":
				color = "#3fb950"
			case "running", "producing":
				color = "#d29922"
			case "failed":
				color = "#f85149"
			case "pending", "assigned":
				color = "#6e7681"
			}
			title := safeTruncate(t.Title, 16)
			pct := team_engine.GetProgress(t.State)
			statusLine := fmt.Sprintf("%s %d%%", string(t.State), pct)
			fullTitle := fmt.Sprintf("%s\nRole: %s\nState: %s (%d%%)", t.Title, string(t.Role), string(t.State), pct)
			role := string(t.Role)

			// Box
			svg += fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="6" fill="%s22" stroke="%s" stroke-width="1.5"/>`, x, y, boxW, boxH, color, color)
			svg += fmt.Sprintf(`<text x="%d" y="%d" fill="#c9d1d9" font-size="12" font-weight="500" text-anchor="middle">%s</text>`, x+boxW/2, y+24, escSVG(title))
			svg += fmt.Sprintf(`<text x="%d" y="%d" fill="#8b949e" font-size="10" text-anchor="middle">%s</text>`, x+boxW/2, y+44, escSVG(role))
			svg += fmt.Sprintf(`<text x="%d" y="%d" fill="#8b949e" font-size="9" text-anchor="middle">%s</text>`, x+boxW/2, y+58, escSVG(statusLine))
			// Tooltip — shows full title, role, state on hover
			svg += fmt.Sprintf(`<title>%s</title>`, escSVG(fullTitle))

			placed[t.ID] = node{x: x + boxW, y: y + boxH/2}

			// Arrow from previous batch tasks
			if ci > 0 {
				for _, prevID := range prevBatchTasks {
					if p, ok := placed[prevID]; ok {
						svg += fmt.Sprintf(`<path d="M%d,%d L%d,%d" stroke="#58a6ff" stroke-width="1.5" marker-end="url(#arrow)" fill="none" opacity="0.5"/>`,
							p.x, p.y, x, y+boxH/2)
					}
				}
			}
		}
		prevBatchTasks = nil
		for _, t := range bg.Tasks {
			prevBatchTasks = append(prevBatchTasks, t.ID)
		}
	}

	svg += `</svg>`
	return svg
}

func escSVG(s string) string {
	result := ""
	for _, r := range s {
		switch r {
		case '&':
			result += "&amp;"
		case '<':
			result += "&lt;"
		case '>':
			result += "&gt;"
		case '"':
			result += "&quot;"
		default:
			result += string(r)
		}
	}
	return result
}

// ResumeMasterTask transitions subtasks to ready state so the main
// Whale CLI engine can pick them up.  The dashboard engine has no
// subagent spawner, so it cannot execute tasks itself.
func (m *MultiEngineManager) ResumeMasterTask(wsID, masterTaskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("workspace not found")) }
		return fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		// Try lazy-open: the DB may have been created after the
		// workspace was first registered (e.g. dashboard started
		// before the first team_plan run).
		dbPath := filepath.Join(ws.Path, ".whale", "team_engine.db")
		wbDir := filepath.Join(ws.Path, ".whale", "team_tasks")
		if _, err := os.Stat(dbPath); err == nil {
			eng, err := team_engine.New(dbPath, wbDir, "", nil)
			if err != nil {
				if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("lazy open engine: %w", err)) }
				return fmt.Errorf("lazy open engine for %s: %w", wsID, err)
			}
			m.wireEngine(eng)
			ws.Engine = eng
		} else {
			if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("no db at %s", dbPath)) }
			return fmt.Errorf("engine not open for %s (no db)", wsID)
		}
	}

	// Load the master task to get its goal and workdir.
	mt, err := ws.Engine.GetMasterTask(masterTaskID)
	if err != nil || mt == nil {
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("master task not found: %v", err)) }
		return fmt.Errorf("master task %s not found", masterTaskID)
	}

	// Set master task status to running.
	if err := ws.Engine.DB.UpdateMasterTaskStatus(masterTaskID, "running"); err != nil {
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("update status: %w", err)) }
		return fmt.Errorf("update master task status: %w", err)
	}

	// Transition all non-terminal subtasks to assigned so the main
	// engine can pick them up.
	subtasks, err := ws.Engine.DB.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("list subtasks: %w", err)) }
		return fmt.Errorf("list subtasks: %w", err)
	}
	for _, t := range subtasks {
		// ResetForResume mirrors team_engine.ResetForResume:
		// failed/suspended -> pending, other runnable -> assigned.
		newState := team_engine.ResetForResume(t.State)
		if newState != t.State {
			if err := ws.Engine.DB.ForceTransitionState(t.ID, newState, "dashboard-resume"); err != nil {
				if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, fmt.Errorf("transition %s from %s: %w", t.ID, t.State, err)) }
				return fmt.Errorf("transition task %s: %w", t.ID, err)
			}
		}
	}

	// Queue a resume command so the main Whale CLI picks it up and
	// actually executes the tasks (dashboard itself has no spawner).
	m.QueueResume(wsID, masterTaskID)
	if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.DashboardResumeMaster(wsID, masterTaskID, nil) }
	return nil
}

// ListSuspendedMasterTasks returns master tasks that have suspended subtasks.
func (m *MultiEngineManager) ListSuspendedMasterTasks(wsID string) ([]string, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil {
		return nil, fmt.Errorf("workspace %s not found", wsID)
	}
	mts, err := ws.Engine.ListSuspendedMasterTasks()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, mt := range mts {
		ids = append(ids, mt.ID)
	}
	return ids, nil
}

// Shutdown deregisters all workspaces and closes engines.
// CancelSubtask cancels a single subtask within a workspace.
// Returns an error string, or empty string on success.
func (m *MultiEngineManager) CancelSubtask(wsID, taskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		// Try lazy-open: the DB may have been created after the
		// workspace was first registered (e.g. dashboard started
		// before the first team_plan run).
		dbPath := filepath.Join(ws.Path, ".whale", "team_engine.db")
		wbDir := filepath.Join(ws.Path, ".whale", "team_tasks")
		if _, err := os.Stat(dbPath); err == nil {
			eng, err := team_engine.New(dbPath, wbDir, "", nil)
			if err != nil {
				return fmt.Errorf("lazy open engine for %s: %w", wsID, err)
			}
			m.wireEngine(eng)
			ws.Engine = eng
		} else {
			return fmt.Errorf("engine not open for %s (no db)", wsID)
		}
	}

	// Use Kill for running tasks (forceful cancel that works from any
	// non-terminal state).  CancelTask only works from pending/assigned.
	if err := ws.Engine.Kill(context.Background(), taskID); err != nil {
		return fmt.Errorf("cancel subtask %s: %w", taskID, err)
	}
	return nil
}

// DeleteMasterTask deletes a master task and all its subtasks.
// Returns an error if any subtask is still running.
func (m *MultiEngineManager) DeleteMasterTask(wsID, masterTaskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		// Try lazy-open: the DB may have been created after the
		// workspace was first registered (e.g. dashboard started
		// before the first team_plan run).
		dbPath := filepath.Join(ws.Path, ".whale", "team_engine.db")
		wbDir := filepath.Join(ws.Path, ".whale", "team_tasks")
		if _, err := os.Stat(dbPath); err == nil {
			eng, err := team_engine.New(dbPath, wbDir, "", nil)
			if err != nil {
				return fmt.Errorf("lazy open engine for %s: %w", wsID, err)
			}
			m.wireEngine(eng)
			ws.Engine = eng
		} else {
			return fmt.Errorf("engine not open for %s (no db)", wsID)
		}
	}
	if err := ws.Engine.DeleteMasterTask(masterTaskID); err != nil {
		return fmt.Errorf("delete master task: %w", err)
	}
	// Flush WAL so a new connection sees the committed delete.
	ws.Engine.DB.Checkpoint()
	oldEngine := ws.Engine
	// Reopen the engine with a fresh DB connection so subsequent
	// reads (GetMasterTasks) see the committed delete immediately.
	dbPath := filepath.Join(ws.Path, ".whale", "team_engine.db")
	wbDir := filepath.Join(ws.Path, ".whale", "team_tasks")
	if eng, err := team_engine.New(dbPath, wbDir, "", nil); err == nil {
		m.wireEngine(eng)
		ws.Engine = eng
	} else {
		log.Printf("dashboard: reopen engine after delete failed: %v", err)
	}
	oldEngine.Close()
	return nil
}

// CancelMasterTask cancels all non-terminal subtasks of a master task.
// Returns the number of subtasks cancelled and any error.
func (m *MultiEngineManager) CancelMasterTask(wsID, masterTaskID string) (int, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		return 0, fmt.Errorf("engine not open for %s", wsID)
	}

	tasks, err := ws.Engine.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		return 0, fmt.Errorf("list subtasks: %w", err)
	}

	cancelled := 0
	for _, t := range tasks {
		if t.State.IsTerminal() {
			continue
		}
		if err := ws.Engine.Kill(context.Background(), t.ID); err != nil {
			log.Printf("dashboard: cancel subtask %s (%s): %v", t.ID, t.Title, err)
			continue
		}
		cancelled++
	}
	// Notify the whale CLI to stop execution immediately.
	m.sendWSCommand(wsID, "cancel_master", masterTaskID)
	return cancelled, nil
}

// Shutdown deregisters all workspaces and closes engines.
func (m *MultiEngineManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ws := range m.states {
		if ws.Engine != nil {
			_ = ws.Engine.Close()
		}
		delete(m.states, id)
	}
}

// DiscoverWhaleWorkspaces scans running Whale processes and returns their
// workspace paths by reading each process's current working directory (CWD).
// On Windows this uses the native API to read the process PEB; on Unix it
// reads /proc/<pid>/cwd.  Handles crash/force-kill gracefully — no cleanup.
func DiscoverWhaleWorkspaces() []string {
	pids := findWhalePids()
	seen := make(map[string]bool)
	var workspaces []string

	for _, pid := range pids {
		cwd := filepath.Clean(getProcessCwd(pid))
		if cwd == "" {
			continue
		}
		if seen[cwd] {
			continue
		}
		seen[cwd] = true
		workspaces = append(workspaces, cwd)
	}
	return workspaces
}

// findWhalePids returns the PIDs of all running whale/whale.exe processes.
func findWhalePids() []int {
	switch runtime.GOOS {
	case "windows":
		return findWhalePidsWindows()
	default:
		return findWhalePidsUnix()
	}
}

func findWhalePidsWindows() []int {
	// Use Windows Toolhelp API to enumerate processes — no cmd window flashing.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	type procEntry struct{ pid, ppid int }
	var entries []procEntry

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil
	}

	for {
		exeName := windows.UTF16ToString(entry.ExeFile[:])
		if strings.EqualFold(exeName, "whale.exe") {
			entries = append(entries, procEntry{
				pid:  int(entry.ProcessID),
				ppid: int(entry.ParentProcessID),
			})
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}

	// Build set of all whale PIDs for quick lookup.
	whalePids := make(map[int]bool)
	for _, e := range entries {
		whalePids[e.pid] = true
	}

	// Filter: only include top-level whale processes (parent is NOT a whale).
	var pids []int
	for _, e := range entries {
		if !whalePids[e.ppid] {
			pids = append(pids, e.pid)
		}
	}
	return pids
}

func findWhalePidsUnix() []int {
	cmd := exec.Command("ps", "-e", "-o", "pid=,ppid=,comm=")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	type procEntry struct{ pid, ppid int }
	var entries []procEntry
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		base := filepath.Base(fields[2])
		if base != "whale" && base != "whale.exe" {
			continue
		}
		var pid, ppid int
		if _, err := fmt.Sscanf(fields[0], "%d", &pid); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(fields[1], "%d", &ppid); err != nil {
			continue
		}
		entries = append(entries, procEntry{pid, ppid})
	}

	whalePids := make(map[int]bool)
	for _, e := range entries {
		whalePids[e.pid] = true
	}
	var pids []int
	for _, e := range entries {
		if !whalePids[e.ppid] {
			pids = append(pids, e.pid)
		}
	}
	return pids
}

// getProcessCwd returns the current working directory of a process by PID.
// Uses OS-specific APIs; returns "" on failure.
func getProcessCwd(pid int) string {
	switch runtime.GOOS {
	case "windows":
		return getProcessCwdWindows(pid)
	default:
		return getProcessCwdUnix(pid)
	}
}

func getProcessCwdUnix(pid int) string {
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return path
}

func getProcessCwdWindows(pid int) string {
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ,
		false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	// Get PEB address via NtQueryInformationProcess.
	var pbi windows.PROCESS_BASIC_INFORMATION
	status := ntQueryInfoProcess(h, &pbi)
	if status != 0 {
		return ""
	}

	// Read ProcessParameters pointer from PEB (offset 0x20 on x64).
	var paramsAddr uintptr
	if err := readProcessMem(h, uintptr(unsafe.Pointer(pbi.PebBaseAddress))+0x20, unsafe.Pointer(&paramsAddr), 8); err != nil {
		return ""
	}
	if paramsAddr == 0 {
		return ""
	}

	// Read RTL_USER_PROCESS_PARAMETERS.CurrentDirectory.DosPath as UNICODE_STRING.
	// Offset 0x38 = CurrentDirectory.DosPath on x64 Windows (verified experimentally).
	type unicodeStr struct {
		Length    uint16
		MaxLength uint16
		_         [4]byte // padding to align Buffer to 8 bytes on x64
		Buffer    uintptr
	}
	var us unicodeStr
	if err := readProcessMem(h, paramsAddr+0x38, unsafe.Pointer(&us), unsafe.Sizeof(us)); err != nil {
		return ""
	}
	if us.Length == 0 || us.Buffer == 0 {
		return ""
	}

	// Read the actual UTF-16 path string.
	b := make([]byte, us.Length+2)
	if err := windows.ReadProcessMemory(h, us.Buffer, &b[0], uintptr(us.Length), nil); err != nil {
		return ""
	}
	return windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), us.Length/2))
}

// ntQueryInfoProcess lazily loads NtQueryInformationProcess from ntdll.dll.
var ntQueryInfoProcess = func() func(windows.Handle, *windows.PROCESS_BASIC_INFORMATION) uint32 {
	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	proc := ntdll.NewProc("NtQueryInformationProcess")
	return func(h windows.Handle, pbi *windows.PROCESS_BASIC_INFORMATION) uint32 {
		var retLen uint32
		status, _, _ := proc.Call(
			uintptr(h),
			0, // ProcessBasicInformation
			uintptr(unsafe.Pointer(pbi)),
			unsafe.Sizeof(*pbi),
			uintptr(unsafe.Pointer(&retLen)),
		)
		return uint32(status)
	}
}()

// readProcessMem reads size bytes from a remote process's memory at addr.
func readProcessMem(h windows.Handle, addr uintptr, ptr unsafe.Pointer, size uintptr) error {
	return windows.ReadProcessMemory(h, addr, (*byte)(ptr), size, nil)
}


// pathsEqual compares two filesystem paths for equality.
// On Windows this is case-insensitive.
func pathsEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// isOnline reports whether a workspace has a running Whale CLI.
// An active WebSocket connection is the authoritative signal.
// Caller must hold m.mu (read lock is sufficient).
func (m *MultiEngineManager) isOnline(ws *WorkspaceState) bool {
	if m.wsConns != nil {
		_, ok := m.wsConns[ws.ID]
		return ok
	}
	return false
}

// sendWSCommand sends a JSON command to the whale CLI via WebSocket.
func (m *MultiEngineManager) sendWSCommand(wsID, command, masterTaskID string) {
	m.mu.Lock()
	conn, ok := m.wsConns[wsID]
	m.mu.Unlock()
	if !ok || conn == nil {
		return
	}
	msg, _ := json.Marshal(map[string]string{
		"command":         command,
		"master_task_id": masterTaskID,
	})
	conn.WriteMessage(websocket.TextMessage, msg)
}

func (m *MultiEngineManager) logResumeDiag(ws *WorkspaceState, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Print("dashboard: " + msg)
	if ws == nil || ws.Path == "" {
		return
	}
	logPath := filepath.Join(ws.Path, ".whale", "team_tasks", "logs", "engine.log")
	os.MkdirAll(filepath.Dir(logPath), 0755)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf("[%s] dashboard-resume: %s", time.Now().UTC().Format(time.RFC3339), msg)
	f.WriteString(line + "\n")
}
