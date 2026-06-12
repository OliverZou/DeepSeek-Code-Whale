package team_engine

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Verifier provides adversarial checking of Worker output.
//
// The Verifier wraps an AgentRunner and uses a structured prompt to
// critically examine a Worker's output against the original task
// requirements, using a Whale subagent instead of an external CLI.
type Verifier struct {
	runner     *AgentRunner
	whiteboard *Whiteboard
	timeout    time.Duration // subagent timeout (from config)
	model      string        // LLM model name; "" = Whale default
}

// NewVerifier creates a Verifier that uses the given runner and whiteboard.
// timeout is the subagent timeout (0 = use RunVerifier's default 120s).
func NewVerifier(wb *Whiteboard, runner *AgentRunner, timeout time.Duration, model ...string) *Verifier {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	v := &Verifier{
		runner:     runner,
		whiteboard: wb,
		timeout:    timeout,
	}
	if len(model) > 0 {
		v.model = model[0]
	}
	return v
}

// ---------------------------------------------------------------------------
// Prompt generation
// ---------------------------------------------------------------------------

// BuildVerifierPrompt generates the adversarial check prompt for code tasks
// (developer, tester, reviewer).
func BuildVerifierPrompt(task *Task, workerOutput string) string {
	return fmt.Sprintf(`
You are a Verifier Agent. Your job is to critically examine the Worker's output
and determine if it meets all requirements.

ORIGINAL TASK:
%s

WORKER OUTPUT:
%s

CHECKLIST:
1. Does the output fulfil ALL requirements from the original task?
2. Are there any bugs, errors, or omissions?
3. Is the code/artifact complete and production-ready?
4. Are there any security concerns?
5. Does it follow project conventions?
%s

IMPORTANT — TOOL-GROUNDED VERIFICATION:
Your verdict MUST be based on actual external tool execution, NOT on your own reasoning.
- For code tasks: use shell_run to execute tests (go test, pytest, etc.), linter, or build
- For format checks: run formatter or style checker via shell_run
- For security: run scanners or grep for known patterns
- Report ACTUAL command output as evidence, not your opinion.

OUTPUT FORMAT:
TOOLS USED: [list all commands you ran, e.g. "shell_run: go test ./..."]
VERDICT: PASS|FAIL
EVIDENCE: [actual tool output excerpts]
ISSUES:
- [list specific issues, or "none" if PASS]
SUGGESTIONS:
- [optional improvement suggestions]
`, task.Description, workerOutput, buildFocusSection(task.VerifierFocus))
}

// BuildContentVerifierPrompt generates a verification prompt for
// content-oriented tasks (researcher, writer, formatter, evaluator).
func BuildContentVerifierPrompt(task *Task, workerOutput string) string {
	return fmt.Sprintf(`
You are a Content Verifier. Critically examine this output against the
original task. Be skeptical — challenge weak claims, unverifiable data,
and logical inconsistencies.

ORIGINAL TASK:
%s

OUTPUT TO VERIFY:
%s

CHECKLIST:
1. SUBSTANCE: Does the output contain concrete facts, data, or analysis?
   Or is it mostly filler (plans to do later, vague promises, "I will...")?
2. SOURCES: Are specific sources cited (URLs, dates, named publications)?
   Flag any unsourced claims that should be verifiable.
3. CONTRADICTIONS: Do any statements contradict each other or the original task?
4. PLAUSIBILITY: Are any claims obviously impossible? (future dates, impossible
   numbers, physically/logically contradictory statements)
5. COMPLETENESS: Does it address ALL requirements from the original task?
%s

TOOL-GROUNDED: Use web_search or fetch to verify claims, check citations,
and confirm facts. Your verdict must be based on actual external
verification, not subjective judgment.

OUTPUT FORMAT:
TOOLS USED: [list searches/fetches you ran]
VERDICT: PASS|FAIL
SUBSTANCE: [substantial|thin|empty]
EVIDENCE: [verification evidence from tools]
ISSUES:
VERDICT: PASS|FAIL
SUBSTANCE: [substantial|thin|empty]
ISSUES:
- [list specific issues, or "none" if PASS]
`, task.Description, workerOutput, buildFocusSection(task.VerifierFocus))
}

// buildFocusSection generates an emphasis block for specified verification
// focus areas.  Returns empty string when no focus is specified.
func buildFocusSection(focus string) string {
	if focus == "" {
		return ""
	}
	areas := strings.Split(focus, ",")
	var b strings.Builder
	b.WriteString("\n**FOCUS AREAS (Leader specified):**\n")
	for _, area := range areas {
		a := strings.TrimSpace(area)
		switch a {
		case "correctness":
			b.WriteString("- **Correctness**: Is the output accurate? Any data or logic errors?\n")
		case "security":
			b.WriteString("- **Security**: Any security vulnerabilities?\n")
		case "completeness":
			b.WriteString("- **Completeness**: Does it cover ALL requirements?\n")
		case "sources":
			b.WriteString("- **Sources**: Are information sources reliable and cited?\n")
		case "plausibility":
			b.WriteString("- **Plausibility**: Are data and conclusions reasonable?\n")
		case "contradictions":
			b.WriteString("- **Consistency**: Any internal contradictions?\n")
		default:
			b.WriteString(fmt.Sprintf("- **%s**: Please focus on this dimension\n", a))
		}
	}
	return b.String()
}

// Verify runs the Verifier subagent against the Worker's output.
//
// It reads the Worker output from the whiteboard, constructs the
// verification prompt, executes the Verifier subagent, and parses the
// VERDICT: PASS|FAIL line.
//
// Returns (passed, feedback).
func (v *Verifier) Verify(task *Task) (bool, string, error) {
	workerOutput, err := v.whiteboard.ReadOutput(task.ID)
	if err != nil {
		return false, "", fmt.Errorf("read worker output: %w", err)
	}

	// Choose the right verification prompt based on task role.
	var prompt string
	if task.Role.IsContentRole() {
		prompt = BuildContentVerifierPrompt(task, workerOutput)
	} else {
		prompt = BuildVerifierPrompt(task, workerOutput)
	}

	workdir := task.Workdir
	if workdir == "" {
		workdir = "."
	}

	result := v.runner.RunVerifier(prompt, workdir, v.timeout, v.model)

	output := result.Stdout
	// Persist the verifier result to the whiteboard.
	if err := v.whiteboard.WriteVerifier(task.ID, output); err != nil {
		return false, output, fmt.Errorf("write verifier result: %w", err)
	}

	// Parse the VERDICT line (case-insensitive).
	// The verifier agent may embed markdown formatting like **VERDICT:** PASS
	// or **VERDICT: PASS** — allow optional ** around/between the label and verdict.
	verdictMatch := regexp.MustCompile(`(?i)VERDICT:\s*\*{0,2}\s*(PASS|FAIL)`).FindStringSubmatch(output)
	passed := false
	if len(verdictMatch) >= 2 {
		passed = verdictMatch[1] == "PASS"
	}
	// Full-text fallback: search for "VERDICT: PASS" literally (ignores all formatting).
	if !passed {
		upper := strings.ToUpper(output)
		passed = strings.Contains(upper, "VERDICT: PASS") || strings.Contains(upper, "VERDICT:  PASS")
	}
	// No clear verdict → treat as FAIL to be safe.

	return passed, output, nil
}
