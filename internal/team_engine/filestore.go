package team_engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileTaskStore is a file-system-based task store.
// Tasks are indexed in memory — directory scanning only happens on startup.
// Task state is derived from files, never stored in meta.json.
//
//	{baseDir}/
//	├── {task_id}/
//	│   ├── meta.json     ← structural metadata (no state field)
//	│   ├── goal.md       ← Engine writes
//	│   ├── input.md      ← Engine writes (WriteInboxFile)
//	│   ├── output.md     ← Agent writes  →  produced
//	│   ├── verify.md     ← Verifier writes → done (with output.md)
//	│   ├── error.md      ← failed
//	│   ├── suspended.md  ← suspended
//	│   ├── confirmation.md ← pending_confirmation
//	│   └── plan.json     ← Leader/self-split writes
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
	// 惰性创建：目录只在首次写入时产生。只读命令（status/list/analyze）
	// 启动即 newTeamEngine，绝不该在未执行任务的地方留下 .whale/team_tasks
	// ——尤其 whale.exe 所在目录（启动目录污染问题）。rebuildIndex 对
	// 不存在的目录按空索引处理；写路径（InsertMasterTask/InsertTask/
	// writeMeta）各自 MkdirAll 保底。
	fs := &FileTaskStore{
		baseDir: baseDir,
		tasks:   make(map[string]*Task),
		masters: make(map[string]*MasterTask),
	}
	fs.rebuildIndex()
	return fs, nil
}

// rebuildIndex scans the baseDir once to rebuild the in-memory index.
// State is derived from files, not read from meta.json.
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
		t := fs.taskFromMeta(meta)
		t.State = fs.deriveState(fs.taskDir(e.Name()))
		fs.tasks[e.Name()] = t
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
			fs.masters[e.Name()] = &MasterTask{ID: meta.ID, Goal: meta.Title, Agent: meta.Agent, SessionID: meta.SessionID, LeaderSessionID: meta.LeaderSessionID, WorkspacePath: meta.WorkspacePath, CreatedAt: meta.CreatedAt}
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

// DeriveState is the public entry point for state derivation.
func (fs *FileTaskStore) DeriveState(dir string) TaskState {
	return fs.deriveState(dir)
}

func (fs *FileTaskStore) taskDir(id string) string   { return filepath.Join(fs.baseDir, id) }
func (fs *FileTaskStore) masterDir(id string) string { return filepath.Join(fs.baseDir, "masters", id) }

// ---------------------------------------------------------------------------
// metadata — structural fields only, no state
// ---------------------------------------------------------------------------

type taskMeta struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	Role             string   `json:"role"`
	Agent            string   `json:"agent,omitempty"`
	SessionID        string   `json:"session_id,omitempty"`
	LeaderSessionID  string   `json:"leader_session_id,omitempty"`
	WorkspacePath    string   `json:"workspace_path,omitempty"`
	Output           string   `json:"output"`
	Workdir          string   `json:"workdir,omitempty"` // 交付工作目录（传播/机械验证的落点）
	ParentIDs        []string `json:"parent_ids,omitempty"`
	UpstreamBatches  []string `json:"upstream_batches,omitempty"`
	BatchID          string   `json:"batch_id,omitempty"`
	MasterTaskID     string   `json:"master_task_id,omitempty"`
	VerifierFocus    string   `json:"verifier_focus,omitempty"`
	VerifierFeedback string   `json:"verifier_feedback,omitempty"`
	MaxRetries       int      `json:"max_retries,omitempty"`
	Complexity       string   `json:"complexity,omitempty"`
	CreatedAt        string   `json:"created_at"`
	// 运行统计（累计，含重试轮）——事后分析与 run_report 的输出依据。
	WorkerDuration   float64 `json:"worker_duration_seconds,omitempty"`
	WorkerTokens     int     `json:"worker_tokens,omitempty"`
	VerifierDuration float64 `json:"verifier_duration_seconds,omitempty"`
	VerifierTokens   int     `json:"verifier_tokens,omitempty"`
	// 缓存分账（DeepSeek 前缀缓存）：hit 以 ~1/31 价计费，run_report 用它
	// 重建全量真实账单，而不是只有 raw 重放体积。store 是 leader 侧与
	// execute 侧两个引擎实例共享的唯一事实源。
	WorkerPromptHit        int `json:"worker_prompt_hit,omitempty"`
	WorkerPromptMiss       int `json:"worker_prompt_miss,omitempty"`
	WorkerCompletion       int `json:"worker_completion,omitempty"`
	VerifierPromptHit      int `json:"verifier_prompt_hit,omitempty"`
	VerifierPromptMiss     int `json:"verifier_prompt_miss,omitempty"`
	VerifierCompletion     int `json:"verifier_completion,omitempty"`
	ToolCalls        int     `json:"tool_calls,omitempty"`
	TopTools         string  `json:"top_tools,omitempty"`
	// 事后分析轨迹：验证结论/验证者 cap 次数/工具等待时长（墙钟归因）。
	Verdict          string  `json:"verdict,omitempty"`
	VerifierCapCount int     `json:"verifier_cap_count,omitempty"`
	ToolWaitSeconds  float64 `json:"tool_wait_seconds,omitempty"`
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

// SessionID returns the member subagent session ID persisted for a task, or ""
// if none has been recorded yet (the task hasn't run through the native
// adapter, or ran through the shell spawner which has no session).
func (fs *FileTaskStore) SessionID(id string) string {
	meta, err := fs.readMeta(fs.taskDir(id))
	if err != nil {
		return ""
	}
	return meta.SessionID
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
		Output:           meta.Output,
		Workdir:          meta.Workdir,
		ParentIDs:        meta.ParentIDs,
		UpstreamBatches:  meta.UpstreamBatches,
		BatchID:          meta.BatchID,
		MasterTaskID:     meta.MasterTaskID,
		VerifierFocus:    meta.VerifierFocus,
		VerifierFeedback: meta.VerifierFeedback,
		MaxRetries:       meta.MaxRetries,
		Complexity:       meta.Complexity,
		RetryCount:       0, // runtime counter, not persisted
		CreatedAt:        meta.CreatedAt,
		WorkerDuration:   meta.WorkerDuration,
		WorkerTokens:     meta.WorkerTokens,
		VerifierDuration: meta.VerifierDuration,
		VerifierTokens:   meta.VerifierTokens,
		WorkerPromptHit:        meta.WorkerPromptHit,
		WorkerPromptMiss:       meta.WorkerPromptMiss,
		WorkerCompletion:       meta.WorkerCompletion,
		VerifierPromptHit:      meta.VerifierPromptHit,
		VerifierPromptMiss:     meta.VerifierPromptMiss,
		VerifierCompletion:     meta.VerifierCompletion,
		ToolCalls:        meta.ToolCalls,
		TopTools:         meta.TopTools,
		Verdict:          meta.Verdict,
		VerifierCapCount: meta.VerifierCapCount,
		ToolWaitSeconds:  meta.ToolWaitSeconds,
	}
}

// ---------------------------------------------------------------------------
// state derivation from files — the ONE source of truth
// ---------------------------------------------------------------------------

// marker files for terminal / explicit states.
var markerFiles = map[string]TaskState{
	"error.md":        TaskStateFailed,
	"suspended.md":    TaskStateSuspended,
	"confirmation.md": TaskStatePendingConfirmation,
}

func (fs *FileTaskStore) deriveState(dir string) TaskState {
	// Check marker files (failed, suspended, pending_confirmation).
	for name, state := range markerFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return state
		}
	}

	hasOutput := fileExists(filepath.Join(dir, "output.md"))
	hasVerify := fileExists(filepath.Join(dir, "verify.md"))
	hasInput := fileExists(filepath.Join(dir, "input.md"))

	if hasOutput && hasVerify {
		return TaskStateDone
	}
	if hasOutput {
		return TaskStateProduced
	}
	if hasInput {
		return TaskStateAssigned
	}
	return TaskStatePending
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	meta := &taskMeta{ID: mt.ID, Title: mt.Goal, Role: "teamleader", Agent: mt.Agent, SessionID: mt.SessionID, LeaderSessionID: mt.LeaderSessionID, WorkspacePath: mt.WorkspacePath, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := fs.writeMeta(dir, meta); err != nil {
		return err
	}
	fs.writeGoal(dir, mt.Goal, "teamleader", mt.Goal, "")

	fs.mu.Lock()
	fs.masters[mt.ID] = &MasterTask{ID: mt.ID, Goal: mt.Goal, Agent: mt.Agent, SessionID: mt.SessionID, LeaderSessionID: mt.LeaderSessionID, WorkspacePath: mt.WorkspacePath, CreatedAt: meta.CreatedAt}
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

func (fs *FileTaskStore) ListMasterTasksBySession(sessionID string) ([]*MasterTask, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	result := make([]*MasterTask, 0)
	for _, mt := range fs.masters {
		if mt.SessionID == sessionID {
			result = append(result, mt)
		}
	}
	return result, nil
}

func (fs *FileTaskStore) DeleteMasterTask(id string) error {
	fs.mu.Lock()
	delete(fs.masters, id)
	fs.mu.Unlock()
	dir := fs.masterDir(id)
	return os.RemoveAll(dir)
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

func (fs *FileTaskStore) CompleteMasterTask(id string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	mt, ok := fs.masters[id]
	if !ok {
		return nil
	}
	mt.Status = "done"
	return nil
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
		ID: task.ID, Title: task.Title, Description: task.Description,
		Role: string(task.Role), Output: task.Output, Workdir: task.Workdir,
		ParentIDs: task.ParentIDs, UpstreamBatches: task.UpstreamBatches, BatchID: task.BatchID, MasterTaskID: task.MasterTaskID,
		VerifierFocus: task.VerifierFocus, MaxRetries: task.MaxRetries, Complexity: task.Complexity,
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
	fs.mu.Lock()
	defer fs.mu.Unlock()
	t, ok := fs.tasks[id]
	if !ok {
		return nil, nil
	}
	fs.refreshState(t)
	return t, nil
}

func (fs *FileTaskStore) ListTasks() ([]*Task, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	result := make([]*Task, 0, len(fs.tasks))
	for _, t := range fs.tasks {
		fs.refreshState(t)
		result = append(result, t)
	}
	return result, nil
}

func (fs *FileTaskStore) ListTasksByMasterTask(masterID string) ([]*Task, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
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
	fs.mu.Lock()
	defer fs.mu.Unlock()
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
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var result []*Task
	for _, t := range fs.tasks {
		fs.refreshState(t)
		if t.State == state {
			result = append(result, t)
		}
	}
	return result, nil
}

// WithReadLock runs fn while holding the store's read lock. Callers that read
// Task fields (State, RetryCount, …) outside a store write method must hold
// this lock so concurrent writers (refreshState, TransitionState, UpdateTask)
// are serialized against the read.
func (fs *FileTaskStore) WithReadLock(fn func()) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	fn()
}

// TaskState returns the derived state of a task as a value, without leaking the
// shared *Task pointer. Callers that only need the state use this instead of
// GetTask so they never read a shared Task field outside the store lock.
func (fs *FileTaskStore) TaskState(id string) TaskState {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	t, ok := fs.tasks[id]
	if !ok {
		return ""
	}
	fs.refreshState(t)
	return t.State
}

// UpstreamOutputs returns named output references for a task's parents and its
// done same-batch siblings. Reads happen under the store lock and the result is
// a value snapshot — no shared *Task pointer escapes, so callers never race with
// refreshState/UpdateTask writers.
func (fs *FileTaskStore) UpstreamOutputs(task *Task) []UpstreamRef {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var refs []UpstreamRef
	seen := make(map[string]bool)
	add := func(t *Task) {
		if t.Output == "" || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		refs = append(refs, UpstreamRef{Name: t.Title, Path: fs.absOutputPath(t)})
	}
	// 父任务（self-split 子任务）。
	for _, pid := range task.ParentIDs {
		if pt, ok := fs.tasks[pid]; ok {
			fs.refreshState(pt)
			add(pt)
		}
	}
	// 上游 batch 的产出（精准）：只收集 task 声明依赖的 batch（UpstreamBatches）
	// 里已完成的任务。无依赖则不看任何同 master 产出，避免把无关任务的文件
	// 路径也塞给 worker 造成信息过载。
	if task.MasterTaskID != "" && len(task.UpstreamBatches) > 0 {
		upstream := make(map[string]bool, len(task.UpstreamBatches))
		for _, bid := range task.UpstreamBatches {
			upstream[bid] = true
		}
		for _, bt := range fs.tasks {
			if bt.MasterTaskID != task.MasterTaskID || bt.ID == task.ID {
				continue
			}
			if !upstream[bt.BatchID] {
				continue
			}
			fs.refreshState(bt)
			if bt.State == TaskStateDone {
				add(bt)
			}
		}
	}
	return refs
}

// absOutputPath returns the absolute path of a task's deliverable in its
// workspace. Workers write deliverables directly into Workdir, so a bare
// relative Output ("game.js") resolves against it for reading upstream files.
func (fs *FileTaskStore) absOutputPath(t *Task) string {
	if t.Workdir == "" {
		return t.Output
	}
	return filepath.Join(t.Workdir, t.Output)
}

// UpdateTask updates structural fields only. State and RetryCount are runtime-only.
func (fs *FileTaskStore) UpdateTask(id string, fields map[string]interface{}) error {
	fs.mu.Lock()
	t, ok := fs.tasks[id]
	if !ok {
		fs.mu.Unlock()
		return nil
	}

	dir := fs.taskDir(id)
	meta, err := fs.readMeta(dir)
	if err != nil {
		fs.mu.Unlock()
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
	if v, ok := fields["output"]; ok {
		meta.Output = fmt.Sprint(v)
		t.Output = meta.Output
	}
	if v, ok := fields["batch_id"]; ok {
		meta.BatchID = fmt.Sprint(v)
		t.BatchID = meta.BatchID
	}
	if v, ok := fields["master_task_id"]; ok {
		meta.MasterTaskID = fmt.Sprint(v)
		t.MasterTaskID = meta.MasterTaskID
	}
	// complexity: leader 的规模判定驱动 worker/verifier 迭代预算（30 轮 vs
	// 80 轮）。此前只在内存里赋值、从未落盘，任务重读后预算全部回落到
	// complex 档——42 轮的调试螺旋因此没有任何预算拦截（2048 实测）。
	if v, ok := fields["complexity"]; ok {
		meta.Complexity = fmt.Sprint(v)
		t.Complexity = meta.Complexity
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
	// retry_count: runtime only, update in-memory but don't persist.
	if v, ok := fields["retry_count"]; ok {
		switch n := v.(type) {
		case int:
			t.RetryCount = n
		case float64:
			t.RetryCount = int(n)
		}
	}
	// session_id: persistence-only field (not on Task struct).
	if v, ok := fields["session_id"]; ok {
		meta.SessionID = fmt.Sprint(v)
	}
	// 运行统计：累加语义——同一任务的多轮执行（重试、多 cycle）都要计入，
	// 这样 run_report / RUN SUMMARY 的 per-task token 才是总账；旧实现把
	// 这四个 key 挡在白名单外静默丢弃，导致并行批次下只能靠计数器 delta
	// 交叉估算（v32 实测虚高 61%）。
	if v, ok := fields["worker_duration_seconds"]; ok {
		if f, ok := v.(float64); ok {
			meta.WorkerDuration += f
			t.WorkerDuration = meta.WorkerDuration
		}
	}
	if v, ok := fields["worker_tokens"]; ok {
		switch n := v.(type) {
		case int:
			meta.WorkerTokens += n
		case float64:
			meta.WorkerTokens += int(n)
		}
		t.WorkerTokens = meta.WorkerTokens
	}
	if v, ok := fields["verifier_duration_seconds"]; ok {
		if f, ok := v.(float64); ok {
			meta.VerifierDuration += f
			t.VerifierDuration = meta.VerifierDuration
		}
	}
	if v, ok := fields["verifier_tokens"]; ok {
		switch n := v.(type) {
		case int:
			meta.VerifierTokens += n
		case float64:
			meta.VerifierTokens += int(n)
		}
		t.VerifierTokens = meta.VerifierTokens
	}
	// 缓存分账（与 worker_tokens/verifier_tokens 相同的累加语义）：raw 数字
	// 里 90%+ 是前缀缓存命中（~1/31 价），run_report 需要 hit/miss 分账才能
	// 算出真实成本，而不是重放体积。
	for _, f := range []struct {
		key  string
		dst  func(v int)
	}{
		{"worker_prompt_hit", func(v int) { meta.WorkerPromptHit += v; t.WorkerPromptHit = meta.WorkerPromptHit }},
		{"worker_prompt_miss", func(v int) { meta.WorkerPromptMiss += v; t.WorkerPromptMiss = meta.WorkerPromptMiss }},
		{"worker_completion", func(v int) { meta.WorkerCompletion += v; t.WorkerCompletion = meta.WorkerCompletion }},
		{"verifier_prompt_hit", func(v int) { meta.VerifierPromptHit += v; t.VerifierPromptHit = meta.VerifierPromptHit }},
		{"verifier_prompt_miss", func(v int) { meta.VerifierPromptMiss += v; t.VerifierPromptMiss = meta.VerifierPromptMiss }},
		{"verifier_completion", func(v int) { meta.VerifierCompletion += v; t.VerifierCompletion = meta.VerifierCompletion }},
	} {
		if v, ok := fields[f.key]; ok {
			switch n := v.(type) {
			case int:
				f.dst(n)
			case float64:
				f.dst(int(n))
			}
		}
	}
	// 工具探针：tool_calls 累加（多轮 worker 都要计入）；top_tools 覆写
	// （run_task 侧已聚合为累计直方图 Top2）。
	if v, ok := fields["tool_calls"]; ok {
		switch n := v.(type) {
		case int:
			meta.ToolCalls += n
		case float64:
			meta.ToolCalls += int(n)
		}
		t.ToolCalls = meta.ToolCalls
	}
	if v, ok := fields["top_tools"]; ok {
		meta.TopTools = fmt.Sprint(v)
		t.TopTools = meta.TopTools
	}
	// 事后分析轨迹：verdict 覆写；cap 次数/工具等待累加（多轮都要计入）。
	if v, ok := fields["verdict"]; ok {
		meta.Verdict = fmt.Sprint(v)
		t.Verdict = meta.Verdict
	}
	if v, ok := fields["verifier_cap_count"]; ok {
		switch n := v.(type) {
		case int:
			meta.VerifierCapCount += n
		case float64:
			meta.VerifierCapCount += int(n)
		}
		t.VerifierCapCount = meta.VerifierCapCount
	}
	if v, ok := fields["tool_wait_seconds"]; ok {
		if f, ok := v.(float64); ok {
			meta.ToolWaitSeconds += f
			t.ToolWaitSeconds = meta.ToolWaitSeconds
		}
	}
	fs.mu.Unlock()
	return fs.writeMeta(dir, meta)
}

// TransitionState updates the in-memory state and writes/clears marker files.
// Terminal states (failed, suspended, pending_confirmation) write marker files.
// Non-terminal states (pending, assigned, producing, etc.) clear terminal markers
// so deriveState works correctly from output files.
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

	fs.applyStateMarkers(dir, newState)
	return nil
}

// ForceTransitionState bypasses the valid-transitions table.
// Used for admin actions (cancel, cleanup, resume).
func (fs *FileTaskStore) ForceTransitionState(id string, newState TaskState, _ string) error {
	fs.mu.Lock()
	if t, ok := fs.tasks[id]; ok {
		t.State = newState
	}
	fs.mu.Unlock()

	dir := fs.findTaskDir(id)
	if dir == "" {
		return nil
	}

	fs.applyStateMarkers(dir, newState)
	return nil
}

// applyStateMarkers writes or clears marker files to match the target state.
func (fs *FileTaskStore) applyStateMarkers(dir string, newState TaskState) {
	// Always clear terminal markers when moving to a non-terminal state.
	switch newState {
	case TaskStateFailed:
		os.WriteFile(filepath.Join(dir, "error.md"), []byte("task failed"), 0644)
		os.Remove(filepath.Join(dir, "suspended.md"))
		os.Remove(filepath.Join(dir, "confirmation.md"))
	case TaskStateSuspended:
		os.WriteFile(filepath.Join(dir, "suspended.md"), []byte("task suspended"), 0644)
		os.Remove(filepath.Join(dir, "error.md"))
		os.Remove(filepath.Join(dir, "confirmation.md"))
	case TaskStatePendingConfirmation:
		os.WriteFile(filepath.Join(dir, "confirmation.md"), []byte("pending confirmation"), 0644)
		os.Remove(filepath.Join(dir, "error.md"))
		os.Remove(filepath.Join(dir, "suspended.md"))
	case TaskStateDone:
		os.Remove(filepath.Join(dir, "error.md"))
		os.Remove(filepath.Join(dir, "suspended.md"))
		os.Remove(filepath.Join(dir, "confirmation.md"))
	default:
		// Non-terminal: clear all terminal markers.
		os.Remove(filepath.Join(dir, "error.md"))
		os.Remove(filepath.Join(dir, "suspended.md"))
		os.Remove(filepath.Join(dir, "confirmation.md"))
	}
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
// compatibility — no-op methods retained for callers that haven't been updated
// ---------------------------------------------------------------------------

func (fs *FileTaskStore) Close() error          { return nil }
func (fs *FileTaskStore) Checkpoint() error     { return nil }
func (fs *FileTaskStore) checkpointAfterWrite() {}

// GetTaskHistory reconstructs the state-transition timeline from file
// modification times.  Each milestone file (input.md, output.md, verify.md,
// error.md, suspended.md, confirmation.md) is mapped to the corresponding
// state, sorted by mtime, and returned as an ordered history.
func (fs *FileTaskStore) GetTaskHistory(taskID string) ([]StateHistoryEntry, error) {
	dir := fs.findTaskDir(taskID)
	if dir == "" {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	type fileEvent struct {
		file  string
		mtime time.Time
		state TaskState
	}

	var events []fileEvent

	// Milestone files => state mapping.
	addEvent := func(filename string, state TaskState) {
		if fi, err := os.Stat(filepath.Join(dir, filename)); err == nil {
			events = append(events, fileEvent{filename, fi.ModTime(), state})
		}
	}

	addEvent("input.md", TaskStateAssigned)
	addEvent("output.md", TaskStateProduced)
	addEvent("verify.md", TaskStateVerified)
	// verifier.md is the verifier's report, written on both PASS and FAIL — it
	// is not a verified marker and must not be mapped to TaskStateVerified.
	addEvent("error.md", TaskStateFailed)
	addEvent("suspended.md", TaskStateSuspended)
	addEvent("confirmation.md", TaskStatePendingConfirmation)

	if len(events) == 0 {
		return nil, nil
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].mtime.Before(events[j].mtime)
	})

	// Read meta.json for the creation timestamp.
	var createdAt string
	if meta, err := fs.readMeta(dir); err == nil && meta != nil {
		createdAt = meta.CreatedAt
	}

	history := []StateHistoryEntry{
		{
			TaskID:    taskID,
			OldState:  "",
			NewState:  string(TaskStatePending),
			Reason:    "task created",
			ChangedAt: createdAt,
			Timestamp: createdAt,
		},
	}

	prevState := string(TaskStatePending)
	for _, evt := range events {
		entry := StateHistoryEntry{
			TaskID:    taskID,
			OldState:  prevState,
			NewState:  string(evt.state),
			ChangedAt: evt.mtime.Format(time.RFC3339),
			Timestamp: evt.mtime.Format(time.RFC3339),
		}
		// Derive transition reason from the file name.
		switch evt.file {
		case "error.md":
			entry.Reason = "task failed"
		case "suspended.md":
			entry.Reason = "task suspended"
		case "confirmation.md":
			entry.Reason = "pending confirmation"
		}
		history = append(history, entry)
		prevState = string(evt.state)
	}

	return history, nil
}

// UpdateMasterTaskStatus updates the master task status in memory.
func (fs *FileTaskStore) UpdateMasterTaskStatus(id, status string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if mt, ok := fs.masters[id]; ok {
		mt.Status = status
	}
	return nil
}

// SaveMasterTaskLeaderSession persists the Leader subagent session ID for a
// master task, both in memory and in the master meta.json, so any engine
// instance (CLI command, agent tool, session picker) can address the Leader
// session later (continuation turns, user conversations).
func (fs *FileTaskStore) SaveMasterTaskLeaderSession(id, sessionID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	mt, ok := fs.masters[id]
	if !ok {
		return nil
	}
	mt.LeaderSessionID = sessionID
	dir := fs.masterDir(id)
	if meta, err := fs.readMeta(dir); err == nil {
		meta.LeaderSessionID = sessionID
		_ = fs.writeMeta(dir, meta)
	}
	return nil
}

// ---------------------------------------------------------------------------
// memory
// ---------------------------------------------------------------------------

const maxMemoriesPerRole = 50

func (fs *FileTaskStore) SaveMemory(memory *MemoryEntry) error {
	memDir := filepath.Join(fs.baseDir, "memory")
	os.MkdirAll(memDir, 0755)
	data, _ := json.Marshal(memory)
	name := fmt.Sprintf("%s_%s_%d.json", memory.AgentRole, memory.Key, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(memDir, name), data, 0644); err != nil {
		return err
	}

	// Eviction: keep at most maxMemoriesPerRole entries per role.
	entries, _ := os.ReadDir(memDir)
	prefix := memory.AgentRole + "_"
	var roleEntries []os.DirEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".json") {
			roleEntries = append(roleEntries, e)
		}
	}
	if len(roleEntries) > maxMemoriesPerRole {
		// Sort by name (contains timestamp), oldest first.
		sort.Slice(roleEntries, func(i, j int) bool {
			return roleEntries[i].Name() < roleEntries[j].Name()
		})
		for _, e := range roleEntries[:len(roleEntries)-maxMemoriesPerRole] {
			os.Remove(filepath.Join(memDir, e.Name()))
		}
	}
	return nil
}

func (fs *FileTaskStore) GetMemories(role AgentRole, key string) ([]MemoryEntry, error) {
	memDir := filepath.Join(fs.baseDir, "memory")
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil, nil
	}
	// Match on the decoded Key field, not the filename prefix. Filenames embed
	// the raw key, so a prefix match on "game" would wrongly also return keys
	// like "game_logic" or "game2".
	var memories []MemoryEntry
	prefix := string(role) + "_"
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(memDir, e.Name()))
		var mem MemoryEntry
		if json.Unmarshal(data, &mem) != nil {
			continue
		}
		if mem.Key == key {
			memories = append(memories, mem)
		}
	}
	return memories, nil
}

// HasMemory checks whether a memory with the same role, key, and content
// already exists.  Used to avoid saving duplicate lessons.
func (fs *FileTaskStore) HasMemory(role AgentRole, key, content string) bool {
	existing, _ := fs.GetMemories(role, key)
	for _, m := range existing {
		if m.Content == content {
			return true
		}
	}
	return false
}

// GetRecentMemories returns the most recent N memories for a role,
// sorted by CreatedAt descending (newest first).  When limit is 0,
// all memories are returned.
func (fs *FileTaskStore) GetRecentMemories(role AgentRole, limit int) ([]MemoryEntry, error) {
	memDir := filepath.Join(fs.baseDir, "memory")
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil, nil
	}
	var memories []MemoryEntry
	prefix := string(role) + "_"
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".json") {
			data, _ := os.ReadFile(filepath.Join(memDir, e.Name()))
			var mem MemoryEntry
			if json.Unmarshal(data, &mem) == nil {
				memories = append(memories, mem)
			}
		}
	}
	sort.Slice(memories, func(i, j int) bool {
		return memories[i].CreatedAt > memories[j].CreatedAt
	})
	if limit > 0 && len(memories) > limit {
		memories = memories[:limit]
	}
	return memories, nil
}

// MemoryStats returns a count of memories per role.
// Filename format: {role}_{key}_{timestamp}.json — role is extracted by
// finding the longest prefix that matches a known role set.  This correctly
// handles role names containing underscores (e.g. "QA_Automation_Engineer").
func (fs *FileTaskStore) MemoryStats() map[string]int {
	memDir := filepath.Join(fs.baseDir, "memory")
	entries, _ := os.ReadDir(memDir)
	stats := make(map[string]int)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		role := extractRoleFromFilename(e.Name())
		if role != "" {
			stats[role]++
		}
	}
	return stats
}

// extractRoleFromFilename extracts the role from a memory filename of the
// form {role}_{key}_{timestamp}.json.  It works by scanning from the right:
// the timestamp is the last component (all digits), the key is between the
// last two underscores, and everything before that is the role.
func extractRoleFromFilename(name string) string {
	name = strings.TrimSuffix(name, ".json")
	// Find the timestamp: last "_" segment is the nanosecond timestamp (all digits).
	lastIdx := strings.LastIndex(name, "_")
	if lastIdx < 0 {
		return ""
	}
	rest := name[:lastIdx]
	// Find the key: the segment before the timestamp.
	keyIdx := strings.LastIndex(rest, "_")
	if keyIdx < 0 {
		return ""
	}
	return rest[:keyIdx]
}
