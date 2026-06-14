package team_engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// Leader provides AI-powered task decomposition.
//
// The Leader calls a Whale subagent (via AgentRunner) to analyse a complex
// goal and decompose it into a structured plan of subtasks.  Each subtask
// specifies its own role, description, and tool profile, enabling the
// intelligent Worker/Verifier matching described in the MiniMax Agent Team
// paper.
type Leader struct {
	runner  *AgentRunner
	loggers *log.Loggers
	team    *TeamConfig
	onLog   func() // called after LogLeader writes a file
}

// NewLeader creates a Leader that uses the given AgentRunner.
func NewLeader(runner *AgentRunner) *Leader {
	return &Leader{runner: runner}
}

// WithLoggers attaches a Loggers for recording decompose/review output.
func (l *Leader) WithLoggers(loggers *log.Loggers) *Leader {
	l.loggers = loggers
	return l
}

// WithTeam attaches a TeamConfig so the decompose prompt includes team role names.
func (l *Leader) WithTeam(team *TeamConfig) *Leader {
	l.team = team
	return l
}

// WithOnLog sets a callback that fires after every LogLeader write.
func (l *Leader) WithOnLog(fn func()) *Leader {
	l.onLog = fn
	return l
}

// DecomposePrompt returns the prompt template for task decomposition.
func DecomposePrompt(goal string) string {
	return fmt.Sprintf(`You are a Team Leader Agent. Your job is to decompose the following goal into a structured plan of subtasks that can be executed by specialized worker agents.

GOAL:
%s

RULES:
1. Break the goal into 1-12 subtasks
2. Each subtask should be self-contained and produce a clear deliverable
3. Order subtasks by dependency (earlier subtasks first)
4. HOW TO SPLIT — Choose the right strategy:
   a) BY DELIVERABLE: One file/artifact per task (e.g. "write API spec" →
      "write implementation" → "write tests").  Use sequential batches.
   b) BY PERSPECTIVE: Same goal, different lenses.  Assign multiple agents
      with different verifier_focus values in the SAME batch, then add a
      synthesizer in the NEXT batch to merge findings.  (Judge Panel pattern)
      Example: Batch 1 = security-reviewer + perf-reviewer + correctness-reviewer
               Batch 2 = synthesizer (aggregate all reviews)
   c) BY WORKLOAD: Only when a single deliverable is unavoidably large
      (e.g. 8-section document).  Split into sequential subtasks of 2-3
      sections each.  Avoid — prefer (a) or (b) when possible.

5. Assign an appropriate ROLE to each subtask:
   - "developer"   — writing code
   - "tester"      — writing tests
   - "reviewer"    — code review
   - "researcher"  — research/analysis
   - "writer"      — documentation/writing
   - "formatter"   — code formatting
   - "evaluator"   — quality evaluation
   - "synthesizer" — merge multiple research results into structured conclusions

6. Group related subtasks into **batches** (stages). Use "batch_id" to group tasks
   that can run in parallel. Use "depends_on_batch" to declare batch-level dependencies.
   Example: tasks in batch "research" must finish before tasks in batch "write" start.

7. Use "depends_on_index" / "depends_on_indices" for task-level dependencies.

8. ORCHESTRATION PATTERNS — Choose the best pattern for your goal:

   a) PIPELINE (default): Sequential batches, each depending on the previous.
      Use when tasks have clear dependencies (e.g. design → code → test → deploy).
      Set depends_on_batch on later batches.

   b) PARALLEL WITH AGGREGATION (Judge Panel): For high-risk review tasks,
      assign MULTIPLE reviewers with different verifier_focus values in the same batch,
      then add a "synthesizer" task in the next batch to aggregate their conclusions.
      Example:
        Batch 1: security-reviewer (focus:security), perf-reviewer (focus:performance)
        Batch 2 (depends on Batch 1): synthesizer (aggregate all reviews)

   c) EXPLORATION LOOP (Deep Research): For research/exploration goals,
      set verifier_focus to "exploration" on research tasks. The engine will
      keep re-running the batch until no new findings emerge (dry).
      Set max_cycles to a reasonable limit (e.g. 3-5) to bound exploration depth.

   d) COMPLETENESS CHECK: For goals requiring full coverage,
      assign a reviewer with verifier_focus="completeness" in a subsequent batch.
      This reviewer checks if the output covers all requirements from the goal.

9. Use "verifier_focus" to control what the Verifier checks:
   - "correctness"    — output is accurate (default)
   - "security"       — security vulnerabilities
   - "performance"    — performance implications
   - "completeness"   — covers all requirements
   - "exploration"    — whether there are still unexplored directions (for Loop-until-dry)
   - "style"          — code style / conventions
   - "sources"        — whether claims are properly sourced (for research)

10. Use "max_cycles" per-batch to limit retry/exploration loops (default 1, max 10).

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
    "verifier_focus": "correctness",
    "max_cycles": 1
  }
]

CRITICAL — Before your final response, verify your JSON:
- Every string is properly closed with double quotes.
- Every object/array element is separated by commas.
- No trailing commas after the last element.
- The outermost structure is a JSON array [ ... ].
- No text, explanation, or markdown outside the JSON array.`, goal)
}

// decomposeInternal runs the leader agent and returns both parsed tasks
// and the raw AI output text.
//
// When the leader returns empty output (common with reasoning models whose
// chain-of-thought can consume the entire token budget), this function
// automatically retries up to 2 additional times with incrementally larger
// token budgets and a brief delay between attempts.
func (l *Leader) decomposeInternal(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	prompt := DecomposePrompt(goal)
	if l.team != nil {
		prompt = l.team.BuildLeaderPrompt(prompt)
	}
	// Use team leader model when no explicit model is provided by the caller.
	if (len(model) == 0 || model[0] == "") && l.team != nil && l.team.Leader.Model != "" {
		model = []string{l.team.Leader.Model}
	}

	mdl := ""
	if len(model) > 0 {
		mdl = model[0]
	}
	// Retry loop: reasoning models can exhaust their token budget on
	// chain-of-thought, yielding empty output.  Each retry adds a short
	// pause so the provider can recover.
	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		start := time.Now()
		// On retries, delay briefly so the provider can recover.
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		result := l.runner.RunDecomposer(prompt, workdir, timeout, model...)
		dur := time.Since(start)

		// Token budget diagnostic: log model + maxTokens + actual usage
		// so operators can see whether the budget is sufficient.
		totalUsed := result.UsagePrompt + result.UsageCompletion
		if totalUsed == 0 {
			// Usage not available from the provider; estimate from output.
			totalUsed = len(result.Stdout) / 4 // rough 4 chars/token estimate
		}
		if l.loggers != nil {
			l.loggers.Engine("leader.decompose: model=%s spawner=%s attempt=%d/%d maxTokens=%d prompt=%d completion=%d total=%d dur=%.1fs output=%d chars success=%v diag=%s",
				mdl, result.SpawnerType, attempt+1, maxRetries+1,
				ReasoningDecomposerMaxTokens,
				result.UsagePrompt, result.UsageCompletion, totalUsed,
				dur.Seconds(), len(result.Stdout), result.Success, result.Stderr)
		}

		// Log decompose for dashboard visibility (last attempt only, to
		// avoid noise from retries).
		if attempt == maxRetries || (result.Success && strings.TrimSpace(result.Stdout) != "") {
			if l.loggers != nil {
				l.loggers.LogLeader("decompose", prompt, result.Stdout, dur, nil)
			}
			if l.onLog != nil {
				l.onLog()
			}
		}

		if !result.Success {
			// If we still have retries left and the failure looks like
			// an empty-output issue (exit 1, empty stderr), retry.
			if attempt < maxRetries && result.ExitCode == 1 && result.Stderr == "" {
								if DefaultTeamLog != nil { DefaultTeamLog.LeaderRetry(attempt+1, "empty output") }
				if l.loggers != nil {
					l.loggers.Engine("leader.decompose: retrying (attempt %d failed with empty output, exit %d)", attempt+1, result.ExitCode)
				}
				continue
			}
			return nil, "", fmt.Errorf("leader agent failed (exit %d): %s", result.ExitCode, result.Stderr)
		}

		output := strings.TrimSpace(result.Stdout)
		if output == "" {
			if attempt < maxRetries {
				if l.loggers != nil {
					l.loggers.Engine("leader.decompose: retrying (attempt %d produced empty output after trim)", attempt+1)
				}
				continue
			}
			return nil, "", fmt.Errorf("leader returned empty output after %d attempts", maxRetries+1)
		}

		tasks, err := ParsePlanTasks(output)
		if err != nil {
			// Retry on parse failure — the model may have produced slightly
			// malformed JSON that a second attempt will fix (e.g. missing
			// code fences or trailing commas).
			if attempt < maxRetries {
				if l.loggers != nil {
					l.loggers.Engine("leader.decompose: retrying (attempt %d parse failed: %v)", attempt+1, err)
				}
				if DefaultTeamLog != nil { DefaultTeamLog.LeaderRetry(attempt+1, err.Error()) }
				continue
			}
			// Last attempt failed — fallback to a single generic task.
			return []PlanTask{{
				Title:           goal,
				Description:     output,
				Role:            "developer",
				BatchID:         "default",
				BatchLabel:      "Execution",
				DependsOnIndex:  -1,
			}}, output, nil
		}
		if l.loggers != nil {
			l.loggers.Engine("leader.decompose: success — %d tasks in plan", len(tasks))
		}
		if DefaultTeamLog != nil {
			DefaultTeamLog.LeaderDecompose(goal, mdl, attempt+1, ReasoningDecomposerMaxTokens, result.UsagePrompt, result.UsageCompletion, dur.Seconds(), len(output), false, true)
		}
		return tasks, output, nil
	}

	return nil, "", fmt.Errorf("leader failed after %d attempts", maxRetries+1)
}

// Decompose calls a Whale subagent to decompose a goal into subtasks.
// timeout is the subagent timeout; if <= 0 defaults to 180s.
// Returns the parsed plan tasks.
func (l *Leader) Decompose(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	tasks, _, err := l.decomposeInternal(goal, workdir, timeout, model...)
	return tasks, err
}

// DecomposeTask re-decomposes a single task that exhausted retries into
// smaller subtasks.  Returns nil if decomposition is not possible.
func (l *Leader) DecomposeTask(task *Task, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	prompt := fmt.Sprintf(`You are a Team Leader. The following task failed after multiple retries because it was too large to complete in a single pass.

FAILED TASK:
Title: %s
Role: %s
Description: %s

Last verifier feedback: %s

Break this task into 2-3 SMALLER subtasks that can each be completed in one pass.
Each subtask must produce ONE concrete deliverable.

OUTPUT FORMAT (pure JSON array, no markdown):
[
  {"title": "...", "description": "...", "role": "%s", "batch_id": "%s", "depends_on_batch": [], "verifier_focus": "%s", "max_cycles": 1}
]

CRITICAL: Verify your JSON syntax — no trailing commas, proper string quoting.`, task.Title, task.Role, task.Description, task.VerifierFeedback, task.Role, task.BatchID, task.VerifierFocus)

	tasks, _, err := l.decomposeInternal(prompt, workdir, timeout, model...)
	return tasks, err
}

// DecomposeFull is like Decompose but also returns the raw AI output text.
func (l *Leader) DecomposeFull(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	return l.decomposeInternal(goal, workdir, timeout, model...)
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
		// Try basic repair on common LLM JSON mistakes.
		repaired := repairJSON(jsonStr)
		if repaired != jsonStr {
			if err2 := json.Unmarshal([]byte(repaired), &tasks); err2 == nil {
				return tasks, nil
			}
		}
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

// CycleReviewJSON is the JSON structure expected from the Leader's review.
type CycleReviewJSON struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Feedback string `json:"feedback,omitempty"`
}

// reviewCycleInternal runs the review subagent and returns both the parsed
// decision and the raw AI output text.
func (l *Leader) reviewCycleInternal(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	prompt := ReviewCyclePrompt(goal, report)
	result := l.runner.RunDecomposer(prompt, workdir, timeout, model...)

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

// ReviewCycle calls a Whale subagent to review a CycleReport and return a decision.
// timeout is the subagent timeout; if <= 0 defaults to 60s.
func (l *Leader) ReviewCycle(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, error) {
	review, _, err := l.reviewCycleInternal(goal, report, workdir, timeout, model...)
	return review, err
}

// ReviewCycleFull is like ReviewCycle but also returns the raw AI output text.
func (l *Leader) ReviewCycleFull(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, string, error) {
	return l.reviewCycleInternal(goal, report, workdir, timeout, model...)
}

// ProactiveLeaderPrompt returns a prompt for the Leader to proactively
// review a single task's progress and provide guidance.
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
- "guidance"   → Task is偏离方向 or stuck, provide course correction
- "redirect"   → Task should be reprioritized or replaced

OUTPUT FORMAT (pure JSON, no markdown):
{"action": "none|guidance|redirect", "reason": "...", "feedback": "specific guidance for the worker"}
`, goal, taskTitle, taskRole, taskState, retryCount, outputPreview)
}

// ReviewProgress calls the Leader to proactively review a single task's
// progress and optionally send guidance via the inbox.
func (l *Leader) ReviewProgress(goal, taskTitle, taskRole, taskState string, retryCount int, lastOutput, workdir string, timeout time.Duration, model ...string) (string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	prompt := ProactiveLeaderPrompt(goal, taskTitle, taskRole, taskState, retryCount, lastOutput)
	result := l.runner.RunDecomposer(prompt, workdir, timeout, model...)

	if !result.Success {
		return "", fmt.Errorf("leader review failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return "", nil
	}

	// Try to parse action; return raw feedback on failure.
	action := extractJSONObject(output)
	if action == "" {
		return output, nil
	}
	return action, nil
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

// repairJSON attempts to fix common LLM JSON mistakes: trailing commas,
// text after the closing bracket, and missing commas between objects.
func repairJSON(s string) string {
	// 1. Strip trailing text after the last ']' (markdown notes etc.)
	if idx := strings.LastIndex(s, "]"); idx > 0 {
		s = s[:idx+1]
	}
	// 2. Remove trailing commas before '}' or ']'.
	s = regexp.MustCompile(`,(\s*[}\]])`).ReplaceAllString(s, "$1")
	// 3. Fix missing commas: `" "` → `", "` (common when LLM forgets commas between string fields).
	s = regexp.MustCompile(`"\s+"`).ReplaceAllString(s, `", "`)
	// 4. Balance brackets — if there are more '[' than ']', append missing ones.
	open := strings.Count(s, "[")
	close := strings.Count(s, "]")
	for close < open {
		s += "]"
		close++
	}
	return s
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
	// Strip markdown code fences.  Use the LAST fenced block — the leader
	// prompt also contains example JSON that must not be matched.
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")
	allMatches := re.FindAllStringSubmatch(output, -1)
	for i := len(allMatches) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(allMatches[i][1])
		if strings.HasPrefix(candidate, "[") {
			return candidate
		}
	}

	// No fenced JSON — search from end for the last raw JSON array.
	re = regexp.MustCompile(`(?s)\[.*\]`)
	allRaw := re.FindAllString(output, -1)
	for i := len(allRaw) - 1; i >= 0; i-- {
		if strings.HasPrefix(allRaw[i], "[") {
			return allRaw[i]
		}
	}

	return ""
}
