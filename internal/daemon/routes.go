package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/usewhale/whale/internal/runtime/protocol"
	"github.com/usewhale/whale/internal/team_engine"
)

// routes 注册 daemon 的全部 HTTP/SSE 端点。仅绑定回环地址，面向本地桌面端。
// 后端（svc/eng）为可选：对应端点在未托管后端时返回 503。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /events", s.handleEvents)

	// 桌面端主会话：转发 protocol.Intent。
	mux.HandleFunc("POST /api/intent", s.handleIntent)

	// team 透明化（DAG 过程可视化）。
	mux.HandleFunc("GET /api/team/masters", s.handleTeamMasters)
	mux.HandleFunc("POST /api/team/masters", s.handleTeamCreateMaster)
	mux.HandleFunc("GET /api/team/masters/{id}/dag", s.handleTeamDAG)
	mux.HandleFunc("GET /api/team/masters/{id}/progress", s.handleTeamProgress)
	mux.HandleFunc("GET /api/team/tasks/{id}", s.handleTeamTask)
	mux.HandleFunc("GET /api/team/tasks/{id}/output", s.handleTeamTaskOutput)
	mux.HandleFunc("GET /api/team/tasks/{id}/session", s.handleTeamTaskSession)

	// team 6 动词 + escalation。
	mux.HandleFunc("POST /api/team/prompt", s.handleTeamPrompt)
	mux.HandleFunc("POST /api/team/spawn", s.handleTeamSpawn)
	mux.HandleFunc("POST /api/team/abort", s.handleTeamAbort)
	mux.HandleFunc("POST /api/team/kill", s.handleTeamKill)
	mux.HandleFunc("POST /api/team/summarize", s.handleTeamSummarize)
	mux.HandleFunc("POST /api/team/fork", s.handleTeamFork)
	mux.HandleFunc("POST /api/team/resolve", s.handleTeamResolve)

	return mux
}

// ---------------------------------------------------------------------------
// 基础 helper
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// requireEngine 校验 team 后端可用；不可用时写出 503 并返回 false。
func (s *Server) requireEngine(w http.ResponseWriter) bool {
	if s.eng == nil {
		writeError(w, http.StatusServiceUnavailable, "team engine backend not enabled")
		return false
	}
	return true
}

func (s *Server) requireService(w http.ResponseWriter) bool {
	if s.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "service backend not enabled")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 健康检查 + SSE
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Healthz())
}

// handleEvents 是 SSE 事件流端点。所有 agent/team/dag 事件统一经 EventHub 推送。
// 心跳注释帧（": ...\\n\\n"）用于防空闲断连。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.hub.Subscribe()
	defer cancel()

	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// 桌面端主会话
// ---------------------------------------------------------------------------

func (s *Server) handleIntent(w http.ResponseWriter, r *http.Request) {
	if !s.requireService(w) {
		return
	}
	var msg protocol.ClientMessage
	if err := decodeBody(r, &msg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg.Type != protocol.ClientMessageIntent || msg.Intent == nil {
		writeError(w, http.StatusBadRequest, "expected type=intent with intent payload")
		return
	}
	s.svc.DispatchProtocol(*msg.Intent)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "session_id": s.svc.SessionID()})
}

// ---------------------------------------------------------------------------
// team：只读查询
// ---------------------------------------------------------------------------

func (s *Server) handleTeamMasters(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	masters, err := s.eng.ListMasterTasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"masters": masters})
}

// handleTeamDAG 返回某个 master task 的 DAG 快照：解析 plan.json →
// batches + edges + 运行时任务状态叠加。plan.json 即 DAG 权威数据源。
func (s *Server) handleTeamDAG(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	masterID := r.PathValue("id")
	snap, err := s.dagSnapshot(masterID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleTeamProgress 返回某个 master task 的原始 plan.json（含 cycle/retry 等静态分解信息）。
func (s *Server) handleTeamProgress(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	masterID := r.PathValue("id")
	path := filepath.Join(s.eng.Whiteboard.BaseDir(), masterID, "plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "plan.json not found: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleTeamTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	task, err := s.eng.GetTask(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleTeamTaskOutput(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	output, err := s.eng.Whiteboard.ReadOutput(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": output})
}

func (s *Server) handleTeamTaskSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	taskID := r.PathValue("id")
	sessionID := s.eng.Store.SessionID(taskID)
	if sessionID == "" {
		sessionID = taskID // allow a raw session ID
	}
	summary, err := s.eng.Summarize(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": sessionID, "summary": summary})
}

// ---------------------------------------------------------------------------
// team：写操作（6 动词 + create + resolve）
// ---------------------------------------------------------------------------

func (s *Server) handleTeamCreateMaster(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	var req struct {
		Goal      string `json:"goal"`
		SessionID string `json:"session_id"`
		Workdir   string `json:"workdir"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Goal == "" {
		writeError(w, http.StatusBadRequest, "goal is required")
		return
	}

	master, err := s.eng.CreateMasterTask(req.Goal, req.Workdir, req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// TeamCycle 是阻塞式调度：放后台执行，进度经 SSE dag/team 事件回流。
	ctx := r.Context()
	go func() {
		_, _ = s.eng.TeamCycle(ctx, req.Goal, req.Workdir, master.ID)
	}()
	s.broadcastMasterUpdate(master.ID)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"master_task_id": master.ID,
		"session_id":     master.SessionID,
		"status":         master.Status,
	})
}

type teamPromptRequest struct {
	ToTaskID  string `json:"to_task_id"`
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Sync      bool   `json:"sync"`
}

func (s *Server) handleTeamPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	var req teamPromptRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Content == "" || (req.ToTaskID == "" && req.SessionID == "") {
		writeError(w, http.StatusBadRequest, "content and (to_task_id|session_id) required")
		return
	}
	// Prompt(Sync=true) 会阻塞等待回复；HTTP 层放后台，回复经 SSE 回流。
	go func() {
		_, _ = s.eng.Prompt(r.Context(), team_engine.PromptRequest{
			ToTaskID:  req.ToTaskID,
			SessionID: req.SessionID,
			From:      "human",
			Content:   req.Content,
			Sync:      req.Sync,
		})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

type teamSpawnRequest struct {
	Title         string `json:"title"`
	Description   string `json:"description"`
	Role          string `json:"role"`
	MaxRetries    int    `json:"max_retries"`
	Workdir       string `json:"workdir"`
	VerifierFocus string `json:"verifier_focus"`
}

func (s *Server) handleTeamSpawn(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	var req teamSpawnRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Description) == "" {
		writeError(w, http.StatusBadRequest, "title and description required")
		return
	}
	if req.Role == "" {
		req.Role = "worker"
	}
	if req.MaxRetries <= 0 {
		req.MaxRetries = 3
	}
	go func() {
		_, _ = s.eng.Spawn(r.Context(), team_engine.SpawnRequest{
			Title:         req.Title,
			Description:   req.Description,
			Role:          team_engine.AgentRole(req.Role),
			MaxRetries:    req.MaxRetries,
			Workdir:       req.Workdir,
			VerifierFocus: req.VerifierFocus,
			From:          "human",
		})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

type teamTaskIDRequest struct {
	TaskID string `json:"task_id"`
}

func (s *Server) handleTeamAbort(w http.ResponseWriter, r *http.Request) {
	s.teamTaskVerb(w, r, "abort")
}

func (s *Server) handleTeamKill(w http.ResponseWriter, r *http.Request) {
	s.teamTaskVerb(w, r, "kill")
}

func (s *Server) teamTaskVerb(w http.ResponseWriter, r *http.Request, verb string) {
	if !s.requireEngine(w) {
		return
	}
	var req teamTaskIDRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "task_id required")
		return
	}
	var err error
	switch verb {
	case "abort":
		err = s.eng.Abort(r.Context(), req.TaskID)
	case "kill":
		err = s.eng.Kill(r.Context(), req.TaskID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verb": verb, "task_id": req.TaskID})
}

type teamSessionRequest struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
}

func (s *Server) resolveSession(req teamSessionRequest) string {
	if req.SessionID != "" {
		return req.SessionID
	}
	if req.TaskID != "" {
		if sid := s.eng.Store.SessionID(req.TaskID); sid != "" {
			return sid
		}
		return req.TaskID
	}
	return ""
}

func (s *Server) handleTeamSummarize(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	var req teamSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sid := s.resolveSession(req)
	if sid == "" {
		writeError(w, http.StatusBadRequest, "task_id or session_id required")
		return
	}
	summary, err := s.eng.Summarize(r.Context(), sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": sid, "summary": summary})
}

func (s *Server) handleTeamFork(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	var req teamSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sid := s.resolveSession(req)
	if sid == "" {
		writeError(w, http.StatusBadRequest, "task_id or session_id required")
		return
	}
	newID, err := s.eng.Fork(r.Context(), sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": newID})
}

type teamResolveRequest struct {
	BatchID  string `json:"batch_id"`
	Decision string `json:"decision"`
}

func (s *Server) handleTeamResolve(w http.ResponseWriter, r *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	var req teamResolveRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	decision := team_engine.EscalationDecision(req.Decision)
	switch decision {
	case team_engine.EscalationContinue, team_engine.EscalationRetry,
		team_engine.EscalationAbort, team_engine.EscalationModify:
	default:
		writeError(w, http.StatusBadRequest, "invalid decision: use continue|retry|abort|modify")
		return
	}
	if err := s.eng.ResolveEscalation(req.BatchID, decision); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "batch_id": req.BatchID, "decision": req.Decision})
}

// broadcastMasterUpdate 在 master 创建/状态变更时广播一个 dag 事件，供前端立即着色。
func (s *Server) broadcastMasterUpdate(masterID string) {
	snap, err := s.dagSnapshot(masterID)
	if err != nil {
		return
	}
	s.broadcastEvent(NewDAGEvent(snap, masterID))
}

// ---------------------------------------------------------------------------
// DAG 快照：plan.json（静态）+ 运行时状态（Task.State）叠加
// ---------------------------------------------------------------------------

// dagSnapshot 读取 <whiteboardDir>/<masterID>/plan.json，按 batch_id 聚合为 DAG 节点，
// 以 depends_on 为 batch 级依赖边，并从 eng.Store 叠加每个任务运行时状态。
func (s *Server) dagSnapshot(masterID string) (DAGEvent, error) {
	path := filepath.Join(s.eng.Whiteboard.BaseDir(), masterID, "plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return DAGEvent{}, fmt.Errorf("read plan.json: %w", err)
	}

	var plan struct {
		Generated string `json:"generated"`
		Tasks     []struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Output      string   `json:"output"`
			Role        string   `json:"role"`
			BatchID     string   `json:"batch_id"`
			DependsOn   []string `json:"depends_on"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return DAGEvent{}, fmt.Errorf("parse plan.json: %w", err)
	}

	// 1. 按 batch_id 聚合静态任务（保持首次出现顺序）。
	type batchAcc struct {
		label     string
		dependsOn map[string]struct{}
	}
	order := make([]string, 0)
	acc := map[string]*batchAcc{}
	for _, t := range plan.Tasks {
		if t.BatchID == "" {
			continue
		}
		if _, ok := acc[t.BatchID]; !ok {
			acc[t.BatchID] = &batchAcc{dependsOn: map[string]struct{}{}}
			order = append(order, t.BatchID)
		}
		for _, dep := range t.DependsOn {
			if dep != "" {
				acc[t.BatchID].dependsOn[dep] = struct{}{}
			}
		}
	}

	// 2. 运行时任务状态：按 batch_id 分组。
	runtimeTasks, err := s.eng.Store.ListTasksByMasterTask(masterID)
	if err != nil {
		return DAGEvent{}, fmt.Errorf("list runtime tasks: %w", err)
	}
	byBatch := map[string][]*team_engine.Task{}
	for _, t := range runtimeTasks {
		byBatch[t.BatchID] = append(byBatch[t.BatchID], t)
	}

	// 3. 组装 DAG 节点。
	snap := DAGEvent{MasterTaskID: masterID}
	for _, batchID := range order {
		b := acc[batchID]
		dep := make([]string, 0, len(b.dependsOn))
		for d := range b.dependsOn {
			dep = append(dep, d)
		}

		node := DAGBatchEvent{
			BatchID:   batchID,
			DependsOn: dep,
		}
		// 优先用计划内同名 task 的 title 作为 batch label。
		for _, t := range plan.Tasks {
			if t.BatchID == batchID && t.Title != "" {
				node.Label = t.Title
				break
			}
		}

		// 叠加运行时任务状态。
		for _, t := range byBatch[batchID] {
			node.Tasks = append(node.Tasks, DAGTaskEvent{
				TaskID: t.ID,
				Title:  t.Title,
				State:  string(t.State),
			})
		}
		node.Status = deriveBatchStatus(byBatch[batchID])
		snap.Batches = append(snap.Batches, node)
	}

	return snap, nil
}

// deriveBatchStatus 从一批运行时任务状态推导 DAG 节点（batch）的着色状态。
// 无运行时任务 → pending；存在活动态 → running；存在失败挂起 → failed；
// 其余（全部 done/verified）→ passed。
func deriveBatchStatus(tasks []*team_engine.Task) string {
	if len(tasks) == 0 {
		return string(team_engine.BatchStatusPending)
	}
	active := false
	failed := false
	for _, t := range tasks {
		switch t.State {
		case team_engine.TaskStateDone, team_engine.TaskStateVerified:
			// terminal success
		case team_engine.TaskStateFailed, team_engine.TaskStateSuspended:
			failed = true
		default:
			active = true
		}
	}
	if failed {
		return string(team_engine.BatchStatusFailed)
	}
	if active {
		return string(team_engine.BatchStatusRunning)
	}
	return string(team_engine.BatchStatusPassed)
}

// broadcastEvent 将统一 daemon.Event 编码后送入 EventHub。
func (s *Server) broadcastEvent(ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.hub.Broadcast(SSEEvent{Name: string(ev.Channel), Data: data})
}
