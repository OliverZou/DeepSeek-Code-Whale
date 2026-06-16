package dashboard

import "github.com/usewhale/whale/internal/team_engine"

// SyncMasterTasks is the payload for a full master-task sync from CLI to Dashboard.
type SyncMasterTasks struct {
	Tasks []MasterTaskJSON `json:"tasks"`
}

// SyncSubtasks is the payload for a full subtask tree sync.
type SyncSubtasks struct {
	MasterTaskID string        `json:"master_task_id"`
	Subtasks     []SubtaskJSON `json:"subtasks"`
}

// WorkspaceCache holds the dashboard's in-memory copy of a workspace's state.
// Updated via WebSocket sync messages; no SQLite reads needed.
type WorkspaceCache struct {
	MasterTasks map[string]*MasterTaskJSON // mtID → master task
	Subtasks    map[string][]SubtaskJSON   // mtID → subtask tree
}

// NewWorkspaceCache creates an empty cache.
func NewWorkspaceCache() *WorkspaceCache {
	return &WorkspaceCache{
		MasterTasks: make(map[string]*MasterTaskJSON),
		Subtasks:    make(map[string][]SubtaskJSON),
	}
}

// ApplySyncMasterTasks replaces all cached master tasks.
func (wc *WorkspaceCache) ApplySyncMasterTasks(tasks []MasterTaskJSON) {
	wc.MasterTasks = make(map[string]*MasterTaskJSON, len(tasks))
	for i := range tasks {
		mt := tasks[i]
		wc.MasterTasks[mt.ID] = &mt
	}
}

// ApplySyncSubtasks replaces the subtask tree for one master task.
func (wc *WorkspaceCache) ApplySyncSubtasks(mtID string, subtasks []SubtaskJSON) {
	wc.Subtasks[mtID] = subtasks
}

// BuildMasterTaskJSON converts team_engine types to the dashboard JSON.
func BuildMasterTaskJSON(mt *team_engine.MasterTask, tasks []*team_engine.Task, online bool) MasterTaskJSON {
	taskCount := len(tasks)
	doneCount := 0
	activeCount := 0
	suspendedCount := 0
	for _, t := range tasks {
		switch t.State {
		case team_engine.TaskStateDone, team_engine.TaskStateFailed:
			doneCount++
		case team_engine.TaskStateSuspended:
			suspendedCount++
		}
		if online && (t.State == team_engine.TaskStateProducing || t.State == team_engine.TaskStateVerifying) {
			activeCount++
		}
	}
	// Master task is "running" but between batches: still active.
	if activeCount == 0 && mt.Status == "running" && online {
		activeCount = 1
	}
	return MasterTaskJSON{
		ID:              mt.ID,
		Goal:            mt.Goal,
		WorkspacePath:   mt.WorkspacePath,
		Status:          mt.Status,
		CreatedAt:       mt.CreatedAt,
		TaskCount:       taskCount,
		DoneCount:       doneCount,
		ActiveCount:     activeCount,
		SuspendedCount:  suspendedCount,
	}
}

// BuildSubtaskJSON converts a team_engine Task tree to the JSON format.
func BuildSubtaskJSON(tasks []*team_engine.Task) []SubtaskJSON {
	nodeMap := make(map[string]*SubtaskJSON, len(tasks))
	taskList := make([]*SubtaskJSON, 0, len(tasks))
	for _, t := range tasks {
		sj := &SubtaskJSON{
			ID:          t.ID,
			Title:       t.Title,
			Description: t.Description,
			Output:      t.Output,
			Role:        string(t.Role),
			State:       string(t.State),
			Progress:    team_engine.GetProgress(t.State),
			CreatedAt:   t.CreatedAt,
			ParentIDs:   t.ParentIDs,
			RetryCount:  t.RetryCount,
			MaxRetries:  t.MaxRetries,
			BatchID:     t.BatchID,
		}
		nodeMap[t.ID] = sj
		taskList = append(taskList, sj)
	}
	// Attach children.
	for _, child := range taskList {
		for _, pid := range child.ParentIDs {
			if parent, ok := nodeMap[pid]; ok {
				parent.Children = append(parent.Children, *child)
			}
		}
	}
	// Recalculate parent progress from children.
	for _, sj := range taskList {
		if len(sj.Children) > 0 {
			sum := 0
			for _, c := range sj.Children {
				sum += c.Progress
			}
			sj.Progress = sum / len(sj.Children)
		}
	}
	// Collect roots.
	roots := make([]SubtaskJSON, 0)
	for _, sj := range taskList {
		hasParent := false
		for _, pid := range sj.ParentIDs {
			if _, ok := nodeMap[pid]; ok {
				hasParent = true
				break
			}
		}
		if !hasParent {
			roots = append(roots, *sj)
		}
	}
	if len(tasks) > 0 {
		leader := SubtaskJSON{
			ID:       "__leader__",
			Title:    "📋 任务规划",
			Role:     "teamleader",
			State:    "done",
			Progress: 100,
		}
		return append([]SubtaskJSON{leader}, roots...)
	}
	return roots
}
