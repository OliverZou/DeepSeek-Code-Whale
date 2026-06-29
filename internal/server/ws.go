// Package server provides the Whale daemon WebSocket server.
//
// The daemon embeds the full Agent loop and TeamEngine, exposing
// everything through a single WebSocket endpoint (ws://host:port/ws).
// A thin frontend (whale-pod) connects as a WS client and relays all
// user interaction.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/usewhale/whale/internal/agent"
	"github.com/usewhale/whale/internal/app"
	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/llm/deepseek"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/team_engine"
	teamlog "github.com/usewhale/whale/internal/team_engine/log"
	"github.com/usewhale/whale/internal/tools"
)

// =========================================================================
// Config
// =========================================================================

// DaemonConfig holds daemon startup parameters.
type DaemonConfig struct {
	Port    int
	DataDir string
	WorkDir string
}

// =========================================================================
// Protocol messages
// =========================================================================

type wsRequest struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type wsResponse struct {
	Type    string      `json:"type"`
	ID      string      `json:"id,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}

type wsPush struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// Chat payloads.
type chatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
	DeepThink bool   `json:"deep_think,omitempty"`
}

type chatStreamChunk struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Done      bool   `json:"done"`
	Error     string `json:"error,omitempty"`
}

// Task payloads.
type taskCreateRequest struct {
	Goal     string `json:"goal"`
	WorkDir  string `json:"workdir,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

// Approval / user-input payloads.
type approvalRequired struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Reason     string `json:"reason"`
	Code       string `json:"code"`
	Key        string `json:"key"`
}

type approvalDecision struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Decision   string `json:"decision"` // allow | deny | allow_session | cancel
}

type userInputRequired struct {
	SessionID  string       `json:"session_id"`
	ToolCallID string       `json:"tool_call_id"`
	Questions  []userInputQ `json:"questions"`
}

type userInputQ struct {
	ID       string         `json:"id"`
	Header   string         `json:"header"`
	Question string         `json:"question"`
	Options  []userInputOpt `json:"options"`
}

type userInputOpt struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type userInputResponsePayload struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Answer     string `json:"answer"`
}

// =========================================================================
// Daemon
// =========================================================================

// Daemon is the WebSocket server that hosts the Agent and TeamEngine.
type Daemon struct {
	cfg    DaemonConfig
	engine *team_engine.TeamEngine

	toolset     *tools.Toolset
	toolReg     *core.ToolRegistry
	store       *store.JSONLStore
	sessionsDir string

	whaleCfg app.Config

	httpServer *http.Server
	clients    map[string]*wsClient
	mu         sync.Mutex

	// pendingApproval maps toolCallID to a channel.
	pendingApproval   map[string]chan policy.ApprovalDecision
	pendingApprovalMu sync.Mutex

	// pendingUserInput maps toolCallID to response channel.
	pendingUserInput   map[string]chan userInputResp
	pendingUserInputMu sync.Mutex

	wg sync.WaitGroup
}

type userInputResp struct {
	Response  core.UserInputResponse
	Cancelled bool
}

type wsClient struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex
	daemon  *Daemon
}

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// =========================================================================
// NewDaemon
// =========================================================================

// NewDaemon creates a daemon with TeamEngine, Agent, and all tools.
func NewDaemon(eng *team_engine.TeamEngine, cfg DaemonConfig) (*Daemon, error) {
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// Team engine logging.
	teamlogDir := filepath.Join(cfg.WorkDir, "team_tasks", "logs")
	os.MkdirAll(teamlogDir, 0755)
	if tl := teamlog.NewTeamLog(cfg.WorkDir); tl != nil {
		tl.AddLog(filepath.Join(teamlogDir, "daemon.log"))
		team_engine.SetLogger(tl)
	}

	// Sessions.
	sessionsDir := store.DefaultSessionsDir(cfg.DataDir)
	sessStore, err := store.NewJSONLStore(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("init session store: %w", err)
	}

	// Tools.
	toolset, err := tools.NewToolset(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("init tools: %w", err)
	}
	toolReg, err := core.NewToolRegistryChecked(toolset.Tools())
	if err != nil {
		return nil, fmt.Errorf("init tool registry: %w", err)
	}

	// Load whale config.
	whaleCfg, err := app.LoadAndApplyConfig(app.DefaultConfig(), cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	d := &Daemon{
		cfg:              cfg,
		engine:           eng,
		toolset:          toolset,
		toolReg:          toolReg,
		store:            sessStore,
		sessionsDir:      sessionsDir,
		whaleCfg:         whaleCfg,
		clients:          make(map[string]*wsClient),
		pendingApproval:  make(map[string]chan policy.ApprovalDecision),
		pendingUserInput: make(map[string]chan userInputResp),
	}

	// Team engine events → broadcast.
	eng.OnEvent(func(evt team_engine.TaskEvent) {
		d.broadcast(wsPush{
			Type: "task.state_changed",
			Payload: map[string]interface{}{
				"task_id":   evt.TaskID,
				"title":     evt.Title,
				"old_state": evt.OldState,
				"new_state": evt.NewState,
				"progress":  evt.Progress,
			},
		})
	})

	// HTTP mux.
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", d.handleWS)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	d.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}

	return d, nil
}

// ListenAndServe starts the HTTP server (blocking).
func (d *Daemon) ListenAndServe() error {
	return d.httpServer.ListenAndServe()
}

// Close shuts down the daemon gracefully.
func (d *Daemon) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.httpServer.Shutdown(ctx)
	d.wg.Wait()
}

// =========================================================================
// WebSocket handler
// =========================================================================

func (d *Daemon) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("daemon: ws upgrade: %v", err)
		return
	}

	client := &wsClient{
		id:     uuid.New().String(),
		conn:   conn,
		daemon: d,
	}

	d.mu.Lock()
	d.clients[client.id] = client
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.clients, client.id)
		d.mu.Unlock()
		conn.Close()
	}()

	// Read loop.
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var req wsRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid json"}})
			continue
		}
		d.handleMessage(client, req)
	}
}

func (d *Daemon) handleMessage(client *wsClient, req wsRequest) {
	switch req.Type {
	case "chat":
		go d.handleChat(client, req)
	case "task.create":
		go d.handleTaskCreate(client, req)
	case "task.list":
		d.handleTaskList(client, req)
	case "task.cancel":
		d.handleTaskCancel(client, req)
	case "task.delete":
		d.handleTaskDelete(client, req)
	case "session.list":
		d.handleSessionList(client, req)
	case "approval.decision":
		d.handleApprovalDecision(client, req)
	case "user_input.response":
		d.handleUserInputResponse(client, req)
	default:
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("unknown type: %s", req.Type)}})
	}
}

// =========================================================================
// Chat
// =========================================================================

func (d *Daemon) handleChat(client *wsClient, req wsRequest) {
	var p chatRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if strings.TrimSpace(p.Message) == "" {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "empty message"}})
		return
	}

	sessionID := p.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	// Determine model.
	model := d.whaleCfg.Model
	if model == "" {
		model = "deepseek-chat"
	}
	if p.DeepThink {
		model = "deepseek-reasoner"
	}

	// Build provider.
	apiKey := resolveAPIKey(d.cfg.DataDir)
	prov, err := deepseek.New(
		deepseek.WithAPIKey(apiKey),
		deepseek.WithModel(model),
		deepseek.WithReasoningEffort(d.whaleCfg.ReasoningEffort),
		deepseek.WithThinking(p.DeepThink),
	)
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("init provider: %v", err)}})
		return
	}

	// Write user message.
	_, err = d.store.Create(context.Background(), core.Message{
		SessionID: sessionID,
		Role:      core.RoleUser,
		Text:      p.Message,
	})
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("write message: %v", err)}})
		return
	}

	// Update session meta.
	session.UpdateSessionMeta(d.sessionsDir, sessionID, func(m *session.SessionMeta) {
		if m.StartedAt.IsZero() {
			m.StartedAt = time.Now()
		}
		m.TurnCount++
		m.Status = "active"
	})

	// Build Agent.
	ag := agent.NewAgentWithRegistry(prov, d.store, d.toolReg,
		agent.WithSessionMode(session.ModeAgent),
		agent.WithSessionsDir(d.sessionsDir),
		agent.WithApprovalFunc(d.makeApprovalFunc(client)),
		agent.WithUserInputFunc(d.makeUserInputFunc(client)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := ag.RunStreamWithContentOptions(ctx, sessionID,
		[]core.MessagePart{{Type: core.MessagePartText, Text: p.Message}},
		agent.RunOptions{},
	)
	if err != nil {
		client.send(wsPush{Type: "chat.stream", Payload: chatStreamChunk{
			SessionID: sessionID, Done: true, Error: err.Error(),
		}})
		return
	}

	// Stream agent events to client.
	var contentBuf, thinkingBuf string
	for ev := range events {
		switch ev.Type {
		case agent.AgentEventTypeAssistantDelta:
			contentBuf += ev.Content
			client.send(wsPush{Type: "chat.stream", Payload: chatStreamChunk{
				SessionID: sessionID, Content: ev.Content,
			}})
		case agent.AgentEventTypeReasoningDelta:
			thinkingBuf += ev.ReasoningDelta
			client.send(wsPush{Type: "chat.stream", Payload: chatStreamChunk{
				SessionID: sessionID, Thinking: ev.ReasoningDelta,
			}})
		case agent.AgentEventTypeToolCall:
			client.send(wsPush{Type: "chat.stream", Payload: chatStreamChunk{
				SessionID: sessionID,
				Content:   fmt.Sprintf("\n[调用工具: %s %s]", ev.ToolCall.Name, ev.ToolCall.Input),
			}})
		case agent.AgentEventTypeToolResult:
			// Don't spam — tool results show in final output.
		case agent.AgentEventTypeDone:
			// Final message already streamed via deltas.
		case agent.AgentEventTypeError:
			client.send(wsPush{Type: "chat.stream", Payload: chatStreamChunk{
				SessionID: sessionID, Error: ev.Err.Error(),
			}})
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	// Write final AI message.
	if contentBuf != "" {
		d.store.Create(context.Background(), core.Message{
			SessionID: sessionID,
			Role:      core.RoleAssistant,
			Text:      contentBuf,
			Reasoning: thinkingBuf,
		})
	}

	// Done.
	client.send(wsPush{Type: "chat.stream", Payload: chatStreamChunk{
		SessionID: sessionID, Done: true,
	}})
	client.send(wsResponse{Type: "chat", ID: req.ID, Payload: map[string]string{
		"session_id": sessionID,
	}})
}

// =========================================================================
// Approval / UserInput bridges
// =========================================================================

func (d *Daemon) makeApprovalFunc(client *wsClient) policy.ApprovalFunc {
	return func(req policy.ApprovalRequest) policy.ApprovalDecision {
		ch := make(chan policy.ApprovalDecision, 1)
		d.pendingApprovalMu.Lock()
		d.pendingApproval[req.ToolCall.ID] = ch
		d.pendingApprovalMu.Unlock()

		defer func() {
			d.pendingApprovalMu.Lock()
			delete(d.pendingApproval, req.ToolCall.ID)
			d.pendingApprovalMu.Unlock()
		}()

		client.send(wsPush{Type: "approval.required", Payload: approvalRequired{
			SessionID:  req.SessionID,
			ToolCallID: req.ToolCall.ID,
			ToolName:   req.ToolCall.Name,
			Reason:     req.Reason,
			Code:       req.Code,
			Key:        req.Key,
		}})

		select {
		case decision := <-ch:
			return decision
		case <-time.After(5 * time.Minute):
			return policy.ApprovalDeny
		}
	}
}

func (d *Daemon) handleApprovalDecision(client *wsClient, req wsRequest) {
	var p approvalDecision
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return
	}

	d.pendingApprovalMu.Lock()
	ch, ok := d.pendingApproval[p.ToolCallID]
	d.pendingApprovalMu.Unlock()

	if !ok {
		return
	}

	var decision policy.ApprovalDecision
	switch p.Decision {
	case "allow":
		decision = policy.ApprovalAllow
	case "allow_session":
		decision = policy.ApprovalAllowForSession
	case "cancel":
		decision = policy.ApprovalCancel
	default:
		decision = policy.ApprovalDeny
	}

	select {
	case ch <- decision:
	default:
	}
}

func (d *Daemon) makeUserInputFunc(client *wsClient) agent.UserInputFunc {
	return func(req agent.UserInputRequest) (core.UserInputResponse, bool) {
		ch := make(chan userInputResp, 1)
		d.pendingUserInputMu.Lock()
		d.pendingUserInput[req.ToolCall.ID] = ch
		d.pendingUserInputMu.Unlock()

		defer func() {
			d.pendingUserInputMu.Lock()
			delete(d.pendingUserInput, req.ToolCall.ID)
			d.pendingUserInputMu.Unlock()
		}()

		qs := make([]userInputQ, len(req.Questions))
		for i, q := range req.Questions {
			opts := make([]userInputOpt, len(q.Options))
			for j, o := range q.Options {
				opts[j] = userInputOpt{Label: o.Label, Description: o.Description}
			}
			qs[i] = userInputQ{
				ID:       q.ID,
				Header:   q.Header,
				Question: q.Question,
				Options:  opts,
			}
		}

		client.send(wsPush{Type: "user_input.required", Payload: userInputRequired{
			SessionID:  req.SessionID,
			ToolCallID: req.ToolCall.ID,
			Questions:  qs,
		}})

		select {
		case resp := <-ch:
			if resp.Cancelled {
				return core.UserInputResponse{}, false
			}
			return resp.Response, true
		case <-time.After(5 * time.Minute):
			return core.UserInputResponse{}, false
		}
	}
}

func (d *Daemon) handleUserInputResponse(client *wsClient, req wsRequest) {
	var p userInputResponsePayload
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return
	}

	d.pendingUserInputMu.Lock()
	ch, ok := d.pendingUserInput[p.ToolCallID]
	d.pendingUserInputMu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- userInputResp{
		Response: core.UserInputResponse{
			Answers: []core.UserInputAnswer{{ID: "answer", Label: p.Answer, Value: p.Answer}},
		},
	}:
	default:
	}
}

// =========================================================================
// Task operations
// =========================================================================

func (d *Daemon) handleTaskCreate(client *wsClient, req wsRequest) {
	var p taskCreateRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}

	workDir := p.WorkDir
	if workDir == "" {
		workDir = d.cfg.WorkDir
	}

	mt, err := d.engine.CreateMasterTask(p.Goal, workDir, "")
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("create task: %v", err)}})
		return
	}

	go func() {
		d.engine.PlanAndRun(context.Background(), p.Goal, workDir, mt.ID)
	}()

	client.send(wsResponse{Type: "task.created", ID: req.ID, Payload: map[string]string{
		"master_task_id": mt.ID,
	}})
}

func (d *Daemon) handleTaskList(client *wsClient, req wsRequest) {
	tasks, err := d.engine.ListMasterTasks()
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("list tasks: %v", err)}})
		return
	}
	result := make([]map[string]interface{}, len(tasks))
	for i, mt := range tasks {
		subtasks, _ := d.engine.Store.ListTasksByMasterTask(mt.ID)
		result[i] = map[string]interface{}{
			"id":         mt.ID,
			"goal":       mt.Goal,
			"status":     mt.Status,
			"workdir":    mt.WorkspacePath,
			"created_at": mt.CreatedAt,
			"task_count": len(subtasks),
		}
	}
	client.send(wsResponse{Type: "task.list", ID: req.ID, Payload: map[string]interface{}{
		"tasks": result,
	}})
}

func (d *Daemon) handleTaskCancel(client *wsClient, req wsRequest) {
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	d.engine.CancelMasterTaskExecution(p.TaskID)
	client.send(wsResponse{Type: "task.canceled", ID: req.ID, Payload: map[string]string{"task_id": p.TaskID}})
}

func (d *Daemon) handleTaskDelete(client *wsClient, req wsRequest) {
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if err := d.engine.DeleteMasterTaskAndChildren(p.TaskID); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("delete task: %v", err)}})
		return
	}
	client.send(wsResponse{Type: "task.deleted", ID: req.ID, Payload: map[string]string{"task_id": p.TaskID}})
}

// handleSessionList returns recent sessions from the JSONL store.
func (d *Daemon) handleSessionList(client *wsClient, req wsRequest) {
	sessions, err := session.ListSessions(d.sessionsDir, 50)
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}
	result := make([]map[string]interface{}, 0, len(sessions))
	for _, s := range sessions {
		if s.Meta.Kind == "subagent" {
			continue
		}
		goal := s.Meta.Title
		if goal == "" {
			goal = s.Conversation
		}
		status := "done"
		if s.Meta.Status == "active" {
			status = "running"
		}
		result = append(result, map[string]interface{}{
			"id":              s.ID,
			"goal":            goal,
			"agent":           s.Meta.Agent,
			"workspace_path":  s.Meta.Workspace,
			"workspace_label": filepath.Base(s.Meta.Workspace),
			"status":          status,
			"created_at":      s.Meta.StartedAt,
			"task_count":      0,
		})
	}
	client.send(wsResponse{Type: "session.list", ID: req.ID, Payload: map[string]interface{}{"sessions": result}})
}

// =========================================================================
// Helpers
// =========================================================================

func (d *Daemon) broadcast(msg wsPush) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.clients {
		c.send(msg)
	}
}

func (c *wsClient) send(msg interface{}) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	c.conn.WriteJSON(msg)
}

func resolveAPIKey(dataDir string) string {
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		return key
	}
	credPath := filepath.Join(dataDir, "credentials.json")
	if data, err := os.ReadFile(credPath); err == nil {
		var creds struct {
			DeepSeekAPIKey string `json:"deepseek_api_key"`
		}
		if json.Unmarshal(data, &creds) == nil && creds.DeepSeekAPIKey != "" {
			return creds.DeepSeekAPIKey
		}
	}
	return ""
}
