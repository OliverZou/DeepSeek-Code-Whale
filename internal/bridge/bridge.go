// Package bridge provides the WebSocket bridge between whale CLI and the
// dashboard process.  It defines the shared JSON types, a local logger, and
// the Client that maintains the WebSocket connection.
package bridge

import (
	"github.com/usewhale/whale/internal/team_engine"
)

// =========================================================================
// Logger 闁?local file-based log, independent of team_engine
// =========================================================================



// SetLogFile is a no-op; all bridge logging goes through team_engine.Log.
func SetLogFile(path string) {}

// Log writes a diagnostic entry via team_engine.Log.
func Log(cat, format string, args ...interface{}) {
	team_engine.Log(cat, format, args...)
}

// =========================================================================
// Log helpers 闁?called by Client
// =========================================================================

// CLIHeartbeat logs a CLI heartbeat registration result.
func CLIHeartbeat(wsID string, registered bool) {
	Log("cli", "heartbeat wsID=%s registered=%v", wsID, registered)
}

// CLIWSConnect logs a CLI WebSocket connection.
func CLIWSConnect(wsID string, err error) { Log("cli", "ws connect %s ok", wsID) }

// CLIWSDisconnect logs a CLI WebSocket disconnection.
func CLIWSDisconnect(wsID string) { Log("cli", "ws disconnect %s", wsID) }

// CLIReceiveResume logs a resume command received from the dashboard.
func CLIReceiveResume(masterTaskID string) {
	Log("cli", "receive resume %s", masterTaskID[:8])
}

// =========================================================================
// TaskEvent 闁?lightweight event sent over WebSocket
// =========================================================================

// TaskEventType mirrors team_engine event types that the bridge forwards.
type TaskEventType int

const (
	EventStateChanged TaskEventType = 0
	EventLeaderLog    TaskEventType = 4
	EventAgentLog     TaskEventType = 5
)

// TaskEvent is a lightweight event sent from the whale CLI via WebSocket.
type TaskEvent struct {
	Type     TaskEventType `json:"type"`
	TaskID   string        `json:"task_id"`
	Title    string        `json:"title"`
	Progress int           `json:"progress"`
	NewState string        `json:"new_state,omitempty"`
}

// =========================================================================
// BridgedEvent 闁?cross-process EventBus message
// =========================================================================

// Event is a simple topic+type+payload envelope.
type Event struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// BridgedEvent is sent over WebSocket between CLI and dashboard.
type BridgedEvent struct {
	Topic string `json:"topic"`
	Event Event  `json:"event"`
}

// =========================================================================
// EnableGlobalBridge 闁?channel pair for cross-process bridge
// =========================================================================

// EnableGlobalBridge returns a read channel and write channel for the bridge.
func EnableGlobalBridge() (<-chan BridgedEvent, chan<- BridgedEvent) {
	ch := make(chan BridgedEvent, 128)
	return ch, ch
}

// =========================================================================
// TaskSessionJSON / SubtaskJSON 闁?sync protocol types
// =========================================================================

// TaskSessionJSON is the JSON representation of a session sent to the frontend.
type TaskSessionJSON struct {
	ID              string `json:"id"`
	Goal            string `json:"goal"`
	WorkspaceID     string `json:"workspace_id"`
	WorkspacePath   string `json:"workspace_path"`
	WorkspaceLabel  string `json:"workspace_label"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	TaskCount       int    `json:"task_count"`
	DoneCount       int    `json:"done_count"`
	ActiveCount     int    `json:"active_count"`
	SuspendedCount  int    `json:"suspended_count"`
	WorkspaceOnline bool   `json:"workspace_online"`
}

// SubtaskJSON is a subtask in a session's plan.
type SubtaskJSON struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Output      string        `json:"output"`
	Role        string        `json:"role"`
	State       string        `json:"state"`
	Progress    int           `json:"progress"`
	CreatedAt   string        `json:"created_at"`
	ParentIDs   []string      `json:"parent_ids"`
	BatchID     string        `json:"batch_id"`
	RetryCount  int           `json:"retry_count"`
	MaxRetries  int           `json:"max_retries"`
	Children    []SubtaskJSON `json:"children,omitempty"`
}
