package team_engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileTaskStore replaces SQLite TaskDB with a file-system-based task store.
// Tasks are indexed in memory — directory scanning only happens on startup.
//
//	{baseDir}/
//	├── {task_id}/
//	│   ├── meta.json    ← state tracking
//	│   ├── goal.md      ← Engine writes
//	│   ├── input.md     ← Engine writes (WriteInboxFile)
//	│   ├── output.md    ← Agent writes
//	│   ├── verify.md    ← Verifier writes
//	│   └── plan.json    ← Leader/self-split writes
//	└── masters/{master_id}/
//	    ├── meta.json
//	    └── goal.md
type FileTaskStore struct {
	baseDir string
	mu      sync.RWMutex
	tasks   map[string]*Task       // taskID → Task (in-memory index)
	masters map[string]*MasterTask // masterID → MasterTask
}

func NewFileTaskStore(baseDir string) (*FileTaskStore, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("create task store: %w", err)
	}
	fs := &FileTaskStore{
		baseDir: baseDir,
		tasks:   make(map[string]*Task),
		masters: make(map[string]*MasterTask),
	}
	fs.rebuildIndex()
	return fs, nil
}

// rebuildIndex scans the baseDir once to rebuild the in-memory index.
func (fs *FileTaskStore) rebuildIndex() {
	// Scan task directories.
	entries, _ := os.ReadDir(fs.baseDir)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "masters" || e.Name() == "memory" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		meta, err := fs.readMeta(fs.taskDir(e.Name()))
		if err != nil {
			continue
		}
		fs.tasks[e.Name()] = fs.taskFromMeta(meta)
	}
	// Scan master directories.
	mastersDir := filepath.Join(fs.baseDir, "masters")
	if mEntries, err := os.ReadDir(mastersDir); err == nil {
		for _, e := range mEntries {
			if !e.IsDir() {
				continue
			}
			meta, err := fs.readMeta(fs.masterDir(e.Name()))
			if err != nil {
				continue
			}
			fs.masters[e.Name()] = &MasterTask{ID: meta.ID, Goal: meta.Title, CreatedAt: meta.CreatedAt}
		}
	}
}

// refreshState re-derives task state from files.
func (fs *FileTaskStore) refreshState(t *Task) {
	if t == nil {
		return
	}
	t.State = fs.deriveState(fs.taskDir(t.ID))
}

func (fs *FileTaskStore) taskDir(id string) string  { return filepath.Join(fs.baseDir, id) }
func (fs *FileTaskStore) masterDir(id string) string { return filepath.Join(fs.baseDir, "masters", id) }

// ---------------------------------------------------------------------------
// metadata
// ---------------------------------------------------------------------------

type taskMeta struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	Role             string   `json:"role"`
	State            string   `json:"state,omitempty"`
	Output           string   `json:"output"`
	ParentIDs        []string `json:"parent_ids,omitempty"`
	BatchID          string   `json:"batch_id,omitempty"`
	MasterTaskID     string   `json:"master_task_id,omitempty"`
	VerifierFocus    string   `json:"verifier_focus,omitempty"`
	VerifierFeedback string   `json:"verifier_feedback,omitempty"`
	MaxRetries       int      `json:"max_retries,omitempty"`
	RetryCount       int      `json:"retry_count,omitempty"`
	CreatedAt        string   `json:"created_at"`
}

func (fs *FileTaskStore) writeMeta(dir string, meta *taskMeta) error {
	data, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0644)
}

func (fs *FileTaskStore) readMeta(dir string) (*taskMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta taskMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (fs *FileTaskStore) writeGoal(dir, title, role, description, output string) error {
	goal := fmt.Sprintf("# %s\n\n**角色**: %s\n\n## 任务描述\n\n%s\n\n## 产出\n\n`%s`\n", title, role, description, output)
	return os.WriteFile(filepath.Join(dir, "goal.md"), []byte(goal), 0644)
}

func (fs *FileTaskStore) taskFromMeta(meta *taskMeta) *Task {
	return &Task{
		ID:               meta.ID,
		Title:            meta.Title,
		Description:      meta.Description,
		Role:             AgentRole(meta.Role),
		State:            TaskState(meta.State),
		Output:           meta.Output,
		ParentIDs:        meta.ParentIDs,
		BatchID:          meta.BatchID,
		MasterTaskID:     meta.MasterTaskID,
		VerifierFocus:    meta.VerifierFocus,
		VerifierFeedback: meta.VerifierFeedback,
		MaxRetries:       meta.MaxRetries,
		RetryCount:       meta.RetryCount,
		CreatedAt:        meta.CreatedAt,
	}
}

// ---------------------------------------------------------------------------
// state derivation from files
// ---------------------------------------------------------------------------

func (fs *FileTaskStore) deriveState(dir string) TaskState {
	out := filepath.Join(dir, "output.md")
	verify := filepath.Join(dir, "verify.md")
	_, outErr := os.Stat(out)
	_, verErr := os.Stat(verify)

	if outErr == nil && verErr == nil {
		return TaskStateDone
	}
	if outErr == nil {
		if meta, err := fs.readMeta(dir); err == nil {
			switch TaskState(meta.State) {
			case TaskStateSuspended, TaskStateFailed, TaskStatePending, TaskStateAssigned:
				return TaskState(meta.State)
			}
		}
		return TaskStateProduced
	}
	if meta, err := fs.readMeta(dir); err == nil && meta.State != "" {
		return TaskState(meta.State)
	}
	return TaskStatePending
}

func (fs *FileTaskStore) findTaskDir(id string) string {
	dir := fs.taskDir(id)
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	return ""
}

// ---------------------------------------------------------------------------
// master task operations
// ---------------------------------------------------------------------------

func (fs *FileTaskStore) InsertMasterTask(mt *MasterTask) error {
	dir := fs.masterDir(mt.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	meta := &taskMeta{ID: mt.ID, Title: mt.Goal, Role: "teamleader", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := fs.writeMeta(dir, meta); err != nil {
		return err
	}
	fs.writeGoal(dir, mt.Goal, "teamleader", mt.Goal, "")

	fs.mu.Lock()
	fs.masters[mt.ID] = &MasterTask{ID: mt.ID, Goal: mt.Goal, CreatedAt: meta.CreatedAt}
	fs.mu.Unlock()
	return nil
}

func (fs *FileTaskStore) ListMasterTasks() ([]*MasterTask, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	result := make([]*MasterTask, 0, len(fs.masters))
	for _, mt := range fs.masters {
		result = append(result, mt)
	}
	return result, nil
}

func (fs *FileTaskStore) GetMasterTask(id string) (*MasterTask, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	mt, ok := fs.masters[id]
	if !ok {
		return nil, nil
	}
	return mt, nil
}

func (fs *FileTaskStore) UpdateMasterTaskStatus(id, status string) error { return nil }
func (fs *FileTaskStore) DeleteMasterTask(id string) error {
	fs.mu.Lock()
	delete(fs.masters, id)
	fs.mu.Unlock()
	return os.RemoveAll(fs.masterDir(id))
}

func (fs *FileTaskStore) SaveMasterTaskProgress(masterTaskID, progressJSON string) error {
	return os.WriteFile(filepath.Join(fs.masterDir(masterTaskID), "plan.json"), []byte(progressJSON), 0644)
}

func (fs *FileTaskStore) GetMasterTaskProgress(masterTaskID string) (string, error) {
	data, err := os.ReadFile(filepath.Join(fs.masterDir(masterTaskID), "plan.json"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ---------------------------------------------------------------------------
// subtask operations
// ---------------------------------------------------------------------------

func (fs *FileTaskStore) InsertTask(task *Task) error {
	dir := fs.taskDir(task.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	meta := &taskMeta{
		ID:            task.ID, Title: task.Title, Description: task.Description,
		Role:          string(task.Role), State: "pending", Output: task.Output,
		ParentIDs:     task.ParentIDs, BatchID: task.BatchID, MasterTaskID: task.MasterTaskID,
		VerifierFocus: task.VerifierFocus, MaxRetries: task.MaxRetries,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := fs.writeMeta(dir, meta); err != nil {
		return err
	}
	fs.writeGoal(dir, task.Title, string(task.Role), task.Description, task.Output)

	fs.mu.Lock()
	task.State = TaskStatePending
	fs.tasks[task.ID] = task
	fs.mu.Unlock()
	return nil
}

func (fs *FileTaskStore) GetTask(id string) (*Task, error) {
	fs.mu.RLock()
	t, ok := fs.tasks[id]
	fs.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	fs.refreshState(t)
	return t, nil
}

func (fs *FileTaskStore) ListTasks() ([]*Task, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	result := make([]*Task, 0, len(fs.tasks))
	for _, t := range fs.tasks {
		fs.refreshState(t)
		result = append(result, t)
	}
	return result, nil
}

func (fs *FileTaskStore) ListTasksByMasterTask(masterID string) ([]*Task, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	var result []*Task
	for _, t := range fs.tasks {
		if t.MasterTaskID == masterID {
			fs.refreshState(t)
			result = append(result, t)
		}
	}
	return result, nil
}

func (fs *FileTaskStore) ListTasksByParent(parentID string) ([]*Task, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	var result []*Task
	for _, t := range fs.tasks {
		for _, pid := range t.ParentIDs {
			if pid == parentID {
				fs.refreshState(t)
				result = append(result, t)
				break
			}
		}
	}
	return result, nil
}

func (fs *FileTaskStore) ListTasksByState(state TaskState) ([]*Task, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	var result []*Task
	for _, t := range fs.tasks {
		fs.refreshState(t)
		if t.State == state {
			result = append(result, t)
		}
	}
	return result, nil
}

func (fs *FileTaskStore) UpdateTask(id string, fields map[string]interface{}) error {
	fs.mu.Lock()
	t, ok := fs.tasks[id]
	fs.mu.Unlock()
	if !ok {
		return nil
	}

	dir := fs.taskDir(id)
	meta, err := fs.readMeta(dir)
	if err != nil {
		return nil
	}

	if v, ok := fields["title"]; ok {
		meta.Title = fmt.Sprint(v)
		t.Title = meta.Title
	}
	if v, ok := fields["description"]; ok {
		meta.Description = fmt.Sprint(v)
		t.Description = meta.Description
	}
	if v, ok := fields["state"]; ok {
		meta.State = fmt.Sprint(v)
	}
	if v, ok := fields["batch_id"]; ok {
		meta.BatchID = fmt.Sprint(v)
		t.BatchID = meta.BatchID
	}
	if v, ok := fields["master_task_id"]; ok {
		meta.MasterTaskID = fmt.Sprint(v)
		t.MasterTaskID = meta.MasterTaskID
	}
	if v, ok := fields["verifier_focus"]; ok {
		meta.VerifierFocus = fmt.Sprint(v)
		t.VerifierFocus = meta.VerifierFocus
	}
	if v, ok := fields["verifier_feedback"]; ok {
		meta.VerifierFeedback = fmt.Sprint(v)
		t.VerifierFeedback = meta.VerifierFeedback
	}
	if v, ok := fields["max_retries"]; ok {
		switch n := v.(type) {
		case int:
			meta.MaxRetries = n
			t.MaxRetries = n
		case float64:
			meta.MaxRetries = int(n)
			t.MaxRetries = int(n)
		}
	}
	if v, ok := fields["retry_count"]; ok {
		switch n := v.(type) {
		case int:
			meta.RetryCount = n
			t.RetryCount = n
		case float64:
			meta.RetryCount = int(n)
			t.RetryCount = int(n)
		}
	}
	return fs.writeMeta(dir, meta)
}

func (fs *FileTaskStore) TransitionState(id string, newState TaskState, _, _ string) error {
	fs.mu.Lock()
	if t, ok := fs.tasks[id]; ok {
		t.State = newState
	}
	fs.mu.Unlock()
	dir := fs.findTaskDir(id)
	if dir == "" {
		return nil
	}
	meta, err := fs.readMeta(dir)
	if err != nil {
		return nil
	}
	meta.State = string(newState)
	return fs.writeMeta(dir, meta)
}

func (fs *FileTaskStore) ForceTransitionState(id string, newState TaskState, _ string) error {
	return fs.TransitionState(id, newState, "", "")
}

func (fs *FileTaskStore) UpdateTaskMasterTaskID(taskID, masterTaskID string) error {
	return fs.UpdateTask(taskID, map[string]interface{}{"master_task_id": masterTaskID})
}

func (fs *FileTaskStore) DeleteTask(id string) error {
	fs.mu.Lock()
	delete(fs.tasks, id)
	fs.mu.Unlock()
	dir := fs.findTaskDir(id)
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// ---------------------------------------------------------------------------
// maintenance
// ---------------------------------------------------------------------------

func (fs *FileTaskStore) Checkpoint() error                             { return nil }
func (fs *FileTaskStore) checkpointAfterWrite()                          {}
func (fs *FileTaskStore) Close() error                                   { return nil }
func (fs *FileTaskStore) RecordStateHistory(_, _, _, _ string) error     { return nil }
func (fs *FileTaskStore) GetTaskHistory(_ string) ([]StateHistoryEntry, error) { return nil, nil }

func (fs *FileTaskStore) SaveMemory(memory *MemoryEntry) error {
	memDir := filepath.Join(fs.baseDir, "memory")
	os.MkdirAll(memDir, 0755)
	data, _ := json.Marshal(memory)
	name := fmt.Sprintf("%s_%s_%d.json", memory.AgentRole, memory.Key, time.Now().UnixNano())
	return os.WriteFile(filepath.Join(memDir, name), data, 0644)
}

func (fs *FileTaskStore) GetMemories(role AgentRole, key string) ([]MemoryEntry, error) {
	memDir := filepath.Join(fs.baseDir, "memory")
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil, nil
	}
	var memories []MemoryEntry
	prefix := fmt.Sprintf("%s_%s_", role, key)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".json") {
			data, _ := os.ReadFile(filepath.Join(memDir, e.Name()))
			var mem MemoryEntry
			if json.Unmarshal(data, &mem) == nil {
				memories = append(memories, mem)
			}
		}
	}
	return memories, nil
}
