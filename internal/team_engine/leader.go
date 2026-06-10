package team_engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Leader provides AI-powered task decomposition.
//
// The Leader calls a Whale subagent (via AgentRunner) to analyse a complex
// goal and decompose it into a structured plan of subtasks.  Each subtask
// specifies its own role, description, and tool profile, enabling the
// intelligent Worker/Verifier matching described in the MiniMax Agent Team
// paper.
type Leader struct {
	runner *AgentRunner
}

// NewLeader creates a Leader that uses the given AgentRunner.
func NewLeader(runner *AgentRunner) *Leader {
	return &Leader{runner: runner}
}

// DecomposePrompt returns the prompt template for task decomposition.
func DecomposePrompt(goal string) string {
	return fmt.Sprintf(`You are a Team Leader Agent. Your job is to decompose the following goal into a structured plan of subtasks that can be executed by specialized worker agents.

GOAL:
%s

RULES:
1. Break the goal into 1-8 subtasks
2. Each subtask should be self-contained and produce a clear deliverable
3. Order subtasks by dependency (earlier subtasks first)
4. Assign an appropriate ROLE to each subtask:
   - "developer" — writing code
   - "tester" — writing tests
   - "reviewer" — code review
   - "researcher" — research/analysis
   - "writer" — documentation/writing
   - "formatter" — code formatting
   - "evaluator" — quality evaluation
   - "synthesizer" — merge multiple research results into structured conclusions (场景3)
5. Set "depends_on_index" to the 0-based index of the subtask this one depends on, or -1 if no dependency
6. For complex tasks with multiple dependencies, use "depends_on_indices" instead

6. Group related subtasks into **batches** (stages). Use "batch_id" to group tasks
   that can run in parallel. Use "depends_on_batch" to declare batch-level dependencies.
   Example: tasks in batch "research" must finish before tasks in batch "write" start.

OUTPUT FORMAT (pure JSON array, no markdown):
[
  {
    "title": "subtask title",
    "description": "detailed instructions for the worker agent",
    "role": "developer",
    "batch_id": "phase-1",
    "batch_label": "Research Phase",
    "depends_on_batch": [],
    "depends_on_index": -1,
    "verifier_focus": "correctness"
  }
]`, goal)
}

// Decompose calls a Whale subagent to decompose a goal into subtasks.
// Returns the parsed plan tasks.
func (l *Leader) Decompose(goal string, workdir string) ([]PlanTask, error) {
	prompt := DecomposePrompt(goal)
	result := l.runner.RunDecomposer(prompt, workdir, 120*time.Second)

	if !result.Success {
		return nil, fmt.Errorf("leader agent failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return nil, fmt.Errorf("leader returned empty output")
	}

	return ParsePlanTasks(output)
}

// ParsePlanTasks parses the AI agent's output into a slice of PlanTask.
// It handles markdown code fences and raw JSON arrays.
func ParsePlanTasks(output string) ([]PlanTask, error) {
	jsonStr := extractJSON(output)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON found in leader output")
	}

	var tasks []PlanTask
	if err := json.Unmarshal([]byte(jsonStr), &tasks); err != nil {
		return nil, fmt.Errorf("parse plan JSON: %w\nRaw output: %s", err, output)
	}

	if len(tasks) == 0 {
		return nil, fmt.Errorf("leader returned empty plan")
	}

	// Validate and normalise.
	for i := range tasks {
		t := &tasks[i]
		if t.Role == "" {
			return nil, fmt.Errorf("subtask %d (%q) missing role", i, t.Title)
		}
		// Normalise depends_on_index: -1 means no dependency.
		if t.DependsOnIndex < 0 && len(t.DependsOnIndices) == 0 {
			t.DependsOnIndex = -1
		}
	}

	return tasks, nil
}

// ---------------------------------------------------------------------------
// CycleReport 审查 — Leader 审阅 Batch 执行结果并做决策
// ---------------------------------------------------------------------------

// ReviewCyclePrompt returns the prompt for the Leader to review a CycleReport.
func ReviewCyclePrompt(goal string, report *CycleReport) string {
	return fmt.Sprintf(`You are the Team Leader. Review this CycleReport and decide the next action.

GOAL:
%s

CYCLE REPORT:
Batch:     %s
Cycle:     %d
Status:    %s

TASKS:
| Task | Role | State | Retries |
|------|------|-------|---------|
`, goal, report.BatchLabelOrID(), report.CycleNumber, report.Status) +
		buildTaskTable(report.Tasks) + `
Board: ` + report.BoardPath + `

DECIDE:
- "accept"    → Results look good, proceed to next batch
- "reject"    → Results need improvement, retry this batch
- "escalate"  → Need human input (risk/ambiguity/cost)

OUTPUT FORMAT (pure JSON, no markdown):
{"decision": "accept|reject|escalate", "reason": "...", "feedback": "optional improvement suggestions"}
`
}

func buildTaskTable(tasks []TaskSummary) string {
	var b strings.Builder
	for _, t := range tasks {
		status := "✅"
		if t.State == TaskStateFailed {
			status = "❌"
		}
		b.WriteString(fmt.Sprintf("| %s %s | %s | %s | %d |\n",
			status, shortIDDisplay(t.ID), t.Role, t.State, t.RetryCount))
	}
	return b.String()
}

func shortIDDisplay(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// CycleReviewJSON is the JSON structure expected from the Leader's review.
type CycleReviewJSON struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Feedback string `json:"feedback,omitempty"`
}

// ReviewCycle calls a Whale subagent to review a CycleReport and return a decision.
func (l *Leader) ReviewCycle(goal string, report *CycleReport, workdir string) (*CycleReview, error) {
	prompt := ReviewCyclePrompt(goal, report)
	result := l.runner.RunDecomposer(prompt, workdir, 60*time.Second)

	if !result.Success {
		return nil, fmt.Errorf("leader review failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return nil, fmt.Errorf("leader review returned empty output")
	}

	return ParseCycleReview(output)
}

// ParseCycleReview parses the Leader's JSON review output.
func ParseCycleReview(output string) (*CycleReview, error) {
	jsonStr := extractJSONObject(output)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON object found in review output")
	}

	var review CycleReviewJSON
	if err := json.Unmarshal([]byte(jsonStr), &review); err != nil {
		return nil, fmt.Errorf("parse review JSON: %w\nRaw: %s", err, output)
	}

	cr := &CycleReview{
		Reason:   review.Reason,
		Feedback: review.Feedback,
	}

	switch review.Decision {
	case "accept":
		cr.Decision = CycleAccept
	case "reject":
		cr.Decision = CycleReject
	case "escalate":
		cr.Decision = CycleEscalate
	default:
		return nil, fmt.Errorf("unknown decision: %s", review.Decision)
	}

	return cr, nil
}

// extractJSONObject finds a JSON object { ... } in the output.
func extractJSONObject(output string) string {
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")
	matches := re.FindStringSubmatch(output)
	if len(matches) >= 2 {
		candidate := strings.TrimSpace(matches[1])
		if strings.HasPrefix(candidate, "{") {
			return candidate
		}
	}

	re = regexp.MustCompile(`(?s)\{.*\}`)
	match := re.FindString(output)
	if match != "" {
		return match
	}
	return ""
}

// BatchLabelOrID returns the batch label if set, otherwise the batch ID.
func (r *CycleReport) BatchLabelOrID() string {
	if r.BatchLabel != "" {
		return r.BatchLabel
	}
	return r.BatchID
}

// extractJSON tries to find a JSON array in the AI's output.
//
// It handles:
//  1. ```json ... ``` markdown code fences
//  2. ``` ... ``` plain code fences
//  3. Raw JSON arrays
func extractJSON(output string) string {
	// Strip markdown code fences if present.
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")
	matches := re.FindStringSubmatch(output)
	if len(matches) >= 2 {
		candidate := strings.TrimSpace(matches[1])
		if strings.HasPrefix(candidate, "[") {
			return candidate
		}
	}

	// Try to find a raw JSON array in the output.
	re = regexp.MustCompile(`(?s)\[.*\]`)
	match := re.FindString(output)
	if match != "" {
		return match
	}

	return output
}
