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

11. Set "use_dw": true for tasks that benefit from multi-perspective verification:
    - Security-critical code, correctness-critical logic, or multi-faceted reviews
    - When verifier_focus includes multiple dimensions (security,correctness,completeness)
    - DEFAULT: false (single verifier is sufficient for simple/formatting/minor tasks)

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

// DecomposeTask re-decomposes a single task that exhausted retries into smaller subtasks.
func (p *Planner) DecomposeTask(task *Task, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
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
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")
	allMatches := re.FindAllStringSubmatch(output, -1)
	for i := len(allMatches) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(allMatches[i][1])
		if strings.HasPrefix(candidate, "[") {
			return candidate
		}
	}

	re = regexp.MustCompile(`(?s)\[.*\]`)
	allRaw := re.FindAllString(output, -1)
	for i := len(allRaw) - 1; i >= 0; i-- {
		if strings.HasPrefix(allRaw[i], "[") {
			return allRaw[i]
		}
	}

	return ""
}

// repairJSON fixes common LLM JSON mistakes.
func repairJSON(s string) string {
	if idx := strings.LastIndex(s, "]"); idx > 0 {
		s = s[:idx+1]
	}
	s = regexp.MustCompile(`,(\s*[}\]])`).ReplaceAllString(s, "$1")
	s = regexp.MustCompile(`"\s+"`).ReplaceAllString(s, `", "`)
	open := strings.Count(s, "[")
	close := strings.Count(s, "]")
	for close < open {
		s += "]"
		close++
	}
	return s
}
