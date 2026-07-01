package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/usewhale/whale/internal/team_engine"
)

// mockSpawner implements team_engine.SubagentSpawner for tests.
type mockSpawner struct{}

func (m *mockSpawner) SpawnSubagent(ctx context.Context, req team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
	return team_engine.SubagentResponse{Output: "mock output", Success: true}, nil
}

func newTestEngine(t *testing.T) *team_engine.TeamEngine {
	t.Helper()
	eng, err := team_engine.New(":memory:", t.TempDir(), "", &mockSpawner{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

// =========================================================================
// Protocol JSON tests — verify pod-compatible message shapes
// =========================================================================

func TestChatStreamChunkJSON(t *testing.T) {
	tests := []struct {
		name  string
		chunk chatStreamChunk
		check func(t *testing.T, raw json.RawMessage)
	}{
		{
			name: "assistant delta",
			chunk: chatStreamChunk{
				SessionID: "s1",
				Event:     "assistant",
				Content:   "Hello, world!",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["event"] != "assistant" {
					t.Errorf("event = %v", m["event"])
				}
				if m["content"] != "Hello, world!" {
					t.Errorf("content = %v", m["content"])
				}
				if m["done"] != false {
					t.Errorf("done should be false for non-terminal events")
				}
			},
		},
		{
			name: "thinking delta",
			chunk: chatStreamChunk{
				SessionID: "s1",
				Event:     "thinking",
				Content:   "Let me think...",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["event"] != "thinking" {
					t.Errorf("event = %v", m["event"])
				}
			},
		},
		{
			name: "tool_call",
			chunk: chatStreamChunk{
				SessionID:  "s1",
				Event:      "tool_call",
				ToolCallID: "tc-1",
				ToolName:   "shell_run",
				ToolInput:  "go build ./...",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["event"] != "tool_call" {
					t.Errorf("event = %v", m["event"])
				}
				if m["tool_call_id"] != "tc-1" {
					t.Errorf("tool_call_id = %v", m["tool_call_id"])
				}
				if m["tool_name"] != "shell_run" {
					t.Errorf("tool_name = %v", m["tool_name"])
				}
				if m["tool_input"] != "go build ./..." {
					t.Errorf("tool_input = %v", m["tool_input"])
				}
			},
		},
		{
			name: "tool_result success",
			chunk: chatStreamChunk{
				SessionID:   "s1",
				Event:       "tool_result",
				ToolCallID:  "tc-1",
				ToolName:    "shell_run",
				ToolOutcome: "success",
				Content:     "exit 0",
				ToolCode:    "0",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["event"] != "tool_result" {
					t.Errorf("event = %v", m["event"])
				}
				if m["tool_outcome"] != "success" {
					t.Errorf("tool_outcome = %v", m["tool_outcome"])
				}
				if _, ok := m["tool_status"]; ok && m["tool_status"] != "" {
					t.Errorf("tool_status should be empty for success, got %v", m["tool_status"])
				}
			},
		},
		{
			name: "tool_result failed",
			chunk: chatStreamChunk{
				SessionID:   "s1",
				Event:       "tool_result",
				ToolCallID:  "tc-2",
				ToolName:    "shell_run",
				ToolOutcome: "error",
				ToolStatus:  "failed",
				Content:     "command not found",
				ToolCode:    "127",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["tool_status"] != "failed" {
					t.Errorf("tool_status = %v", m["tool_status"])
				}
			},
		},
		{
			name: "subagent started",
			chunk: chatStreamChunk{
				SessionID:  "s1",
				Event:      "subagent",
				TaskID:     "sa-1",
				TaskTitle:  "explore",
				TaskStatus: "started",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["event"] != "subagent" {
					t.Errorf("event = %v", m["event"])
				}
				if m["task_status"] != "started" {
					t.Errorf("task_status = %v", m["task_status"])
				}
			},
		},
		{
			name: "plan delta",
			chunk: chatStreamChunk{
				SessionID: "s1",
				Event:     "plan",
				Content:   "- step 1",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["event"] != "plan" {
					t.Errorf("event = %v", m["event"])
				}
			},
		},
		{
			name: "done",
			chunk: chatStreamChunk{
				SessionID: "s1",
				Event:     "done",
				Done:      true,
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["done"] != true {
					t.Errorf("done = %v", m["done"])
				}
			},
		},
		{
			name: "error",
			chunk: chatStreamChunk{
				SessionID: "s1",
				Event:     "error",
				Error:     "something went wrong",
			},
			check: func(t *testing.T, raw json.RawMessage) {
				var m map[string]interface{}
				json.Unmarshal(raw, &m)
				if m["error"] != "something went wrong" {
					t.Errorf("error = %v", m["error"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.chunk)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			tt.check(t, raw)
		})
	}
}

// =========================================================================
// wsRequest / wsResponse protocol tests
// =========================================================================

func TestWsRequestUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    wsRequest
		wantErr bool
	}{
		{
			name: "chat request",
			raw:  `{"type":"chat","id":"req-1","payload":{"message":"hello"}}`,
			want: wsRequest{Type: "chat", ID: "req-1", Payload: json.RawMessage(`{"message":"hello"}`)},
		},
		{
			name: "task.create request",
			raw:  `{"type":"task.create","id":"req-2","payload":{"goal":"build","workdir":"/tmp"}}`,
			want: wsRequest{Type: "task.create", ID: "req-2", Payload: json.RawMessage(`{"goal":"build","workdir":"/tmp"}`)},
		},
		{
			name: "task.list request",
			raw:  `{"type":"task.list","id":"req-3"}`,
			want: wsRequest{Type: "task.list", ID: "req-3"},
		},
		{
			name: "chat.cancel request",
			raw:  `{"type":"chat.cancel","id":"req-4","payload":{"session_id":"s1"}}`,
			want: wsRequest{Type: "chat.cancel", ID: "req-4", Payload: json.RawMessage(`{"session_id":"s1"}`)},
		},
		{
			name: "empty type",
			raw:  `{"id":"req-5"}`,
			want: wsRequest{ID: "req-5"},
		},
		{
			name:    "invalid json",
			raw:     `{invalid`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req wsRequest
			err := json.Unmarshal([]byte(tt.raw), &req)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if req.Type != tt.want.Type {
				t.Errorf("Type = %q, want %q", req.Type, tt.want.Type)
			}
			if req.ID != tt.want.ID {
				t.Errorf("ID = %q, want %q", req.ID, tt.want.ID)
			}
		})
	}
}

func TestWsResponseMarshal(t *testing.T) {
	resp := wsResponse{
		Type:    "task.list",
		ID:      "req-3",
		Payload: map[string]interface{}{"tasks": []interface{}{}},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	if m["type"] != "task.list" {
		t.Errorf("type = %v", m["type"])
	}
	if m["id"] != "req-3" {
		t.Errorf("id = %v", m["id"])
	}
}

func TestWsPushMarshal(t *testing.T) {
	push := wsPush{
		Type: "task.state_changed",
		Payload: map[string]interface{}{
			"task_id":   "t1",
			"new_state": "producing",
			"progress":  50,
		},
	}
	// wsPush should NOT have an "id" field.
	raw, _ := json.Marshal(push)
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	if _, ok := m["id"]; ok {
		t.Error("push messages must not have id field")
	}
	if m["type"] != "task.state_changed" {
		t.Errorf("type = %v", m["type"])
	}
}

// =========================================================================
// summarizeToolInput tests
// =========================================================================

func TestSummarizeToolInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"shell_run", `{"command":"go build ./..."}`, "go build ./..."},
		{"shell_run empty", `{"command":""}`, `{"command":""}`},
		{"shell_run no command", `{}`, "shell_run no command"},
		{"read_file", `{"file_path":"main.go"}`, "main.go"},
		{"write", `{"file_path":"out.txt"}`, "out.txt"},
		{"edit", `{"file_path":"out.txt"}`, "out.txt"},
		{"grep", `{"pattern":"TODO"}`, "TODO"},
		{"web_search", `{"query":"golang"}`, "golang"},
		{"web_fetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"fetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"spawn_subagent", `{"role":"explore","task":"find bugs"}`, "explore: find bugs"},
		{"spawn_subagent no task", `{"role":"explore"}`, `{"role":"explore"}`},
		{"parallel_reason", `{"prompts":["a","b","c"]}`, "3 prompts"},
		{"parallel_reason empty", `{"prompts":[]}`, `{"prompts":[]}`},
		{"multi_edit", `{"file_path":"x.go","edits":[{"search":"a","replace":"b"}]}`, "x.go (1 edits)"},
		{"unknown tool", `{"foo":"bar"}`, `{"foo":"bar"}`},
		{"empty input", "", "empty input"},
		{"empty object", "{}", "empty object"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeToolInput(tt.name, tt.input)
			if got != tt.want {
				t.Errorf("summarizeToolInput(%q, %q) = %q, want %q", tt.name, tt.input, got, tt.want)
			}
		})
	}
}

// =========================================================================
// resolveAPIKey tests
// =========================================================================

func TestResolveAPIKey(t *testing.T) {
	// Env var takes priority.
	t.Setenv("DEEPSEEK_API_KEY", "sk-env")
	got := resolveAPIKey("")
	if got != "sk-env" {
		t.Errorf("env var: got %q, want %q", got, "sk-env")
	}

	// Credentials file fallback.
	t.Setenv("DEEPSEEK_API_KEY", "")
	dir := t.TempDir()
	creds := map[string]string{"deepseek_api_key": "sk-file"}
	data, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0644)

	got = resolveAPIKey(dir)
	if got != "sk-file" {
		t.Errorf("file fallback: got %q, want %q", got, "sk-file")
	}

	// No key available.
	got = resolveAPIKey(t.TempDir())
	if got != "" {
		t.Errorf("no key: got %q, want empty", got)
	}
}

// =========================================================================
// Daemon struct tests
// =========================================================================

func TestDaemonNewNoDirs(t *testing.T) {
	// NewDaemon should create data dir if missing.
	dataDir := filepath.Join(t.TempDir(), "nonexistent", "whale_data")
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Error("data dir was not created")
	}
}

func TestDaemonHealthEndpoint(t *testing.T) {
	dataDir := t.TempDir()
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	// Test the mux directly without starting the server.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	d.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("health: status %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("health: body = %q", rec.Body.String())
	}
}

// =========================================================================
// WebSocket upgrade tests
// =========================================================================

func TestHandleWSUpgrade(t *testing.T) {
	dataDir := t.TempDir()
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	srv := httptest.NewServer(http.HandlerFunc(d.handleWS))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Give the handler goroutine time to register the client.
	time.Sleep(50 * time.Millisecond)

	// Verify client is registered.
	d.mu.Lock()
	n := len(d.clients)
	d.mu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 client, got %d", n)
	}

	// Send a request and get response.
	req := wsRequest{
		Type: "session.list",
		ID:   "test-1",
	}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read response.
	var resp wsResponse
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != "session.list" {
		t.Errorf("response type = %q", resp.Type)
	}
	if resp.ID != "test-1" {
		t.Errorf("response id = %q", resp.ID)
	}

	// Close connection and verify client removed.
	conn.Close()
	// Give the read loop goroutine time to clean up.
	time.Sleep(50 * time.Millisecond)
	d.mu.Lock()
	n = len(d.clients)
	d.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 clients after close, got %d", n)
	}
}

func TestHandleWSUpgradeInvalidMethod(t *testing.T) {
	dataDir := t.TempDir()
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	// GET /ws without upgrade headers → should fail.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	rec := httptest.NewRecorder()
	d.handleWS(rec, req)

	// Should not be 101 Switching Protocols.
	if rec.Code == http.StatusSwitchingProtocols {
		t.Error("expected non-upgrade response without WS headers")
	}
}

// =========================================================================
// Unknown message type
// =========================================================================

func TestHandleUnknownMessageType(t *testing.T) {
	dataDir := t.TempDir()
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	srv := httptest.NewServer(http.HandlerFunc(d.handleWS))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send unknown type.
	req := wsRequest{Type: "unknown.foo", ID: "bad-1"}
	conn.WriteJSON(req)

	var resp wsResponse
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != "error" {
		t.Errorf("expected error response for unknown type, got %q", resp.Type)
	}
}

// =========================================================================
// Broadcast tests
// =========================================================================

func TestBroadcastToMultipleClients(t *testing.T) {
	dataDir := t.TempDir()
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	srv := httptest.NewServer(http.HandlerFunc(d.handleWS))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	// Connect 3 clients.
	var conns []*websocket.Conn
	for i := 0; i < 3; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial client %d: %v", i, err)
		}
		defer conn.Close()
		conns = append(conns, conn)
	}

	// Broadcast.
	msg := wsPush{Type: "task.state_changed", Payload: map[string]string{"task_id": "t1"}}
	d.broadcast(msg)

	// Each client should receive it.
	for i, conn := range conns {
		var got wsPush
		if err := conn.ReadJSON(&got); err != nil {
			t.Errorf("client %d: read error: %v", i, err)
			continue
		}
		if got.Type != "task.state_changed" {
			t.Errorf("client %d: type = %q", i, got.Type)
		}
	}
}

// =========================================================================
// wsClient.send tests
// =========================================================================

func TestWsClientSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
		// Read one message and echo back.
		var msg map[string]interface{}
		conn.ReadJSON(&msg)
		conn.WriteJSON(msg)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	c := &wsClient{id: "test", conn: conn, writeMu: sync.Mutex{}}

	// send should not error.
	resp := wsResponse{Type: "test.ok", ID: "1"}
	c.send(resp)

	// Read echoed message.
	var got map[string]interface{}
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got["type"] != "test.ok" {
		t.Errorf("type = %v", got["type"])
	}
}

// =========================================================================
// pushChat convenience wrapper
// =========================================================================

func TestPushChat(t *testing.T) {
	dataDir := t.TempDir()
	eng := newTestEngine(t)
	defer eng.Close()

	cfg := DaemonConfig{Port: 0, DataDir: dataDir, WorkDir: t.TempDir()}
	d, err := NewDaemon(eng, cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	srv := httptest.NewServer(http.HandlerFunc(d.handleWS))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Get the registered client from the daemon's client map.
	time.Sleep(50 * time.Millisecond)
	d.mu.Lock()
	var wsc *wsClient
	for _, c := range d.clients {
		wsc = c
		break
	}
	d.mu.Unlock()
	if wsc == nil {
		t.Fatal("no client registered")
	}

	d.pushChat(wsc, "s1", chatStreamChunk{Event: "assistant", Content: "hello"})

	var push wsPush
	if err := conn.ReadJSON(&push); err != nil {
		t.Fatalf("read: %v", err)
	}
	if push.Type != "chat.stream" {
		t.Errorf("type = %q", push.Type)
	}
}

// =========================================================================
// Daemon Config defaults
// =========================================================================

func TestDaemonConfigDefaults(t *testing.T) {
	cfg := DaemonConfig{Port: 18900, DataDir: "/tmp/whale", WorkDir: "/tmp/work"}
	if cfg.Port != 18900 {
		t.Errorf("port = %d", cfg.Port)
	}
	if cfg.DataDir != "/tmp/whale" {
		t.Errorf("dataDir = %s", cfg.DataDir)
	}
	if cfg.WorkDir != "/tmp/work" {
		t.Errorf("workDir = %s", cfg.WorkDir)
	}
}

// =========================================================================
// Agent/Expert/Team dir helpers
// =========================================================================

func TestDaemonDirs(t *testing.T) {
	base := filepath.Join(string(os.PathSeparator), "tmp", "whale")
	cfg := DaemonConfig{DataDir: base}
	d := &Daemon{cfg: cfg}
	if d.agentsDir() != filepath.Join(base, "agents") {
		t.Errorf("agentsDir = %s, want %s", d.agentsDir(), filepath.Join(base, "agents"))
	}
	if d.expertsDir() != filepath.Join(base, "experts") {
		t.Errorf("expertsDir = %s, want %s", d.expertsDir(), filepath.Join(base, "experts"))
	}
	if d.teamsDir() != filepath.Join(base, "teams") {
		t.Errorf("teamsDir = %s, want %s", d.teamsDir(), filepath.Join(base, "teams"))
	}
}

// =========================================================================
// Task create/list/cancel/delete message shapes
// =========================================================================

func TestTaskCreateRequestJSON(t *testing.T) {
	raw := []byte(`{"goal":"write tests","workdir":"/tmp/test","team_name":"dev"}`)
	var req taskCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Goal != "write tests" {
		t.Errorf("goal = %q", req.Goal)
	}
	if req.WorkDir != "/tmp/test" {
		t.Errorf("workdir = %q", req.WorkDir)
	}
	if req.TeamName != "dev" {
		t.Errorf("team_name = %q", req.TeamName)
	}
}

func TestChatCancelRequestJSON(t *testing.T) {
	raw := []byte(`{"session_id":"s1"}`)
	var req chatCancelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.SessionID != "s1" {
		t.Errorf("session_id = %q", req.SessionID)
	}
}

// =========================================================================
// approvalRequired / userInputRequired message shapes
// =========================================================================

func TestApprovalRequiredJSON(t *testing.T) {
	msg := approvalRequired{
		SessionID:  "s1",
		ToolCallID: "tc-1",
		ToolName:   "shell_run",
		Reason:     "runs: go build",
		Code:       "0",
		Key:        "",
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	if m["tool_name"] != "shell_run" {
		t.Errorf("tool_name = %v", m["tool_name"])
	}
	if m["tool_call_id"] != "tc-1" {
		t.Errorf("tool_call_id = %v", m["tool_call_id"])
	}
}

func TestUserInputRequiredJSON(t *testing.T) {
	msg := userInputRequired{
		SessionID:  "s1",
		ToolCallID: "tc-2",
		Questions: []userInputQ{
			{
				ID:       "q1",
				Header:   "Choose",
				Question: "A or B?",
				Options: []userInputOpt{
					{Label: "A", Description: "Option A"},
					{Label: "B", Description: "Option B"},
				},
			},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	qs := m["questions"].([]interface{})
	if len(qs) != 1 {
		t.Fatalf("questions len = %d", len(qs))
	}
}

// =========================================================================
// Approval decision message shapes
// =========================================================================

func TestApprovalDecisionJSON(t *testing.T) {
	tests := []string{"allow", "deny", "allow_session", "cancel"}
	for _, d := range tests {
		raw := `{"session_id":"s1","tool_call_id":"tc-1","decision":"` + d + `"}`
		var req approvalDecision
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Errorf("unmarshal %q: %v", d, err)
		}
		if req.Decision != d {
			t.Errorf("decision = %q", req.Decision)
		}
	}
}

// =========================================================================
// Edge cases
// =========================================================================

func TestWsClientSendNilDaemon(t *testing.T) {
	// send should not panic even with nil fields.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
		var msg map[string]interface{}
		conn.ReadJSON(&msg)
		conn.WriteJSON(msg)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	defer conn.Close()

	// Client without daemon reference should still send.
	c := &wsClient{id: "orphan", conn: conn, writeMu: sync.Mutex{}}
	c.send(wsResponse{Type: "ok"})
}
