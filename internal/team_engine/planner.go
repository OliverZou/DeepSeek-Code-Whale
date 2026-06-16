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
func DecomposePrompt(goal string) string {
	return fmt.Sprintf(`You are a Team Leader Agent. Your job is to decompose the following goal into a structured plan of subtasks that can be executed by specialized worker agents.

GOAL:
%s

RULES:
0. DEVELOPMENT PROCESS — Every phase produces the basis for the next
   phase, and every phase is constrained by the phase before it:

     Requirements → Architecture → API Design → Coding → Verification

   - Requirements: must satisfy the user's goal AND be sufficient to
     guide architecture.  The architect works from this doc, not from
     guesswork.
   - Architecture: must satisfy the requirements AND be sufficient to
     guide API design.
   - API Design: must satisfy the architecture AND be sufficient to
     guide coding.  Coders implement to the spec, not to their own
     judgment.
   - Coding: implements the API spec.  No feature that is not in the spec.
   - Verification: checks that the code matches the spec and the spec
     matches the goal.

   SCALE TO THE GOAL — The chain is standard, but the SIZE of each
   document depends on the scope of the goal.  A small project (one
   module, one developer) needs a short spec (≤ 30 lines).  A large
   project (multiple subsystems) needs more.  The rule: write enough
   for the next phase to proceed without ambiguity — and no more.

1. Break the goal into subtasks.  Most goals need 3-8 subtasks; complex goals
   may need up to 12.  Fewer, larger tasks are MORE LIKELY TO FAIL than several
   smaller, focused ones.

2. Each subtask must be self-contained and produce ONE clear deliverable.
   A Worker should be able to complete it in a single pass.

3. Order subtasks by dependency — foundation types and utilities first,
   code that depends on them later.

4. HOW TO SPLIT — You have 3 independent AXES.  Examine the goal and
   combine them.  Not every axis applies to every goal.

   ═══ AXIS A — STRUCTURE (what are the pieces?)
   Split the scope into self-contained units.  The "size ruler" depends
   on the task TYPE:

     BUILD tasks (code, configs, docs):
       Split by module / component / file.
       Size limit: ≤ ~150 lines of code or ≤ ~3 functions per subtask.
       If a file needs 200+ lines → split into 2 subtasks.
       Split points: types/constants first, then algorithm core, then
       integration/glue code last.

     RESEARCH tasks (analysis, investigation, comparison):
       Split by sub-question / topic / hypothesis.
       Size limit: each subtask should yield 3-5 concrete findings
       with 2+ citable sources per finding.

     DECISION tasks (evaluation, recommendation):
       Split by criterion / option / scenario.
       Size limit: each subtask covers ONE criterion or ONE option
       in depth, with evidence and trade-off analysis.

   ═══ AXIS B — PERSPECTIVE (who is looking?)
   For the SAME scope, assign multiple agents with DIFFERENT
   verifier_focus values in the SAME batch.  Add a "synthesizer"
   task in the NEXT batch to merge their findings.

     Used for: high-risk verification, security audits, multi-faceted
     analysis, adversarial review — any situation where a single
     viewpoint risks missing something important.
     Example:
       Batch 1: security-reviewer + perf-reviewer + correctness-reviewer
       Batch 2 (depends on Batch 1): synthesizer (aggregate all reviews)

   ═══ AXIS C — DEPTH (how deep do we go?)
   For goals where the answer is NOT known upfront, use iterative
   deepening:
     Pass 1 — BREADTH: cover the surface, identify key areas.
     Pass 2 — DEPTH: drill into the most important findings.
     Pass 3 — VERIFY: cross-check and consolidate conclusions.

     Set verifier_focus="exploration" and max_cycles ≥ 3.
     The engine will keep re-running the batch until no NEW findings
     emerge (loop-until-dry).

   ═══ COMBINING AXES
   - A BUILD project (code, docs): Axis A dominates. Axis C optional.
   - A RESEARCH project: Axis A + C. Axis B for critical claims.
   - An AUDIT / REVIEW: Axis B dominates. Axis A for scope.
   - Use ONLY the axes that fit.  Not every goal needs all three.

5. Assign an appropriate ROLE to each subtask:
   - "developer"   — writing code
   - "tester"      — writing tests
   - "reviewer"    — code review
   - "researcher"  — research/analysis
   - "writer"      — documentation/writing
   - "formatter"   — code formatting
   - "evaluator"   — quality evaluation
   - "synthesizer" — merge multiple research results into structured conclusions

6. Group tasks into **batches**.  A batch is a DEPENDENCY BARRIER:
   all tasks in a batch must finish before the next batch starts.
   Put tasks in the SAME batch when they can run independently.
   Put tasks in DIFFERENT batches when one MUST wait for another.
   Use "depends_on_batch" to declare batch-level ordering.

   IMPORTANT — You do NOT control parallelism.  The engine decides
   how many tasks run simultaneously (respecting a global agent limit).
   You only declare WHAT depends on WHAT.  If tasks have no dependency,
   put them in the same batch — the engine parallelizes them for you.

7. Use "depends_on_index" / "depends_on_indices" for task-level
   dependencies WITHIN a batch.  -1 means no dependency.

8. Use "verifier_focus" to control what the Verifier checks:
   - "correctness"    — output is accurate (default for code)
   - "security"       — security vulnerabilities
   - "performance"    — performance implications
   - "completeness"   — covers all requirements
   - "exploration"    — unexplored directions remain (for Axis C)
   - "style"          — code style / conventions
   - "sources"        — claims are properly sourced (for research)

9. Use "max_cycles" per-batch to limit retry/exploration loops
   (default 1, max 10).  For Axis C (exploration), set max_cycles ≥ 3.

10. Set "use_dw": true only for tasks that need multi-perspective
    verification (Axis B).  DEFAULT: false.

OUTPUT FORMAT (pure JSON array, no markdown):
[
  {
    "title": "subtask title",
    "description": "detailed instructions for the worker agent",
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

CRITICAL — Before your final response, verify your JSON:
- Every string is properly closed with double quotes.
- Every object/array element is separated by commas.
- No trailing commas after the last element.
- The outermost structure is a JSON array [ ... ].
- No text, explanation, or markdown outside the JSON array.`, goal)
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
	if (len(model) == 0 || model[0] == "") && p.team != nil && p.team.Leader.Model != "" {
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
				ReasoningDecomposerMaxTokens,
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
				if DefaultTeamLog != nil {
					DefaultTeamLog.LeaderRetry(attempt+1, "empty output")
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

		tasks, err := ParsePlanTasks(output)
		if err != nil {
			if attempt < maxRetries {
				if p.loggers != nil {
					p.loggers.Engine("leader.decompose: retrying (attempt %d parse failed: %v)", attempt+1, err)
				}
				if DefaultTeamLog != nil {
					DefaultTeamLog.LeaderRetry(attempt+1, err.Error())
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
		if DefaultTeamLog != nil {
			DefaultTeamLog.LeaderDecompose(goal, mdl, attempt+1, ReasoningDecomposerMaxTokens, result.UsagePrompt, result.UsageCompletion, dur.Seconds(), len(output), false, true)
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
