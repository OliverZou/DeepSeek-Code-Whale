package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/usewhale/whale/internal/eventbus"
	"github.com/usewhale/whale/internal/team_engine"
	"golang.org/x/sys/windows"
)

type WorkspaceState struct {
	ID         string
	Path       string
	Label      string
	Engine     *team_engine.TeamEngine
	Registered time.Time
	LastSeen   time.Time
}

// WorkspaceJSON is the JSON representation sent to the frontend.
type WorkspaceJSON struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Label      string `json:"label"`
	Registered string `json:"registered"`
	LastSeen   string `json:"last_seen"`
	Online     bool   `json:"online"`
	TaskCount  int    `json:"task_count"`
	ActiveCount int   `json:"active_count"`
}

// MasterTaskJSON is the JSON representation of a master task (总任务).
type MasterTaskJSON struct {
	ID             string `json:"id"`
	Goal           string `json:"goal"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspacePath  string `json:"workspace_path"`
	WorkspaceLabel string `json:"workspace_label"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	TaskCount      int    `json:"task_count"`
	DoneCount      int    `json:"done_count"`
	ActiveCount    int    `json:"active_count"`
	SuspendedCount int    `json:"suspended_count"`
	WorkspaceOnline bool  `json:"workspace_online"`
}

// SubtaskJSON is a subtask in a master task's plan.
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

// AgentDialogueJSON is one message in the agent's conversation.
type AgentDialogueJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TaskJSON is a per-workspace task summary sent to the frontend.
type TaskJSON struct {
	WorkspaceID string `json:"workspace_id"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Role        string `json:"role"`
	ElapsedSec  int    `json:"elapsed_sec"`
	RetryCount  int    `json:"retry_count"`
	MaxRetries  int    `json:"max_retries"`
	ProgressPct int    `json:"progress_pct"`
}

// LogFileJSON describes one log file available for a task.
type LogFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// MultiEngineManager manages a map of workspace ID → WorkspaceState.
// It is safe for concurrent use.
type MultiEngineManager struct {
	mu           sync.RWMutex
	states       map[string]*WorkspaceState
	seq          int64
	updateSeq    int64 // 单调递增版本号，emitUpdate 用去重
	eventHandler func(event team_engine.TaskEvent) // 可选的事件回调

	// pendingResume maps wsID → masterTaskID for commands waiting to be
	// picked up by the main Whale CLI via heartbeat response.
	pendingResume map[string]string
    
	// wsConns tracks active WebSocket connections keyed by workspace ID.
	wsConns map[string]*websocket.Conn

	// EventBus bridge fan-out: one goroutine reads from the global
	// bridgeOut and fans out to all connected whale CLI processes.
	bridgeFanOut    sync.Once
	bridgeWriters   map[string]chan<- []byte // wsID → write channel
	bridgeWritersMu sync.Mutex

	// dashboardDir is the directory containing the dashboard executable,
	// used for persisting workspace discovery data (workspaces.json).
	dashboardDir string

	// workspaceCaches holds in-memory task state synced from whale CLI.
	// No SQLite reads needed — data arrives via WebSocket sync messages.
	workspaceCaches map[string]*WorkspaceCache