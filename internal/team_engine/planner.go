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
	runner                 *AgentRunner
	loggers                *log.Loggers
	team                   *TeamConfig
	onLog                  func()
	complexity             string
	lastDecomposeResult    string // raw JSON plan from last successful decompose, for review context
	lastDecomposeSessionID string // Leader subagent session ID from last successful decompose
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

// WithComplexity sets the goal-size hint (simple/medium/complex) that
// decompose uses to right-size the number of tasks/batches. Empty string
// leaves decomposition at its default behavior.
func (p *Planner) WithComplexity(c string) *Planner {
	p.complexity = c
	return p
}

// DecomposePrompt returns the prompt template for task decomposition.
func DecomposePrompt(goal string, complexity ...string) string {
	c := ""
	if len(complexity) > 0 {
		c = complexity[0]
	}
	sizeGuide := ""
	switch c {
	case "simple":
		sizeGuide = "目标规模评估为 simple（小任务）：尽量合并为 1 个 batch、≤2 个任务，不要为不同文件各建一个任务。"
	case "complex":
		sizeGuide = "目标规模评估为 complex（大任务）：可拆成多个 batch 并行，用 depends_on 串联有依赖的阶段。"
	case "medium":
		sizeGuide = "目标规模评估为 medium：按关注点正常拆分，相互独立的关注点放入不同 batch 并行。"
	default:
		sizeGuide = "目标规模未评估：按关注点正常拆分。"
	}
	return fmt.Sprintf(`你是任务分解与角色分配器。将目标一次拆到底，输出的每个任务都必须是叶子
（单个 Worker 一次可完成，单次输出约 150 行以内）。
如果某个 concern 仍超出可吞性，在本次调用中继续分解——最终只输出叶子列表。

目标：%s

## 分类与分解

%s

先判断结构再分解：
SINGLE — 单一关注点 → 1 个任务
MULTI  — 多个关注点 → 每个 concern 一个任务，放入独立 batch 并行
MANY   — 大量相同单元 → 每个单元一个任务，相同 role、相同 batch（并行）

## 可吞性判断

按难度裁定，不只看体量：
- 体量小但需深度推理（如复杂算法、精密的正确性证明）→ 拆
- 体量大但机械重复（如模板化、批量同类内容）→ 留

## 产出物与其自动验证件的归属（关键）

某个模块的核心产出和它配套的自动验证件，是同一个关注点，
必须合并为同一个任务、由同一个 worker 同时产出（在同一目录写产出文件和验证文件）。
原因：
- 验证件要引用被验证产出的位置，同 worker 产出才能保证引用路径正确；
- 同一个 worker 产出实现与验证件，两者语义一致，自动验证才能稳定通过；
- 拆成两个任务会引入跨 worker 的文件依赖和语义错位，导致自动验证反复失败重试。

若确因体量过大必须拆开，验证任务必须 depends_on 产出任务（串行执行）。
只有跨模块的集成验证（涉及多模块协作/端到端行为）才拆成独立任务。

## depends_on_batch 使用规则（关键）

depends_on_batch 声明「下游任务依赖其产出的上游 batch」——无论是读取还是修改。
以下情况必须声明 depends_on_batch：
- 下游任务要读取上游产出的文件（如某模块读取另一模块产出的结构或定义）；
- 下游任务会修改/覆盖上游任务产出的文件。

以下情况不要加 depends_on_batch，让 batch 并行执行：
- 下游不依赖任何上游文件（完全独立）；
- 契约已在 description 内联（文字约定，见下方契约共享），无需读上游文件；
- 逻辑上的先后理解 ≠ 执行上的硬依赖。

只有存在真实文件依赖才声明 depends_on_batch；滥用会把本可并行的任务串成链，显著拖慢整体进度。

## 接口契约（contract）共享（关键）

同一 batch 内并行的任务，若存在接口/契约耦合，集成时极易冲突。典型耦合：
- 结构 ↔ 呈现（结构定义 ↔ 引用它的呈现层，两边各发明一套标识 = 呈现全部失效）；
- 接口 ↔ 实现（函数/模块签名 ↔ 调用方）；
- 数据 ↔ 消费（数据格式 ↔ 解析方）。

分解时必须显式指定契约，禁止让并行任务各自发明接口：
1. 把「契约定义权」指派给产出结构/接口/数据的那一方（如产出结构的一方）；
2. 在定义方任务的 description 中写出契约的具体内容（名称、标识、字段、签名）；
3. 在所有耦合任务的 description 中附上同一份契约要点，保证每个 worker 拿到的
   description 都自含契约（例：呈现任务的 description 写明「命名契约由结构任务
   定义：<列举标识>，呈现必须使用这些标识」）。
   契约耦合任务的 description 允许超出「≤3句话」以容纳契约要点。

契约是文字约定，不是文件依赖——契约耦合任务仍保持并行，不要为此加 depends_on_batch。

## 跨任务引用闭环与集成验证（关键）

目标拆成多个任务后，产出物之间常有真实文件引用（文档章节互链、模块间引用、报告引用调研、组件间的组装关系）。这类引用只要断一处，整体交付物就
无法工作（例：某模块漏引依赖产出，导致整体无法工作）。

分解完成后必须自查并满足：
1. 引用双向闭环——引用方任务（如组装/入口方）的 description 与 acceptance_criteria
   显式列出它引用的全部上游产出（「引入 <列举依赖产出>」并列为验收项）；
   被引用方任务（如被引用的产出方）的 description 注明「本产出被引用方引用」。
   写下游任务时，回头补全上游任务的引用清单，保证两边一致。
2. 追加集成验证任务——当目标最终交付为一个可运行/可阅读的整体（网页、应用、文档集、
   研究报告、设计方案等），额外产出一个 title 含「集成验证/端到端验收」的任务，
   depends_on 所有上游 batch，role 用对应类型的验证角色（对应类型的验证角色），
   acceptance_criteria = 最终产物端到端可工作（网页：浏览器打开能实际运行；文档：
   章节引用完整一致；研究：结论合并无矛盾；设计：模块接口对齐）。
3. 禁止：遗漏引用清单，或把集成验证并入某个具体产出任务而不单独成任务。

## 角色分配

每个任务必须分配：
- role：与领域精确匹配的具体角色名
- verifier_role：质量审查的 agent 名。主观任务（架构、安全、文档）
  用真实 verifier agent。纯机械任务（算法实现、格式转换）留 ""。
- verifier_focus：审查维度（correctness、security、performance…）

## 验收标准（关键）

每个任务必须产出 2-5 条可验收、可核对的完成条件（acceptance_criteria），
供 Worker 交付前自检、Verifier 独立验收共用。禁止笼统条件（如「做好 X」）。
按任务类型产出合适的验收标准：
- 代码类：功能行为 / 接口契约（给定输入 → 期望输出/行为）；
- 文档类：覆盖的主题 / 准确性 / 风格约束；
- 研究类：论证完整性 / 来源可靠 / 结论可信度；
- 设计类：满足约束 / 权衡合理 / 边界覆盖。

## 输出

纯 JSON 数组，不加 markdown 包裹。无 depends_on_batch 的 batch 并行执行；有 depends_on_batch 的 batch 等依赖完成后执行。

[
  {"title":"…","description":"≤3句话","output":"产物路径","role":"角色名","verifier_role":"agent名或空","acceptance_criteria":["条件1","条件2"],"batch_id":"1","batch_label":"阶段名","depends_on_batch":[],"depends_on_index":-1,"verifier_focus":"审查维度","max_cycles":1}
]`, goal, sizeGuide)
}

// ---------------------------------------------------------------------------
// Phase 0: Goal Elaboration
//
// Follows the 4-step process defined in goal-elaboration-methodology.md:
//   Step 1+2 — merged completeness check + domain research (single call)
//   Step 3   — spec production (YAML spec, only when gaps found)
//   Step 4   — user confirmation (reserved — see below)
//
// Prompt templates live in elaboration_prompts.go.
// Types live in elaboration_types.go.
// ---------------------------------------------------------------------------

// Elaborate runs the multi-step goal elaboration pipeline and returns the
// elaborated (or original) goal.  Fully-specified goals short-circuit after
// the merged check+research step, avoiding wasted LLM rounds.
func (p *Planner) Elaborate(rawGoal string, workdir string, timeout time.Duration, model ...string) (string, error) {
	goal, _, err := p.ElaborateFull(rawGoal, workdir, timeout, model...)
	return goal, err
}

// ElaborateFull is like Elaborate but also returns the goal-size hint
// (simple/medium/complex) derived from the flash completeness check — used
// to right-size decomposition without skipping any pipeline stage.
func (p *Planner) ElaborateFull(rawGoal string, workdir string, timeout time.Duration, model ...string) (string, string, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	// Elaboration is domain-knowledge gap-filling — flash is sufficient.
	// Override any pro model setting for this phase only.
	flashModel := "deepseek-v4-flash"

	// ── Merged Step 1+2: Completeness check + domain research ──────
	verdict, err := p.checkAndResearch(rawGoal, workdir, timeout, flashModel)
	if err != nil && p.loggers != nil {
		p.loggers.Engine("leader.elaborate.check: check failed: %v, proceeding with elaboration", err)
	}
	if p.onLog != nil {
		p.onLog()
	}

	// Complexity uses the deterministic lexical rule — the flash model's
	// estimate is a non-deterministic sample that cascades into decompose's
	// size guide and amplifies task-count variance across identical goals.
	complexity := lexicalComplexity(rawGoal)

	// If the check failed entirely (nil verdict), treat as incomplete
	// and proceed to spec production with LLM's built-in domain knowledge.
	if verdict == nil {
		if p.loggers != nil {
			p.loggers.Engine("leader.elaborate.check: nil verdict, proceeding with spec production")
		}
		elaborated, err := p.produceSpec(rawGoal, workdir, timeout, flashModel, nil, nil)
		if err != nil || elaborated == "" {
			return rawGoal, complexity, nil
		}
		return elaborated, complexity, nil
	}

	// Early exit: goal is already fully specified.
	if verdict.IsComplete() {
		if p.loggers != nil {
			p.loggers.Engine("leader.elaborate: SKIP — all 6 dimensions complete, using raw goal")
		}
		return rawGoal, complexity, nil
	}

	gaps := verdict.Gaps()
	if p.loggers != nil {
		p.loggers.Engine("leader.elaborate.check: INCOMPLETE — %d gaps: %v", len(gaps), gapNames(gaps))
	}

	// ── Step 3: Produce concrete spec ───────────────────────────────
	elaborated, err := p.produceSpec(rawGoal, workdir, timeout, flashModel, verdict.DomainFacts, gaps)
	if err != nil || elaborated == "" {
		if defaultTeamLog != nil {
			defaultTeamLog.LeaderRetry(0, "elaboration spec production failed, using raw goal")
		}
		return rawGoal, complexity, nil
	}
	if p.onLog != nil {
		p.onLog()
	}

	// ── Step 4: User confirmation (reserved) ────────────────────────
	// TODO(stretch): When the elaborated spec contains genuinely ambiguous
	// choices where user preference is the deciding factor (e.g. "library vs
	// CLI", "Go vs Python" when neither was specified), prompt the user for
	// confirmation before proceeding.
	//
	// Principle: don't ask what the TeamLeader can decide itself.
	// Only ask when there are ≥2 reasonable options and no industry default.

	return elaborated, complexity, nil
}

// checkAndResearch runs merged Step 1+2: ask a cheap model to judge the
// raw goal against the 6 completeness dimensions AND fill domain facts
// for any gaps.  Returns a structured verdict with optional DomainFacts.
func (p *Planner) checkAndResearch(rawGoal, workdir string, timeout time.Duration, model string) (*CompletenessVerdict, error) {
	prompt := CheckAndResearchPrompt(rawGoal)
	// No team prompt injection — pure dimension analysis + domain facts.

	start := time.Now()
	result := p.runner.RunElaborationStep(prompt, workdir, timeout, 2048, model)
	dur := time.Since(start)

	if p.loggers != nil {
		p.loggers.Engine("leader.elaborate.check: model=%s dur=%.1fs output=%d chars success=%v",
			model, dur.Seconds(), len(result.Stdout), result.Success)
		p.loggers.LogLeader("elaborate_check", prompt, result.Stdout, dur, nil)
	}

	output := strings.TrimSpace(result.Stdout)
	if !result.Success || output == "" {
		return nil, fmt.Errorf("completeness check failed: success=%v output_empty=%v", result.Success, output == "")
	}

	jsonStr := extractJSONObject(output)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON found in completeness check output")
	}

	var verdict CompletenessVerdict
	if err := json.Unmarshal([]byte(jsonStr), &verdict); err != nil {
		return nil, fmt.Errorf("parse completeness verdict: %w\nRaw: %s", err, output)
	}

	if verdict.Verdict != "COMPLETE" && verdict.Verdict != "INCOMPLETE" {
		return nil, fmt.Errorf("unknown verdict: %q", verdict.Verdict)
	}

	return &verdict, nil
}

// produceSpec runs Step 3: produce the final YAML spec by combining
// the raw goal, domain research, and identified gaps.
func (p *Planner) produceSpec(rawGoal, workdir string, timeout time.Duration, model string, research *DomainResearch, gaps []DimensionGap) (string, error) {
	prompt := SpecProductionPrompt(rawGoal, research, gaps)
	// Only inject the leader's quality philosophy — roles, capabilities, and
	// routing tables are irrelevant for writing a spec (decomposition uses them).
	if p.team != nil && p.team.Leader.Prompt != "" {
		prompt += "\n\n## Team Standards\n" + p.team.Leader.Prompt
	}

	start := time.Now()
	result := p.runner.RunElaborationStep(prompt, workdir, timeout, 4096, model)
	dur := time.Since(start)

	if p.loggers != nil {
		p.loggers.Engine("leader.elaborate.spec: model=%s dur=%.1fs output=%d chars success=%v",
			model, dur.Seconds(), len(result.Stdout), result.Success)
		p.loggers.LogLeader("elaborate_spec", prompt, result.Stdout, dur, nil)
	}

	output := strings.TrimSpace(result.Stdout)
	if !result.Success || output == "" {
		return "", fmt.Errorf("spec production failed: success=%v output_empty=%v", result.Success, output == "")
	}

	return output, nil
}

// gapNames returns a compact list of gap dimension names for logging.
func gapNames(gaps []DimensionGap) []string {
	names := make([]string, len(gaps))
	for i, g := range gaps {
		names[i] = g.Name
	}
	return names
}

// lexicalComplexity returns a cheap size hint for a goal when the flash
// completeness check did not produce a complexity estimate. It is only a
// fallback — the flash model's verdict is preferred when available.
func lexicalComplexity(goal string) string {
	g := strings.TrimSpace(goal)
	for _, k := range complexSignalKeywords {
		if strings.Contains(g, k) {
			return "complex"
		}
	}
	if len(g) <= 80 {
		return "simple"
	}
	return "medium"
}

var complexSignalKeywords = []string{"重构", "迁移", "微服务", "分布式", "高并发", "多模块", "复杂"}

// decomposeInternal runs the leader agent and returns both parsed tasks
// and the raw AI output text.
func (p *Planner) decomposeInternal(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	prompt := DecomposePrompt(goal, p.complexity)
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
				Title:          goal,
				Description:    output,
				Role:           "developer",
				BatchID:        "default",
				BatchLabel:     "Execution",
				DependsOnIndex: -1,
			}}, output, nil
		}
		if p.loggers != nil {
			p.loggers.Engine("leader.decompose: success — %d tasks in plan", len(tasks))
		}
		if defaultTeamLog != nil {
			defaultTeamLog.LeaderDecompose(goal, mdl, attempt+1, 0, result.UsagePrompt, result.UsageCompletion, dur.Seconds(), len(output), false, true)
		}
		p.lastDecomposeResult = output
		p.lastDecomposeSessionID = result.SessionID
		return tasks, output, nil
	}

	return nil, "", fmt.Errorf("leader failed after %d attempts", maxRetries+1)
}

// DecomposeContext returns the raw JSON output from the last successful
// decompose, so the Leader can reference its own decisions during review.
func (p *Planner) DecomposeContext() string {
	return p.lastDecomposeResult
}

// DecomposeSessionID returns the Leader subagent session ID from the last
// successful decompose, so the engine can persist it for prompt/fork/summarize.
func (p *Planner) DecomposeSessionID() string {
	return p.lastDecomposeSessionID
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
