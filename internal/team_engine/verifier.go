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
	runner     *AgentRunner
	whiteboard *Whiteboard
	router     *Router
	timeout    time.Duration
	model      string
	agentName  string // agent definition name (e.g. "review", "verifier")
	LastPrompt string
}

// NewVerifier creates a Verifier that spawns an LLM agent for semantic
// verification.  The router is used to resolve timeouts.
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

// WithAgentName sets the agent definition name for the verifier.
// When set, the agent definition's system prompt, tools, and skills
// are injected by the spawner.  When empty, falls back to ProfileVerify tools.
func (v *Verifier) WithAgentName(name string) *Verifier {
	v.agentName = name
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

	// Build the Verifier prompt — task context only.
	// The agent's system prompt (from .md or builtin) provides verification
	// methodology, persona, and output format instructions.
	desc := stripVerifierFeedback(task.Description)
	var promptBuilder strings.Builder
	promptBuilder.WriteString(fmt.Sprintf(`TASK:
%s

WORKER OUTPUT (%d chars):
%s
`, desc, len(workerOutput), truncateStr(workerOutput, 8000)))

	if inbox != "" {
		promptBuilder.WriteString(fmt.Sprintf(`
UPSTREAM CONTEXT:
%s
`, inbox))
	}

	if task.VerifierFocus != "" {
		promptBuilder.WriteString(buildFocusSection(task.VerifierFocus))
	}

	// If the Worker output is short, the real deliverable is on disk.
	if len(workerOutput) < 500 {
		promptBuilder.WriteString(fmt.Sprintf(`
NOTE: The worker output is very short (%d chars).  The actual deliverable
was likely written to files in %s.  Use list_dir and read_file to
explore — look for recently created/modified files.
`, len(workerOutput), workdir))
	}

	prompt := promptBuilder.String()

	// Resolve timeout via router.
	timeout := time.Duration(v.router.ResolveTimeout(task.Role, true)) * time.Second
	model := v.model

	// Spawn the Verifier LLM agent.
	v.LastPrompt = prompt
	result := v.runner.RunVerifier(prompt, workdir, timeout, v.agentName, model)

	output := result.Stdout

	// Lazy verdict detection: if the Verifier didn't actually run tools, retry.
	if v.isLazyVerdict(output) {
		hardenedPrompt := "YOUR PREVIOUS RESPONSE WAS REJECTED — it lacked tool evidence. " +
			"You MUST run at least 2 tools (read_file + shell_run or grep) and report " +
			"their ACTUAL output. A bare PASS/FAIL without tool output will be rejected again.\n\n" +
			prompt
		result2 := v.runner.RunVerifier(hardenedPrompt, workdir, timeout, v.agentName, model)
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
// Shared helpers
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
