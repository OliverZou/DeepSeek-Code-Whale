package team_engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// =============================================================================
// AgentChannel — Uniform interface for prompt / spawn / abort / kill
//
// "团队将用户对 Agent 可执行的操作（如 prompt、spawn、abort、kill）抽象为接口。
//  这意味着任何渠道 (用户、其他 Agent 或 team-engine 本身) 都可以通过这套接口操作
//  Agent。Agent 之间可以像人类一样进行多轮交互，包括主动推送和按需查询。"
//
// This interface IS that abstraction.  It is implemented by TeamEngine and
// callable by:
//   - Humans  (via CLI: `whale team prompt <task-id> "..."`)
//   - Agents  (via their toolset: `agent_prompt(task_id, message)`)
//   - Engine  (internal orchestration: SendFeedback → Prompt)
// =============================================================================

// AgentChannel is the unified interface shared equally by humans, agents,
// and the engine itself for all agent operations.
//
// The five operations (prompt, spawn, abort, kill, resolve) form the
// complete control surface described in the MiniMax Agent Team paper.
type AgentChannel interface {
	// Prompt sends a message to a running (or checkpointed) agent.
	// The receiving agent sees it in its inbox on the next turn.
	// Returns the agent's reply when it responds, or nil if async.
	Prompt(ctx context.Context, req PromptRequest) (*Message, error)

	// Spawn creates a new child agent task. Returns the created task.
	Spawn(ctx context.Context, req SpawnRequest) (*Task, error)

	// Abort gracefully stops a running agent (allows cleanup).
	Abort(ctx context.Context, taskID string) error

	// Kill forcefully terminates an agent (immediate, no cleanup).
	Kill(ctx context.Context, taskID string) error

	// ResolveEscalation resolves a pending escalation with a decision.
	// Available to both humans (CLI) and agents (via tools).
	ResolveEscalation(batchID string, decision EscalationDecision) error
}

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// PromptRequest carries a message addressed to a specific task/agent.
type PromptRequest struct {
	ToTaskID string // Target task / agent
	From     string // Sender identity: "human", "agent:<task-id>", "system"
	Content  string // The message body
	Sync     bool   // If true, wait for a reply; if false, fire-and-forget
}

// Message is a single unit of agent-to-agent or human-to-agent communication.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`       // Sender identity
	To        string    `json:"to"`         // Target task ID
	Content   string    `json:"content"`    // Message body
	ReplyTo   string    `json:"reply_to"`   // Non-empty when this is a reply
	CreatedAt string    `json:"created_at"` // ISO 8601
}

// NewMessage creates a Message with a UUID and timestamp.
func NewMessage(to, from, content, replyTo string) Message {
	return Message{
		ID:        uuid.New().String(),
		From:      from,
		To:        to,
		Content:   content,
		ReplyTo:   replyTo,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// SpawnRequest carries parameters for creating a new child agent.
// Same fields as CreateTask but includes an optional parent message.
type SpawnRequest struct {
	Title         string
	Description   string
	Role          AgentRole
	Profile       ToolProfile
	ParentIDs     []string
	MaxRetries    int
	Workdir       string
	VerifierFocus string
	// From identifies who requested the spawn.
	From string
	// ParentMessage is an optional message that triggered this spawn.
	ParentMessage *Message
}

// ---------------------------------------------------------------------------
// AgentChannel implementation on TeamEngine
// ---------------------------------------------------------------------------

// Ensure TeamEngine implements AgentChannel.
var _ AgentChannel = (*TeamEngine)(nil)

// Prompt implements AgentChannel. It delivers a message to a task's inbox
// and, when Sync=true, waits for the agent to produce a reply.
func (e *TeamEngine) Prompt(ctx context.Context, req PromptRequest) (*Message, error) {
	task, err := e.DB.GetTask(req.ToTaskID)
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", req.ToTaskID, err)
	}
	if task == nil {
		return nil, fmt.Errorf("task %q not found", req.ToTaskID)
	}

	msg := NewMessage(req.ToTaskID, req.From, req.Content, "")
	if err := e.Whiteboard.WriteMessage(req.ToTaskID, msg); err != nil {
		return nil, fmt.Errorf("write message: %w", err)
	}

	if !req.Sync {
		return nil, nil // Fire-and-forget: no reply expected
	}

	// Sync mode: append to task description and re-run to get a reply.
	newDesc := task.Description + fmt.Sprintf("\n\n[FROM %s]\n%s", req.From, req.Content)
	if err := e.DB.UpdateTask(req.ToTaskID, map[string]interface{}{
		"description": newDesc,
	}); err != nil {
		return nil, fmt.Errorf("update task description: %w", err)
	}

	// Only re-run if the task is in a non-terminal state that supports it.
	if !task.State.IsTerminal() && task.State != TaskStateVerifying {
		if _, err := e.RunTask(req.ToTaskID); err != nil {
			return nil, fmt.Errorf("re-run task for prompt reply: %w", err)
		}
	}

	// Read the reply from whiteboard output.
	output, err := e.Whiteboard.ReadOutput(req.ToTaskID)
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}

	reply := NewMessage(req.ToTaskID, "agent:"+req.ToTaskID, output, msg.ID)
	return &reply, nil
}

// Spawn implements AgentChannel. Delegates to CreateTask.
func (e *TeamEngine) Spawn(_ context.Context, req SpawnRequest) (*Task, error) {
	return e.CreateTask(
		req.Title,
		req.Description,
		req.Role,
		req.Profile,
		req.ParentIDs,
		req.MaxRetries,
		req.Workdir,
		req.VerifierFocus,
	)
}

// Abort implements AgentChannel. Graceful cancel.
func (e *TeamEngine) Abort(_ context.Context, taskID string) error {
	return e.CancelTask(taskID)
}

// Kill implements AgentChannel. Forceful termination.
// Transitions the task to suspended so it can be resumed later.
func (e *TeamEngine) Kill(_ context.Context, taskID string) error {
	task, err := e.DB.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	if task.State.IsTerminal() {
		return fmt.Errorf("task %q is already in terminal state %s", taskID, task.State)
	}
	if task.State == TaskStateSuspended {
		return fmt.Errorf("task %q is already suspended", taskID)
	}

	// Cancel the active subagent context if one exists, so the spawn
	// unblocks promptly instead of waiting for the full timeout.
	e.mu.Lock()
	if cancel, ok := e.activeCancels[taskID]; ok {
		cancel()
		delete(e.activeCancels, taskID)
	}
	e.mu.Unlock()

	// Transition to suspended (not failed!) so the task can be resumed.
	if err := e.DB.TransitionState(taskID, TaskStateSuspended, "suspended by user", ""); err != nil {
		return fmt.Errorf("suspend task: %w", err)
	}
	_ = e.Whiteboard.WriteStatus(taskID, string(TaskStateSuspended))
	_ = e.Whiteboard.AppendOutput(taskID, "\n[SUSPENDED by user]")
	return nil
}
