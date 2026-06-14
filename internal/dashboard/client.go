package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/usewhale/whale/internal/team_engine"
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

	pendingResume   string
	pendingResumeMu sync.Mutex
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
		log.Printf("dashboard: register failed: %v", err)
		if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.CLIHeartbeat("", false) }
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("dashboard: register returned %d: %s", resp.StatusCode, string(body))
		return
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("dashboard: register decode: %v", err)
		return
	}
	c.wsID = result["id"]
	log.Printf("dashboard: registered as %s (path=%s)", c.wsID, c.workspacePath)
	if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.CLIHeartbeat(c.wsID, true) }
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
func (c *Client) connectAndRead() {
	u := url.URL{Scheme: "ws", Host: "127.0.0.1:8520", Path: "/ws", RawQuery: "wsid=" + c.wsID}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return
	}
	defer conn.Close()

	log.Printf("dashboard: ws connected as %s", c.wsID)
	if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.CLIWSConnect(c.wsID, nil) }

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

	// Read loop: receive commands from dashboard.
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			close(heartbeatDone)
			return
		}

		var body struct {
			Command      string `json:"command"`
			MasterTaskID string `json:"master_task_id"`
		}
		if err := json.Unmarshal(msg, &body); err != nil {
			continue
		}
		if body.Command == "resume" && body.MasterTaskID != "" {
			log.Printf("dashboard: received resume command for master task %s", body.MasterTaskID)
			c.pendingResumeMu.Lock()
			c.pendingResume = body.MasterTaskID
			c.pendingResumeMu.Unlock()
			if c.OnResume != nil {
				if team_engine.DefaultTeamLog != nil { team_engine.DefaultTeamLog.CLIReceiveResume(body.MasterTaskID) }
				c.OnResume(body.MasterTaskID)
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
	log.Printf("dashboard: deregistered %s", c.wsID)
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

