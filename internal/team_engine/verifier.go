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
	// LastSystemPrompt captures the assembled extra system-prompt content the
	// verifier ran with (adapter only; "" for shell) — logged for auditability.
	LastSystemPrompt string
	// LastPromptTokens/LastCompletionTokens record the tokens consumed by the
	// most recent Verify run (worker+verifier accounting; 0 when unavailable).
	LastPromptTokens     int
	LastCompletionTokens int
	// Cache split of the last Verify run's prompt tokens (prefix-cache hit vs
	// full-price miss) — feeds the engine's effective-token accounting.
	LastPromptCacheHit  int
	LastPromptCacheMiss int
	// previousReport is the last verification report when this run is a
	// re-review (worker retried after a FAIL): the verifier must only re-verify
	// the items marked failed/unverifiable, not re-verify the whole deliverable.
	previousReport string
	// LastCapRetries counts tool-cap interruptions observed in the last Verify
	// call (including the automatic minimal re-run) — surfaced in run_report
	// as verifier_cap_count for bottleneck analysis.
	LastCapRetries int
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
// Verifier inspects task.Workdir, where the Worker writes its deliverables
// directly.
func (v *Verifier) WithWorkdir(wd string) *Verifier {
	v.workdir = wd
	return v
}

// WithPreviousReport seeds a re-review: the previous verification report is
// injected into the prompt and the verifier is told to re-verify only the
// items it previously marked 未通过/无法验证 — pass items are not re-checked
// (collaboration protocol, and avoids re-paying the same verification cost).
func (v *Verifier) WithPreviousReport(report string) *Verifier {
	v.previousReport = report
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
`, desc, len(workerOutput), truncateStr(workerOutput, 2000)))

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

	if v.previousReport != "" {
		b.WriteString(fmt.Sprintf(`
PREVIOUS VERIFICATION REPORT (attempt before this run):
%s

RE-REVIEW CONTRACT:
- 这是复审轮：只对上一轮标记为「未通过/无法验证」的验收点做复审，已通过项不要重复验证。
- 逐项核对上一轮的问题是否已被修复；修复不完整的分项列出仍缺失的部分。
- 若上一轮全部通过（不应发生），直接对交付物做一次快速确认并说明。
- 不要重新通读全文、不要重跑已通过的整体验证，成本应显著低于首轮。
`, truncateStr(v.previousReport, 2500)))
	}

	b.WriteString(`

VERIFICATION METHOD (独立验证，不依赖 worker 的结论转述; 手段与粒度由你按任务特征判定，引擎不预设):
1. 判定任务特征（可多个），据此选择验证策略：
   - 有客观正确答案（计算、事实查询）→ 独立核算或交叉查证关键结论（不重算全部）。
   - 有明确格式/结构规范 → 逐项核对结构合规性；读一次，用 grep/搜索定位片段。
   - 依赖主观判断（文案、设计）→ 检查是否满足显式约束，对标参考基准，不评判审美偏好。
   - 涉及多步骤链路 → 检查关键环节是否有断裂或遗漏，抽样验证中间产出，不必全链路重走。
   - 不可逆/高影响交付 → 全量核对，不做抽样。
2. 成本纪律：优先选择成本最低且能给出确定性结论的手段；同一文件本次会话最多读一次，
   需要定位时用 grep 搜索；抽样能判定就不要全量；不要重复读取或重跑结论的产出过程。
3. 基于 ACCEPTANCE CRITERIA 逐条独立验收，对每一条明确判定：通过 / 未通过 / 无法验证。
   「无法验证」不等于「通过」——证据不足以判定通过时必须标 FAIL 并说明缺什么。
4. 你是检查者，不是写作者：禁止修改交付物或写入任何文件（含测试脚本）；验证产物
   （命令输出、检查清单）只出现在你的最终报告中。
5. 所有结论必须能被文件内容或工具输出证明，不凭空臆断；引用具体文件/行号/命令输出。

OUTPUT FORMAT (REQUIRED):
VERDICT: PASS | FAIL | RETRY
METHOD: <一句话：任务特征 → 你选用的验证手段与粒度（如：计算类→独立核算关键结论；多步链路→抽样2处）>
ISSUES:
- [specific issues, or "none" if PASS]

## FINDINGS (structured JSON — only issues that justify FAIL/RETRY)
---json
[
  {"id": "unique", "title": "one-line summary", "severity": "critical|major|minor", "evidence": "tool output"}
]
---

篇幅限制（严格遵守）：最终报告整体 ≤ 1200 字；逐项验收表最多 12 行，每行证据只写文件行号/命令输出结论，禁止复制源码或完整日志原文；ISSUES 只列未通过/无法验证项，通过项不逐条重复。`)

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
	vIters, vCalls, vTokens := effectiveVerifierBudget(task)

	v.LastPrompt = prompt
	result := v.runner.RunVerifier(prompt, workdir, timeout, vIters, vCalls, vTokens, v.agentName, model)
	v.LastPromptTokens = result.UsagePrompt
	v.LastCompletionTokens = result.UsageCompletion
	v.LastPromptCacheHit = result.UsagePromptCacheHit
	v.LastPromptCacheMiss = result.UsagePromptCacheMiss
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
		v.LastPromptCacheHit += result2.UsagePromptCacheHit
		v.LastPromptCacheMiss += result2.UsagePromptCacheMiss
		output = result2.Stdout
	}

	// Tool-cap interruption (输出 "auto-interrupted") ≠ 交付物缺陷：再给一次
	// 最小化验证机会（只对最关键的验收点给结论），避免 run_task 的挂起 guard
	// 把一次可修复的中断升级成批次失败（v41/v43 模式）。第二次仍中断才走挂起。
	if isVerifierCapInterrupt(output) {
		v.LastCapRetries++
		minimalPrompt := "YOUR PREVIOUS VERIFICATION TURN WAS INTERRUPTED (tool iteration cap). " +
			"Finish with a MINIMAL pass: check only the 2 most critical acceptance points, " +
			"use at most 2 tool calls, and mark everything else 无法验证 with a reason.\n\n" +
			prompt
		result2 := v.runner.RunVerifier(minimalPrompt, workdir, timeout, vIters, vCalls, vTokens, v.agentName, model)
		v.LastPromptTokens += result2.UsagePrompt
		v.LastCompletionTokens += result2.UsageCompletion
		v.LastPromptCacheHit += result2.UsagePromptCacheHit
		v.LastPromptCacheMiss += result2.UsagePromptCacheMiss
		output = result2.Stdout
	}

	// Write verifier result to whiteboard.
	if err := v.whiteboard.WriteVerifier(task.ID, output); err != nil {
		return false, false, output, fmt.Errorf("write verifier result: %w", err)
	}

	passed, retry = parseVerdict(output)
	return passed, retry, output, nil
}

// isVerifierCapInterrupt reports whether a verifier output is the tool-cap
// interruption template (not a real verdict): the turn was force-stopped, so
// anything inside it is NOT evidence about the deliverable. Matches both the
// English interruption marker uses in the guard and the Chinese wording the
// LLM verifier actually emits (工具调用上限中断 / cap-limited / 工具轮数预算),
// else the guard in run_task never fires and a verifier that simply could not
// finish is retried as if the deliverable were defective.
func isVerifierCapInterrupt(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "auto-interrupted") ||
		strings.Contains(lower, "tool iteration cap") ||
		strings.Contains(lower, "iteration cap reached") ||
		strings.Contains(lower, "工具调用上限") ||
		strings.Contains(lower, "工具轮数预算") ||
		strings.Contains(lower, "cap-limited")
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
// Verifier (e.g. a spawn error string), so it must never auto-pass.

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
