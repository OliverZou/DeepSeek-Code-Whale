package team_engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// Planner decomposes goals into structured plans of subtasks.
// Stateless: all state comes from injected dependencies.
type Planner struct {
	runner  *AgentRunner
	loggers *log.Loggers
	team    *TeamConfig
	onLog   func()
}

// NewPlanner creates a Planner that uses the given AgentRunner.
func NewPlanner(runner *AgentRunner) *Planner {
	return &Planner{runner: runner}
}

// WithLoggers attaches a Loggers for recording decompose output.
func (p *Planner) WithLoggers(loggers *log.Loggers) *Planner {
	p.loggers = loggers
	return p
}

// WithTeam attaches a TeamConfig so the decompose prompt includes team role names.
func (p *Planner) WithTeam(team *TeamConfig) *Planner {
	p.team = team
	return p
}

// WithOnLog sets a callback that fires after every LogLeader write.
func (p *Planner) WithOnLog(fn func()) *Planner {
	p.onLog = fn
	return p
}

// DecomposePrompt returns the prompt template for task decomposition.
// Leader splits by domain/role only. Worker agents handle their own
// size assessment and can self-split if a task is too large.
func DecomposePrompt(goal string) string {
	return fmt.Sprintf(`You are a Team Leader. Decompose the goal into a structured plan of subtasks assigned to specialized roles.

GOAL:
%s

RULES:

0. ⚠️ MOST IMPORTANT — ASSESS BEFORE DECOMPOSING ⚠️
   Simple goals that one worker can complete in a single pass do NOT need the full chain. Having multiple team members available does NOT mean you must use them all. Pick ONLY the most relevant role(s) — if 1 role is enough, use 1 role.

   - Trivial (narrow scope, one deliverable)  → 1 task, 1 batch
   - Moderate (2-3 distinct concerns)         → 2-3 tasks, 1-2 batches
   - Complex (cross-role, multi-phase)        → full chain

   Examples: "调研 X 趋势" = researcher only. "写一个 add.go 函数" = developer only. Only use the full chain when the goal genuinely requires multiple roles and phases. OVER-DECOMPOSITION IS A BUG.

1. DEVELOPMENT CHAIN (complex software projects only) — If a goal genuinely spans roles and phases:

     Requirements → Architecture → API Design → Coding → Verification

   Each phase feeds the next. Workers see ONLY the output of the previous phase, never the original goal. Produce EXACTLY ONE subtask per role per phase. If a phase needs multiple documents, assign ONE role and let the Worker self-split.

2. ONE TASK PER ROLE — Never give the same role multiple tasks in one phase. A subtask is a unit of responsibility, not a unit of size. If the work is too large, the Worker will self-split into smaller pieces.

3. NO SEPARATE REVIEW TASKS — Do NOT create subtasks for "审查", "验证", "review", "verification". Every task already has a built-in Checker that checks its output automatically. The Verification phase means Checker runs for each task — not a separate task.

4. ORDER BY DEPENDENCY — Upstream phases before downstream. Foundation before implementation.

4. BATCHES — A batch is a dependency barrier: all tasks in a batch must finish before the next batch starts. Put independent tasks in the same batch. Use "depends_on_batch" for cross-batch ordering.

5. WITHIN-BATCH DEPENDENCIES — Use "depends_on_index" (-1 means no dependency).

6. VERIFIER FOCUS — For each task, choose the verification lens:
   - "correctness"   — output is accurate
   - "completeness"  — covers all requirements
   - "security"      — security review
   - "style"         — code style / conventions

7. MAX CYCLES — Default 1. Set higher (≤5) for exploratory/research tasks.

OUTPUT FORMAT (pure JSON array, no markdown):
[
  {
    "title": "subtask title",
    "description": "detailed instructions for the worker agent. Include what inputs this task receives (from upstream tasks)",
    "output": "the single deliverable this task produces — file path, document, code module, or decision record",
    "role": "developer",
    "batch_id": "phase-1",
    "batch_label": "Foundation",
    "depends_on_batch": [],
    "depends_on_index": -1,
    "verifier_focus": "correctness",
    "use_dw": false,
    "max_cycles": 1
  }
]

Each task MUST have exactly one output. The output is the input for downstream tasks.

CRITICAL: Verify your JSON — no trailing commas, proper quoting, outermost structure is a JSON array [ ... ].`, goal)
}

// decomposeInternal runs the leader agent and returns both parsed tasks
// and the raw AI output text.
func (p *Planner) decomposeInternal(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	prompt := DecomposePrompt(goal)
	if p.team != nil {
		prompt = p.team.BuildLeaderPrompt(prompt)
	}
	// Team leader model takes priority over router default.
	if p.team != nil && p.team.Leader.Model != "" {
		model = []string{p.team.Leader.Model}
	}

	mdl := ""
	if len(model) > 0 {
		mdl = model[0]
	}
	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		start := time.Now()
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		result := p.runner.RunDecomposer(prompt, workdir, timeout, model...)
		dur := time.Since(start)

		totalUsed := result.UsagePrompt + result.UsageCompletion
		if totalUsed == 0 {
			totalUsed = len(result.Stdout) / 4
		}
		if p.loggers != nil {
			p.loggers.Engine("leader.decompose: model=%s spawner=%s attempt=%d/%d maxTokens=%d prompt=%d completion=%d total=%d dur=%.1fs output=%d chars success=%v diag=%s",
				mdl, result.SpawnerType, attempt+1, maxRetries+1,
				0,
				result.UsagePrompt, result.UsageCompletion, totalUsed,
				dur.Seconds(), len(result.Stdout), result.Success, result.Stderr)
		}

		if attempt == maxRetries || (result.Success && strings.TrimSpace(result.Stdout) != "") {
			if p.loggers != nil {
				p.loggers.LogLeader("decompose", prompt, result.Stdout, dur, nil)
			}
			if p.onLog != nil {
				p.onLog()
			}
		}

		if !result.Success {
			if attempt < maxRetries && result.ExitCode == 1 && result.Stderr == "" {
				if defaultTeamLog != nil {
					defaultTeamLog.LeaderRetry(attempt+1, "empty output")
				}
				if p.loggers != nil {
					p.loggers.Engine("leader.decompose: retrying (attempt %d failed with empty output, exit %d)", attempt+1, result.ExitCode)
				}
				continue
			}
			return nil, "", fmt.Errorf("leader agent failed (exit %d): %s", result.ExitCode, result.Stderr)
		}

		output := strings.TrimSpace(result.Stdout)
		if output == "" {
			if attempt < maxRetries {
				if p.loggers != nil {
					p.loggers.Engine("leader.decompose: retrying (attempt %d produced empty output after trim)", attempt+1)
				}
				continue
			}
			return nil, "", fmt.Errorf("leader returned empty output after %d attempts", maxRetries+1)
		}


		// If structured output was forced via OutputSchema, use it directly.
		if result.Structured != nil {
			tasks, err := structuredToPlanTasks(result.Structured)
			if err == nil && len(tasks) > 0 {
				if p.loggers != nil {
					p.loggers.Engine("leader.decompose: success — %d tasks in plan (structured output)", len(tasks))
				}
				return tasks, result.Stdout, nil
			}
		}

		tasks, err := ParsePlanTasks(output)
		if err != nil {
			if attempt < maxRetries {
				if p.loggers != nil {
					p.loggers.Engine("leader.decompose: retrying (attempt %d parse failed: %v)", attempt+1, err)
				}
				if defaultTeamLog != nil {
					defaultTeamLog.LeaderRetry(attempt+1, err.Error())
				}
				continue
			}
			return []PlanTask{{
				Title:           goal,
				Description:     output,
				Role:            "developer",
				BatchID:         "default",
				BatchLabel:      "Execution",
				DependsOnIndex:  -1,
			}}, output, nil
		}
		if p.loggers != nil {
			p.loggers.Engine("leader.decompose: success — %d tasks in plan", len(tasks))
		}
		if defaultTeamLog != nil {
			defaultTeamLog.LeaderDecompose(goal, mdl, attempt+1, 0, result.UsagePrompt, result.UsageCompletion, dur.Seconds(), len(output), false, true)
		}
		return tasks, output, nil
	}

	return nil, "", fmt.Errorf("leader failed after %d attempts", maxRetries+1)
}

// Decompose calls a Whale subagent to decompose a goal into subtasks.
func (p *Planner) Decompose(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	tasks, _, err := p.decomposeInternal(goal, workdir, timeout, model...)
	return tasks, err
}

// DecomposeFull is like Decompose but also returns the raw AI output text.
func (p *Planner) DecomposeFull(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	return p.decomposeInternal(goal, workdir, timeout, model...)
}

// stripVerifierFeedback removes accumulated [VERIFIER FEEDBACK ...] blocks
// from a task description, keeping only the original task requirements.
// This prevents prompt bloat when re-decomposing tasks that have gone through
// multiple retry rounds.
func stripVerifierFeedback(desc string) string {
	if idx := strings.Index(desc, "\n\n[VERIFIER FEEDBACK"); idx >= 0 {
		return strings.TrimSpace(desc[:idx])
	}
	// Also strip [Leader Feedback ...] blocks.
	if idx := strings.Index(desc, "\n\n[Leader Feedback"); idx >= 0 {
		return strings.TrimSpace(desc[:idx])
	}
	return desc
}

// DecomposeTask re-decomposes a single task that exhausted retries into smaller subtasks.
func (p *Planner) DecomposeTask(task *Task, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	// Strip accumulated verifier feedback to keep the prompt lean —
	// the original task requirements are all the Leader needs to re-decompose.
	cleanDesc := stripVerifierFeedback(task.Description)
	prompt := fmt.Sprintf(`You are a Team Leader. The following task failed after multiple retries because it was too large to complete in a single pass.

FAILED TASK:
Title: %s
Role: %s
Description: %s

Last verifier feedback: %s

The task failed because a Worker agent could not produce the full output in one pass —
the output was truncated, the code was incomplete, or the file ended mid-statement.
Workers have limited output capacity (~150 lines of code max per task).  The original
task description likely asked for too much in a single pass.

Break this task into 2-3 SMALLER subtasks that each stay under the ~150-line limit.
Apply the same splitting strategies as the original decomposition:
  - BY DOMAIN: split by module boundary if the task spans multiple packages
  - BY FUNCTION: split by self-contained capability (one function or small set)
  - BY DEPENDENCY: types/constants first, then algorithm, then integration

Each subtask must produce ONE concrete, testable deliverable.

OUTPUT FORMAT (pure JSON array, no markdown):
[
  {"title": "...", "description": "...", "role": "%s", "batch_id": "%s", "depends_on_batch": [], "verifier_focus": "%s", "max_cycles": 1}
]

CRITICAL: Verify your JSON syntax — no trailing commas, proper string quoting.`, task.Title, task.Role, cleanDesc, task.VerifierFeedback, task.Role, task.BatchID, task.VerifierFocus)

	tasks, _, err := p.decomposeInternal(prompt, workdir, timeout, model...)
	return tasks, err
}

// ConsensusDecompose runs decomposition with two models in parallel and
// uses a third model (or the first) to select the best plan.
func (p *Planner) ConsensusDecompose(goal, workdir string, timeout time.Duration, models []string) ([]PlanTask, string, error) {
	if len(models) < 2 {
		return p.DecomposeFull(goal, workdir, timeout, models...)
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}

	type planResult struct {
		tasks []PlanTask
		raw   string
		err   error
		model string
	}
	resultCh := make(chan planResult, 2)
	for i := 0; i < 2 && i < len(models); i++ {
		mdl := models[i]
		go func(model string) {
			tasks, raw, err := p.decomposeInternal(goal, workdir, timeout, model)
			resultCh <- planResult{tasks: tasks, raw: raw, err: err, model: model}
		}(mdl)
	}

	var validPlans []planResult
	for i := 0; i < 2 && i < len(models); i++ {
		pr := <-resultCh
		if pr.err == nil && len(pr.tasks) > 0 {
			validPlans = append(validPlans, pr)
		}
	}

	if len(validPlans) == 0 {
		return nil, "", fmt.Errorf("consensus decompose: both models failed")
	}
	if len(validPlans) == 1 {
		return validPlans[0].tasks, validPlans[0].raw, nil
	}

	// Use third model (or first) to select the better plan.
	selectorModel := ""
	if len(models) > 2 {
		selectorModel = models[2]
	} else {
		selectorModel = models[0]
	}
	// Convert validPlans to consensusResult slice for selectBestPlan.
	consPlans := make([]consensusResult, len(validPlans))
	for i, vp := range validPlans {
		consPlans[i] = consensusResult{tasks: vp.tasks, raw: vp.raw, model: vp.model}
	}
	selected, err := p.selectBestPlan(goal, consPlans, selectorModel, workdir, timeout)
	if err != nil {
		return validPlans[0].tasks, validPlans[0].raw, nil
	}
	return selected.tasks, selected.raw, nil
}

type consensusResult struct {
	tasks []PlanTask
	raw   string
	model string
}

func (p *Planner) selectBestPlan(goal string, plans []consensusResult, selectorModel, workdir string, timeout time.Duration) (*consensusResult, error) {
	var sb strings.Builder
	sb.WriteString("You are a planning evaluator. Two AI planners produced plans for the same goal.\n\n")
	sb.WriteString(fmt.Sprintf("GOAL:\n%s\n\n", goal))
	for i, pr := range plans {
		sb.WriteString(fmt.Sprintf("=== PLAN %d (model: %s) ===\n", i+1, pr.model))
		sb.WriteString(pr.raw)
		sb.WriteString("\n\n")
	}
	sb.WriteString(`Compare both plans. Select the one that:
1. Best decomposes the goal into self-contained subtasks
2. Has clear dependency ordering
3. Uses appropriate roles
4. Is most likely to complete successfully

OUTPUT FORMAT (pure JSON, no markdown):
{"selection": 1, "reason": "Plan 1 is better because..."}
`)

	result := p.runner.RunDecomposer(sb.String(), workdir, timeout, selectorModel)
	if !result.Success {
		return nil, fmt.Errorf("selector agent failed: %s", result.Stderr)
	}

	type selectionJSON struct {
		Selection int    `json:"selection"`
		Reason    string `json:"reason"`
	}
	jsonStr := extractJSONObject(strings.TrimSpace(result.Stdout))
	var sel selectionJSON
	if err := json.Unmarshal([]byte(jsonStr), &sel); err != nil || sel.Selection < 1 || sel.Selection > len(plans) {
		return nil, fmt.Errorf("invalid selection: %w", err)
	}
	idx := sel.Selection - 1
	return &plans[idx], nil
}

// ParsePlanTasks parses the AI agent's output into a slice of PlanTask.
func ParsePlanTasks(output string) ([]PlanTask, error) {
	jsonStr := extractJSON(output)
	// If no properly-closed JSON array was found, try to extract a
	// truncated (unclosed) one so repairJSON can attempt recovery.
	if jsonStr == "" {
		if start, _ := findJSONArray(output, false); start >= 0 {
			jsonStr = output[start:]
		}
	}
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON found in leader output")
	}

	var tasks []PlanTask
	if err := json.Unmarshal([]byte(jsonStr), &tasks); err != nil {
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

	for i := range tasks {
		t := &tasks[i]
		if t.Role == "" {
			return nil, fmt.Errorf("subtask %d (%q) missing role", i, t.Title)
		}
		if t.DependsOnIndex < 0 && len(t.DependsOnIndices) == 0 {
			t.DependsOnIndex = -1
		}
	}

	return tasks, nil
}

// extractJSON finds a JSON array in the AI's output.
func extractJSON(output string) string {
	// Prefer code-fenced blocks — the model is instructed to wrap JSON in ```json.
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")
	allMatches := re.FindAllStringSubmatch(output, -1)
	for i := len(allMatches) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(allMatches[i][1])
		if strings.HasPrefix(candidate, "[") {
			return candidate
		}
	}

	// Fallback: find the outermost JSON array using balanced bracket counting.
	// Only return properly closed arrays — truncated arrays are handled by
	// ParsePlanTasks which calls repairJSON on the raw output.
	if start, end := findJSONArray(output, true); start >= 0 {
		return output[start : end+1]
	}
	return ""
}

// findJSONArray finds the outermost balanced JSON array in s.
// Returns (start, end) byte offsets of the opening '[' and closing ']',
// or (-1, -1) if not found.
// requireClose: if true, only returns when a matching ']' is found.
func findJSONArray(s string, requireClose bool) (int, int) {
	depth := 0
	arrayStart := -1
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '[' {
			if depth == 0 {
				arrayStart = i
			}
			depth++
		} else if c == ']' {
			depth--
			if depth == 0 && arrayStart >= 0 {
				return arrayStart, i
			}
		}
	}
	if requireClose {
		return -1, -1
	}
	if arrayStart >= 0 {
		return arrayStart, len(s) - 1 // truncated: return to end of string
	}
	return -1, -1
}

// repairJSON fixes common LLM JSON mistakes, including truncated output.
func repairJSON(s string) string {
	s = strings.TrimSpace(s)

	// 0. Fix invalid JSON escape sequences.  Models sometimes emit \\(, \\),
	//    or other backslash-letter combos that aren't valid JSON escapes.
	//    Replace them with the literal character (just drop the backslash).
	s = regexp.MustCompile(`\\([^\"\\/bfnrtu])`).ReplaceAllString(s, "$1")

	// 1. Remove trailing incomplete elements (e.g. trailing comma with no value).
	s = regexp.MustCompile(`,(\s*)$`).ReplaceAllString(s, "$1")

	// 2. Close unclosed strings — use a state machine to detect whether
	//    we're inside a string at the end of the truncated output.
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
		}
	}
	if inString {
		s += `"`
	}

	// 3. Close unclosed objects: count { vs } (ignoring strings).
	openObj := countBraces(s, '{')
	closeObj := countBraces(s, '}')
	for closeObj < openObj {
		s += "}"
		closeObj++
	}

	// 4. Close unclosed arrays: count [ vs ] (ignoring strings).
	openArr := countBraces(s, '[')
	closeArr := countBraces(s, ']')
	for closeArr < openArr {
		s += "]"
		closeArr++
	}

	// 5. Remove trailing commas before closing brackets/braces.
	s = regexp.MustCompile(`,(\s*[}\]])`).ReplaceAllString(s, "$1")

	// 6. Fix consecutive quoted strings (missing comma): "key""key2" → "key", "key2".
	s = regexp.MustCompile(`"\s+"`).ReplaceAllString(s, `", "`)

	return s
}

// countBraces counts occurrences of the given brace character in s,
// ignoring characters inside JSON strings.
func countBraces(s string, brace byte) int {
	count := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == brace {
			count++
		}
	}
	return count
}

// structuredToPlanTasks converts the structured output (from OutputSchema)
// into PlanTask objects.  The runtime already validated the schema, so this
// is a direct JSON round-trip.
func structuredToPlanTasks(v any) ([]PlanTask, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal structured: %w", err)
	}
	// The OutputSchema wraps tasks in {"tasks": [...]}.
	// Try that first, then fall back to a bare array.
	var wrapper struct {
		Tasks []PlanTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Tasks) > 0 {
		return wrapper.Tasks, nil
	}
	var tasks []PlanTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("unmarshal structured: %w", err)
	}
	return tasks, nil
}
