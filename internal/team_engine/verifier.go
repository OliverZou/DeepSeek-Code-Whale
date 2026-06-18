package team_engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	runner       *AgentRunner
	whiteboard   *Whiteboard
	timeout      time.Duration // subagent timeout (from config)
	model        string        // LLM model name; "" = Whale default
	customPrompt string        // team-configured verifier prompt (from team.yaml)
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

// WithCustomPrompt sets a team-configured verifier prompt that is prepended
// to the default verification prompt.  Use "" to clear.
func (v *Verifier) WithCustomPrompt(p string) *Verifier {
	v.customPrompt = p
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

CRITICAL — INDEPENDENT VERIFICATION:
1. The Worker often MISREPORTS its own results. Workers say "failed" when
   code is actually correct, or "done" when code is broken. NEVER trust
   their self-assessment. You are the INDEPENDENT JUDGE.
2. You MUST execute at least 2 different tools (read_file + one of
   shell_run / grep / list_dir) before giving ANY verdict.  Verify the
   actual files on disk — do not echo the Worker's conclusions.
3. If your TOOLS USED list is empty, or if your EVIDENCE just repeats the
   Worker's words, your verdict is INVALID and will be rejected automatically.

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

## FINDINGS (structured JSON array)
---json
[
  {"id": "unique-key", "title": "one-line summary", "severity": "critical|major|minor", "evidence": "specific reason"}
]
---
Use stable IDs for cross-round comparison (e.g. "missing-section-3" not "issue-1").
`, task.Description, workerOutput, buildFocusSection(task.VerifierFocus))
}

// BuildContentVerifierPrompt generates a verification prompt for
// content-oriented tasks (researcher, writer, formatter, evaluator).
func BuildContentVerifierPrompt(task *Task, workerOutput string) string {
	return fmt.Sprintf(`
You are a Content Verifier. Critically examine this output against the
original task. Be skeptical — challenge weak claims, unverifiable data,
and logical inconsistencies.

CRITICAL — INDEPENDENT VERIFICATION:
1. The Worker often MISREPORTS its own results. Workers say "failed" when
   output is actually correct, or "done" when output is broken. NEVER trust
   their self-assessment. You are the INDEPENDENT JUDGE.
2. You MUST execute at least 2 different tools (read_file + one of
   list_dir / grep / web_search) before giving ANY verdict.  Verify the
   actual files and facts on disk — do not echo the Worker's conclusions.
3. If your TOOLS USED list is empty, or if your EVIDENCE just repeats the
   Worker's words, your verdict is INVALID and will be rejected automatically.

IMPORTANT — INCREMENTAL PROGRESS:
Large documents (requirements, architecture, analysis reports) often
cannot be completed in a single pass.  The Worker writes to files on disk.
Use list_dir and read_file to check the actual file content.
If the files show meaningful progress (sections completed, content growing),
and the Worker's output describes continuing the work, respond with:
  VERDICT: RETRY
  SPECIFIC NEXT STEPS: (list 2-3 concrete sections to add, max 100 words each)
Use RETRY instead of FAIL when progress is being made but the task is
not yet complete.  Use FAIL only for empty output, circular loops, or
content that contradicts the original task.

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
   If incomplete but making progress, use RETRY with specific next steps.
%s

TOOL-GROUNDED: Use list_dir and read_file to check files on disk, then
web_search or fetch to verify factual claims.  Your verdict must be
based on actual external verification, not subjective judgment.

OUTPUT FORMAT:
TOOLS USED: [list tools you ran]
VERDICT: PASS|RETRY|FAIL
SUBSTANCE: [substantial|thin|empty]
EVIDENCE: [verification evidence from tools]
ISSUES:
- [list specific issues, or "none" if PASS]

## FINDINGS (structured JSON array)
---json
[
  {"id": "unique-key", "title": "one-line summary", "severity": "critical|major|minor", "evidence": "specific reason"}
]
---
Use stable IDs for cross-round comparison (e.g. "missing-section-3" not "issue-1").
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
func (v *Verifier) Verify(task *Task) (passed bool, retry bool, feedback string, err error) {
	workerOutput, err := v.whiteboard.ReadOutput(task.ID)
	if err != nil {
		return false, false, "", fmt.Errorf("read worker output: %w", err)
	}

	// Prepend team-configured verifier prompt if set.
	var prompt string
	if v.customPrompt != "" {
		prompt = v.customPrompt + "\n\n---\n\n"
	}
	// Choose the right verification prompt based on task role.
	if task.Role.IsContentRole() {
		prompt += BuildContentVerifierPrompt(task, workerOutput)
	} else {
		prompt += BuildVerifierPrompt(task, workerOutput)
	}
	// When the worker output is very short, the actual deliverable is
	// likely in workspace files written via tool calls.  Tell the
	// verifier to explore the workspace instead of judging the empty
	// output.md.
	if len(workerOutput) < 500 {
		wd := task.Workdir
		if wd == "" {
			wd = "."
		}
		prompt += fmt.Sprintf(`

	NOTE: The worker output above is very short (%d chars).  The actual
	deliverable was likely written to files in the workspace.  Use list_dir
	and read_file to explore the working directory (%s) — look for recently
	created or modified .md, .rs, .go, .py, or other project files.  Check
	those files against the requirements, not the empty output above.
	`, len(workerOutput), wd)
	}

	workdir := task.Workdir
	if workdir == "" {
		workdir = "."
	}

	result := v.runner.RunVerifier(prompt, workdir, v.timeout, v.model)

	output := result.Stdout

	// Guard: if the verifier produced a lazy verdict (no tools, no evidence,
	// bare PASS/FAIL), re-run once with a hardened prompt that explicitly
	// demands tool usage.  This prevents the model from echoing the Worker's
	// self-assessment without independent verification.
	if v.isLazyVerdict(output) {
		hardenedPrompt := "YOUR PREVIOUS RESPONSE WAS REJECTED — it lacked tool evidence. " +
			"You MUST run at least 2 tools (read_file + shell_run or grep) and report " +
			"their ACTUAL output. A bare PASS/FAIL without tool output will be rejected again.\n\n" +
			prompt
		result2 := v.runner.RunVerifier(hardenedPrompt, workdir, v.timeout, v.model)
		output = result2.Stdout
	}

	// Persist the verifier result to the whiteboard.
	if err := v.whiteboard.WriteVerifier(task.ID, output); err != nil {
		return false, false, output, fmt.Errorf("write verifier result: %w", err)
	}

	// Parse the VERDICT line (case-insensitive).  Supports PASS, FAIL, RETRY.
	verdictMatch := regexp.MustCompile(`(?i)VERDICT:\s*\*{0,2}\s*(PASS|FAIL|RETRY)`).FindStringSubmatch(output)
	upper := strings.ToUpper(output)
	if len(verdictMatch) >= 2 {
		switch verdictMatch[1] {
		case "PASS":
			passed = true
		case "RETRY":
			retry = true
		}
	}
	// Full-text fallback.
	if !passed && !retry {
		passed = strings.Contains(upper, "VERDICT: PASS") || strings.Contains(upper, "VERDICT:  PASS")
		if !passed {
			retry = strings.Contains(upper, "VERDICT: RETRY")
		}
	}
	// No clear verdict → treat as FAIL to be safe.

	// File-reference check: verify that all files referenced in output.md
	// actually exist.  Missing referenced files → FAIL.
	if passed && !retry {
		missingFiles := findMissingRefs(workerOutput, workdir)
		if len(missingFiles) > 0 {
			passed = false
			output += fmt.Sprintf("\n\nVERDICT: FAIL — 引用的文件不存在:\n- %s", strings.Join(missingFiles, "\n- "))
		}
	}

	return passed, retry, output, nil
}

// findMissingRefs parses markdown links from output and returns paths of
// referenced files that don't exist on disk.
func findMissingRefs(output, workdir string) []string {
	re := regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)
	matches := re.FindAllStringSubmatch(output, -1)
	var missing []string
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		ref := strings.TrimSpace(m[2])
		if ref == "" || strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			continue
		}
		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(workdir, path)
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, ref)
		}
	}
	return missing
}

// isLazyVerdict detects verifier responses that lack independent tool evidence.
// A "lazy" verdict is one where the LLM echoed the Worker's self-assessment
// without running any tools — typically very short and missing the TOOLS USED
// section required by the prompt.
func (v *Verifier) isLazyVerdict(output string) bool {
	trimmed := strings.TrimSpace(output)
	// Too short to contain tool evidence — the required format alone
	// (TOOLS USED + VERDICT + EVIDENCE + ISSUES + FINDINGS) is >100 chars.
	if len(trimmed) < 100 {
		return true
	}
	// The prompt mandates a "TOOLS USED:" section.  Missing it is a strong
	// signal that the model skipped verification.
	upper := strings.ToUpper(trimmed)
	if !strings.Contains(upper, "TOOLS USED:") {
		return true
	}
	// Must mention at least one recognizable tool name.
	re := regexp.MustCompile(`TOOLS USED:.*(read_file|list_dir|shell_run|grep|search_files|web_search|web_fetch|fetch)`)
	if !re.MatchString(trimmed) {
		return true
	}
	return false
}

// ParseFindings extracts structured Finding objects from verifier output.
func ParseFindings(output string) []Finding {
	// Find the FINDINGS section and extract the JSON array from it,
	// using the same balanced-bracket + repair logic as the decompose path.
	idx := strings.Index(output, "## FINDINGS")
	if idx < 0 {
		idx = strings.Index(output, "FINDINGS")
	}
	if idx < 0 {
		return nil
	}
	section := output[idx:]
	jsonStr := extractJSON(section)
	if jsonStr == "" {
		return nil
	}
	// Try parsing; if truncated, repair and retry.
	var findings []Finding
	if err := json.Unmarshal([]byte(jsonStr), &findings); err != nil {
		repaired := repairJSON(jsonStr)
		if repaired != jsonStr {
			if err2 := json.Unmarshal([]byte(repaired), &findings); err2 != nil {
				return nil
			}
		} else {
			return nil
		}
	}
	return findings
}
