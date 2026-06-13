package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/dashboard"
	"github.com/usewhale/whale/internal/team_engine"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails application backend.  Exported methods are callable
// from the frontend via window.go.main.App.<Method>().
type App struct {
	ctx  context.Context
	mgr  *dashboard.MultiEngineManager
	done chan struct{}
	wg   sync.WaitGroup
}

func NewApp() *App {
	dashboardDir := "."
	if exe, err := os.Executable(); err == nil {
		dashboardDir = filepath.Dir(exe)
	}
	return &App{
		mgr:  dashboard.NewMultiEngineManager(dashboardDir),
		done: make(chan struct{}),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Register event callback: engine 状态变化 → 实时推送到前端
	// 前端收到 "task-event" 后只刷新受影响的部分，不再全量轮询。
	evtCtx := a.ctx
	a.mgr.OnEngineEvent(func(event team_engine.TaskEvent) {
		runtime.EventsEmit(evtCtx, "task-event", event)
	})

	// Start background HTTP server for whale registration.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.startRegistrationServer()
	}()

	// Discover running Whale instances by scanning processes, then
	// auto-register their workspaces.  This works even if Whale started
	// before the Dashboard, and handles crashes gracefully (no cleanup).
	for _, path := range dashboard.DiscoverWhaleWorkspaces() {
		if ws, err := a.mgr.Register(path); err != nil {
			log.Printf("dashboard: auto-register %s: %v", path, err)
		} else {
			log.Printf("dashboard: auto-registered workspace %s (%s)", ws.ID, path)
		}
	}

	// Load previously-registered workspace paths from workspaces.json.
	// Paths already discovered above are skipped (Register is idempotent).
	// Historical paths whose whale process is not running will appear offline.
	a.mgr.LoadWorkspacePaths()

	// Push workspace updates to the frontend every 2 seconds (replaces SSE).
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.pushLoop()
	}()
}

func (a *App) shutdown(ctx context.Context) {
	close(a.done)
	a.wg.Wait()
	a.mgr.Shutdown()
}

// startRegistrationServer listens on :8520 for whale registrations.
// If the port is already in use, logs and returns (another dashboard is running).
func (a *App) startRegistrationServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", a.handleRegister)
	mux.HandleFunc("/api/heartbeat", a.handleHeartbeat)
	mux.HandleFunc("/api/deregister", a.handleDeregister)
	mux.HandleFunc("/ws", a.mgr.HandleWebSocket)

	ln, err := net.Listen("tcp", "127.0.0.1:8520")
	if err != nil {
		log.Printf("registration server: %v (another dashboard may be running)", err)
		return
	}

	log.Printf("registration server: http://%s", ln.Addr().String())

	// Listener closer goroutine: tracked by WaitGroup so shutdown() waits
	// for it to finish before closing engines.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		<-a.done
		ln.Close()
	}()

	// http.Serve blocks until the listener is closed.
	if err := http.Serve(ln, mux); err != nil {
		log.Printf("registration server: %v", err)
	}
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ Path string `json:"path"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	ws, err := a.mgr.Register(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": ws.ID})
}

func (a *App) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ ID string `json:"id"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.mgr.Heartbeat(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Check for pending commands and return them in the response.
	resp := map[string]interface{}{}
	if mtID, ok := a.mgr.DequeueResume(req.ID); ok {
		resp["command"] = "resume"
		resp["master_task_id"] = mtID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (a *App) handleDeregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ ID string `json:"id"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.mgr.Deregister(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushLoop emits "update" events to the frontend every 2 seconds.
func (a *App) pushLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Immediate first push.
	a.emitUpdate()

	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			a.emitUpdate()
		}
	}
}

func (a *App) emitUpdate() {
	runtime.EventsEmit(a.ctx, "update", a.mgr.ListWorkspaces())
}

// ---------------------------------------------------------------------------
// Frontend-callable methods (bound via wails.Run Bind)
// ---------------------------------------------------------------------------

// GetWorkspaces returns all registered workspaces.
func (a *App) GetWorkspaces() []dashboard.WorkspaceJSON {
	return a.mgr.ListWorkspaces()
}

// GetTasks returns tasks for a workspace.
func (a *App) GetTasks(wsID string) []dashboard.TaskJSON {
	tasks, _ := a.mgr.ListTasks(wsID)
	if tasks == nil {
		return []dashboard.TaskJSON{}
	}
	return tasks
}

// GetLogFiles returns available log files for a task.
func (a *App) GetLogFiles(wsID, taskID string) []dashboard.LogFileJSON {
	files, _ := a.mgr.ListLogFiles(wsID, taskID)
	if files == nil {
		return []dashboard.LogFileJSON{}
	}
	return files
}

// GetLogContent returns the content of a log file.
func (a *App) GetLogContent(wsID, taskID, fileID string) string {
	content, err := a.mgr.ReadLogFile(wsID, taskID, fileID)
	if err != nil {
		return "// " + err.Error()
	}
	return content
}

// RegisterWorkspace registers a workspace path and returns the assigned ID.
func (a *App) RegisterWorkspace(path string) string {
	ws, err := a.mgr.Register(path)
	if err != nil {
		return ""
	}
	return ws.ID
}

// ---------------------------------------------------------------------------
// Master Task (总任务) frontend bindings
// ---------------------------------------------------------------------------

// CancelSubtask cancels a single subtask.
// Returns empty string on success, or an error message.
func (a *App) CancelSubtask(wsID, taskID string) string {
	if err := a.mgr.CancelSubtask(wsID, taskID); err != nil {
		return err.Error()
	}
	return ""
}

// DeleteMasterTask deletes a master task and all its subtasks.
// Returns an error message if any subtask is still running; empty string on success.
func (a *App) DeleteMasterTask(wsID, masterTaskID string) string {
	if err := a.mgr.DeleteMasterTask(wsID, masterTaskID); err != nil {
		return err.Error()
	}
	return ""
}

// CancelMasterTask cancels all non-terminal subtasks of a master task.
// Returns empty string on success, or an error message.
func (a *App) CancelMasterTask(wsID, masterTaskID string) string {
	if _, err := a.mgr.CancelMasterTask(wsID, masterTaskID); err != nil {
		return err.Error()
	}
	return ""
}

// ResumeMasterTask resumes a suspended master task.
// Returns empty string on success, or an error message.
func (a *App) ResumeMasterTask(wsID, masterTaskID string) string {
	if err := a.mgr.ResumeMasterTask(wsID, masterTaskID); err != nil {
		return err.Error()
	}
	return ""
}

// GetMasterTasks returns all master tasks across all workspaces.
func (a *App) GetMasterTasks() []dashboard.MasterTaskJSON {
	return a.mgr.GetMasterTasks()
}

// GetSubtasks returns subtasks for a master task.
func (a *App) GetSubtasks(wsID, masterTaskID string) []dashboard.SubtaskJSON {
	return a.mgr.GetSubtasks(wsID, masterTaskID)
}

// GetAgentDialogue returns the conversation for a subtask.
func (a *App) GetAgentDialogue(wsID, taskID string) []dashboard.AgentDialogueJSON {
	return a.mgr.GetAgentDialogue(wsID, taskID)
}

// GetLeaderPlan returns the 任务规划 decomposition text.
func (a *App) GetLeaderPlan(wsID string) []dashboard.AgentDialogueJSON {
	return a.mgr.GetLeaderPlan(wsID)
}

// GetLeaderFlowchart returns an SVG flowchart of the plan.
func (a *App) GetLeaderFlowchart(wsID, masterTaskID string) string {
	return a.mgr.GetLeaderFlowchart(wsID, masterTaskID)
}
