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

// Verifier is an LLM agent that performs semantic verification of a task's
// output.  It runs AFTER the deterministic Checker passes and has shell_run
// access so it can execute tests and inspect actual code behaviour.
//
// The Verifier checks: requirements coverage, logic correctness, security,
// professional quality, and whether the output meets the original task intent.
type Verifier struct {
	runner       *AgentRunner
	whiteboard   *Whiteboard
	router       *Router
	timeout      time.Duration
	model        string
	customPrompt string
}

// NewVerifier creates a Verifier that spawns an LLM agent for semantic
// verification.  The router is used to resolve tool profiles (ProfileVerify
// by default) and timeouts.
func NewVerifier(wb *Whiteboard, runner *AgentRunner, router *Router, timeout time.Duration, model ...string) *Verifier {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	v := &Verifier{
		runner:     runner,
		whiteboard: wb,
		router:     router,
		timeout:    timeout,
	}
	if len(model) > 0 {
		v.model = model[0]
	}
	return v
}

// WithCustomPrompt sets a prefix prompt for the verifier (e.g. role context).
func (v *Verifier) WithCustomPrompt(p string) *Verifier {
	v.customPrompt = p
	return v
}

// ---------------------------------------------------------------------------
// Verify — LLM-based semantic verification
// ---------------------------------------------------------------------------

// Verify spawns an LLM agent to semantically verify the task's Worker output.
// The agent has shell_run access (ProfileVerify) so it can execute tests,
// run linters, and inspect actual code behaviour.
//
// Returns (passed, retry, feedback, err).  passed=true means the output
// meets all requirements semantically.
func (v *Verifier) Verify(task *Task) (passed bool, retry bool, feedback string, err error) {
	workerOutput, err := v.whiteboard.ReadOutput(task.ID)
	if err != nil {
		return false, false, "", fmt.Errorf("read worker output: %w", err)
	}
	// Read the full inbox so the Verifier sees upstream outputs, templates,
	// and team memory — not just the one-line task description.
	inbox, _ := v.whiteboard.ReadInput(task.ID)

	workdir := task.Workdir
	if workdir == "" {
		workdir = "."
	}

	// Build the Verifier prompt.
	var prompt string
	if v.customPrompt != "" {
		prompt = v.customPrompt + "\n\n---\n\n"
	}
	if task.Role.IsContentRole() {
		prompt += BuildVerifierContentPrompt(task, workerOutput, inbox)
	} else {
		prompt += BuildVerifierSemanticPrompt(task, workerOutput, inbox)
	}

	// If the Worker output is short, the real deliverable is on disk.
	if len(workerOutput) < 500 {
		prompt += fmt.Sprintf(`

NOTE: The worker output above is very short (%d chars).  The actual
deliverable was likely written to files in the workspace.  Use list_dir
and read_file to explore the working directory (%s) — look for recently
created or modified .md, .go, .py, or other project files.  Check
those files against the requirements, not the empty output above.
`, len(workerOutput), workdir)
	}

	// Resolve tool profile and timeout via router.
	profile := v.router.ResolveProfile(task.Role, task.Description, true)
	timeout := time.Duration(v.router.ResolveTimeout(task.Role, true)) * time.Second
	model := v.model

	// Spawn the Verifier LLM agent.
	result := v.runner.RunVerifier(prompt, workdir, timeout, profile, model)

	output := result.Stdout

	// Lazy verdict detection: if the Verifier didn't actually run tools, retry.
	if v.isLazyVerdict(output) {
		hardenedPrompt := "YOUR PREVIOUS RESPONSE WAS REJECTED — it lacked tool evidence. " +
			"You MUST run at least 2 tools (read_file + shell_run or grep) and report " +
			"their ACTUAL output. A bare PASS/FAIL without tool output will be rejected again.\n\n" +
			prompt
		result2 := v.runner.RunVerifier(hardenedPrompt, workdir, timeout, profile, model)
		output = result2.Stdout
	}

	// Write verifier result to whiteboard.
	if err := v.whiteboard.WriteVerifier(task.ID, output); err != nil {
		return false, false, output, fmt.Errorf("write verifier result: %w", err)
	}

	// Parse verdict.
	passed, retry = parseVerdict(output)

	return passed, retry, output, nil
}

// ---------------------------------------------------------------------------
// Lazy verdict detection
// ---------------------------------------------------------------------------

func (v *Verifier) isLazyVerdict(output string) bool {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) < 100 {
		return true
	}
	upper := strings.ToUpper(trimmed)
	if !strings.Contains(upper, "TOOLS USED:") {
		return true
	}
	re := regexp.MustCompile(`TOOLS USED:.*(read_file|list_dir|shell_run|grep|search_files|web_search|web_fetch|fetch)`)
	if !re.MatchString(trimmed) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Verdict parsing
// ---------------------------------------------------------------------------

func parseVerdict(output string) (passed bool, retry bool) {
	// Handle "VERDICT: PASS", "**VERDICT: ✅ PASS**", "VERDICT:  PASS", etc.
	verdictMatch := regexp.MustCompile(`(?i)VERDICT:\s*\*{0,2}[\s\p{S}\p{P}]*?(PASS|FAIL|RETRY)`).FindStringSubmatch(output)
	if len(verdictMatch) >= 2 {
		switch verdictMatch[1] {
		case "PASS":
			return true, false
		case "RETRY":
			return false, true
		default:
			return false, false
		}
	}
	// Fallback: search for PASS/FAIL/RETRY after VERDICT, skipping
	// up to 30 chars of formatting (emoji, bold markers, spaces).
	upper := strings.ToUpper(output)
	if idx := strings.Index(upper, "VERDICT"); idx >= 0 {
		tail := upper[idx:]
		if len(tail) > 50 {
			tail = tail[:50]
		}
		if strings.Contains(tail, "PASS") {
			return true, false
		}
		if strings.Contains(tail, "RETRY") {
			return false, true
		}
	}
	return false, false
}

// ---------------------------------------------------------------------------
// Prompts — semantic focus (checker already verified build/lint/files)
// ---------------------------------------------------------------------------

// BuildVerifierSemanticPrompt creates a prompt for the Verifier LLM agent.
// The deterministic Checker has already verified build/lint/file-existence,
// so the Verifier focuses on SEMANTIC concerns.
func BuildVerifierSemanticPrompt(task *Task, workerOutput, inbox string) string {
	// Include the full inbox if available — it contains upstream outputs,
	// templates, team memory, and other context the Worker was given.
	var inboxSection string
	if inbox != "" {
		inboxSection = fmt.Sprintf("\n\nFULL TASK INBOX (what the Worker was given):\n%s", inbox)
	}

	return fmt.Sprintf(`You are a Semantic Verifier. The deterministic Checker has already verified:
- Output files exist
- Code compiles / builds
- Format / lint checks pass
- All referenced files exist

Your job is to check DEEPER, SEMANTIC concerns that mechanical checks cannot catch.  Read
the FULL TASK INBOX below — it contains upstream outputs, templates, and team memory that
the Worker was expected to read and follow.  Verify that the Worker's output satisfies ALL
of it, not just the one-line task description.

ORIGINAL TASK:
%s%s

WORKER OUTPUT:
%s

SEMANTIC CHECKLIST:
1. INBOX REQUIREMENTS: Read the inbox carefully. Does the output satisfy every requirement
   stated there? Note any upstream outputs the Worker was told to read — did they actually
   read and incorporate them?
2. LOGIC CORRECTNESS: Is the logic correct? Are edge cases handled?
3. SECURITY: Any vulnerabilities? (injection, auth bypass, leaked secrets, unsafe patterns)
4. PROFESSIONAL QUALITY: Is this production-ready? Error handling? Documentation? Tests?
5. CONSISTENCY: Does the output contradict itself, the inbox templates, or established conventions?
%s

IMPORTANT — TOOL-GROUNDED VERIFICATION:
- Use shell_run to execute tests (go test, pytest, npm test, cargo test)
- Use shell_run to run the actual code and verify its behaviour
- Use grep / search_files to find patterns (security issues, missing error handling)
- Use read_file to inspect actual code, not just the Worker's summary
- Report ACTUAL command output as evidence — NOT your opinion

OUTPUT FORMAT:
TOOLS USED: [list all commands you ran, e.g. "shell_run: go test ./..."]
VERDICT: PASS|FAIL|RETRY
EVIDENCE: [actual tool output excerpts]
ISSUES:
- [list specific issues found, or "none" if all pass]

## FINDINGS (structured JSON array)
---json
[
  {"id": "unique-key", "title": "one-line summary", "severity": "critical|major|minor", "evidence": "specific reason"}
]
---
Use stable IDs for cross-round comparison (e.g. "missing-requirement-3" not "issue-1").
`, task.Description, inboxSection, workerOutput, buildFocusSection(task.VerifierFocus))
}

// BuildVerifierContentPrompt creates a Verifier prompt for content roles
// (researcher, writer, formatter, evaluator, synthesizer).
func BuildVerifierContentPrompt(task *Task, workerOutput, inbox string) string {
	var inboxSection string
	if inbox != "" {
		inboxSection = fmt.Sprintf("\n\nFULL TASK INBOX (what the Worker was given):\n%s", inbox)
	}

	return fmt.Sprintf(`You are a Content Verifier. The deterministic Checker has already verified
output files exist. Your job is SEMANTIC verification of the content.  Read
the FULL TASK INBOX — it contains upstream outputs, templates, and context that
the Worker was expected to read and follow.

ORIGINAL TASK:
%s%s

OUTPUT TO VERIFY:
%s

CONTENT CHECKLIST:
1. SUBSTANCE: Concrete facts, data, or analysis? Or mostly filler?
2. INBOX REQUIREMENTS: Read the inbox carefully. Does the output satisfy every
   requirement stated there? Did the Worker incorporate upstream outputs?
3. SOURCES: Are specific sources cited (URLs, dates, publications)?
4. CONTRADICTIONS: Do any statements contradict each other or the task?
5. PLAUSIBILITY: Are any claims obviously impossible?
6. COMPLETENESS: Does it address ALL requirements from the inbox?
%s

TOOL-GROUNDED: Use read_file + list_dir to check files, web_search/fetch to
verify factual claims. Your verdict must be based on external verification.

OUTPUT FORMAT:
TOOLS USED: [list tools you ran]
VERDICT: PASS|RETRY|FAIL
EVIDENCE: [verification evidence from tools]
ISSUES:
- [list specific issues, or "none" if PASS]

## FINDINGS (structured JSON array)
---json
[
  {"id": "unique-key", "title": "one-line summary", "severity": "critical|major|minor", "evidence": "specific reason"}
]
---
`, task.Description, inboxSection, workerOutput, buildFocusSection(task.VerifierFocus))
}

// ---------------------------------------------------------------------------
// Backward-compatible stubs (keep old API surface)
// ---------------------------------------------------------------------------

// Deprecated: Checker.Check with runner is replaced by deterministic Checker.
// NewChecker(wb, timeout) no longer needs a runner.
// Use NewChecker(wb, timeout) for deterministic checking.
// Use NewVerifier(wb, runner, router, timeout, model...) for LLM verification.

// Deprecated: BuildVerifierPrompt kept for external callers.
// Use BuildVerifierSemanticPrompt instead.
func BuildVerifierPrompt(task *Task, workerOutput string) string {
	return BuildVerifierSemanticPrompt(task, workerOutput, "")
}

// Deprecated: BuildContentVerifierPrompt kept for external callers.
// Use BuildVerifierContentPrompt instead.
func BuildContentVerifierPrompt(task *Task, workerOutput string) string {
	return BuildVerifierContentPrompt(task, workerOutput, "")
}

// ---------------------------------------------------------------------------
// Shared helpers (moved from old checker.go)
// ---------------------------------------------------------------------------

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

func ParseFindings(output string) []Finding {
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

// ExtractSection extracts a named markdown section (## Title) from content.
func ExtractSection(content, sectionTitle string) string {
	heading := "## " + sectionTitle
	idx := strings.Index(content, heading)
	if idx < 0 {
		return ""
	}
	body := content[idx+len(heading):]
	nextSection := regexp.MustCompile(`\n## `).FindStringIndex(body)
	if nextSection != nil {
		body = body[:nextSection[0]]
	}
	return strings.TrimSpace(body)
}
