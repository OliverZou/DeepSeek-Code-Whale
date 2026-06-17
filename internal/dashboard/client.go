package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/usewhale/whale/internal/eventbus"
	
)

const (
	dashboardAddr     = "http://127.0.0.1:8520"
	heartbeatInterval = 30 * time.Second
	registerTimeout   = 5 * time.Second
	wsReconnectDelay  = 5 * time.Second
)

// Client handles registration and WebSocket communication with the dashboard.
type Client struct {
	workspacePath string
	wsID          string
	httpClient    *http.Client
	done          chan struct{}

	// OnResume is called when the dashboard sends a resume command.
	// masterTaskID is the task to execute.
	OnResume func(masterTaskID string)
	// OnCancel is called when the dashboard sends a cancel command.
	OnCancel func(masterTaskID string)

	pendingResume   string
	pendingResumeMu sync.Mutex
	wsConn          *websocket.Conn
	wsConnMu        sync.Mutex
}

// NewClient creates a dashboard client.  It immediately tries to register
// with the dashboard.  Returns nil if the dashboard is unreachable (whale
// works normally without it).
func NewClient(workspacePath string) *Client {
	c := &Client{
		workspacePath: workspacePath,
		httpClient:    &http.Client{Timeout: registerTimeout},
		done:          make(chan struct{}),
	}
	c.tryRegister()
	return c
}

// tryRegister attempts to register with the dashboard.
func (c *Client) tryRegister() {
	payload, _ := json.Marshal(map[string]string{"path": c.workspacePath})
	resp, err := c.httpClient.Post(
		dashboardAddr+"/api/register",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
CLIHeartbeat("", false)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		Log("dashboard", "register returned %d: %s", resp.StatusCode, string(body))
		return
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		Log("dashboard", "register decode: %v", err)
		return
	}
	c.wsID = result["id"]
	Log("dashboard", "registered as %s (path=%s)", c.wsID, c.workspacePath)
CLIHeartbeat(c.wsID, true)
}


// StartHeartbeat begins the WebSocket connection and heartbeat loop.
func (c *Client) StartHeartbeat() {
	go c.wsLoop()
}

// wsLoop maintains the WebSocket connection with automatic reconnection.
func (c *Client) wsLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		if c.wsID == "" {
			c.tryRegister()
			time.Sleep(wsReconnectDelay)
			continue
		}

		c.connectAndRead()

		// Connection dropped 鈥?clear wsID so we re-register via
		// HTTP on the next loop iteration.  This handles dashboard
		// restarts (the old wsID is unknown to the new dashboard).
		c.wsID = ""

		// Reconnect on disconnect.
		select {
		case <-c.done:
			return
		case <-time.After(wsReconnectDelay):
		}
	}
}

	// connectAndRead opens a WebSocket connection and reads messages.
	// Bridges the process-local EventBus to the WebSocket so that events
	// published in the dashboard process reach the whale CLI and vice versa.
	func (c *Client) connectAndRead() {
		u := url.URL{Scheme: "ws", Host: "127.0.0.1:8520", Path: "/ws", RawQuery: "wsid=" + c.wsID}
		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			return
		}
		defer func() {
			c.wsConnMu.Lock()
			c.wsConn = nil
			c.wsConnMu.Unlock()
			conn.Close()
		}()

		c.wsConnMu.Lock()
		c.wsConn = conn
		c.wsConnMu.Unlock()
		Log("dashboard", "ws connected as %s", c.wsID)
CLIWSConnect(c.wsID, nil)

		// Enable cross-process EventBus bridge.  bridgeOut carries events
		// published in THIS process (send to dashboard over WebSocket);
		// bridgeIn receives events from the dashboard (inject locally).
		bridgeOut, bridgeIn := eventbus.EnableGlobalBridge()

		// Forward whale CLI-side EventBus events to the dashboard.
		bridgeDone := make(chan struct{})
		defer close(bridgeDone)
		go func() {
			for {
				select {
				case <-bridgeDone:
					return
				case be, ok := <-bridgeOut:
					if !ok {
						return
					}
					data, err := json.Marshal(be)
					if err != nil {
						continue
					}
					if werr := conn.WriteMessage(websocket.TextMessage, data); werr != nil {
						Log("dashboard", "bridge write failed: %v", werr)
						// Don't return — a single failed write doesn't mean
						// the connection is dead.  Continue processing events.
					}
				}
			}
		}()

		// Write loop: periodic heartbeat pings.
		heartbeatDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(heartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-c.done:
					return
				case <-heartbeatDone:
					return
				case <-ticker.C:
					if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
						return
					}
				}
			}
		}()

		// Read loop: receive bridged events and commands from dashboard.
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				close(heartbeatDone)
				return
			}

			// Try BridgedEvent format first (cross-process EventBus).
			var be eventbus.BridgedEvent
			if err := json.Unmarshal(msg, &be); err == nil && be.Topic != "" {
				select {
				case bridgeIn <- be:
				default:
					Log("dashboard", "cli bridgeIn full, dropped topic=%s type=%s", be.Topic, be.Event.Type)
				}
				continue
			}

			// Legacy: direct command format.
			var body struct {
				Command      string `json:"command"`
				MasterTaskID string `json:"master_task_id"`
			}
			if err := json.Unmarshal(msg, &body); err != nil {
				continue
			}
			if body.Command == "resume" && body.MasterTaskID != "" {
				Log("dashboard", "received resume command for master task %s", body.MasterTaskID)
				c.pendingResumeMu.Lock()
				c.pendingResume = body.MasterTaskID
				c.pendingResumeMu.Unlock()
				if c.OnResume != nil {
CLIReceiveResume(body.MasterTaskID)
					c.OnResume(body.MasterTaskID)
				}
			}
			if body.Command == "cancel_master" && body.MasterTaskID != "" {
				Log("dashboard", "received cancel command for master task %s", body.MasterTaskID)
				if c.OnCancel != nil {
					c.OnCancel(body.MasterTaskID)
				}
			}
		}
	}

// Deregister unregisters from the dashboard.
func (c *Client) Deregister() {
	close(c.done)
	if c.wsID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{"id": c.wsID})
	resp, err := c.httpClient.Post(
		dashboardAddr+"/api/deregister",
		"application/json",
		bytes.NewReader(payload),
	)
	if err == nil {
		resp.Body.Close()
	}
	Log("dashboard", "deregistered %s", c.wsID)
}

// PendingResume returns the master task ID of a pending resume command.
func (c *Client) PendingResume() string {
	c.pendingResumeMu.Lock()
	defer c.pendingResumeMu.Unlock()
	id := c.pendingResume
	c.pendingResume = ""
	return id
}

// IsRegistered returns true if the client successfully registered.
func (c *Client) IsRegistered() bool {
	return c.wsID != ""
}

// SyncState pushes the full master-task and subtask state to the dashboard.
// Called on connect and after major state changes.
func (c *Client) SyncState(mts []MasterTaskJSON, sts map[string][]SubtaskJSON, wsLabel string) {
	c.wsConnMu.Lock()
	conn := c.wsConn
	c.wsConnMu.Unlock()
	if conn == nil {
		return
	}
	// Inject workspace label into master tasks.
	for i := range mts {
		mts[i].WorkspaceLabel = wsLabel
		mts[i].WorkspaceOnline = true
	}
	msg, _ := json.Marshal(map[string]interface{}{
		"type":          "sync_full",
		"master_tasks":  mts,
		"subtasks":      sts,
		"workspace_id":  c.wsID,
	})
	conn.WriteMessage(websocket.TextMessage, msg)
}

// SendTaskEvent sends a team-engine task event to the dashboard over WebSocket.
// Non-blocking: if the write would block, the event is dropped to avoid
// stalling the team engine's event loop.
func (c *Client) SendTaskEvent(event TaskEvent) {
	c.wsConnMu.Lock()
	conn := c.wsConn
	c.wsConnMu.Unlock()
	if conn == nil {
		return
	}
	data, _ := json.Marshal(event)
	// Set a short write deadline to avoid blocking the publisher.
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		// Best-effort; drop on error rather than blocking.
	}
}


// formatPayload returns a compact string representation of an event payload
// for bridge logging.  Truncates to 120 chars to avoid log bloat.
func formatPayload(payload interface{}) string {
	if payload == nil {
		return "{}"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("<marshal err: %v>", err)
	}
	s := string(data)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
