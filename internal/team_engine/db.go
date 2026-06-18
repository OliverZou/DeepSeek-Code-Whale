package team_engine

// MasterTask represents a top-level task (总任务).
type MasterTask struct {
	ID            string `json:"id"`
	Goal          string `json:"goal"`
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
