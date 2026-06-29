package pod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/usewhale/whale/internal/team_engine"
)

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// =========================================================================
// JSON types
// =========================================================================

type MasterTaskJSON struct {
	ID              string `json:"id"`
	Goal            string `json:"goal"`
	Agent           string `json:"agent,omitempty"`
	SessionPath     string `json:"session_path,omitempty"`
	WorkspaceID     string `json:"workspace_id"`
	WorkspacePath   string `json:"workspace_path"`
	WorkspaceLabel  string `json:"workspace_label"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	TaskCount       int    `json:"task_count"`
	DoneCount       int    `json:"done_count"`
	ActiveCount     int    `json:"active_count"`
	SuspendedCount  int    `json:"suspended_count"`
	WorkspaceOnline bool   `json:"workspace_online"`
}

type SubtaskJSON struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Output      string        `json:"output"`
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

type AgentDialogueJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatMessageJSON struct {
	Time       string `json:"time"`
	From       string `json:"from"`
	Content    string `json:"content"`
	Thinking   string `json:"thinking,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	To         string `json:"to,omitempty"`
}

type WorkspaceJSON struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Label       string `json:"label"`
	Registered  string `json:"registered"`
	LastSeen    string `json:"last_seen"`
	Online      bool   `json:"online"`
	TaskCount   int    `json:"task_count"`
	ActiveCount int    `json:"active_count"`
}

type TeamDetailJSON struct {
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Category     string   `json:"category,omitempty"`
	Description  string   `json:"description"`
	Roles        []string `json:"roles"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type AgentInfoJSON struct {
	Name        string   `json:"name"`
	Role        string   `json:"role,omitempty"`
	Description string   `json:"description"`
	WhenToUse   string   `json:"whenToUse,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Skills      []string `json:"skills,omitempty"`
}

type SummonedItemJSON struct {
	Type        string `json:"type"` // "expert" or "team"
	Name        string `json:"name"`
	Label       string `json:"label"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
}

// =========================================================================
// MultiEngineManager
// =========================================================================

type MultiEngineManager struct {
	mu            sync.RWMutex
	states        map[string]*WorkspaceState
	seq           int64
	updateSeq     int64
	eventHandler  func(event TaskEvent)
	pendingResume map[string]string
	wsConns       map[string]*websocket.Conn
	dashboardDir  string
}

func NewMultiEngineManager(dashboardDir string) *MultiEngineManager {
	return &MultiEngineManager{
		states:       make(map[string]*WorkspaceState),
		dashboardDir: dashboardDir,
		wsConns:      make(map[string]*websocket.Conn),
	}
}

func (m *MultiEngineManager) OnEngineEvent(h func(event TaskEvent)) {
	m.mu.Lock()
	m.eventHandler = h
	m.mu.Unlock()
}
func (m *MultiEngineManager) BumpUpdateSeq() { m.mu.Lock(); m.updateSeq++; m.mu.Unlock() }
func (m *MultiEngineManager) GetUpdateSeq() int64 {
	m.mu.RLock(); defer m.mu.RUnlock(); return m.updateSeq
}

// ---------------------------------------------------------------------------
// Workspace registration
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) Register(workspacePath string) (*WorkspaceState, error) {
	workspacePath = filepath.Clean(workspacePath)
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ws := range m.states {
		if strings.EqualFold(ws.Path, workspacePath) {
			ws.LastSeen = time.Now()
			if ws.Engine == nil { ws.Engine = newEngine(workspacePath) }
			return ws, nil
		}
	}
	m.seq++
	id := fmt.Sprintf("ws_%04d", m.seq)
	eng := newEngine(workspacePath)
	ws := &WorkspaceState{ID: id, Path: workspacePath, Label: filepath.Base(workspacePath), Engine: eng, Registered: time.Now(), LastSeen: time.Now()}
	m.states[id] = ws
	m.saveWorkspacePath(workspacePath)
	return ws, nil
}

func (m *MultiEngineManager) Deregister(wsID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.states[wsID]
	if !ok { return fmt.Errorf("not found") }
	if ws.Engine != nil { ws.Engine.Close() }
	delete(m.states, wsID)
	return nil
}

func (m *MultiEngineManager) Lookup(wsID string) (*WorkspaceState, bool) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	return ws, ok
}

func (m *MultiEngineManager) Heartbeat(wsID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.states[wsID]
	if !ok { return fmt.Errorf("not found") }
	ws.LastSeen = time.Now()
	if ws.Engine == nil { ws.Engine = newEngine(ws.Path) }
	return nil
}

func (m *MultiEngineManager) LoadEngineByPath(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ws := range m.states {
		if strings.EqualFold(ws.Path, path) {
			if ws.Engine != nil { ws.Engine.Close() }
			ws.Engine = newEngine(path)
			return nil
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Master tasks
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) GetMasterTasks() []MasterTaskJSON {
	m.mu.RLock()
	states := make([]*WorkspaceState, 0, len(m.states))
	for _, ws := range m.states { states = append(states, ws) }
	m.mu.RUnlock()

	var result []MasterTaskJSON
	for _, ws := range states {
		if ws.Engine == nil { continue }
		mts, _ := ws.Engine.ListMasterTasks()
		for _, mt := range mts {
			tasks, _ := ws.Engine.ListTasksByMasterTask(mt.ID)
			doneCount, suspendedCount := 0, 0
			for _, t := range tasks {
				if t.State == team_engine.TaskStateDone || t.State == team_engine.TaskStateFailed { doneCount++ }
				if t.State == team_engine.TaskStateSuspended { suspendedCount++ }
			}
			status := "running"
			if len(tasks) > 0 && doneCount >= len(tasks) { status = "done" }
			result = append(result, MasterTaskJSON{
				ID: mt.ID, Goal: mt.Goal, WorkspaceID: ws.ID,
				WorkspacePath: ws.Path, WorkspaceLabel: ws.Label,
				Status: status, CreatedAt: mt.CreatedAt,
				TaskCount: len(tasks), DoneCount: doneCount, SuspendedCount: suspendedCount,
				WorkspaceOnline: m.isOnline(ws),
			})
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Subtasks
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) GetSubtasks(wsID, masterTaskID string) []SubtaskJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil { return nil }

	tasks, _ := ws.Engine.ListTasksByMasterTask(masterTaskID)
	if len(tasks) == 0 { return nil }

	nodeMap := make(map[string]*SubtaskJSON)
	taskList := make([]*SubtaskJSON, 0, len(tasks))
	for _, t := range tasks {
		sj := &SubtaskJSON{
			ID: t.ID, Title: t.Title, Description: t.Description,
			Output: t.Output, Role: string(t.Role), State: string(t.State),
			Progress: GetProgress(t.State), CreatedAt: t.CreatedAt,
			ParentIDs: t.ParentIDs, BatchID: t.BatchID,
			RetryCount: t.RetryCount, MaxRetries: t.MaxRetries,
		}
		nodeMap[t.ID] = sj
		taskList = append(taskList, sj)
	}
	for _, child := range taskList {
		for _, pid := range child.ParentIDs {
			if parent, ok := nodeMap[pid]; ok {
				parent.Children = append(parent.Children, *child)
			}
		}
	}
	for _, sj := range taskList {
		if len(sj.Children) > 0 {
			sum := 0
			for _, c := range sj.Children { sum += c.Progress }
			sj.Progress = sum / len(sj.Children)
		}
	}
	var roots []SubtaskJSON
	for _, sj := range taskList {
		hasParent := false
		for _, pid := range sj.ParentIDs {
			if _, ok := nodeMap[pid]; ok { hasParent = true; break }
		}
		if !hasParent { roots = append(roots, *sj) }
	}
	if len(tasks) > 0 {
		leader := SubtaskJSON{ID: "__leader__", Title: "📋 任务规划", Role: "teamleader", State: "done", Progress: 100}
		return append([]SubtaskJSON{leader}, roots...)
	}
	return roots
}

// ---------------------------------------------------------------------------
// Dialogue
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) GetAgentDialogue(wsID, taskID string) []AgentDialogueJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return nil }

	tasksDir := filepath.Join(ws.Path, ".whale", "team_tasks")
	taskDir := filepath.Join(tasksDir, taskID)
	logsDir := filepath.Join(tasksDir, "logs", "tasks", taskID)

	roleName := "worker"
	verifierName := "verifier"
	if t, _ := ws.Engine.GetTask(taskID); t != nil {
		roleName = string(t.Role)
		verifierName = "审查: " + roleName
	}

	var dialogue []AgentDialogueJSON
	if input, err := os.ReadFile(filepath.Join(taskDir, "input.md")); err == nil && len(input) > 0 {
		dialogue = append(dialogue, AgentDialogueJSON{Role: "input", Content: string(input)})
	}
	for round := 1; round <= 99; round++ {
		wc, werr := os.ReadFile(filepath.Join(logsDir, fmt.Sprintf("worker_%03d.md", round)))
		vc, verr := os.ReadFile(filepath.Join(logsDir, fmt.Sprintf("verifier_%03d.md", round)))
		if werr == nil && len(wc) > 0 {
			dialogue = append(dialogue, AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", roleName, round), Content: string(wc)})
		}
		if verr == nil && len(vc) > 0 {
			dialogue = append(dialogue, AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", verifierName, round), Content: string(vc)})
		}
		if werr != nil && verr != nil { break }
	}
	return dialogue
}

func (m *MultiEngineManager) GetLeaderPlan(wsID string) []AgentDialogueJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return nil }

	leaderDir := filepath.Join(ws.Path, ".whale", "team_tasks", "logs", "leader")
	var dialogue []AgentDialogueJSON
	for round := 1; round <= 9; round++ {
		for _, prefix := range []string{"decompose", "review"} {
			data, err := os.ReadFile(filepath.Join(leaderDir, fmt.Sprintf("%s_%03d.md", prefix, round)))
			if err != nil || len(data) == 0 { continue }
			label := "📋 目标分解"
			if prefix == "review" { label = "📋 执行审查" }
			dialogue = append(dialogue, AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", label, round), Content: string(data)})
		}
	}
	return dialogue
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) SendFeedback(wsID, taskID, message string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return fmt.Errorf("not found") }
	msgDir := filepath.Join(ws.Path, ".whale", "team_tasks", taskID, "messages")
	os.MkdirAll(msgDir, 0755)
	msgFile := filepath.Join(msgDir, fmt.Sprintf("%d.md", time.Now().UnixNano()))
	return os.WriteFile(msgFile, []byte(message), 0644)
}

func (m *MultiEngineManager) GetChatMessages(wsID, taskID string) []ChatMessageJSON {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return nil }
	msgDir := filepath.Join(ws.Path, ".whale", "team_tasks", taskID, "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil { return nil }
	var msgs []ChatMessageJSON
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") { continue }
		content, _ := os.ReadFile(filepath.Join(msgDir, e.Name()))
		from := "human"
		if strings.Contains(e.Name(), "_agent") { from = "agent" }
		msgs = append(msgs, ChatMessageJSON{Time: strings.TrimSuffix(strings.Split(e.Name(), "_")[0], ".md"), From: from, Content: string(content)})
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Time < msgs[j].Time })
	return msgs
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) ResumeMasterTask(wsID, masterTaskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil { return fmt.Errorf("workspace not found") }

	tasks, _ := ws.Engine.ListTasksByMasterTask(masterTaskID)
	for _, t := range tasks {
		newState := ResetForResume(t.State)
		if newState != t.State { updateMetaState(ws.Path, t.ID, string(newState)) }
	}
	m.QueueResume(wsID, masterTaskID)
	return nil
}

func (m *MultiEngineManager) CancelSubtask(wsID, taskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return fmt.Errorf("not found") }
	ws.Engine.Kill(context.Background(), taskID)
	updateMetaState(ws.Path, taskID, "suspended")
	return nil
}

func (m *MultiEngineManager) CancelMasterTask(wsID, masterTaskID string) (int, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return 0, fmt.Errorf("not found") }

	tasks, _ := ws.Engine.ListTasksByMasterTask(masterTaskID)
	cancelled := 0
	for _, t := range tasks {
		if IsTerminal(t.State) { continue }
		updateMetaState(ws.Path, t.ID, "suspended")
		cancelled++
	}
	m.mu.Lock(); delete(m.pendingResume, wsID); m.mu.Unlock()
	return cancelled, nil
}

func (m *MultiEngineManager) DeleteMasterTask(wsID, masterTaskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok { return fmt.Errorf("not found") }
	ws.Engine.DeleteMasterTask(masterTaskID)
	return nil
}

func (m *MultiEngineManager) RunSubtask(wsID, taskID string) error {
	m.sendWSCommand(wsID, "run_task", taskID)
	return nil
}

// ---------------------------------------------------------------------------
// WebSocket + queue
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) QueueResume(wsID, masterTaskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, _ := json.Marshal(map[string]string{"command": "resume", "master_task_id": masterTaskID})
	if conn, ok := m.wsConns[wsID]; ok { conn.WriteMessage(websocket.TextMessage, msg); return }
	if m.pendingResume == nil { m.pendingResume = make(map[string]string) }
	m.pendingResume[wsID] = masterTaskID
}

func (m *MultiEngineManager) DequeueResume(wsID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingResume == nil { return "", false }
	id, ok := m.pendingResume[wsID]
	if ok { delete(m.pendingResume, wsID) }
	return id, ok
}

func (m *MultiEngineManager) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("wsid")
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil { return }
	m.mu.Lock()
	if m.wsConns == nil { m.wsConns = make(map[string]*websocket.Conn) }
	if old, ok := m.wsConns[wsID]; ok { old.Close() }
	m.wsConns[wsID] = conn
	m.mu.Unlock()
	go func() {
		defer func() { conn.Close(); m.mu.Lock(); delete(m.wsConns, wsID); m.mu.Unlock() }()
		for { if _, _, err := conn.ReadMessage(); err != nil { return } }
	}()
}

func (m *MultiEngineManager) sendWSCommand(wsID, command, id string) {
	m.mu.Lock()
	conn, ok := m.wsConns[wsID]
	m.mu.Unlock()
	if !ok || conn == nil { return }
	payload := map[string]string{"command": command}
	if command == "run_task" { payload["task_id"] = id } else { payload["master_task_id"] = id }
	msg, _ := json.Marshal(payload)
	conn.WriteMessage(websocket.TextMessage, msg)
}

func (m *MultiEngineManager) isOnline(ws *WorkspaceState) bool {
	m.mu.RLock(); defer m.mu.RUnlock()
	_, ok := m.wsConns[ws.ID]; return ok
}

// ---------------------------------------------------------------------------
// Workspace list + shutdown
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) ListWorkspaces() []WorkspaceJSON {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]WorkspaceJSON, 0, len(m.states))
	for _, ws := range m.states {
		result = append(result, WorkspaceJSON{
			ID: ws.ID, Path: ws.Path, Label: ws.Label,
			Registered: ws.Registered.Format(time.RFC3339), LastSeen: ws.LastSeen.Format(time.RFC3339),
			Online: m.isOnline(ws),
		})
	}
	return result
}

func (m *MultiEngineManager) ListTasks(wsID string) ([]*team_engine.Task, error) {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok || ws.Engine == nil { return nil, nil }
	return ws.Engine.ListTasks()
}

func (m *MultiEngineManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ws := range m.states {
		if ws.Engine != nil { ws.Engine.Close() }
		delete(m.states, ws.ID)
	}
}

// ---------------------------------------------------------------------------
// Workspace persistence
// ---------------------------------------------------------------------------

func (m *MultiEngineManager) workspacesFilePath() string {
	if m.dashboardDir == "" { return "workspaces.json" }
	return filepath.Join(m.dashboardDir, "workspaces.json")
}

func (m *MultiEngineManager) saveWorkspacePath(path string) {
	paths, _ := m.readWorkspacePaths()
	for _, p := range paths { if p == path { return } }
	paths = append(paths, path)
	m.writeWorkspacePaths(paths)
}

func (m *MultiEngineManager) LoadWorkspacePaths() []string {
	paths, _ := m.readWorkspacePaths()
	for _, p := range paths {
		p = filepath.Clean(p)
		if p != "" && p != "." { m.Register(p) }
	}
	return paths
}

func (m *MultiEngineManager) readWorkspacePaths() ([]string, error) {
	data, err := os.ReadFile(m.workspacesFilePath())
	if err != nil { return nil, nil }
	var paths []string
	if json.Unmarshal(data, &paths) != nil { return nil, nil }
	return paths, nil
}

func (m *MultiEngineManager) writeWorkspacePaths(paths []string) error {
	data, _ := json.MarshalIndent(paths, "", "  ")
	return os.WriteFile(m.workspacesFilePath(), data, 0644)
}

func (m *MultiEngineManager) GetLeaderFlowchart(wsID, masterTaskID string) string {
	return ""
}

func (m *MultiEngineManager) LogFrontend(msg string) {}

// DiscoverWhaleWorkspaces returns workspace paths from workspaces.json.
func DiscoverWhaleWorkspaces() []string {
	paths, _ := (&MultiEngineManager{}).readWorkspacePaths()
	return paths
}
