package dashboard

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

const (
	dashboardAddr     = "http://127.0.0.1:8520"
	heartbeatInterval = 30 * time.Second
	registerTimeout   = 5 * time.Second
)

// Client handles registration and heartbeat with the dashboard server.
type Client struct {
	workspacePath string
	wsID          string
	httpClient    *http.Client
	done          chan struct{}
}

// NewClient creates a dashboard client.  It immediately tries to register
// with the dashboard.  Returns nil if the dashboard is unreachable (whale
// works normally without it).
func NewClient(workspacePath string) *Client {
	c := &Client{
		workspacePath: workspacePath,
		httpClient: &http.Client{Timeout: registerTimeout},
		done:         make(chan struct{}),
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
		// Dashboard unreachable — whale works fine without it.
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}
	c.wsID = result["id"]
	log.Printf("dashboard: registered as %s", c.wsID)
}

// StartHeartbeat begins periodic heartbeats in a background goroutine.
func (c *Client) StartHeartbeat() {
	if c.wsID == "" {
		return
	}
	go c.heartbeatLoop()
}

// heartbeatLoop sends periodic heartbeats to the dashboard.
func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.sendHeartbeat()
		}
	}
}

func (c *Client) sendHeartbeat() {
	payload, _ := json.Marshal(map[string]string{"id": c.wsID})
	_, _ = c.httpClient.Post(
		dashboardAddr+"/api/heartbeat",
		"application/json",
		bytes.NewReader(payload),
	)
}

// Deregister unregisters from the dashboard.  Call on whale shutdown.
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

// IsRegistered returns true if the client successfully registered.
func (c *Client) IsRegistered() bool {
	return c.wsID != ""
}
