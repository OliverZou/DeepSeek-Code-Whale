// Package dashboard provides a multi-workspace team-engine dashboard server
// that aggregates tasks from all registered Whale instances on the machine.
package dashboard

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/team_engine"
)

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
}

// SubtaskJSON is a subtask in a master task's plan.
type SubtaskJSON struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Role        string `json:"role"`
	State       string `json:"state"`
	Progress    int    `json:"progress"`
	CreatedAt   string `json:"created_at"`
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
}

// OnEngineEvent 注册一个事件回调，当 engine 有任务状态变化时触发。
// 由 Wails App 调用，用于向前端推送实时更新。
func (m *MultiEngineManager) OnEngineEvent(handler func(event team_engine.TaskEvent)) {
	m.mu.Lock()
	m.eventHandler = handler
	m.mu.Unlock()
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
func NewMultiEngineManager() *MultiEngineManager {
	return &MultiEngineManager{
		states: make(map[string]*WorkspaceState),
	}
}

// Register adds a workspace.  Returns the assigned ID and any error.
// If the engine cannot be opened (db doesn't exist yet), it still registers
// but engine will be nil — it gets lazy-loaded on next heartbeat.
func (m *MultiEngineManager) Register(workspacePath string) (*WorkspaceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Deduplicate by path.
	for _, ws := range m.states {
		if ws.Path == workspacePath {
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

	ws := &WorkspaceState{
		ID:         id,
		Path:       workspacePath,
		Label:      filepath.Base(workspacePath),
		Engine:     eng,
		Registered: time.Now(),
		LastSeen:   time.Now(),
	}
	m.states[id] = ws
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

// PruneStale removes workspaces that haven't heartbeated within timeout.
func (m *MultiEngineManager) PruneStale(timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-timeout)
	for id, ws := range m.states {
		if ws.LastSeen.Before(cutoff) {
			log.Printf("dashboard: pruning stale workspace %s (%s)", id, ws.Label)
			if ws.Engine != nil {
				_ = ws.Engine.Close()
			}
			delete(m.states, id)
		}
	}
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
			Online:     time.Since(ws.LastSeen) < 90*time.Second,
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

	var out []MasterTaskJSON
	seen := make(map[string]bool) // "path:mtID" dedup key
	for _, ws := range m.states {
		if ws.Engine == nil {
			continue
		}
		mts, err := ws.Engine.ListMasterTasks()
		if err != nil {
			log.Printf("dashboard: list master tasks for %s: %v", ws.ID, err)
			continue
		}
		for _, mt := range mts {
			// Dedup: same workspace path + same master task ID → skip.
			key := ws.Path + ":" + mt.ID
			if seen[key] {
				continue
			}
			seen[key] = true

			subtasks, _ := ws.Engine.ListTasksByMasterTask(mt.ID)
			taskCount := len(subtasks)
			doneCount := 0
			activeCount := 0
			suspendedCount := 0
			for _, t := range subtasks {
				if t.State == team_engine.TaskStateDone {
					doneCount++
				}
				if t.State == team_engine.TaskStateSuspended {
					suspendedCount++
				}
				// 正在运行中（非终止状态）才算 active
				if !t.State.IsTerminal() && t.State != team_engine.TaskStateSuspended {
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

	out := make([]SubtaskJSON, 0, 1+len(tasks))
	// Prepend synthetic 任务规划 entry.
		out = append(out, SubtaskJSON{
			ID:          "__leader__",
			Title:       "📋 任务规划",
			Description: masterTaskID,
		Role:        "teamleader",
		State:       "done",
		Progress:    100,
	})
	for _, t := range tasks {
		out = append(out, SubtaskJSON{
			ID:          t.ID,
			Title:       t.Title,
			Description: t.Description,
			Role:        string(t.Role),
			State:       string(t.State),
			Progress:    team_engine.GetProgress(t.State),
			CreatedAt:   t.CreatedAt,
		})
	}
	return out
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

	// 1. Task input (prompt from the system)
	if input, err := readFileString(filepath.Join(taskDir, "input.md")); err == nil && input != "" {
		dialogue = append(dialogue, AgentDialogueJSON{Role: "input", Content: input})
	}

	// 2. Multi-round Worker ⟷ Verifier logs
	for round := 1; round <= 99; round++ {
		workerFile := fmt.Sprintf("worker_%03d.md", round)
		verifierFile := fmt.Sprintf("verifier_%03d.md", round)
		workerPath := filepath.Join(logsDir, workerFile)
		verifierPath := filepath.Join(logsDir, verifierFile)

		workerContent, werr := readFileString(workerPath)
		if werr == nil && workerContent != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("worker (round %d)", round),
				Content: workerContent,
			})
		}

		verifierContent, verr := readFileString(verifierPath)
		if verr == nil && verifierContent != "" {
			dialogue = append(dialogue, AgentDialogueJSON{
				Role:    fmt.Sprintf("verifier (round %d)", round),
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
			break
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
			break
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
			break
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
	tasksPerCol := 6

	colW := boxW + padX*2
	totalW := len(batchOrder) * colW
	if totalW < 600 {
		totalW = 600
	}
	totalH := swimH + tasksPerCol*(boxH+padY) + padY

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" style="max-width:100%%;background:#181b22;border-radius:8px;font-family:system-ui,sans-serif;">`, totalW, totalH)
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
			fullTitle := fmt.Sprintf("%s\n[%s] %s\n%s %d%%", t.Title, t.State, string(t.Role), string(t.State), pct)
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

// ResumeMasterTask resumes a suspended master task execution.
// Returns empty string on success, or error message.
func (m *MultiEngineManager) ResumeMasterTask(wsID, masterTaskID string) error {
	m.mu.RLock()
	ws, ok := m.states[wsID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("workspace %s not found", wsID)
	}
	if ws.Engine == nil {
		return fmt.Errorf("engine not open for %s", wsID)
	}

	// Load the master task to get its goal and workdir.
	mt, err := ws.Engine.GetMasterTask(masterTaskID)
	if err != nil || mt == nil {
		return fmt.Errorf("master task %s not found", masterTaskID)
	}

	_, err = ws.Engine.ResumeMasterTask(context.Background(), masterTaskID, mt.Goal, mt.WorkspacePath)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
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
		return fmt.Errorf("engine not open for %s", wsID)
	}

	// Use Kill for running tasks (forceful cancel that works from any
	// non-terminal state).  CancelTask only works from pending/assigned.
	if err := ws.Engine.Kill(context.Background(), taskID); err != nil {
		return fmt.Errorf("cancel subtask %s: %w", taskID, err)
	}
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

func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
