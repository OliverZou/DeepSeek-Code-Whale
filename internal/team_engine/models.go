// Package team_engine implements a state-machine driven multi-agent
// orchestration runtime — the Leader-Worker-Verifier collaboration model
// described in the MiniMax Agent Team paper.
//
// Unlike the original team-engine-go which calls external agent CLIs
// (Claude Code, OpenCode, Codex), this Whale integration uses Whale's
// own subagent spawning mechanism for all three roles, giving full
// control over agent behavior, tool permissions, and execution context.
package team_engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// TaskState enumerates the states a task can be in.
type TaskState string

const (
	TaskStatePending             TaskState = "pending"
	TaskStateAssigned            TaskState = "assigned"
	TaskStateProducing           TaskState = "producing"
	TaskStateProduced            TaskState = "produced"
	TaskStateVerifying           TaskState = "verifying"
	TaskStateVerified            TaskState = "verified"
	TaskStateFailed              TaskState = "failed"
	TaskStateDone                TaskState = "done"
	TaskStateSuspended           TaskState = "suspended"
	TaskStatePendingConfirmation TaskState = "pending_confirmation"
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
	return s == TaskStateSuspended || s == TaskStatePendingConfirmation
}

// SystemVerifierAgentName is the name of the system-provided verifier agent.
// Teams never define a verifier role: the verifier is always the embedded
// system definition, which carries the generic-task verification methodology.
const SystemVerifierAgentName = "verifier"

// AgentRole is the role an agent plays in the collaboration.
type AgentRole string

const (
	RoleDeveloper   AgentRole = "developer"
	RoleTester      AgentRole = "tester"
	RoleReviewer    AgentRole = "reviewer"
	RoleResearcher  AgentRole = "researcher"
	RoleWriter      AgentRole = "writer"
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
	ID                 string      `json:"id"`                            // UUID
	Title              string      `json:"title"`                         // 任务标题
	Description        string      `json:"description"`                   // 任务描述（给 Agent 的 prompt）
	Output             string      `json:"output,omitempty"`              // 声明产出（不进DB，运行时传递）
	Role               AgentRole   `json:"role"`                          // 角色
	VerifierRole       string      `json:"verifier_role,omitempty"`       // 验证者角色（agent name）— deprecated：验证者恒为系统 verifier
	VerifyMode         string      `json:"verify_mode,omitempty"`         // 验证深度：""=auto / "mechanical" / "semantic"（Leader 分解时判定）
	AcceptanceCriteria []string    `json:"acceptance_criteria,omitempty"` // 验收标准（Worker 自检 + Verifier 验收共用）
	Profile            ToolProfile `json:"profile"`                       // 工具权限配置
	State              TaskState   `json:"state"`                         // 当前状态
	MaxRetries         int         `json:"max_retries"`                   // 最大重试次数
	RetryCount         int         `json:"retry_count"`                   // 已重试次数
	Workdir            string      `json:"workdir"`                       // 工作目录
	ParentIDs          []string    `json:"parent_ids"`                    // 依赖的上游任务
	UpstreamBatches    []string    `json:"upstream_batches,omitempty"`    // 所在 batch 依赖的上游 batch（精准注入上游产出）
	ArtifactPath       string      `json:"artifact_path"`                 // 产出文件路径（白板）
	VerifierFeedback   string      `json:"verifier_feedback"`             // Verifier 反馈
	VerifierFocus      string      `json:"verifier_focus"`                // 验证重点 (correctness,security,sources,plausibility,...)
	Complexity         string      `json:"complexity,omitempty"`          // 目标规模提示 simple/medium/complex（驱动 worker/verifier 迭代预算）
	BatchID            string      `json:"batch_id"`                      // 所属 Batch（stage）
	MasterTaskID       string      `json:"master_task_id"`                // 所属总任务
	UseDW              bool        `json:"use_dw"`                        // use Dynamic Workflow for verification
	CreatedAt          string      `json:"created_at"`                    // ISO 8601
	UpdatedAt          string      `json:"updated_at"`                    // ISO 8601
	// 运行统计(监控/事后分析):累计耗时与 token（含重试轮；由 UpdateTask 累加）。
	WorkerDuration   float64 `json:"worker_duration_seconds,omitempty"`
	WorkerTokens     int     `json:"worker_tokens,omitempty"`
	VerifierDuration float64 `json:"verifier_duration_seconds,omitempty"`
	VerifierTokens   int     `json:"verifier_tokens,omitempty"`
	// 缓存分账（DeepSeek 前缀缓存，hit ≈ 1/31 价）：run_report 从 store 重建
	// 全量账单的持久字段（leader 侧与 execute 侧共享的事实源）。
	WorkerPromptHit    int `json:"worker_prompt_hit,omitempty"`
	WorkerPromptMiss   int `json:"worker_prompt_miss,omitempty"`
	WorkerCompletion   int `json:"worker_completion,omitempty"`
	VerifierPromptHit  int `json:"verifier_prompt_hit,omitempty"`
	VerifierPromptMiss int `json:"verifier_prompt_miss,omitempty"`
	VerifierCompletion int `json:"verifier_completion,omitempty"`
	// 工具探针：worker 会话的进度事件计数与最重工具 Top2（如 "bash:18,read:9"），
	// 让事后分析直接看出「哪个任务在烧轮数/卡在哪个工具」。
	ToolCalls int    `json:"tool_calls,omitempty"`
	TopTools  string `json:"top_tools,omitempty"`
	// 事后分析：验证结论/验证者 cap 次数/工具等待时长。
	Verdict          string  `json:"verdict,omitempty"`
	VerifierCapCount int     `json:"verifier_cap_count,omitempty"`
	ToolWaitSeconds  float64 `json:"tool_wait_seconds,omitempty"`
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
	ID            string      `json:"id"`
	Label         string      `json:"label,omitempty"` // e.g. "文献调研批"
	Tasks         []*Task     `json:"tasks"`
	DependsOn     []string    `json:"depends_on"` // batch IDs this batch depends on
	Status        BatchStatus `json:"status"`
	Concurrency   int         `json:"concurrency"` // max parallel tasks (0 = all)
	MaxCycles     int         `json:"max_cycles"`  // 0 = unlimited
	CycleCount    int         `json:"cycle_count"`
	UseDW         bool        `json:"use_dw"`         // use DW pipeline execution (multi-verifier per task, no Leader review)
	TotalTokens   int         `json:"total_tokens"`   // cumulative prompt+completion across all cycles
	TotalDuration float64     `json:"total_duration"` // cumulative seconds across all cycles
}

// Finding is a structured issue found by the Verifier.  Used for
// loop-until-dry: the engine compares findings across cycles; when no
// new findings appear, the batch is "dry" and the cycle ends.
type Finding struct {
	ID       string `json:"id"`       // stable key for dedup (e.g. "prd-s3-missing")
	Title    string `json:"title"`    // one-line summary
	Severity string `json:"severity"` // "critical" | "major" | "minor"
	Evidence string `json:"evidence"` // specific file/line/reason
}

// CycleFindingsSet holds the deduplicated findings from one batch cycle.
type CycleFindingsSet struct {
	Cycle    int       `json:"cycle"`
	Findings []Finding `json:"findings"`
}

// HasNewFindings reports whether this cycle produced any findings not in prev.
func (c *CycleFindingsSet) HasNewFindings(prev *CycleFindingsSet) bool {
	if prev == nil {
		return len(c.Findings) > 0
	}
	seen := make(map[string]bool)
	for _, f := range prev.Findings {
		seen[f.ID] = true
	}
	for _, f := range c.Findings {
		if !seen[f.ID] {
			return true
		}
	}
	return false
}

// FlexibleStringSlice 用于解析 planner 输出的数组字段：LLM 可能输出数字数组
// （如 depends_on_batch: [1,2]）或字符串数组（["1","2"]），统一归一为字符串。
type FlexibleStringSlice []string

// UnmarshalJSON 先尝试字符串数组，失败再尝试数字数组（转字符串）。
func (s *FlexibleStringSlice) UnmarshalJSON(data []byte) error {
	var strs []string
	if err := json.Unmarshal(data, &strs); err == nil {
		*s = strs
		return nil
	}
	var ints []int
	if err := json.Unmarshal(data, &ints); err == nil {
		out := make([]string, len(ints))
		for i, n := range ints {
			out[i] = strconv.Itoa(n)
		}
		*s = out
		return nil
	}
	return fmt.Errorf("expected array of string or int, got %s", string(data))
}

// PlanTask is a single subtask in the leader's decomposition plan, returned
// as JSON by the planning agent.
type PlanTask struct {
	Title              string              `json:"title"`
	Description        string              `json:"description"`
	Output             string              `json:"output,omitempty"` // declared deliverable
	Role               string              `json:"role"`
	VerifierRole       string              `json:"verifier_role,omitempty"`       // deprecated — replaced by verify_mode
	VerifyMode         string              `json:"verify_mode,omitempty"`         // ""=auto / "mechanical" / "semantic"（Leader 分解时判定）
	AcceptanceCriteria []string            `json:"acceptance_criteria,omitempty"` // 验收标准（Worker 自检 + Verifier 验收共用）
	BatchID            string              `json:"batch_id,omitempty"`            // which batch (stage) this belongs to
	BatchLabel         string              `json:"batch_label,omitempty"`         // human label for the batch
	DependsOnBatch     FlexibleStringSlice `json:"depends_on_batch,omitempty"`    // batch dependencies
	DependsOnIndex     int                 `json:"depends_on_index"`
	DependsOnIndices   []int               `json:"depends_on_indices,omitempty"`
	Profile            string              `json:"profile,omitempty"`
	VerifierFocus      string              `json:"verifier_focus,omitempty"`
	UseDW              bool                `json:"use_dw"`                // enable multi-verifier Dynamic Workflow
	Concurrency        int                 `json:"concurrency,omitempty"` // per-batch override
	MaxCycles          int                 `json:"max_cycles,omitempty"`  // per-batch override
}

// NewTask creates a Task with sensible defaults and auto-generated
// timestamps.
func NewTask(id, title, description string, role AgentRole, profile ToolProfile, maxRetries int, workdir string, parentIDs []string, batchID, masterTaskID string) *Task {
	now := time.Now().UTC().Format(time.RFC3339)
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if profile == "" {
		profile = ProfileDefault
	}
	if parentIDs == nil {
		parentIDs = []string{}
	}
	return &Task{
		ID:           id,
		Title:        title,
		Description:  description,
		Role:         role,
		Profile:      profile,
		State:        TaskStatePending,
		MaxRetries:   maxRetries,
		RetryCount:   0,
		Workdir:      workdir,
		ParentIDs:    parentIDs,
		BatchID:      batchID,
		MasterTaskID: masterTaskID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// ValidTransitions is the legal state-transition table.
//
// Terminal states (failed, done) have empty transition lists.
// Backward transitions (Verified→Assigned, Produced→Assigned, Verifying→Assigned)
// support CycleReject — the Leader may reject a batch and request retries even
// after individual task verification passed.
var ValidTransitions = map[TaskState][]TaskState{
	TaskStatePending:             {TaskStateAssigned, TaskStateSuspended, TaskStateFailed},
	TaskStateAssigned:            {TaskStateProducing, TaskStateSuspended, TaskStateFailed},
	TaskStateProducing:           {TaskStateProduced, TaskStateSuspended, TaskStateFailed},
	TaskStateProduced:            {TaskStateVerifying, TaskStateDone, TaskStateAssigned, TaskStateSuspended, TaskStateFailed},
	TaskStateVerifying:           {TaskStateVerified, TaskStateProducing, TaskStateSuspended, TaskStateAssigned, TaskStateFailed},
	TaskStateVerified:            {TaskStateDone, TaskStateSuspended, TaskStateAssigned, TaskStateFailed},
	TaskStateSuspended:           {TaskStatePending, TaskStatePendingConfirmation},
	TaskStatePendingConfirmation: {TaskStateProducing, TaskStateSuspended},
	TaskStateFailed:              {},
	TaskStateDone:                {},
}

// ResetForResume transitions stuck tasks back to a runnable state so
// ResumeMasterTask can re-execute them.  This bypasses the normal
// transition table because resume is a recovery operation.
func ResetForResume(state TaskState) TaskState {
	if state == TaskStateFailed || state == TaskStateSuspended || state == TaskStatePendingConfirmation {
		return TaskStatePending
	}
	if state == TaskStateDone {
		return TaskStateDone
	}
	if state == TaskStateVerified {
		return TaskStateDone
	}
	if state.IsTerminal() {
		return TaskStatePending
	}
	return TaskStateAssigned
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

// CodeRoles lists roles that produce code (as opposed to natural-language content).
var CodeRoles = map[AgentRole]bool{
	RoleDeveloper: true,
	RoleTester:    true,
}

// IsCodeRole reports whether this role produces code that can be built/tested/linted.
func (r AgentRole) IsCodeRole() bool {
	return CodeRoles[r]
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
	CycleAccept    CycleDecision = "accept"    // 接受结果，继续下一 Batch
	CycleReject    CycleDecision = "reject"    // 拒绝，重试当前 Batch
	CycleEscalate  CycleDecision = "escalate"  // 需要用户介入
	CycleEscalated CycleDecision = "escalated" // 升级策略后重试（换模型/调参数）
)

// TaskSummary is a lightweight view of a task for inclusion in reports.
type TaskSummary struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Role        string    `json:"role"`
	State       TaskState `json:"state"`
	RetryCount  int       `json:"retry_count"`
	OutputBrief string    `json:"output_brief,omitempty"`
}

// CycleReport is the periodic report sent from Engine to Leader after
// each batch completes.
type CycleReport struct {
	BatchID     string        `json:"batch_id"`
	BatchLabel  string        `json:"batch_label,omitempty"`
	CycleNumber int           `json:"cycle_number"` // 第几轮（重试次数）
	Status      BatchStatus   `json:"status"`
	Tasks       []TaskSummary `json:"tasks"`
	BoardPath   string        `json:"board_path,omitempty"`
	Deliverable string        `json:"deliverable_path,omitempty"`
}

// CycleReview is the Leader's structured response to a CycleReport.
type CycleReview struct {
	Decision    CycleDecision `json:"decision"`
	Reason      string        `json:"reason"`
	Feedback    string        `json:"feedback,omitempty"`     // 给 Worker 的改进意见
	PlanChanges string        `json:"plan_changes,omitempty"` // 如果需要改计划
}

// BatchSummary is a lightweight view of a batch for plan-level cycle reports.
type BatchSummary struct {
	ID     string        `json:"id"`
	Label  string        `json:"label,omitempty"`
	Status BatchStatus   `json:"status"`
	Tasks  []TaskSummary `json:"tasks"`
}

// PlanCycleReport is the report sent to the Leader after one full pass over the
// entire plan (all non-passed batches). Unlike CycleReport (which is per-batch),
// this is the plan-level report the Leader reviews to accept or reject a Cycle.
type PlanCycleReport struct {
	CycleNumber int            `json:"cycle_number"`
	Status      BatchStatus    `json:"status"`
	Batches     []BatchSummary `json:"batches"`
	BoardPath   string         `json:"board_path,omitempty"`
	Deliverable string         `json:"deliverable_path,omitempty"`
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
	// EventVerifierResult — Checker/Verifier 得出了结论（PASS/FAIL）
	EventVerifierResult
	// EventTaskDone — 任务最终完成（done 或 failed）
	EventTaskDone
	// EventLeaderLog — Leader 写入了新的日志（decompose/review/summary）
	EventLeaderLog
	// EventAgentLog — Worker/Verifier 写入了新的日志
	EventAgentLog
)

// TaskEvent 是 engine 向监听者推送的事件
type TaskEvent struct {
	Type         TaskEventType `json:"type"`
	TaskID       string        `json:"task_id"`
	MasterID     string        `json:"master_id,omitempty"` // 定位所属 run（进展注入把任务映射回 leader 会话）
	Title        string        `json:"title,omitempty"`
	Deliverables []string      `json:"deliverables,omitempty"` // 交付文件（相对 workdir），完成事件带出供链接展示
	Workdir      string        `json:"workdir,omitempty"`      // 交付工作目录（拼绝对链接）
	Role         string        `json:"role,omitempty"`         // 成员角色（TUI 成员卡片显示）
	OldState     string        `json:"old_state,omitempty"`
	NewState     string        `json:"new_state,omitempty"`
	Data         string        `json:"data,omitempty"`
	Progress     int           `json:"progress"`
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
		TaskStatePendingConfirmation,
		TaskStateDone,
	}
}
