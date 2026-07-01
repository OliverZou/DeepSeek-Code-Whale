package team_engine

// TaskSession represents a top-level task session container (任务会话容器).
// Distinct from chat Session to avoid naming confusion.
type TaskSession struct {
	ID            string `json:"id"`
	Goal          string `json:"goal"`
	Agent         string `json:"agent,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at,omitempty"`
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
