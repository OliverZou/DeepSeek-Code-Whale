package team_engine

// MasterTask represents a top-level task (总任务).
type MasterTask struct {
	ID string `json:"id"`
	// Goal is the task goal string provided by the initiator.
	Goal string `json:"goal"`
	// Agent is the team/expert label for display (team:foo / expert:bar).
	Agent string `json:"agent,omitempty"`
	// SessionID is the initiator session — who launched the run, who receives
	// the final report.
	SessionID string `json:"session_id,omitempty"`
	// LeaderSessionID is the Leader subagent session spawned for this run (P2).
	// Addresses the Leader for continuation (the drive turn, user
	// conversations, prompt/fork/summarize).
	LeaderSessionID string `json:"leader_session_id,omitempty"`
	WorkspacePath   string `json:"workspace_path,omitempty"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at,omitempty"`
}

// StateHistoryEntry records a state transition for auditing.
type StateHistoryEntry struct {
	TaskID    string `json:"task_id"`
	OldState  string `json:"old_state"`
	NewState  string `json:"new_state"`
	Reason    string `json:"reason"`
	ErrorMsg  string `json:"error_msg"`
	ChangedAt string `json:"changed_at"`
	Timestamp string `json:"timestamp"`
}

// MemoryEntry is a persisted agent experience/learning.
type MemoryEntry struct {
	ID         string `json:"id"`
	AgentRole  string `json:"agent_role"`
	Key        string `json:"key"`
	Content    string `json:"content"`
	SourceTask string `json:"source_task"`
	CreatedAt  string `json:"created_at"`
}
