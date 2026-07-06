// Package server provides the Whale daemon WebSocket server.
//
// The daemon embeds the full Agent loop and TeamEngine, exposing
// everything through a single WebSocket endpoint (ws://host:port/ws).
// A thin frontend (whale-pod) connects as a WS client and relays all
// user interaction.
//
// # Quick Start (pod developer)
//
//	// 1. Connect
//	conn, _, _ := websocket.DefaultDialer.Dial("ws://localhost:18900/ws", nil)
//
//	// 2. Send a chat request
//	conn.WriteJSON(map[string]interface{}{
//	    "type": "chat",
//	    "id":   "req-1",
//	    "payload": map[string]interface{}{
//	        "message": "Write a Go function",
//	    },
//	})
//
//	// 3. Read streaming responses + final response
//	for {
//	    var msg map[string]interface{}
//	    conn.ReadJSON(&msg)
//	    switch msg["type"] {
//	    case "chat.stream":
//	        event := msg["payload"].(map[string]interface{})["event"]
//	        // render based on event type (assistant, thinking, tool_call, ...)
//	    case "chat":
//	        sessionID := msg["payload"].(map[string]interface{})["session_id"]
//	        // chat done — save session_id for next turn
//	    }
//	}
//
// # Complete Protocol Reference
//
// All messages are JSON. Requests and responses carry an "id" for correlation.
// Pushes have no "id" and are server-initiated.
//
// ## Chat & Agent
//
//	→ {"type":"chat", "id":"1", "payload":{"message":"...","session_id":"(optional)","deep_think":false}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"assistant","content":"delta..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"thinking","content":"delta..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"tool_call","tool_call_id":"...","tool_name":"...","tool_input":"..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"tool_result","tool_call_id":"...","tool_name":"...","tool_outcome":"success","tool_status":"","content":"..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"plan","content":"delta..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"subagent","task_id":"...","task_title":"...","task_status":"started|progress|done"}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"task","task_id":"...","task_title":"...","task_status":"started|done"}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"hook","task_title":"hook_name: decision"}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"error","error":"..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"response_reset"}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"context_compacted"}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"provider_retry","content":"provider retrying..."}}
//	← {"type":"chat.stream", "payload":{"session_id":"...","event":"done","done":true}}
//	← {"type":"chat", "id":"1", "payload":{"session_id":"..."}}
//
//	→ {"type":"chat.cancel", "id":"2", "payload":{"session_id":"..."}}
//	← {"type":"chat.canceled", "id":"2", "payload":{"session_id":"..."}}
//
// ## Approval & User Input
//
//	← {"type":"approval.required", "payload":{"session_id":"...","tool_call_id":"...","tool_name":"...","reason":"...","code":"...","key":"..."}}
//	→ {"type":"approval.decision", "id":"3", "payload":{"session_id":"...","tool_call_id":"...","decision":"allow|deny|allow_session|cancel"}}
//
//	← {"type":"user_input.required", "payload":{"session_id":"...","tool_call_id":"...","questions":[{"id":"...","header":"...","question":"...","options":[{"label":"...","description":"..."}]}]}}
//	→ {"type":"user_input.response", "id":"4", "payload":{"session_id":"...","tool_call_id":"...","answer":"..."}}
//
// ## Task Management (Team Engine)
//
//	→ {"type":"task.create", "id":"5", "payload":{"goal":"...","workdir":"...","team_name":"(optional)"}}
//	← {"type":"task.created", "id":"5", "payload":{"master_task_id":"..."}}
//
//	→ {"type":"task.list", "id":"6"}
//	← {"type":"task.list", "id":"6", "payload":{"tasks":[{"id":"...","goal":"...","status":"...","workdir":"...","created_at":"...","task_count":0}]}}
//
//	→ {"type":"task.cancel", "id":"7", "payload":{"task_id":"..."}}
//	← {"type":"task.canceled", "id":"7", "payload":{"task_id":"..."}}
//
//	→ {"type":"task.delete", "id":"8", "payload":{"task_id":"..."}}
//	← {"type":"task.deleted", "id":"8", "payload":{"task_id":"..."}}
//
//	← {"type":"task.state_changed", "payload":{"task_id":"...","title":"...","old_state":"...","new_state":"...","progress":50}}
//	← {"type":"task.log", "payload":{"task_id":"...","role":"leader|agent"}}
//
//	→ {"type":"task.subtasks", "id":"5a", "payload":{"master_task_id":"..."}}
//	← {"type":"task.subtasks", "id":"5a", "payload":{"subtasks":[{...recursive...}]}}
//
//	→ {"type":"task.dialogue", "id":"5b", "payload":{"task_id":"..."}}
//	← {"type":"task.dialogue", "id":"5b", "payload":{"task_id":"...","output":"...","verifier":"...","confirmation":"..."}}
//
//	→ {"type":"task.plan", "id":"5c", "payload":{"master_task_id":"..."}}
//	← {"type":"task.plan", "id":"5c", "payload":{"master_task_id":"...","plan_md":"...","plan_json":"...","spec_md":"..."}}
//
//	→ {"type":"task.feedback", "id":"5d", "payload":{"task_id":"...","message":"..."}}
//	← {"type":"task.feedback", "id":"5d", "payload":{"task_id":"...","status":"sent"}}
//
//	→ {"type":"task.confirm", "id":"5e", "payload":{"task_id":"...","decision":"confirm|reject","comment":"(optional)"}}
//	← {"type":"task.confirmed", "id":"5e", "payload":{"task_id":"...","decision":"..."}}
//
//	→ {"type":"task.confirmations", "id":"5f", "payload":{"master_task_id":"..."}}
//	← {"type":"task.confirmations", "id":"5f", "payload":{"pending":[...]}}
//
//	→ {"type":"task.updateGoal", "id":"5g", "payload":{"task_id":"...","goal":"..."}}
//	← {"type":"task.goalUpdated", "id":"5g", "payload":{"task_id":"...","goal":"...","old_goal":"..."}}
//
// ## Session Management
//
//	→ {"type":"session.list", "id":"9"}
//	← {"type":"session.list", "id":"9", "payload":{"sessions":[{"id":"...","goal":"...","agent":"...","workspace_path":"...","workspace_label":"...","workspace_id":"...","session_path":"...","status":"...","created_at":"...","task_count":0,"done_count":0,"active_count":0,"suspended_count":0,"workspace_online":true}]}}
//
//	→ {"type":"session.listByAgent", "id":"10", "payload":{"agent":"...","offset":0,"limit":20}}
//	← {"type":"session.listByAgent", "id":"10", "payload":{"sessions":[...],"has_more":false}}
//
//	→ {"type":"session.getMessages", "id":"11", "payload":{"id":"session-uuid"}}
//	← {"type":"session.getMessages", "id":"11", "payload":{"messages":[{"time":"...","from":"human|agent","content":"...","thinking":"...","durationMs":0}]}}
//
//	→ {"type":"session.delete", "id":"12", "payload":{"id":"session-uuid"}}
//	← {"type":"session.delete", "id":"12", "payload":{"id":"session-uuid"}}
//
//	→ {"type":"session.deleteAll", "id":"13", "payload":{"agent":"(optional)"}}
//	← {"type":"session.deleteAll", "id":"13", "payload":{}}
//
//	→ {"type":"session.clearEmpty", "id":"14", "payload":{"agent":"(optional)"}}
//	← {"type":"session.clearEmpty", "id":"14", "payload":{}}
//
// ## Resource Listing
//
//	→ {"type":"agent.list", "id":"15"}
//	← {"type":"agent.list", "id":"15", "payload":{"agents":[...]}}
//
//	→ {"type":"expert.list", "id":"16"}
//	← {"type":"expert.list", "id":"16", "payload":{"experts":[...]}}
//
//	→ {"type":"team.list", "id":"17"}
//	← {"type":"team.list", "id":"17", "payload":{"teams":[...]}}
//
// ## MCP Management
//
//	→ {"type":"mcp.list", "id":"18"}
//	← {"type":"mcp.list", "id":"18", "payload":{"servers":[{"name":"...","status":"...","disabled":false,"tools":0}]}}
//
//	→ {"type":"mcp.setEnabled", "id":"19", "payload":{"name":"...","enabled":true}}
//	← {"type":"mcp.setEnabled", "id":"19", "payload":{"name":"...","enabled":true}}
//
// ## File Read
//
//	→ {"type":"file.read", "id":"20", "payload":{"path":"relative/or/absolute"}}
//	← {"type":"file.read", "id":"20", "payload":{"path":"...","content":"...","size":1234}}
//
// ## Team Chat
//
//	→ {"type":"team.chat.send", "id":"21", "payload":{"master_task_id":"...","from":"...","to":"...","content":"..."}}
//	← {"type":"team.chat.sent", "id":"21", "payload":{...}}
//	← {"type":"team.chat.message", "payload":{...}} (broadcast push)
//
//	→ {"type":"team.chat.messages", "id":"22", "payload":{"master_task_id":"..."}}
//	← {"type":"team.chat.messages", "id":"22", "payload":{"messages":[...]}}
//
// ## Health
//
//	GET /health → 200 "ok"
//
// ## Errors
//
//	← {"type":"error", "id":"...", "payload":{"message":"..."}}
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
	"gopkg.in/yaml.v3"

	"github.com/usewhale/whale/internal/agent"
	"github.com/usewhale/whale/internal/app"
	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/llm/deepseek"
	whalemcp "github.com/usewhale/whale/internal/mcp"
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
//
// Port: WebSocket listen port (default 18900).
// DataDir: whale data directory (~/.whale) for sessions, creds, agents, etc.
// WorkDir: working directory for team engine tasks and whiteboard.
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

// chatRequest is a chat message from the pod to the daemon.
// SessionID is optional; omit to create a new session.
// Set DeepThink to true for deepseek-reasoner model.
type chatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	DeepThink bool   `json:"deep_think,omitempty"`
	WorkDir   string `json:"workdir,omitempty"`
}

// chatStreamChunk is a single streaming event from the daemon to the pod.
//
// The pod should switch on the "event" field to decide how to render:
//
//	event="assistant"  → append Content to the markdown message body
//	event="thinking"   → append Content to the collapsible reasoning area
//	event="tool_call"  → show a tool-call card (ToolCallID + ToolName + ToolInput)
//	event="tool_result"→ show a tool-result card (ToolOutcome + ToolStatus + Content)
//	event="plan"       → append Content as plan step delta, or show PlanText as complete plan
//	event="subagent"   → update subagent card (TaskID + TaskTitle + TaskStatus)
//	event="task"       → update parallel-reason card
//	event="hook"       → show hook notification (TaskTitle = "name: decision")
//	event="error"      → show error banner (Content or Error field)
//	event="response_reset" → clear previous partial assistant content
//	event="context_compacted" → note that context was compacted (no Content)
//	event="provider_retry" → show "retrying..." indicator
//	event="done"       → turn complete (Done=true), finalize the message
//
// IMPORTANT: Both "assistant" and "thinking" events use the Content field.
// The pod must check the "event" field to know where to render the text.
// There is no separate "thinking" field — reasoning deltas arrive as
// event="thinking" with the text in Content.
//
// ToolOutcome: "success", "error", "skipped", "no_result"
// ToolStatus: "failed" or empty (empty = success)
// TaskStatus: "started", "progress", "done"
type chatStreamChunk struct {
	SessionID string `json:"session_id"`
	Event     string `json:"event"` // rendering target: assistant, thinking, tool_call, tool_result, plan, subagent, task, hook, error, response_reset, context_compacted, provider_retry, done

	// Deltas
	Content string `json:"content,omitempty"`

	// Tool call
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolInput  string `json:"tool_input"`

	// Tool result
	ToolOutcome string `json:"tool_outcome,omitempty"` // success, error, skipped, no_result
	ToolStatus  string `json:"tool_status,omitempty"`  // "failed" or empty
	ToolCode    string `json:"tool_code,omitempty"`

	// Plan
	PlanText string `json:"plan_text,omitempty"`

	// Subagent / task
	TaskID     string `json:"task_id,omitempty"`
	TaskTitle  string `json:"task_title,omitempty"`
	TaskStatus string `json:"task_status,omitempty"` // started, progress, done

	// Completion
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

type chatCancelRequest struct {
	SessionID string `json:"session_id"`
}

// Subtask / dialogue / plan payloads.
type subtaskRequest struct {
	MasterTaskID string `json:"master_task_id"`
	SessionID    string `json:"session_id,omitempty"`
}

type dialogueRequest struct {
	TaskID string `json:"task_id"`
}

type planRequest struct {
	MasterTaskID string `json:"master_task_id"`
}

// Feedback / confirmation payloads.
type feedbackRequest struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

type confirmRequest struct {
	TaskID   string `json:"task_id"`
	Decision string `json:"decision"` // "confirm" or "reject"
	Comment  string `json:"comment,omitempty"`
}

type confirmationsRequest struct {
	MasterTaskID string `json:"master_task_id"`
}

// MCP payloads.
type mcpSetEnabledRequest struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// File read payload.
type fileReadRequest struct {
	Path string `json:"path"`
}

// Team chat payloads.
type teamChatSendRequest struct {
	MasterTaskID string `json:"master_task_id"`
	From         string `json:"from"`
	To           string `json:"to"`
	Content      string `json:"content"`
}

type teamChatMessagesRequest struct {
	MasterTaskID string `json:"master_task_id"`
}

// Task updateGoal payload.
type taskUpdateGoalRequest struct {
	TaskID string `json:"task_id"`
	Goal   string `json:"goal"`
}

// taskCreateRequest is a team engine task creation request.
// Goal is the task description. WorkDir is the workspace path.
type taskCreateRequest struct {
	Goal     string `json:"goal"`
	WorkDir  string `json:"workdir,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

// approvalRequired is pushed when the agent needs permission to run a tool.
// The pod should show a confirmation dialog and send approval.decision.
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
//
// Create with NewDaemon(), start with ListenAndServe(). A single Daemon
// handles multiple concurrent WebSocket clients. All clients receive the
// same broadcast pushes (task_state_changed, chat.stream).
//
// Lifecycle:
//
//	eng := team_engine.New(...)
//	d, _ := server.NewDaemon(eng, server.DaemonConfig{Port: 18900, ...})
//	go d.ListenAndServe()  // blocks
//	defer d.Close()        // graceful shutdown
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

	// pendingCancels maps sessionID to cancel func for in-flight chats.
	pendingCancels   map[string]context.CancelFunc
	pendingCancelsMu sync.Mutex

	// MCP manager for mcp.list / mcp.setEnabled.
	mcpManager *whalemcp.Manager

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
//
// It initializes:
//   - Team engine logging (team_tasks/logs/)
//   - Session store (JSONL)
//   - Tool registry (all built-in tools)
//   - Whale config (model, effort, etc.)
//   - Team engine event → WebSocket broadcast bridge
//   - HTTP mux with /ws and /health endpoints
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

	// MCP manager.
	mcpConfigPath := whalemcp.DefaultConfigPath(cfg.DataDir)
	mcpCfg, _ := whalemcp.LoadConfig(mcpConfigPath)
	mcpMgr := whalemcp.NewManager(mcpCfg, cfg.WorkDir)
	mcpMgr.SetSecretsDir(cfg.DataDir)

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
		pendingCancels:   make(map[string]context.CancelFunc),
		mcpManager:       mcpMgr,
	}

	// Team engine events → broadcast.
	eng.OnEvent(func(evt team_engine.TaskEvent) {
		switch evt.Type {
		case team_engine.EventStateChanged, team_engine.EventTaskDone,
			team_engine.EventWorkerOutput, team_engine.EventVerifierResult:
			d.broadcast(wsPush{
				Type: "task.state_changed",
				Payload: map[string]interface{}{
					"task_id":   evt.TaskID,
					"title":     evt.Title,
					"old_state": evt.OldState,
					"new_state": evt.NewState,
					"progress":  evt.Progress,
					"data":      evt.Data,
				},
			})
		case team_engine.EventLeaderLog:
			d.broadcast(wsPush{
				Type: "task.log",
				Payload: map[string]interface{}{
					"task_id": evt.TaskID,
					"role":    "leader",
				},
			})
		case team_engine.EventAgentLog:
			d.broadcast(wsPush{
				Type: "task.log",
				Payload: map[string]interface{}{
					"task_id": evt.TaskID,
					"role":    "agent",
				},
			})
		}
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
	case "session.listByAgent":
		d.handleSessionListByAgent(client, req)
	case "session.delete":
		d.handleSessionDelete(client, req)
	case "session.deleteAll":
		d.handleSessionDeleteAll(client, req)
	case "session.clearEmpty":
		d.handleSessionClearEmpty(client, req)
	case "session.getMessages":
		d.handleSessionGetMessages(client, req)
	case "session.getToolResult":
		d.handleSessionGetToolResult(client, req)
	case "agent.list":
		d.handleAgentList(client, req)
	case "expert.list":
		d.handleExpertList(client, req)
	case "team.list":
		d.handleTeamList(client, req)
	case "chat.cancel":
		d.handleChatCancel(client, req)
	case "task.subtasks":
		d.handleTaskSubtasks(client, req)
	case "task.dialogue":
		d.handleTaskDialogue(client, req)
	case "task.plan":
		d.handleTaskPlan(client, req)
	case "task.feedback":
		d.handleTaskFeedback(client, req)
	case "task.confirm":
		d.handleTaskConfirm(client, req)
	case "task.confirmations":
		d.handleTaskConfirmations(client, req)
	case "task.updateGoal":
		d.handleTaskUpdateGoal(client, req)
	case "mcp.list":
		d.handleMCPList(client, req)
	case "mcp.setEnabled":
		d.handleMCPSetEnabled(client, req)
	case "mcp.setEnv":
		d.handleMCPSetEnv(client, req)
	case "file.read":
		d.handleFileRead(client, req)
	case "team.chat.send":
		d.handleTeamChatSend(client, req)
	case "team.chat.messages":
		d.handleTeamChatMessages(client, req)
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

// handleChat is the main agent chat handler.
// It creates a new agent, runs the user message through the full agent loop,
// and streams all events as structured chat.stream pushes.
// The response is sent asynchronously via push; the final "chat" response
// carries the session_id for subsequent turns.
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
		model = "deepseek-v4-flash"
	}
	if p.DeepThink {
		model = "deepseek-v4-pro"
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

	// Update session meta.
	session.UpdateSessionMeta(d.sessionsDir, sessionID, func(m *session.SessionMeta) {
		if m.StartedAt.IsZero() {
			m.StartedAt = time.Now()
			m.Workspace = p.WorkDir
		}
		m.TurnCount++
		if p.Agent != "" {
			m.Agent = p.Agent
		}
		// workspace set on first turn inside StartedAt.IsZero() block
		m.Status = "active"
	})

	chatStart := time.Now()

	// Build Agent.
	ag := agent.NewAgentWithRegistry(prov, d.store, d.toolReg,
		agent.WithSessionMode(session.ModeAgent),
		agent.WithSessionsDir(d.sessionsDir),
		agent.WithApprovalFunc(d.makeApprovalFunc(client)),
		agent.WithUserInputFunc(d.makeUserInputFunc(client)),
	)

	ctx, cancel := context.WithCancel(context.Background())

	// Register cancellation for this session.
	d.pendingCancelsMu.Lock()
	d.pendingCancels[sessionID] = cancel
	d.pendingCancelsMu.Unlock()

	defer func() {
		cancel()
		d.pendingCancelsMu.Lock()
		delete(d.pendingCancels, sessionID)
		d.pendingCancelsMu.Unlock()
	}()

	events, err := ag.RunStreamWithContentOptions(ctx, sessionID,
		[]core.MessagePart{{Type: core.MessagePartText, Text: p.Message}},
		agent.RunOptions{},
	)
	if err != nil {
		d.pushChat(client, sessionID, chatStreamChunk{Event: "error", Error: err.Error(), Done: true})
		return
	}

	// Stream agent events to client as structured chat.stream pushes.
	var contentBuf, thinkingBuf string
	var collectedTools []core.ToolCall
	flushThinking := func() {
		if thinkingBuf == "" { return }
		// Reasoning persisted by agent (stream_ingest.go) — no separate store.Create here.
		thinkingBuf = ""
	}
	for ev := range events {
		chunk := chatStreamChunk{SessionID: sessionID}

		switch ev.Type {
		case agent.AgentEventTypeAssistantDelta:
		flushThinking()
			contentBuf += ev.Content
			chunk.Event = "assistant"
			chunk.Content = ev.Content

		case agent.AgentEventTypeReasoningDelta:
			thinkingBuf += ev.ReasoningDelta
			chunk.Event = "thinking"
			chunk.Content = ev.ReasoningDelta

		case agent.AgentEventTypeToolCall:
		flushThinking()
			if ev.ToolCall != nil {
				chunk.Event = "tool_call"
				chunk.ToolCallID = ev.ToolCall.ID
				chunk.ToolName = ev.ToolCall.Name
				chunk.ToolInput = summarizeToolInput(ev.ToolCall.Name, ev.ToolCall.Input)
				collectedTools = append(collectedTools, *ev.ToolCall)
			}

		case agent.AgentEventTypeToolResult:
			if ev.Result != nil {
				outcome := string(core.ToolResultOutcome(*ev.Result))
				chunk.Event = "tool_result"
				chunk.ToolCallID = ev.Result.ToolCallID
				chunk.ToolName = ev.Result.Name
				chunk.Content = core.ToolResultModelText(*ev.Result)
				chunk.ToolOutcome = outcome
				chunk.ToolCode = ev.Result.Code
				if outcome != string(core.OutcomeSuccess) && outcome != string(core.OutcomeNoResult) {
					chunk.ToolStatus = "failed"
				}
			}

		case agent.AgentEventTypePlanDelta:
			chunk.Event = "plan"
			chunk.Content = ev.Content

		case agent.AgentEventTypePlanCompleted:
			chunk.Event = "plan"
			chunk.PlanText = ev.Content

		case agent.AgentEventTypePlanUpdate:
			if ev.PlanUpdate != nil {
				chunk.Event = "plan"
				chunk.PlanText = agent.FormatPlanUpdateForDisplay(*ev.PlanUpdate)
			}

		case agent.AgentEventTypeSubagentStarted:
			if ev.Task != nil {
				chunk.Event = "subagent"
				chunk.TaskID = ev.Task.ToolCallID
				chunk.TaskTitle = ev.Task.ToolName
				chunk.TaskStatus = "started"
			}

		case agent.AgentEventTypeTaskProgress:
			if ev.Task != nil {
				chunk.Event = "subagent"
				chunk.TaskID = ev.Task.ToolCallID
				chunk.TaskTitle = ev.Task.ToolName
				chunk.TaskStatus = "progress"
			}

		case agent.AgentEventTypeSubagentDone:
			if ev.Task != nil {
				chunk.Event = "subagent"
				chunk.TaskID = ev.Task.ToolCallID
				chunk.TaskTitle = ev.Task.ToolName
				chunk.TaskStatus = "done"
			}

		case agent.AgentEventTypeParallelReasonStarted:
			if ev.Task != nil {
				chunk.Event = "task"
				chunk.TaskID = ev.Task.ToolCallID
				chunk.TaskTitle = ev.Task.ToolName
				chunk.TaskStatus = "started"
			}

		case agent.AgentEventTypeParallelReasonDone:
			if ev.Task != nil {
				chunk.Event = "task"
				chunk.TaskID = ev.Task.ToolCallID
				chunk.TaskTitle = ev.Task.ToolName
				chunk.TaskStatus = "done"
			}

		case agent.AgentEventTypeHookStarted, agent.AgentEventTypeHookCompleted:
			if ev.Hook != nil {
				chunk.Event = "hook"
				chunk.TaskTitle = fmt.Sprintf("%s: %s", ev.Hook.Name, ev.Hook.Decision)
			}

		case agent.AgentEventTypeProviderRetryScheduled:
			chunk.Event = "provider_retry"
			chunk.Content = "provider retrying..."

		case agent.AgentEventTypeToolArgsRepaired:
			// Transparent to frontend — agent auto-repaired broken JSON.

		case agent.AgentEventTypeResponseReset:
			chunk.Event = "response_reset"
			contentBuf = ""

		case agent.AgentEventTypeDone:
			// Handled after the loop.

		case agent.AgentEventTypeError:
			chunk.Event = "error"
			chunk.Error = ev.Err.Error()

		case agent.AgentEventTypeTurnCancelled:
			chunk.Event = "error"
			chunk.Content = "cancelled"

		case agent.AgentEventTypeContextCompacted:
			chunk.Event = "context_compacted"

		case agent.AgentEventTypeBudgetWarning:
			chunk.Event = "error"
			chunk.Content = ev.Content

		default:
			// Silently skip events the pod doesn't need to render.
			continue
		}

		// Only send if we populated the chunk.
		if chunk.Event != "" {
			d.pushChat(client, sessionID, chunk)
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	flushThinking()
	// Done.
	d.pushChat(client, sessionID, chatStreamChunk{Event: "done", Done: true})

	// Persist reasoning the agent may have omitted — stored as hidden so
	// handleSessionGetMessages can merge it into the preceding visible message.
	if contentBuf != "" || thinkingBuf != "" {
		d.store.Create(context.Background(), core.Message{
			SessionID:  sessionID,
			Role:       core.RoleAssistant,
			Hidden:     true,
			Text:       contentBuf,
			DurationMs: time.Since(chatStart).Milliseconds(),
		})
	}

	client.send(wsResponse{Type: "chat", ID: req.ID, Payload: map[string]string{
		"session_id": sessionID,
	}})
}

// handleChatCancel cancels an in-flight chat for the given session.
// The pod sends this when the user clicks the "stop" button during generation.
// The running agent will receive context cancellation and stop at the next yield point.
func (d *Daemon) handleChatCancel(client *wsClient, req wsRequest) {
	var p chatCancelRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	d.pendingCancelsMu.Lock()
	cancel, ok := d.pendingCancels[p.SessionID]
	d.pendingCancelsMu.Unlock()
	if ok {
		cancel()
	}
	client.send(wsResponse{Type: "chat.canceled", ID: req.ID, Payload: map[string]string{
		"session_id": p.SessionID,
	}})
}

// pushChat sends a chat.stream push to the requesting client only.
// Chat messages are per-session and must not leak across connections.
func (d *Daemon) pushChat(client *wsClient, sessionID string, chunk chatStreamChunk) {
	chunk.SessionID = sessionID
	client.send(wsPush{Type: "chat.stream", Payload: chunk})
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

// handleApprovalDecision receives the pod's response to an approval.required push.
// Decisions: "allow", "deny", "allow_session", "cancel".
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

// handleUserInputResponse receives the pod's answer to a user_input.required push.
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

// handleTaskCreate creates a master task and starts PlanAndRun in a goroutine.
// Returns immediately with the master_task_id; progress is delivered via
// task.state_changed and task.log pushes.
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

	// Resolve team if specified.
	if p.TeamName != "" {
		roots := team_engine.DefaultTeamRoots(workDir)
		tc, err := team_engine.FindTeamInRoots(roots, p.TeamName)
		if err == nil {
			team_engine.ResolveTeamRoles(tc)
			d.engine.SetTeam(tc)
		} else {
			log.Printf("daemon: team %q not found: %v", p.TeamName, err)
		}
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

// handleTaskList returns all master tasks with their subtask counts.
func (d *Daemon) handleTaskList(client *wsClient, req wsRequest) {
	tasks, err := d.engine.ListMasterTasks()
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": fmt.Sprintf("list tasks: %v", err)}})
		return
	}
	result := make([]map[string]interface{}, len(tasks))
	for i, mt := range tasks {
		subtasks, _ := d.engine.Store.ListTasksByMasterTask(mt.ID)
		doneCount, activeCount, suspendedCount := 0, 0, 0
		for _, t := range subtasks {
			switch t.State {
			case team_engine.TaskStateDone:
				doneCount++
			case team_engine.TaskStateFailed, team_engine.TaskStateSuspended:
				suspendedCount++
			default:
				activeCount++
			}
		}
		result[i] = map[string]interface{}{
			"id":              mt.ID,
			"goal":            mt.Goal,
			"status":          mt.Status,
			"workdir":         mt.WorkspacePath,
			"created_at":      mt.CreatedAt,
			"task_count":      len(subtasks),
			"done_count":      doneCount,
			"active_count":    activeCount,
			"suspended_count": suspendedCount,
		}
	}
	client.send(wsResponse{Type: "task.list", ID: req.ID, Payload: map[string]interface{}{
		"tasks": result,
	}})
}

// handleTaskCancel cancels a running master task.
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

// handleTaskDelete deletes a master task and all its children.
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

// =========================================================================
// Subtask / dialogue / plan
// =========================================================================

// handleTaskSubtasks returns the full subtask tree for a master task.
func (d *Daemon) handleTaskSubtasks(client *wsClient, req wsRequest) {
	var p subtaskRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	masterID := p.MasterTaskID
	if masterID == "" && p.SessionID != "" {
		// Look up master task by session ID.
		tasks, _ := d.engine.ListMasterTasksBySession(p.SessionID)
		if len(tasks) > 0 {
			masterID = tasks[0].ID
		}
	}
	if masterID == "" {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "master_task_id or session_id required"}})
		return
	}

	subtasks, err := d.engine.ListTasksByMasterTask(masterID)
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}

	client.send(wsResponse{Type: "task.subtasks", ID: req.ID, Payload: map[string]interface{}{
		"subtasks": buildSubtaskTree(subtasks),
	}})
}

// buildSubtaskTree builds a recursive tree from a flat subtask list.
func buildSubtaskTree(tasks []*team_engine.Task) []map[string]interface{} {
	if len(tasks) == 0 {
		return nil
	}
	// Index by ID.
	byID := make(map[string]*team_engine.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	// Build children map.
	children := make(map[string][]string)
	roots := make([]string, 0)
	for _, t := range tasks {
		if len(t.ParentIDs) == 0 {
			roots = append(roots, t.ID)
		} else {
			for _, pid := range t.ParentIDs {
				children[pid] = append(children[pid], t.ID)
			}
		}
	}
	// If nothing has no parent, treat all as roots.
	if len(roots) == 0 {
		for _, t := range tasks {
			roots = append(roots, t.ID)
		}
	}

	var build func(id string) map[string]interface{}
	build = func(id string) map[string]interface{} {
		t := byID[id]
		if t == nil {
			return nil
		}
		m := map[string]interface{}{
			"id":            t.ID,
			"title":         t.Title,
			"description":   t.Description,
			"output":        t.Output,
			"role":          string(t.Role),
			"state":         string(t.State),
			"progress":      team_engine.GetProgress(t.State),
			"created_at":    t.CreatedAt,
			"parent_ids":    t.ParentIDs,
			"batch_id":      t.BatchID,
			"retry_count":   t.RetryCount,
			"max_retries":   t.MaxRetries,
		}
		if kids := children[id]; len(kids) > 0 {
			cs := make([]map[string]interface{}, 0, len(kids))
			for _, k := range kids {
				if n := build(k); n != nil {
					cs = append(cs, n)
				}
			}
			m["children"] = cs
		}
		return m
	}

	result := make([]map[string]interface{}, 0, len(roots))
	for _, r := range roots {
		if n := build(r); n != nil {
			result = append(result, n)
		}
	}
	return result
}

// handleTaskDialogue returns agent dialogue logs for a subtask.
func (d *Daemon) handleTaskDialogue(client *wsClient, req wsRequest) {
	var p dialogueRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if p.TaskID == "" {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "task_id required"}})
		return
	}

	// Read agent output + verifier feedback from whiteboard.
	output, _ := d.engine.Whiteboard.ReadOutput(p.TaskID)
	verifier, _ := d.engine.Whiteboard.ReadVerifier(p.TaskID)
	confirmation, _ := d.engine.Whiteboard.ReadConfirmation(p.TaskID)

	client.send(wsResponse{Type: "task.dialogue", ID: req.ID, Payload: map[string]interface{}{
		"task_id":      p.TaskID,
		"output":       output,
		"verifier":     verifier,
		"confirmation": confirmation,
	}})
}

// handleTaskPlan returns the leader's decomposition plan for a master task.
func (d *Daemon) handleTaskPlan(client *wsClient, req wsRequest) {
	var p planRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if p.MasterTaskID == "" {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "master_task_id required"}})
		return
	}

	// Read plan files from the master task's whiteboard directory.
	masterDir := d.engine.Whiteboard.MasterDir(p.MasterTaskID)
	planMD, _ := os.ReadFile(filepath.Join(masterDir, "plan.md"))
	planJSON, _ := os.ReadFile(filepath.Join(masterDir, "plan.json"))
	specMD, _ := os.ReadFile(filepath.Join(masterDir, "spec.md"))

	client.send(wsResponse{Type: "task.plan", ID: req.ID, Payload: map[string]interface{}{
		"master_task_id": p.MasterTaskID,
		"plan_md":        string(planMD),
		"plan_json":      string(planJSON),
		"spec_md":        string(specMD),
	}})
}

// =========================================================================
// Feedback / confirmation
// =========================================================================

// handleTaskFeedback sends human feedback to a running subtask.
func (d *Daemon) handleTaskFeedback(client *wsClient, req wsRequest) {
	var p feedbackRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if err := d.engine.SendFeedback(p.TaskID, p.Message); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}
	client.send(wsResponse{Type: "task.feedback", ID: req.ID, Payload: map[string]string{
		"task_id": p.TaskID, "status": "sent",
	}})
}

// handleTaskConfirm handles user confirmation/rejection of a pending confirmation.
func (d *Daemon) handleTaskConfirm(client *wsClient, req wsRequest) {
	var p confirmRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if !d.engine.Whiteboard.HasConfirmation(p.TaskID) {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "no pending confirmation"}})
		return
	}
	if p.Decision == "confirm" {
		d.engine.Whiteboard.ClearConfirmation(p.TaskID)
		if p.Comment != "" {
			d.engine.SendFeedback(p.TaskID, "confirmed: "+p.Comment)
		} else {
			d.engine.SendFeedback(p.TaskID, "confirmed by user")
		}
	} else {
		d.engine.Whiteboard.ClearConfirmation(p.TaskID)
		msg := "rejected by user"
		if p.Comment != "" {
			msg = "rejected: " + p.Comment
		}
		d.engine.SendFeedback(p.TaskID, msg)
	}
	client.send(wsResponse{Type: "task.confirmed", ID: req.ID, Payload: map[string]string{
		"task_id": p.TaskID, "decision": p.Decision,
	}})
}

// handleTaskConfirmations lists all pending confirmations for a master task.
func (d *Daemon) handleTaskConfirmations(client *wsClient, req wsRequest) {
	var p confirmationsRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	subtasks, _ := d.engine.ListTasksByMasterTask(p.MasterTaskID)
	var pending []map[string]interface{}
	for _, t := range subtasks {
		if d.engine.Whiteboard.HasConfirmation(t.ID) {
			content, _ := d.engine.Whiteboard.ReadConfirmation(t.ID)
			pending = append(pending, map[string]interface{}{
				"task_id":  t.ID,
				"title":    t.Title,
				"content":  content,
				"state":    string(t.State),
			})
		}
	}
	client.send(wsResponse{Type: "task.confirmations", ID: req.ID, Payload: map[string]interface{}{
		"pending": pending,
	}})
}

// handleTaskUpdateGoal updates a master task's goal.
func (d *Daemon) handleTaskUpdateGoal(client *wsClient, req wsRequest) {
	var p taskUpdateGoalRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	mt, err := d.engine.GetMasterTask(p.TaskID)
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "task not found"}})
		return
	}
	masterDir := d.engine.Whiteboard.MasterDir(p.TaskID)
	goalPath := filepath.Join(masterDir, "goal.md")
	if _, err := os.Stat(goalPath); err == nil {
		os.WriteFile(goalPath, []byte(p.Goal), 0644)
	}
	client.send(wsResponse{Type: "task.goalUpdated", ID: req.ID, Payload: map[string]string{
		"task_id": p.TaskID, "goal": p.Goal, "old_goal": mt.Goal,
	}})
}

// =========================================================================
// MCP
// =========================================================================

// handleMCPList returns all MCP servers and their status.
func (d *Daemon) handleMCPList(client *wsClient, req wsRequest) {
	if d.mcpManager == nil {
		client.send(wsResponse{Type: "mcp.list", ID: req.ID, Payload: map[string]interface{}{"servers": []interface{}{}}})
		return
	}
	states := d.mcpManager.States()
	result := make([]map[string]interface{}, 0, len(states))
	for _, s := range states {
		result = append(result, map[string]interface{}{
			"name":      s.Name,
			"status":    s.Status,
			"disabled":  s.Disabled,
			"connected": s.Connected,
			"error":     s.Error,
			"tools":     s.Tools,
			"tool_names": s.ToolNames,
			"command":   s.Command,
			"url":       s.URL,
		})
	}
	client.send(wsResponse{Type: "mcp.list", ID: req.ID, Payload: map[string]interface{}{"servers": result}})
}

// handleMCPSetEnabled enables or disables an MCP server.
func (d *Daemon) handleMCPSetEnabled(client *wsClient, req wsRequest) {
	var p mcpSetEnabledRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if d.mcpManager == nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "mcp not available"}})
		return
	}

	var err error
	if p.Enabled {
		err = d.mcpManager.EnableServer(context.Background(), p.Name)
	} else {
		err = d.mcpManager.DisableServer(p.Name)
	}
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}
	client.send(wsResponse{Type: "mcp.setEnabled", ID: req.ID, Payload: map[string]interface{}{
		"name": p.Name, "enabled": p.Enabled,
	}})
}

// handleMCPSetEnv stores a secret env value (e.g. token) for an MCP server.
func (d *Daemon) handleMCPSetEnv(client *wsClient, req wsRequest) {
	var p struct {
		Server string `json:"server"`
		Key    string `json:"key"`
		Value  string `json:"value"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if d.mcpManager == nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "mcp not available"}})
		return
	}
	if err := d.mcpManager.SetServerEnv(p.Server, p.Key, p.Value); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}
	client.send(wsResponse{Type: "mcp.setEnv", ID: req.ID, Payload: map[string]interface{}{
		"server": p.Server, "key": p.Key,
	}})
}


// =========================================================================
// File read
// =========================================================================

// handleFileRead reads a file from the daemon's workdir or an allowed path.
func (d *Daemon) handleFileRead(client *wsClient, req wsRequest) {
	var p fileReadRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if p.Path == "" {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "path required"}})
		return
	}

	// Resolve path: absolute paths must be under workdir.
	readPath := p.Path
	if !filepath.IsAbs(readPath) {
		readPath = filepath.Join(d.cfg.WorkDir, readPath)
	}
	// Basic safety: ensure path is under workdir or data dir.
	abs, _ := filepath.Abs(readPath)
	if !strings.HasPrefix(abs, d.cfg.WorkDir) && !strings.HasPrefix(abs, d.cfg.DataDir) {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "access denied: path outside workspace"}})
		return
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}
	client.send(wsResponse{Type: "file.read", ID: req.ID, Payload: map[string]interface{}{
		"path":    abs,
		"content": string(data),
		"size":    len(data),
	}})
}

// =========================================================================
// Team chat
// =========================================================================

// handleTeamChatSend sends a chat message within a team task context.
func (d *Daemon) handleTeamChatSend(client *wsClient, req wsRequest) {
	var p teamChatSendRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	if err := d.engine.Whiteboard.WriteChatMessage(p.MasterTaskID, p.From, p.To, p.Content); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}

	// Broadcast to all clients.
	msg := map[string]interface{}{
		"master_task_id": p.MasterTaskID,
		"from":           p.From,
		"to":             p.To,
		"content":         p.Content,
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	}
	d.broadcast(wsPush{Type: "team.chat.message", Payload: msg})

	client.send(wsResponse{Type: "team.chat.sent", ID: req.ID, Payload: msg})
}

// handleTeamChatMessages returns all chat messages for a master task.
func (d *Daemon) handleTeamChatMessages(client *wsClient, req wsRequest) {
	var p teamChatMessagesRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": "invalid payload"}})
		return
	}
	messages, _ := d.engine.Whiteboard.ReadChatMessages(p.MasterTaskID)
	result := make([]map[string]interface{}, 0, len(messages))
	for _, m := range messages {
		result = append(result, map[string]interface{}{
			"from":      m.From,
			"to":        m.To,
			"content":   m.Content,
			"timestamp": m.Timestamp,
		})
	}
	client.send(wsResponse{Type: "team.chat.messages", ID: req.ID, Payload: map[string]interface{}{
		"messages": result,
	}})
}

// handleSessionList returns recent sessions (excluding subagent sessions).
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
		// Compute real task counts from TeamEngine.
		taskCount, doneCount, activeCount, suspendedCount := 0, 0, 0, 0
		if d.engine != nil {
			if mts, _ := d.engine.ListMasterTasksBySession(s.ID); len(mts) > 0 {
				subs, _ := d.engine.ListTasksByMasterTask(mts[0].ID)
				taskCount = len(subs)
				for _, t := range subs {
					switch t.State {
					case team_engine.TaskStateDone:
						doneCount++
					case team_engine.TaskStateFailed, team_engine.TaskStateSuspended:
						suspendedCount++
					default:
						activeCount++
					}
				}
			}
		}
		result = append(result, map[string]interface{}{
			"id":               s.ID,
			"goal":             goal,
			"agent":            s.Meta.Agent,
			"workspace_path":   s.Meta.Workspace,
			"workspace_label":  filepath.Base(s.Meta.Workspace),
			"workspace_id":     s.Meta.Workspace,
			"session_path":     filepath.Join(d.sessionsDir, s.ID+".jsonl"),
			"status":           status,
			"created_at":       s.Meta.StartedAt,
			"task_count":       taskCount,
			"done_count":       doneCount,
			"active_count":     activeCount,
			"suspended_count":  suspendedCount,
			"workspace_online": true,
		})
	}
	client.send(wsResponse{Type: "session.list", ID: req.ID, Payload: map[string]interface{}{"sessions": result}})
}

// handleSessionListByAgent returns sessions filtered by agent name, with offset/limit pagination.
func (d *Daemon) handleSessionListByAgent(client *wsClient, req wsRequest) {
	var p struct {
		Agent  string `json:"agent"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	json.Unmarshal(req.Payload, &p)
	if p.Limit <= 0 {
		p.Limit = 20
	}
	sessions, _ := session.ListSessions(d.sessionsDir, p.Offset+p.Limit)
	result := make([]map[string]interface{}, 0)
	count := 0
	for _, s := range sessions {
		if s.Meta.Kind == "subagent" {
			continue
		}
// Always filter by agent — empty string means "whale" sessions.
if s.Meta.Agent != p.Agent {
			continue
		}
		count++
		if count <= p.Offset {
			continue
		}
		goal := s.Meta.Title
		if goal == "" {
			goal = s.Conversation
		}
		result = append(result, map[string]interface{}{
			"id": s.ID, "goal": goal, "agent": s.Meta.Agent,
			"workspace_path": s.Meta.Workspace,
			"status": s.Meta.Status,
			"created_at": s.Meta.StartedAt,
		})
		if len(result) >= p.Limit {
			break
		}
	}
	hasMore := count > p.Offset+len(result)
	client.send(wsResponse{Type: "session.listByAgent", ID: req.ID, Payload: map[string]interface{}{"sessions": result, "has_more": hasMore}})
}

// handleSessionDelete removes a single session (JSONL + meta files).
// removeSessionFiles deletes all files associated with a session ID.
func (d *Daemon) removeSessionFiles(id string) {
	safe := core.SanitizeSessionID(id)
	for _, ext := range []string{
		".jsonl",
		".meta.json",
		".todo.json",
		".approvals.json",
		core.ToolInputEventsSuffix,
		core.ApprovalEventsSuffix,
	} {
		os.Remove(filepath.Join(d.sessionsDir, safe+ext))
	}
}

func (d *Daemon) handleSessionDelete(client *wsClient, req wsRequest) {
	var p struct{ ID string `json:"id"` }
	json.Unmarshal(req.Payload, &p)
	d.removeSessionFiles(p.ID)
	client.send(wsResponse{Type: "session.delete", ID: req.ID, Payload: map[string]string{"id": p.ID}})
}

// handleSessionDeleteAll removes all sessions, optionally filtered by agent.
func (d *Daemon) handleSessionDeleteAll(client *wsClient, req wsRequest) {
	var p struct{ Agent string `json:"agent"` }
	json.Unmarshal(req.Payload, &p)
	sessions, _ := session.ListSessions(d.sessionsDir, 0)
	for _, s := range sessions {
		if s.Meta.Kind == "subagent" {
			continue
		}
		// Always filter by agent — empty string means "whale" sessions.
		if s.Meta.Agent != p.Agent {
			continue
		}
		d.removeSessionFiles(s.ID)
	}
	client.send(wsResponse{Type: "session.deleteAll", ID: req.ID, Payload: map[string]string{}})
}

// handleSessionClearEmpty removes sessions with empty/trivial meta files.
func (d *Daemon) handleSessionClearEmpty(client *wsClient, req wsRequest) {
	var p struct{ Agent string `json:"agent"` }
	json.Unmarshal(req.Payload, &p)
	entries, _ := os.ReadDir(d.sessionsDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if strings.Contains(id, "--subagent-") {
			continue
		}
		metaPath := filepath.Join(d.sessionsDir, id+".meta.json")
		info, err := os.Stat(metaPath)
		if err != nil || info.Size() < 10 {
			os.Remove(filepath.Join(d.sessionsDir, e.Name()))
			os.Remove(metaPath)
		}
	}
	client.send(wsResponse{Type: "session.clearEmpty", ID: req.ID, Payload: map[string]string{}})
}

// handleExpertList returns experts from YAML definitions in the data dir.
func (d *Daemon) handleExpertList(client *wsClient, req wsRequest) {
	result := listExpertYAML(d.expertsDir())
	client.send(wsResponse{Type: "expert.list", ID: req.ID, Payload: map[string]interface{}{"experts": result}})
}

// handleAgentList returns agents from markdown definitions in the data dir.
func (d *Daemon) handleAgentList(client *wsClient, req wsRequest) {
	result := listAgentMarkdown(d.agentsDir())
	client.send(wsResponse{Type: "agent.list", ID: req.ID, Payload: map[string]interface{}{"agents": result}})
}

// handleTeamList returns teams from team.yaml files in the data dir.
func (d *Daemon) handleTeamList(client *wsClient, req wsRequest) {
	result := listTeamsFromDisk(d.teamsDir())
	client.send(wsResponse{Type: "team.list", ID: req.ID, Payload: map[string]interface{}{"teams": result}})
}

func (d *Daemon) agentsDir() string  { return filepath.Join(d.cfg.DataDir, "agents") }
func (d *Daemon) expertsDir() string { return filepath.Join(d.cfg.DataDir, "experts") }
func (d *Daemon) teamsDir() string   { return filepath.Join(d.cfg.DataDir, "teams") }

// handleSessionGetMessages returns all messages in a session (human/agent, content, thinking, timing).
// handleSessionGetMessages returns session messages with consecutive
// assistant messages merged into single turns (matching live-stream behaviour).
func (d *Daemon) handleSessionGetMessages(client *wsClient, req wsRequest) {
	var p struct{ ID string `json:"id"` }
	json.Unmarshal(req.Payload, &p)
	msgs, err := d.store.List(context.Background(), p.ID)
	if err != nil {
		client.send(wsResponse{Type: "error", ID: req.ID, Payload: map[string]string{"message": err.Error()}})
		return
	}

	// Collect tool results from RoleTool messages for correlation
	toolResultMap := make(map[string]struct {
		outcome string
		failed  bool
		output  string
	})
	for _, m := range msgs {
		for _, tr := range m.ToolResults {
			outcome := string(tr.Outcome)
			toolResultMap[tr.ToolCallID] = struct {
				outcome string
				failed  bool
				output  string
			}{
				outcome: outcome,
				failed:  outcome != "" && outcome != "success" && outcome != "no_result",
				output:  core.ToolResultModelText(tr),
			}
		}
	}

	result := make([]map[string]interface{}, 0, len(msgs))

	// Accumulator for consecutive assistant messages
	var accText    string
	var accTools   []map[string]interface{}
	var accReason  string
	var accDurMs   int64
	var accTime    time.Time
	flushAcc := func() {
		if accText != "" || len(accTools) > 0 {
			result = append(result, map[string]interface{}{
				"time":       accTime.Format(time.RFC3339),
				"from":       "agent",
				"content":    accText,
				"tools":      accTools,
				"thinking":   accReason,
				"durationMs": accDurMs,
			})
		}
		accText = ""
		accTools = nil
		accReason = ""
		accDurMs = 0
	}

	for _, m := range msgs {
		if m.Role == core.RoleTool {
			continue // tool results already collected above
		}

		// User message: flush any accumulated assistant, then emit user
		if m.Role == core.RoleUser && !m.Hidden {
			flushAcc()
			text := core.MessagePlainText(m)
			result = append(result, map[string]interface{}{
				"time":    m.CreatedAt.Format(time.RFC3339),
				"from":    "human",
				"content": text,
			})
			continue
		}

		// Hidden message: merge metadata to accumulator or last entry
		if m.Hidden {
			if m.Reasoning != "" {
				flushAcc()
				result = append(result, map[string]interface{}{
					"time":     m.CreatedAt.Format(time.RFC3339),
					"from":     "agent",
					"thinking": m.Reasoning,
				})
			}
			if m.DurationMs > 0 {
				if accText != "" || len(accTools) > 0 {
					accDurMs = m.DurationMs
				} else if len(result) > 0 {
					result[len(result)-1]["durationMs"] = m.DurationMs
				}
			}
			continue
		}

		// Assistant message: accumulate (skip duplicates from old sessions)
		if m.Role == core.RoleAssistant {
			text := core.MessagePlainText(m)
			// Thinking-only message — standalone entry like TUI.
			// Skip if duplicate: scan past tool entries to find last non-tool thinking.
			if text == "" && len(m.ToolCalls) == 0 && m.Reasoning != "" {
				dup := false
				for j := len(result) - 1; j >= 0; j-- {
					if _, hasTools := result[j]["tools"]; hasTools {
						continue // skip tool entries
					}
					if t, _ := result[j]["thinking"].(string); t == m.Reasoning {
						dup = true
					}
					break // stop at first non-tool entry
				}
				if dup {
					continue
				}
				flushAcc()
				result = append(result, map[string]interface{}{
					"time": m.CreatedAt.Format(time.RFC3339), "from": "agent", "thinking": m.Reasoning, "durationMs": m.DurationMs,
				})
				continue
			}
			// Tools arrived: flush text first (came before tools in LLM response).
			if len(m.ToolCalls) > 0 && accText != "" {
				// Flush text-only entry before processing tools
				result = append(result, map[string]interface{}{
					"time":    accTime.Format(time.RFC3339),
					"from":    "agent",
					"content":  accText,
					"thinking": accReason,
				})
				accText = ""
				accReason = ""
				accTime = time.Time{}
			}
			if text != "" && !strings.HasSuffix(accText, text) {
				if accText != "" {
					accText += "\n\n"
				}
				accText += text
			}
			for _, tc := range m.ToolCalls {
				tool := map[string]interface{}{
					"name":  tc.Name,
					"input": summarizeToolInput(tc.Name, tc.Input),
					"id":    tc.ID,
				}
				tr, hasResult := toolResultMap[tc.ID]
				tool["has_result"] = hasResult
				if hasResult {
					tool["outcome"] = tr.outcome
					tool["failed"] = tr.failed
					tool["output"] = tr.output
				}
				accTools = append(accTools, tool)
			}
			if m.Reasoning != "" {
				accReason = m.Reasoning
			}
			// Flush tools as standalone segment.
			if len(m.ToolCalls) > 0 {
				flushAcc()
			}
			if m.DurationMs > 0 {
				accDurMs = m.DurationMs
			}
			if accTime.IsZero() {
				accTime = m.CreatedAt
			}
		}
	}
	flushAcc()

	client.send(wsResponse{Type: "session.getMessages", ID: req.ID, Payload: map[string]interface{}{"messages": result}})
}

// broadcast sends a push message to all connected WebSocket clients.
// Non-blocking: slow clients may miss messages but won't stall the server.
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

// resolveAPIKey reads the DeepSeek API key from:
//   1. DEEPSEEK_API_KEY environment variable (preferred)
//   2. {dataDir}/credentials.json → deepseek_api_key field
// Returns empty string if neither is set.
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

// listAgentMarkdown scans a directory of markdown agent definitions.
// Expected layout: {dir}/{category}/{agent-name}.md
// Each .md file uses YAML front matter (between --- markers) for
// name, role, description, skills, tools, and whenToUse.
func listAgentMarkdown(dir string) []map[string]interface{} {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var result []map[string]interface{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		subs, _ := os.ReadDir(filepath.Join(dir, e.Name()))
		for _, se := range subs {
			if se.IsDir() || !strings.HasSuffix(se.Name(), ".md") {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(dir, e.Name(), se.Name()))
			name := strings.TrimSuffix(se.Name(), ".md")
			desc := ""
			role := ""
			var skills []string
			var tools []string
			var whenToUse string
			parts := strings.SplitN(string(data), "---", 3)
			if len(parts) >= 3 {
				var raw struct {
					Name        string            `yaml:"name"`
					Role        string            `yaml:"role"`
					Description string            `yaml:"description"`
					Skills      []string          `yaml:"skills"`
					Tools       []string          `yaml:"tools"`
					WhenToUse   string            `yaml:"whenToUse"`
					Profession  map[string]string `yaml:"profession"`
					DisplayName map[string]string `yaml:"displayName"`
				}
				if yaml.Unmarshal([]byte(parts[1]), &raw) == nil {
					if raw.Name != "" {
						name = raw.Name
					}
					desc = raw.Description
					role = raw.Role
					skills = raw.Skills
					tools = raw.Tools
					whenToUse = raw.WhenToUse
					if role == "" && raw.Profession != nil {
						if zh, ok := raw.Profession["zh"]; ok {
							role = zh
						}
					}
					if role == "" && raw.DisplayName != nil {
						if zh, ok := raw.DisplayName["zh"]; ok {
							role = zh
						}
					}
				}
			}
			if desc == "" {
				desc = strings.TrimSpace(string(data))
			}
			result = append(result, map[string]interface{}{
				"name": name, "role": role, "description": desc,
				"category": e.Name(),
				"skills": skills, "tools": tools, "whenToUse": whenToUse,
			})
		}
	}
	return result
}

// =========================================================================
// Helpers
// =========================================================================

// summarizeToolInput returns a short human-readable summary of a tool call's JSON input.
//
// Each tool gets a custom extraction:
//   - shell_run      → the command string
//   - read_file/write/edit → the file path
//   - grep           → the pattern
//   - web_search     → the query
//   - web_fetch      → the URL
//   - spawn_subagent → "role: task summary"
//   - parallel_reason → "N prompts"
//   - multi_edit     → "file (N edits)"
//   - empty/unknown  → the original input or tool name
//
// This is the value sent as ToolInput in chat.stream tool_call events.
func toolLabel(name string) string {
	switch name {
	case "read_file": return "Read"
	case "write", "edit", "multi_edit": return "Write"
	case "shell_run": return "Run"
	case "grep", "web_search": return "Search"
	case "web_fetch", "fetch": return "Fetch"
	case "spawn_subagent": return "Agent"
	case "parallel_reason": return "Think"
	default: return "Tool"
	}
}

func summarizeToolInput(name string, input string) string {
	if input == "" || input == "{}" {
		return name
	}
	switch name {
	case "shell_run":
		var body struct{ Command string `json:"command"` }
		if json.Unmarshal([]byte(input), &body) == nil && body.Command != "" {
			return strings.TrimSpace(body.Command)
		}
	case "read_file":
		var body struct{ FilePath string `json:"file_path"` }
		if json.Unmarshal([]byte(input), &body) == nil && body.FilePath != "" {
			return body.FilePath
		}
	case "write", "edit":
		var body struct{ FilePath string `json:"file_path"` }
		if json.Unmarshal([]byte(input), &body) == nil && body.FilePath != "" {
			return body.FilePath
		}
	case "grep":
		var body struct{ Pattern string `json:"pattern"` }
		if json.Unmarshal([]byte(input), &body) == nil && body.Pattern != "" {
			return body.Pattern
		}
	case "web_search":
		var body struct{ Query string `json:"query"` }
		if json.Unmarshal([]byte(input), &body) == nil && body.Query != "" {
			return body.Query
		}
	case "web_fetch", "fetch":
		var body struct{ URL string `json:"url"` }
		if json.Unmarshal([]byte(input), &body) == nil && body.URL != "" {
			return body.URL
		}
	case "spawn_subagent":
		var body struct{ Role string `json:"role"`; Task string `json:"task"` }
		if json.Unmarshal([]byte(input), &body) == nil {
			if body.Role != "" && body.Task != "" {
				return body.Role + ": " + body.Task
			}
		}
	case "parallel_reason":
		var body struct{ Prompts []string `json:"prompts"` }
		if json.Unmarshal([]byte(input), &body) == nil && len(body.Prompts) > 0 {
			return fmt.Sprintf("%d prompts", len(body.Prompts))
		}
	case "multi_edit":
		var body struct{ FilePath string `json:"file_path"`; Edits []struct{} `json:"edits"` }
		if json.Unmarshal([]byte(input), &body) == nil {
			return fmt.Sprintf("%s (%d edits)", body.FilePath, len(body.Edits))
		}
	}
	return input
}

// listExpertYAML scans a directory of expert YAML files.
// Each .yaml file defines a domain with multiple experts, each having
// name, name_en, description, and skills.
func listExpertYAML(dir string) []map[string]interface{} {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var result []map[string]interface{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var ef struct {
			Domain  string `yaml:"domain"`
			Experts []struct {
				Name        string        `yaml:"name"`
				NameEn      string        `yaml:"name_en"`
				Description string        `yaml:"description"`
				Skills      []interface{} `yaml:"skills"`
			} `yaml:"experts"`
		}
		if yaml.Unmarshal(data, &ef) != nil {
			continue
		}
		for _, exp := range ef.Experts {
			skills := make([]string, 0, len(exp.Skills))
			for _, s := range exp.Skills {
				switch v := s.(type) {
				case string:
					skills = append(skills, v)
				case map[string]interface{}:
					if zh, ok := v["zh"].(string); ok {
						skills = append(skills, zh)
					} else if en, ok := v["en"].(string); ok {
						skills = append(skills, en)
					}
				}
			}
			name := exp.Name
			if name == "" {
				name = exp.NameEn
			}
			result = append(result, map[string]interface{}{
			"name": name, "role": exp.NameEn,
			"description": exp.Description,
			"category":    ef.Domain,
			"skills":      skills,
			})
		}
	}
	return result
}

// listTeamsFromDisk scans team directories for team.yaml files.
// Expected layout: {dir}/{team-name}/team.yaml
// Each team.yaml defines name, label, category, description, roles, capabilities.
func listTeamsFromDisk(dir string) []map[string]interface{} {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var result []map[string]interface{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "team.yaml"))
		if err != nil {
			continue
		}
		var raw struct {
			Name         string   `yaml:"name"`
			Label        string   `yaml:"label"`
			Category     string   `yaml:"category"`
			Description  string   `yaml:"description"`
			Roles        []string `yaml:"roles"`
			Capabilities []string `yaml:"capabilities"`
		}
		if yaml.Unmarshal(data, &raw) != nil {
			continue
		}
		name := raw.Name
		if name == "" {
			name = e.Name()
		}
		label := raw.Label
		if label == "" {
			label = name
		}
		result = append(result, map[string]interface{}{
			"name": name, "label": label,
			"category": raw.Category, "description": raw.Description,
			"roles": raw.Roles, "capabilities": raw.Capabilities,
		})
	}
	return result
}
