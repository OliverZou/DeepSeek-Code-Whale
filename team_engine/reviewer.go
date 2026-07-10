package team_engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"team-engine/log"
)

// Reviewer examines batch execution results and returns accept/reject/escalate decisions.
type Reviewer struct {
	runner  *AgentRunner
	loggers *log.Loggers
	team    *TeamConfig
	onLog   func()
}

// NewReviewer creates a Reviewer that uses the given AgentRunner.
func NewReviewer(runner *AgentRunner) *Reviewer {
	return &Reviewer{runner: runner}
}

// WithLoggers attaches a Loggers for recording review output.
func (r *Reviewer) WithLoggers(loggers *log.Loggers) *Reviewer {
	r.loggers = loggers
	return r
}

// WithTeam attaches a TeamConfig for the review prompt context.
func (r *Reviewer) WithTeam(team *TeamConfig) *Reviewer {
	r.team = team
	return r
}

// WithOnLog sets a callback that fires after every LogLeader write.
func (r *Reviewer) WithOnLog(fn func()) *Reviewer {
	r.onLog = fn
	return r
}

// ReviewCyclePrompt returns the prompt for reviewing a CycleReport.
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
- "accept"     → Results look good or exploration is dry, proceed to next batch
- "reject"     → Results need improvement or there are still unexplored directions, retry this batch
- "escalated"  → Strategy needs adjustment. Failed tasks will be reset to pending
                 and retried with escalated capabilities (different model/approach).
                 Use when standard retries keep failing.
- "escalate"   → Need human input (risk/ambiguity/cost)

IMPORTANT — CONTEXT-DEPENDENT DECISIONS:
- For EXPLORATION tasks (verifier_focus="exploration"): "reject" means
  "there are still unexplored directions, keep digging". "accept" means
  "the topic is exhausted, no new findings in this round".
- For COMPLETENESS tasks (verifier_focus="completeness"): "reject" means
  "not all requirements are covered yet".
- For regular tasks: "reject" means "quality is insufficient, fix the issues".

Use "feedback" to tell the Worker what to improve, fix, or explore next.
For exploration tasks, point the Worker to specific unexplored angles.

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

// CycleReviewJSON is the JSON structure expected from the review.
type CycleReviewJSON struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Feedback string `json:"feedback,omitempty"`
}

// reviewCycleInternal runs the review subagent and returns the parsed decision and raw output.
func (r *Reviewer) reviewCycleInternal(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	prompt := ReviewCyclePrompt(goal, report)
	result := r.runner.RunDecomposer(prompt, workdir, timeout, model...)

	if !result.Success {
		return nil, "", fmt.Errorf("leader review failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return nil, "", fmt.Errorf("leader review returned empty output")
	}

	review, err := ParseCycleReview(output)
	if err != nil {
		return nil, output, err
	}
	return review, output, nil
}

// ReviewCycle reviews a CycleReport and returns a decision.
func (r *Reviewer) ReviewCycle(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, error) {
	review, _, err := r.reviewCycleInternal(goal, report, workdir, timeout, model...)
	return review, err
}

// ReviewCycleFull is like ReviewCycle but also returns the raw AI output text.
func (r *Reviewer) ReviewCycleFull(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, string, error) {
	return r.reviewCycleInternal(goal, report, workdir, timeout, model...)
}

// ProactiveLeaderPrompt returns a prompt for proactively reviewing a task's progress.
func ProactiveLeaderPrompt(goal string, taskTitle string, taskRole string, taskState string, retryCount int, lastOutput string) string {
	outputPreview := lastOutput
	if len(outputPreview) > 500 {
		outputPreview = outputPreview[:500] + "..."
	}
	return fmt.Sprintf(`You are a proactive Team Leader overseeing a running pipeline.

GOAL:
%s

A task needs your attention:

Task:     %s
Role:     %s
State:    %s
Retries:  %d

Latest output:
%s

Decide if you need to intervene:
- "none"       → Task is on track, no intervention needed
- "guidance"   → Task is off track or stuck, provide course correction
- "redirect"   → Task should be reprioritized or replaced

OUTPUT FORMAT (pure JSON, no markdown):
{"action": "none|guidance|redirect", "reason": "...", "feedback": "specific guidance for the worker"}
`, goal, taskTitle, taskRole, taskState, retryCount, outputPreview)
}

// ReviewProgress proactively reviews a single task's progress.
func (r *Reviewer) ReviewProgress(goal, taskTitle, taskRole, taskState string, retryCount int, lastOutput, workdir string, timeout time.Duration, model ...string) (string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	prompt := ProactiveLeaderPrompt(goal, taskTitle, taskRole, taskState, retryCount, lastOutput)
	result := r.runner.RunDecomposer(prompt, workdir, timeout, model...)

	if !result.Success {
		return "", fmt.Errorf("leader review failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return "", nil
	}

	action := extractJSONObject(output)
	if action == "" {
		return output, nil
	}
	return action, nil
}

// BatchLabelOrID returns the batch label if set, otherwise the batch ID.
func (r *CycleReport) BatchLabelOrID() string {
	if r.BatchLabel != "" {
		return r.BatchLabel
	}
	return r.BatchID
}

// ParseCycleReview parses the JSON review output into a CycleReview.
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
	case "escalated":
		cr.Decision = CycleEscalated
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
