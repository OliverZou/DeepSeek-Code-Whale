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
	teampglog "github.com/usewhale/whale/internal/team_engine/log"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

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
	team_engine.SetDefaultTeamLog(teampglog.NewTeamLogAt(filepath.Join(dashboardDir, "whale-dashboard.teamlog")))
	return &App{
		mgr:  dashboard.NewMultiEngineManager(dashboardDir),
		done: make(chan struct{}),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	team_engine.DefaultTeamLog.Log("startup", "started, DefaultTeamLog=%v", team_engine.DefaultTeamLog != nil)

	// Register event callback: engine 状态变化 → 实时推送到前端。
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

	// Discover running Whale instances and auto-register their workspaces.
	for _, path := range dashboard.DiscoverWhaleWorkspaces() {
		if ws, err := a.mgr.Register(path); err != nil {
			log.Printf("dashboard: auto-register %s: %v", path, err)
		} else {
			log.Printf("dashboard: auto-registered workspace %s (%s)", ws.ID, path)
		}
	}

	a.mgr.LoadWorkspacePaths()

	// Push workspace updates to the frontend every 2 seconds.
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

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		<-a.done
		ln.Close()
	}()

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

func (a *App) pushLoop() {
	// Wait for frontend to register event listeners before first push.
	time.Sleep(3 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
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

var lastUpdateJSON string

func (a *App) emitUpdate() {
	tasks := a.mgr.GetMasterTasks()
	// Skip if nothing changed — prevents stale push-loop events from
	// overwriting a just-deleted master task list (race with engine reopen).
	data, _ := json.Marshal(tasks)
	payload := string(data)
	if payload == lastUpdateJSON {
		return
	}
	lastUpdateJSON = payload
	team_engine.DefaultTeamLog.Log("frontend", "emitUpdate: %d tasks", len(tasks))
	runtime.EventsEmit(a.ctx, "update", tasks)
}

// --- Frontend-callable methods ---

func (a *App) GetWorkspaces() []dashboard.WorkspaceJSON {
	result := a.mgr.ListWorkspaces()
	team_engine.DefaultTeamLog.Log("frontend", "GetWorkspaces called, returned %d", len(result))
	return result
}

func (a *App) GetTasks(wsID string) []dashboard.TaskJSON {
	tasks, _ := a.mgr.ListTasks(wsID)
	team_engine.DefaultTeamLog.Log("frontend", "GetTasks called for %s, returned %d", wsID, len(tasks))
	if tasks == nil {
		return []dashboard.TaskJSON{}
	}
	return tasks
}

func (a *App) GetLogFiles(wsID, taskID string) []dashboard.LogFileJSON {
	files, _ := a.mgr.ListLogFiles(wsID, taskID)
	if files == nil {
		return []dashboard.LogFileJSON{}
	}
	return files
}

func (a *App) GetLogContent(wsID, taskID, fileID string) string {
	content, err := a.mgr.ReadLogFile(wsID, taskID, fileID)
	if err != nil {
		return "// " + err.Error()
	}
	return content
}

func (a *App) RegisterWorkspace(path string) string {
	ws, err := a.mgr.Register(path)
	if err != nil {
		return ""
	}
	return ws.ID
}

func (a *App) CancelSubtask(wsID, taskID string) string {
	if err := a.mgr.CancelSubtask(wsID, taskID); err != nil {
		return err.Error()
	}
	return ""
}

func (a *App) DeleteMasterTask(wsID, masterTaskID string) string {
	if err := a.mgr.DeleteMasterTask(wsID, masterTaskID); err != nil {
		return err.Error()
	}
	return ""
}

func (a *App) CancelMasterTask(wsID, masterTaskID string) string {
	if _, err := a.mgr.CancelMasterTask(wsID, masterTaskID); err != nil {
		return err.Error()
	}
	return ""
}

func (a *App) ResumeMasterTask(wsID, masterTaskID string) string {
	if err := a.mgr.ResumeMasterTask(wsID, masterTaskID); err != nil {
		return err.Error()
	}
	return ""
}

func (a *App) GetMasterTasks() []dashboard.MasterTaskJSON {
	result := a.mgr.GetMasterTasks()
	team_engine.DefaultTeamLog.Log("frontend", "GetMasterTasks called, returned %d tasks", len(result))
	return result
}

func (a *App) GetSubtasks(wsID, masterTaskID string) []dashboard.SubtaskJSON {
	return a.mgr.GetSubtasks(wsID, masterTaskID)
}

func (a *App) GetAgentDialogue(wsID, taskID string) []dashboard.AgentDialogueJSON {
	return a.mgr.GetAgentDialogue(wsID, taskID)
}

func (a *App) GetLeaderPlan(wsID string) []dashboard.AgentDialogueJSON {
	return a.mgr.GetLeaderPlan(wsID)
}

func (a *App) GetLeaderFlowchart(wsID, masterTaskID string) string {
	return a.mgr.GetLeaderFlowchart(wsID, masterTaskID)
}
