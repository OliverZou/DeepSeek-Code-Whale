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
// output.  The agent definition (from .md or builtin) provides methodology,
// persona, tools, and skills.
type Verifier struct {
	runner     *AgentRunner
	whiteboard *Whiteboard
	router     *Router
	timeout    time.Duration
	model      string
	agentName  string
	workdir    string
	LastPrompt string
}

// NewVerifier creates a Verifier.
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
func (v *Verifier) WithAgentName(name string) *Verifier {
	v.agentName = name
	return v
}

// WithWorkdir overrides the directory the Verifier inspects. By default the
// Verifier inspects task.Workdir (the original workspace); callers that run the
// Worker in a sandboxed out/ directory must point the Verifier there instead,
// otherwise it looks for deliverables in the wrong place.
func (v *Verifier) WithWorkdir(wd string) *Verifier {
	v.workdir = wd
	return v
}

// ---------------------------------------------------------------------------
// BuildPrompt — task context for the Verifier agent
// ---------------------------------------------------------------------------

// BuildPrompt constructs the Verifier's task prompt from Worker output and
// task context.  The agent's system prompt (from .md or builtin) provides
// verification methodology and persona — this is task-context only.
func (v *Verifier) BuildPrompt(task *Task) string {
	workerOutput, _ := v.whiteboard.ReadOutput(task.ID)
	inbox, _ := v.whiteboard.ReadInput(task.ID)

	workdir := v.workdir
	if workdir == "" {
		workdir = task.Workdir
	}
	if workdir == "" {
		workdir = "."
	}

	desc := stripVerifierFeedback(task.Description)
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`TASK:
%s

WORKER OUTPUT (%d chars):
%s
`, desc, len(workerOutput), truncateStr(workerOutput, 3000)))

	// Only include inbox when it carries upstream outputs, templates,
	// or memory — not when it's just a duplicate of the TASK section.
	if inbox != "" && (strings.Contains(inbox, "## 📥") ||
		strings.Contains(inbox, "## 📄") ||
		strings.Contains(inbox, "## 🧠") ||
		strings.Contains(inbox, "## 🔄")) {
		b.WriteString(fmt.Sprintf(`
UPSTREAM CONTEXT:
%s
`, inbox))
	}

	if task.VerifierFocus != "" {
		b.WriteString(buildFocusSection(task.VerifierFocus))
	}

	if len(workerOutput) < 500 {
		b.WriteString(fmt.Sprintf(`
NOTE: The worker output is very short (%d chars).  The actual deliverable
was likely written to files in %s.  Use list_dir and read_file to
explore — look for recently created/modified files.
`, len(workerOutput), workdir))
	}

	b.WriteString(`

VERIFICATION METHOD (follow this order — do NOT author a new test suite):
1. The worker's own automated tests (go test / node --test / npm test) were
   already run by an objective gate. Do NOT write a fresh test suite from
   scratch — it is slow and its expectations are often wrong.
2. Read the deliverable files and check they match the TASK requirements.
   Look for completeness, correctness, and obvious bugs.
3. Only report issues you can prove with file contents or tool output.

OUTPUT FORMAT (REQUIRED):
VERDICT: PASS | FAIL | RETRY
ISSUES:
- [specific issues, or "none" if PASS]

## FINDINGS (structured JSON — only issues that justify FAIL/RETRY)
---json
[
  {"id": "unique", "title": "one-line summary", "severity": "critical|major|minor", "evidence": "tool output"}
]
---
`)

	v.LastPrompt = b.String()
	return v.LastPrompt
}

// ---------------------------------------------------------------------------
// Verify — LLM-based semantic verification
// ---------------------------------------------------------------------------

// Verify spawns an LLM agent to semantically verify the task's Worker output.
// Returns (passed, retry, feedback, err).
func (v *Verifier) Verify(task *Task) (passed bool, retry bool, feedback string, err error) {
	prompt := v.BuildPrompt(task)
	workdir := v.workdir
	if workdir == "" {
		workdir = task.Workdir
	}
	if workdir == "" {
		workdir = "."
	}

	timeout := time.Duration(v.router.ResolveTimeout(task.Role, true)) * time.Second
	model := v.model

	v.LastPrompt = prompt
	result := v.runner.RunVerifier(prompt, workdir, timeout, v.agentName, model)

	output := result.Stdout

	// Lazy verdict detection.
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

	passed, retry = parseVerdict(output)
	return passed, retry, output, nil
}

// ---------------------------------------------------------------------------
// Lazy verdict detection
// ---------------------------------------------------------------------------

func (v *Verifier) isLazyVerdict(output string) bool {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) == 0 {
		return true
	}
	// A bare PASS/FAIL with no evidence of having inspected the deliverable is
	// lazy. Accept any tool name or a concrete file reference as evidence.
	// No longer require the "TOOLS USED:" label or a minimum length — a short
	// but tool-backed verdict is legitimate, and the label itself was forcing
	// verifiers to pad their reports.
	re := regexp.MustCompile(`(?i)(read_file|list_dir|shell_run|grep|search_files|web_search|web_fetch|fetch|\b[a-zA-Z0-9_./-]+\.(go|js|css|html|py|rs|ts|tsx|java|json|md)\b)`)
	if !re.MatchString(trimmed) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Verdict parsing
// ---------------------------------------------------------------------------

func parseVerdict(output string) (passed bool, retry bool) {
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

// hasVerdictMarkers reports whether output carries any of the format markers a
// Verifier's required output format mandates (VERDICT:, ## FINDINGS, or the
// ---json block).  A response with none of them did not come from a functioning
// Verifier (e.g. a spawn error string like "error: unknown flag: --persist"),
// so it must never auto-pass.
func hasVerdictMarkers(output string) bool {
	upper := strings.ToUpper(output)
	return strings.Contains(upper, "VERDICT") ||
		strings.Contains(upper, "FINDINGS") ||
		strings.Contains(upper, "---JSON")
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
	// Look for FINDINGS section or bare ---json block.
	section := ""
	if idx := strings.Index(output, "## FINDINGS"); idx >= 0 {
		section = output[idx:]
	} else if idx := strings.Index(output, "FINDINGS"); idx >= 0 {
		section = output[idx:]
	} else if idx := strings.Index(output, "---json"); idx >= 0 {
		section = output[idx:]
	}
	if section == "" {
		return nil
	}
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
