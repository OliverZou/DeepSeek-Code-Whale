// Package team_engine implements a state-machine driven multi-agent
// orchestration runtime — the Leader-Worker-Verifier collaboration model
// described in the MiniMax Agent Team paper.
//
// Unlike the original team-engine-go which calls external agent CLIs
// (Claude Code, OpenCode, Codex), this Whale integration uses Whale's
// own subagent spawning mechanism for all three roles, giving full
// control over agent behavior, tool permissions, and execution context.
package team_engine

import "time"

// TaskState enumerates the states a task can be in.
type TaskState string

const (
	TaskStatePending   TaskState = "pending"
	TaskStateAssigned  TaskState = "assigned"
	TaskStateProducing TaskState = "producing"
	TaskStateProduced  TaskState = "produced"
	TaskStateVerifying TaskState = "verifying"
	TaskStateVerified  TaskState = "verified"
	TaskStateFailed     TaskState = "failed"
	TaskStateDone       TaskState = "done"
	TaskStateSuspended  TaskState = "suspended"
)

// IsTerminal reports whether the state is a terminal state
// (no further transitions allowed). Suspended is NOT terminal—
// it can be resumed.
func (s TaskState) IsTerminal() bool {
	return s == TaskStateFailed || s == TaskStateDone
}

// IsResumable reports whether the task can be resumed.
// Only suspended tasks can be resumed.
func (s TaskState) IsResumable() bool {
	return s == TaskStateSuspended
}

// AgentRole is the role an agent plays in the collaboration.
type AgentRole string

const (
	RoleDeveloper  AgentRole = "developer"
	RoleTester     AgentRole = "tester"
	RoleReviewer   AgentRole = "reviewer"
	RoleResearcher AgentRole = "researcher"
	RoleWriter     AgentRole = "writer"
	RoleFormatter   AgentRole = "formatter"
	RoleEvaluator   AgentRole = "evaluator"
	RoleSynthesizer AgentRole = "synthesizer" // 场景3: 合并多条研究结论
)

// ToolProfile defines the set of capabilities assigned to a role.
// Instead of selecting an external CLI backend (cc/opencode/codex),
// it selects a tool permission profile for the Whale subagent.
type ToolProfile string

const (
	// ProfileDefault — full read/write/shell access (default Worker).
	ProfileDefault ToolProfile = "default"
	// ProfileReadOnly — read-only inspection tools only (Reviewer).
	ProfileReadOnly ToolProfile = "read_only"
	// ProfileResearch — read + web search/fetch (Researcher).
	ProfileResearch ToolProfile = "research"
	// ProfileContent — read + write, no shell (Writer/Formatter).
	ProfileContent ToolProfile = "content"
	// ProfileTest — read/write/shell focused on test execution (Tester).
	ProfileTest ToolProfile = "test"
	// ProfileVerify — read + shell (no write) for tool-grounded verification.
	// Verifier can run go test, linter, format check but cannot modify code.
	ProfileVerify ToolProfile = "verify"
)

// Task is a single unit of work in the Team Engine pipeline.
type Task struct {
	ID               string     `json:"id"`                // UUID
	Title            string     `json:"title"`             // 任务标题
	Description      string     `json:"description"`       // 任务描述（给 Agent 的 prompt）
	Role             AgentRole  `json:"role"`              // 角色
	Profile          ToolProfile `json:"profile"`          // 工具权限配置
	State            TaskState  `json:"state"`             // 当前状态
	MaxRetries       int        `json:"max_retries"`       // 最大重试次数
	RetryCount       int        `json:"retry_count"`       // 已重试次数
	Workdir          string     `json:"workdir"`           // 工作目录
	ParentIDs        []string   `json:"parent_ids"`        // 依赖的上游任务
	ArtifactPath     string     `json:"artifact_path"`     // 产出文件路径（白板）
	VerifierFeedback string     `json:"verifier_feedback"` // Verifier 反馈
	VerifierFocus    string     `json:"verifier_focus"`    // 验证重点 (correctness,security,sources,plausibility,...)
	BatchID          string     `json:"batch_id"`          // 所属 Batch（stage）
 	MasterTaskID     string     `json:"master_task_id"`    // 所属总任务
	CreatedAt        string     `json:"created_at"`        // ISO 8601
	UpdatedAt        string     `json:"updated_at"`        // ISO 8601
}

// BatchStatus enumerates the states a task batch can be in.
type BatchStatus string

const (
	BatchStatusPending BatchStatus = "pending"
	BatchStatusRunning BatchStatus = "running"
	BatchStatusPassed  BatchStatus = "passed"
	BatchStatusFailed  BatchStatus = "failed"
)

// Batch is a group of tasks that run in parallel within a stage.
// Batches within the same stage can run concurrently; batches across stages
// have depends_on relationships.
type Batch struct {
	ID          string       `json:"id"`
	Label       string       `json:"label,omitempty"` // e.g. "文献调研批"
	Tasks       []*Task      `json:"tasks"`
	DependsOn   []string     `json:"depends_on"`   // batch IDs this batch depends on
	Status      BatchStatus  `json:"status"`
	Concurrency int          `json:"concurrency"`  // max parallel tasks (0 = all)
	MaxCycles   int          `json:"max_cycles"`   // 0 = unlimited
	CycleCount  int          `json:"cycle_count"`
}

// PlanTask is a single subtask in the leader's decomposition plan, returned
// as JSON by the planning agent.
type PlanTask struct {
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	Role             string   `json:"role"`
	BatchID          string   `json:"batch_id,omitempty"`     // which batch (stage) this belongs to
	BatchLabel       string   `json:"batch_label,omitempty"`  // human label for the batch
	DependsOnBatch   []string `json:"depends_on_batch,omitempty"` // batch dependencies
	DependsOnIndex   int      `json:"depends_on_index"`
	DependsOnIndices []int    `json:"depends_on_indices,omitempty"`
	Profile          string   `json:"profile,omitempty"`
	VerifierFocus    string   `json:"verifier_focus,omitempty"`
	Concurrency      int      `json:"concurrency,omitempty"`   // per-batch override
	MaxCycles        int      `json:"max_cycles,omitempty"`     // per-batch override
}

// NewTask creates a Task with sensible defaults and auto-generated
// timestamps.
func NewTask(id, title, description string, role AgentRole, profile ToolProfile, maxRetries int, workdir string, parentIDs []string, batchID, masterTaskID string) *Task {
	now := time.Now().UTC().Format(time.RFC3339)
	if maxRetries <= 0 {
		maxRetries = 9
	}
	if profile == "" {
		profile = ProfileDefault
	}
	if parentIDs == nil {
		parentIDs = []string{}
	}
	return &Task{
		ID:            id,
		Title:         title,
		Description:   description,
		Role:          role,
		Profile:       profile,
		State:         TaskStatePending,
		MaxRetries:    maxRetries,
		RetryCount:    0,
		Workdir:       workdir,
		ParentIDs:     parentIDs,
		BatchID:       batchID,
		MasterTaskID:  masterTaskID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// ValidTransitions is the legal state-transition table.
//
// Terminal states (failed, done) have empty transition lists.
// Backward transitions (Verified→Assigned, Produced→Assigned, Verifying→Assigned)
// support CycleReject — the Leader may reject a batch and request retries even
// after individual task verification passed.
var ValidTransitions = map[TaskState][]TaskState{
	TaskStatePending:   {TaskStateAssigned, TaskStateSuspended, TaskStateFailed},
	TaskStateAssigned:  {TaskStateProducing, TaskStateSuspended, TaskStateFailed},
	TaskStateProducing: {TaskStateProduced, TaskStateSuspended, TaskStateFailed},
	TaskStateProduced:  {TaskStateVerifying, TaskStateDone, TaskStateAssigned, TaskStateSuspended, TaskStateFailed},
	TaskStateVerifying: {TaskStateVerified, TaskStateProducing, TaskStateSuspended, TaskStateAssigned, TaskStateFailed},
	TaskStateVerified:  {TaskStateDone, TaskStateSuspended, TaskStateAssigned, TaskStateFailed},
	TaskStateSuspended: {TaskStatePending}, // resume
	TaskStateFailed:    {}, // terminal
	TaskStateDone:      {}, // terminal
}

// CanTransition reports whether a transition from oldState to newState is
// legal according to ValidTransitions.
func CanTransition(oldState, newState TaskState) bool {
	for _, allowed := range ValidTransitions[oldState] {
		if allowed == newState {
			return true
		}
	}
	return false
}

// ContentRoles lists roles that produce natural-language output rather than
// code/artifacts.
var ContentRoles = map[AgentRole]bool{
	RoleResearcher:  true,
	RoleWriter:      true,
	RoleFormatter:   true,
	RoleEvaluator:   true,
	RoleSynthesizer: true,
}

// IsContentRole reports whether this role produces natural-language output
// rather than code/artifacts.
func (r AgentRole) IsContentRole() bool {
	return ContentRoles[r]
}

// LabelOrID returns the batch label if set, otherwise the batch ID.
func (b *Batch) LabelOrID() string {
	if b.Label != "" {
		return b.Label
	}
	return b.ID
}

// ---------------------------------------------------------------------------
// CycleReport — 每 Batch 完成后向 Leader 的周期性汇报
// ---------------------------------------------------------------------------

// CycleDecision is the Leader's response to a CycleReport.
type CycleDecision string

const (
	CycleAccept   CycleDecision = "accept"    // 接受结果，继续下一 Batch
	CycleReject   CycleDecision = "reject"    // 拒绝，重试当前 Batch
	CycleEscalate CycleDecision = "escalate"  // 需要用户介入
	CycleEscalated CycleDecision = "escalated" // 升级策略后重试（换模型/调参数）
)

// TaskSummary is a lightweight view of a task for inclusion in reports.
type TaskSummary struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Role        string     `json:"role"`
	State       TaskState  `json:"state"`
	RetryCount  int        `json:"retry_count"`
	OutputBrief string     `json:"output_brief,omitempty"`
}

// CycleReport is the periodic report sent from Engine to Leader after
// each batch completes.
type CycleReport struct {
	BatchID      string         `json:"batch_id"`
	BatchLabel   string         `json:"batch_label,omitempty"`
	CycleNumber  int            `json:"cycle_number"`   // 第几轮（重试次数）
	Status       BatchStatus    `json:"status"`
	Tasks        []TaskSummary  `json:"tasks"`
	BoardPath    string         `json:"board_path,omitempty"`
	Deliverable  string         `json:"deliverable_path,omitempty"`
}

// CycleReview is the Leader's structured response to a CycleReport.
type CycleReview struct {
	Decision    CycleDecision `json:"decision"`
	Reason      string        `json:"reason"`
	Feedback    string        `json:"feedback,omitempty"`    // 给 Worker 的改进意见
	PlanChanges string        `json:"plan_changes,omitempty"` // 如果需要改计划
}

// ---------------------------------------------------------------------------
// TaskEvent — 主动事件推送，替代轮询
// ---------------------------------------------------------------------------

// TaskEventType 标识事件种类
type TaskEventType int

const (
	// EventStateChanged — 任务状态转换（pending→assigned→producing→...）
	EventStateChanged TaskEventType = iota
	// EventWorkerOutput — Worker 产出了一段输出（流式）
	EventWorkerOutput
	// EventVerifierResult — Verifier 得出了结论（PASS/FAIL）
	EventVerifierResult
	// EventTaskDone — 任务最终完成（done 或 failed）
	EventTaskDone
)

// TaskEvent 是 engine 向监听者推送的事件
type TaskEvent struct {
	Type     TaskEventType `json:"type"`
	TaskID   string        `json:"task_id"`
	Title    string        `json:"title,omitempty"`
	OldState string        `json:"old_state,omitempty"`
	NewState string        `json:"new_state,omitempty"`
	Data     string        `json:"data,omitempty"`
	Progress int           `json:"progress"`
}

// TaskEventCallback 是事件监听函数
type TaskEventCallback func(event TaskEvent)

// AllStates returns every TaskState in declaration order.
func AllStates() []TaskState {
	return []TaskState{
		TaskStatePending,
		TaskStateAssigned,
		TaskStateProducing,
		TaskStateProduced,
		TaskStateVerifying,
		TaskStateVerified,
		TaskStateFailed,
		TaskStateSuspended,
		TaskStateDone,
	}
}
