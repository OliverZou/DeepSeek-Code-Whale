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
	verifyDir  string
	LastPrompt string
	// LastSystemPrompt captures the assembled extra system-prompt content the
	// verifier ran with (adapter only; "" for shell) — logged for auditability.
	LastSystemPrompt string
	// LastPromptTokens/LastCompletionTokens record the tokens consumed by the
	// most recent Verify run (worker+verifier accounting; 0 when unavailable).
	LastPromptTokens     int
	LastCompletionTokens int
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

// WithVerifyDir sets the directory where the Verifier writes its verification
// artifacts (acceptance-level black-box tests, checklists). It lives beside the
// deliverable out/ dir and is not propagated to the user workspace.
func (v *Verifier) WithVerifyDir(dir string) *Verifier {
	v.verifyDir = dir
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

	verifyDir := v.verifyDir
	if verifyDir == "" {
		verifyDir = filepath.Join(workdir, "verify")
	}

	desc := stripVerifierFeedback(task.Description)
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`TASK:
%s

WORKER OUTPUT (%d chars):
%s
`, desc, len(workerOutput), truncateStr(workerOutput, 3000)))

	if len(task.AcceptanceCriteria) > 0 {
		b.WriteString("\nACCEPTANCE CRITERIA（独立验收标准，逐条核对）:\n")
		for i, c := range task.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, c))
		}
	}

	// Only include the upstream-output section (## 📥) — the verifier needs to
	// know which upstream files to check for integration, but not the worker's
	// template (## 📄), role memory (## 🧠), or retry feedback (## 🔄).
	if upstream := extractUpstreamSection(inbox); upstream != "" {
		b.WriteString(fmt.Sprintf(`
UPSTREAM CONTEXT:
%s
`, upstream))
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

	b.WriteString(fmt.Sprintf(`

VERIFICATION METHOD (独立验证，不依赖 worker 的结论转述):
1. 基于 ACCEPTANCE CRITERIA 逐条独立验收，判断交付物是否满足每一条。
2. 按交付物性质选择验证手段（以比重新执行任务更低的成本）：
   - 可执行代码/脚本：编写并运行黑盒测试（断言行为符合契约），用 write
     工具直接写文件到验证产物目录 %s 下（禁止用 shell 命令落盘，如
     mkdir/cat heredoc——Windows 下会写出 -p 目录等垃圾文件）；审查 worker
     的单元测试是否覆盖边界/异常分支——只报告遗漏，不替 worker 补写。
   - 文档/研究/设计/其他：逐条核对验收标准，核查事实、引用、逻辑一致性。
3. 所有结论必须能被文件内容或工具输出证明，不凭空臆断。

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
`, verifyDir))

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
	vIters, vCalls, vTokens := iterationBudget(task.Complexity, true)

	v.LastPrompt = prompt
	result := v.runner.RunVerifier(prompt, workdir, timeout, vIters, vCalls, vTokens, v.agentName, model)
	v.LastPromptTokens = result.UsagePrompt
	v.LastCompletionTokens = result.UsageCompletion
	v.LastSystemPrompt = result.SystemPrompt

	output := result.Stdout

	// Lazy verdict detection.
	if v.isLazyVerdict(output) {
		hardenedPrompt := "YOUR PREVIOUS RESPONSE WAS REJECTED — it lacked tool evidence. " +
			"You MUST run at least 2 tools (read_file + shell_run or grep) and report " +
			"their ACTUAL output. A bare PASS/FAIL without tool output will be rejected again.\n\n" +
			prompt
		result2 := v.runner.RunVerifier(hardenedPrompt, workdir, timeout, vIters, vCalls, vTokens, v.agentName, model)
		v.LastPromptTokens += result2.UsagePrompt
		v.LastCompletionTokens += result2.UsageCompletion
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

// extractUpstreamSection pulls only the "## 📥 上游产出" block out of an inbox
// document, discarding the task description, template, memory, and retry-feedback
// sections.  The verifier only needs the upstream file list to check integration;
// everything else in the inbox is worker-scoped context it should not re-read.
func extractUpstreamSection(inbox string) string {
	if inbox == "" {
		return ""
	}
	idx := strings.Index(inbox, "## 📥")
	if idx < 0 {
		return ""
	}
	body := inbox[idx:]
	if next := regexp.MustCompile(`\n## `).FindStringIndex(body); next != nil {
		body = body[:next[0]]
	}
	return strings.TrimSpace(body)
}
