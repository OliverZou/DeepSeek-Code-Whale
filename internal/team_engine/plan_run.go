package team_engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Pipeline: Plan → Batches → Tasks (with parallelism)
// ---------------------------------------------------------------------------

// TeamCycle is the TE's execution cycle loop. The Leader decomposes the plan
// (Cycle 0) and hands it to the TE, which runs one full pass over the batches —
// a Cycle — and reports back with a plan-level CycleReport. The Leader reviews
// the report and accepts or rejects; on reject the feedback is applied and the
// next Cycle incrementally re-runs only the not-passed batches. The loop ends
// on accept, escalation, cancellation, or cycle-budget exhaustion.
// RunLeaderDriven is the P2 plan-and-run entry — the leader-driven replacement
// for the engine-driven TeamCycle loop. The initiator (CLI, main agent) states
// the master task, then hands control to the Leader: an ordinary sessioned
// subagent whose toolset carries OrchestrationToolNames. The Leader drives
// plan-and-run from its own session in two turns —
//
//	turn 1: the Leader's decompose spawn produces the structured plan (the
//	        spawn records the session with OrchestrationTools, so continuation
//	        turns can address it and rebuild the same agent);
//	turn 2: a Continue on that session carries the drive instruction
//	        (leaderDrivePrompt); the Leader runs the plan with team_run /
//	        team_status / team_feedback / team_result and ends in a final
//	        report.
//
// The engine never steps batches itself here — plan-and-run is the Leader's
// responsibility.
//
// This method waits for the Leader's final report, persists the leader session
// (so the run can be resumed and the user can talk to the Leader afterwards —
// the Leader keeps living in its session), writes the report as the master
// output.md, and returns it. The initiator receives the report.
// LeaderDrivenOption customizes a RunLeaderDriven execution.
type LeaderDrivenOption func(*leaderDrivenConfig)

type leaderDrivenConfig struct {
	plan       []PlanTask
	complexity string
}

// WithPreDecomposedPlan runs an externally-provided plan instead of LLM
// decomposition: the plan is used verbatim (no decompose call, byte-for-byte
// fixed — head-to-head A/B compares only worker/verifier behavior), and the
// Leader's session is bootstrapped with a single minimal turn so the drive
// (turn 2) and review (turn 3) turns still work. complexity drives worker/
// verifier iteration budgets (simple/medium/complex); empty falls back to
// lexicalComplexity(goal).
func WithPreDecomposedPlan(tasks []PlanTask, complexity string) LeaderDrivenOption {
	return func(c *leaderDrivenConfig) {
		c.plan = tasks
		c.complexity = complexity
	}
}

func (e *TeamEngine) RunLeaderDriven(ctx context.Context, goal, workdir, masterTaskID string, options ...LeaderDrivenOption) (string, error) {
	var cfg leaderDrivenConfig
	for _, o := range options {
		o(&cfg)
	}
	runStart := time.Now()

	// Register a cancel for this execution so the dashboard stop button works.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	e.mu.Lock()
	e.masterTaskCancels[masterTaskID] = execCancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.masterTaskCancels, masterTaskID)
		e.mu.Unlock()
	}()

	// Scope files and logs under the master task directory.
	e.Whiteboard.SetMaster(masterTaskID)
	if e.Loggers != nil {
		masterDir := filepath.Join(e.Whiteboard.BaseDir(), masterTaskID)
		if err := e.Loggers.SetBaseDir(masterDir); err != nil {
			return "", fmt.Errorf("set log dir: %w", err)
		}
	}

	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")

	// Turn 1: the Leader decomposes the goal in its own session and hands the
	// plan to the TE, which turns it into executable batches.
	planTasks, complexity, elabDur, decompDur, err := e.decomposePlan(goal, workdir, masterTaskID, leader, decomposerTimeout, leaderModel, cfg.complexity, cfg.plan...)
	if err != nil {
		return "", err
	}
	batches, err := e.createBatchesFromPlan(planTasks, goal, workdir, masterTaskID, complexity)
	if err != nil {
		return "", err
	}

	// Always bootstrap a Leader session that carries the orchestration tools
	// (team_run_plan / team_run / team_status ...) for the drive (submit) and
	// review turns. The decompose spawn is a LEAN single generation (no tools),
	// so its session cannot call team_run_plan; bootstrapping separately gives
	// the Leader a session whose continuation can actually submit the plan.
	// plan-file mode also goes through here (the plan came from the file, so
	// the bootstrap is a minimal ack, never a re-decomposition).
	if leader.DecomposeSessionID() == "" || len(cfg.plan) == 0 {
		if err := leader.BootstrapSession(goal, workdir, decomposerTimeout, leaderModel); err != nil {
			_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
			return "", fmt.Errorf("bootstrap leader session: %w", err)
		}
	}

	leaderSessionID := leader.DecomposeSessionID()
	// Persist the leader session so continuation turns (the drive turn, user
	// conversations, prompt/fork/summarize) can address it.
	if leaderSessionID != "" {
		_ = e.Store.SaveMasterTaskLeaderSession(masterTaskID, leaderSessionID)
	}
	if defaultTeamLog != nil {
		Log("leader", "leader-driven START master=%s model=%s session=%s batches=%d", masterTaskID, leaderModel, leaderSessionID, len(batches))
	}

	if leaderSessionID == "" {
		_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		return "", errors.New("leader-driven run failed: leader session missing after decompose")
	}
	ops := e.sessionOps
	if ops == nil {
		ops = DefaultSessionOps()
	}
	if ops == nil {
		_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		return "", errors.New("leader-driven run requires SessionOps (SetSessionOps/SetDefaultSessionOps)")
	}

	// Turn 2: the submit turn. The plan exists and the TE already turned it
	// into batches — the Leader hands it over with a single team_run_plan call
	// and stops. Waiting is deliberately NOT the Leader's job: an LLM polling
	// loop burns the session's tool-iteration budget (repetitive team_list
	// calls get storm_blocked, then the cap auto-interrupts the turn — v22's
	// failure mode, killing both the turn and its report). The engine waits on
	// the plan run below, then the review turn replays the results.
	res, err := ops.Continue(execCtx, leaderSessionID, leaderDriveSubmitPrompt(goal, workdir, masterTaskID))
	// The submit turn is part of the orchestration cost — fold its usage into
	// the engine totals so the run summary reflects the whole Leader-driven
	// run, not only the worker/verifier batches.
	e.addTokens(res.UsagePrompt, res.UsageCompletion, res.UsagePromptCacheHit, res.UsagePromptCacheMiss)
	if defaultTeamLog != nil {
		Log("leader", "leader-driven submit DONE master=%s success=%v session=%s", masterTaskID, res.Success, leaderSessionID)
	}
	if err != nil {
		_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		return res.Output, fmt.Errorf("leader-driven run failed: %w", err)
	}
	if !res.Success {
		_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		if strings.TrimSpace(res.Output) == "" {
			res.Output = res.Diagnostic
		}
		return res.Output, fmt.Errorf("leader-driven run failed: %s", res.Diagnostic)
	}

	// The engine waits for the plan run to settle while the Leader's session
	// idles — the wait is engine-side, not an LLM polling loop.
	view, settled, waitErr := e.waitPlanRunSettled(masterTaskID)
	if defaultTeamLog != nil {
		Log("leader", "leader-driven run settled=%v waitErr=%v summary=%q", settled, waitErr, view.Summary)
	}

	// Turn 3: the review turn. The execution outcome is replayed to the Leader;
	// it verifies the tasks, redispatches failures (team_run is synchronous —
	// the result comes back in the call) and writes the final report.
	res, err = ops.Continue(execCtx, leaderSessionID, leaderDriveReviewPrompt(goal, workdir, masterTaskID, reviewResultForMaster(view, settled, waitErr)))
	e.addTokens(res.UsagePrompt, res.UsageCompletion, res.UsagePromptCacheHit, res.UsagePromptCacheMiss)
	if defaultTeamLog != nil {
		Log("leader", "leader-driven DONE master=%s success=%v report=%d chars session=%s", masterTaskID, res.Success, len(res.Output), leaderSessionID)
	}
	if err != nil {
		_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		return res.Output, fmt.Errorf("leader-driven run failed: %w", err)
	}
	if !res.Success {
		_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		if strings.TrimSpace(res.Output) == "" {
			res.Output = res.Diagnostic
		}
		return res.Output, fmt.Errorf("leader-driven run failed: %s", res.Diagnostic)
	}

	// Deliver: the Leader's final report IS the master output.md.
	outPath := filepath.Join(e.Whiteboard.MasterDir(masterTaskID), "output.md")
	if err := os.WriteFile(outPath, []byte(res.Output), 0644); err != nil {
		return res.Output, fmt.Errorf("write master output: %w", err)
	}
	_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "done")
	// 统计用执行批次的实例：执行在 StartPlanRun 通过 recoverMasterBatches 从磁盘
	// 恢复的 Batch 对象上发生(TotalDuration/TotalTokens 在 runBatch/RunBatch 里
	// 填充);本地的 batches 只是计划时的副本,直接传会出现 batch 行 0.0s/0 tokens。
	summaryBatches := batches
	if settled && len(view.Batches) > 0 {
		summaryBatches = view.Batches
	}
	e.logRunSummary(runStart, elabDur, decompDur, summaryBatches)
	// Leader review 轮可能重派过 suspended 任务并全部 done：用最终任务状态
	// 重写 run_report（execute 侧快照会停留 failed——v47 教训）。
	e.refreshRunReportAfterLeaderReview(masterTaskID, goal, runStart, summaryBatches, e.tokenTotal())
	return res.Output, nil
}

func (e *TeamEngine) TeamCycle(ctx context.Context, goal, workdir, masterTaskID string, preDecomposed ...PlanTask) ([]*Batch, error) {
	runStart := time.Now()

	// Register a cancel for this execution so the dashboard stop button works.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	e.mu.Lock()
	e.masterTaskCancels[masterTaskID] = execCancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.masterTaskCancels, masterTaskID)
		e.mu.Unlock()
	}()

	// Scope files and logs under the master task directory.
	e.Whiteboard.SetMaster(masterTaskID)
	if e.Loggers != nil {
		masterDir := filepath.Join(e.Whiteboard.BaseDir(), masterTaskID)
		if err := e.Loggers.SetBaseDir(masterDir); err != nil {
			return nil, fmt.Errorf("set log dir: %w", err)
		}
	}

	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")

	// Cycle 0: the Leader decomposes the goal in its own session and hands the
	// plan to the TE, which turns it into executable batches.
	planTasks, complexity, elabDur, decompDur, err := e.decomposePlan(goal, workdir, masterTaskID, leader, decomposerTimeout, leaderModel, "", preDecomposed...)
	if err != nil {
		return nil, err
	}
	batches, err := e.createBatchesFromPlan(planTasks, goal, workdir, masterTaskID, complexity)
	if err != nil {
		return nil, err
	}

	// Progress maps are owned across Cycles so checkpoints keep the full
	// history of passed batches (needed for incremental re-run and Resume).
	completedBatches := make(map[string]bool)
	passedBatches := make(map[string]bool)
	completedBatchOutputs := make(map[string]string)

	maxCycles := e.Config.Batch.DefaultMaxCycles
	if maxCycles <= 0 {
		maxCycles = 3
	}

	// Cycle loop: TE runs a pass, reports, Leader decides.
	for cycle := 1; ; cycle++ {
		select {
		case <-execCtx.Done():
			e.logRunSummary(runStart, elabDur, decompDur, batches)
			return batches, fmt.Errorf("cancelled: %w", ctx.Err())
		default:
		}

		// TE executes one full pass (incremental: passed batches skipped).
		if err := e.runBatchesToCompletion(execCtx, batches, masterTaskID, workdir, decomposerTimeout, leaderModel, completedBatches, passedBatches, completedBatchOutputs, nil); err != nil {
			e.logRunSummary(runStart, elabDur, decompDur, batches)
			return batches, err
		}

		// TE reports the Cycle back to the Leader.
		report := e.buildPlanCycleReport(batches, cycle)
		review, err := leader.ReviewPlanCycle(goal, report, workdir, decomposerTimeout, leaderModel)
		if err != nil {
			// The Leader could not produce a decision — escalate so the user
			// can resolve instead of looping silently.
			review = &CycleReview{Decision: CycleEscalate, Reason: fmt.Sprintf("plan review failed: %v", err)}
		}
		if e.Loggers != nil {
			e.Loggers.LogLeader("review", fmt.Sprintf("Cycle: %d\nDecision: %s", cycle, review.Decision), review.Reason, decomposerTimeout, nil)
			e.fireEvent(TaskEvent{Type: EventLeaderLog})
		}

		switch review.Decision {
		case CycleAccept:
			// Deliver.
			if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
				_ = e.Whiteboard.WriteDeliverable(delContent)
			}
			e.assembleParentOutputs(batches)
			e.writeMasterOutput(goal, batches, workdir)
			e.logRunSummary(runStart, elabDur, decompDur, batches)
			return batches, nil

		case CycleReject:
			if cycle >= maxCycles {
				e.escalateFirstOpenBatch(batches, review)
				e.logRunSummary(runStart, elabDur, decompDur, batches)
				return batches, nil
			}
			// Apply the Leader's feedback to every not-passed batch and
			// re-run them in the next Cycle.
			for _, b := range batches {
				if b.Status == BatchStatusPassed {
					continue
				}
				// 验证任务不参与自动重置：它 FAIL 的缺陷属于上游实现，重跑
				// 验证任务只会重复发现同一缺陷（v15：集成验证 FAIL 后 cycle
				// 重置重跑 batch，白烧 ~300K raw token 且无进展）。保持
				// suspended 交 Leader 决策——Leader 的 review 轮有
				// team_run/team_feedback 工具，可直接驱动上游任务修复。
				hasNonVerification := false
				for _, t := range b.Tasks {
					if !isVerificationTask(t) {
						hasNonVerification = true
						break
					}
				}
				if !hasNonVerification {
					Log("plan", "plan: verification batch %s kept failed (defects belong upstream — Leader decides)", b.ID)
					continue
				}
				b.Status = BatchStatusPending
				for _, t := range b.Tasks {
					if isVerificationTask(t) {
						continue // 保留 suspended 与缺陷报告
					}
					e.SendFeedback(t.ID, review.Feedback)
					if !t.State.IsTerminal() && t.State != TaskStateSuspended {
						_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
					}
				}
			}
			continue

		case CycleEscalated:
			if cycle >= maxCycles {
				e.escalateFirstOpenBatch(batches, review)
				e.logRunSummary(runStart, elabDur, decompDur, batches)
				return batches, nil
			}
			// Escalation strategy applied (e.g. model/parameters changed):
			// retry the same Cycle with failed tasks reset.
			for _, b := range batches {
				if b.Status == BatchStatusPassed {
					continue
				}
				hasNonVerification := false
				for _, t := range b.Tasks {
					if !isVerificationTask(t) {
						hasNonVerification = true
						break
					}
				}
				if !hasNonVerification {
					Log("plan", "plan: verification batch %s kept failed (defects belong upstream — Leader decides)", b.ID)
					continue
				}
				b.Status = BatchStatusPending
				for _, t := range b.Tasks {
					if isVerificationTask(t) {
						continue // 保留 suspended 与缺陷报告
					}
					e.SendFeedback(t.ID, review.Feedback)
					if t.State == TaskStateFailed || t.State == TaskStateSuspended {
						_ = e.Store.TransitionState(t.ID, TaskStatePending, "escalated retry", "")
					} else if !t.State.IsTerminal() {
						_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
					}
				}
			}
			continue

		case CycleEscalate:
			e.escalateFirstOpenBatch(batches, review)
			e.logRunSummary(runStart, elabDur, decompDur, batches)
			return batches, nil
		}
	}
}

// escalateFirstOpenBatch escalates on behalf of the first not-passed batch —
// the plan-level loop has no single batch to attach the escalation to.
func (e *TeamEngine) escalateFirstOpenBatch(batches []*Batch, review *CycleReview) {
	for _, b := range batches {
		if b.Status != BatchStatusPassed {
			e.handleEscalation(b, review)
			return
		}
	}
}

// decomposePlan is the Leader's job: elaborate the goal and decompose it into
// concrete plan tasks (or use the pre-decomposed plan), splitting overloaded
// "god tasks". The resulting plan is then handed to the TE — createBatchesFromPlan
// — which turns it into executable batches. Returns the plan tasks, the
// complexity classification, and the elapsed elaboration/decomposition durations
// (for run-summary logging).
func (e *TeamEngine) decomposePlan(goal, workdir, masterTaskID string, leader *Leader, decomposerTimeout time.Duration, leaderModel string, complexityHint string, preDecomposed ...PlanTask) ([]PlanTask, string, time.Duration, time.Duration, error) {
	var elabDur, decompDur time.Duration

	var planTasks []PlanTask
	var complexity string
	if len(preDecomposed) > 0 {
		planTasks = preDecomposed
		// External plan (plan-file / pre-decomposed): complexity comes from the
		// file when provided; otherwise fall back to the lexical size hint so
		// worker/verifier do NOT silently get the widest (default) budget.
		complexity = complexityHint
		if complexity == "" {
			complexity = lexicalComplexity(goal)
		}
		if defaultTeamLog != nil {
			Log("plan", "plan: using pre-decomposed plan: %d tasks in %d batches", len(planTasks), countBatches(planTasks))
		}
	} else {
		// Phase 0: Goal Elaboration — expand fuzzy goals into concrete specs.
		if defaultTeamLog != nil {
			Log("plan", "plan: elaborate START model=%s", leaderModel)
		}
		elabStart := time.Now()
		var elaboratedGoal string
		var elabErr error
		elaboratedGoal, complexity, elabErr = leader.ElaborateFull(goal, workdir, time.Duration(e.Router.ResolveDecomposerTimeout())*time.Second, leaderModel)
		elabDur = time.Since(elabStart)
		if elabErr != nil {
			if defaultTeamLog != nil {
				Log("plan", "plan: elaborate FAIL: %v, continuing with raw goal", elabErr)
			}
			elaboratedGoal = goal
		}
		if elaboratedGoal != goal {
			if defaultTeamLog != nil {
				Log("plan", "plan: elaborate OK — %d chars", len(elaboratedGoal))
			}
		}

		if defaultTeamLog != nil {
			Log("plan", "plan: decompose START model=%s", leaderModel)
		}
		if defaultTeamLog != nil {
			Log("plan", "plan: complexity=%s", complexity)
		}
		leader.WithComplexity(complexity)

		// Decompose is a stochastic LLM call: a single split may hand the same
		// physical deliverable file to two tasks (e.g. game.js to both a logic
		// layer task and a DOM task), which the output-ownership mechanical gate
		// rejects. One bad draw doesn't mean the plan is infeasible, so retry a
		// few times instead of failing the whole run on the first conflict.
		const maxDecomposeAttempts = 3
		for attempt := 1; ; attempt++ {
			decompStart := time.Now()
			var err error
			planTasks, err = leader.Decompose(elaboratedGoal, workdir, decomposerTimeout, leaderModel)
			decompDur = time.Since(decompStart)
			if err != nil {
				if defaultTeamLog != nil {
					Log("plan", "plan: decompose FAIL: %v", err)
				}
				return nil, "", elabDur, decompDur, fmt.Errorf("decompose goal: %w", err)
			}
			if len(planTasks) == 0 {
				if defaultTeamLog != nil {
					Log("plan", "plan: decompose EMPTY")
				}
				return nil, "", elabDur, decompDur, fmt.Errorf("plan is empty")
			}
			planTasks = e.splitOverloadedPlanTasks(planTasks, workdir, decomposerTimeout, leaderModel)
			conflictErr := checkPlanTaskOutputConflicts(planTasks)
			if conflictErr == nil {
				if defaultTeamLog != nil {
					Log("plan", "plan: decompose OK: %d tasks in %d batches (attempt %d)", len(planTasks), countBatches(planTasks), attempt)
				}
				// Wire decompose context into review prompts so the Leader
				// references its own decisions during batch review.
				leader.WithDecomposeContext()
				// Persist the Leader subagent session so prompt/fork/summarize can
				// address it later (P0).
				e.leaderSessionID = leader.DecomposeSessionID()
				break
			}
			if defaultTeamLog != nil {
				Log("plan", "plan: decompose attempt %d rejected by output-ownership gate: %v", attempt, conflictErr)
			}
			if attempt >= maxDecomposeAttempts {
				return nil, "", elabDur, decompDur, conflictErr
			}
		}
	}

	// 后置校验（仅 preDecomposed 分支需要）：decompose 可能拆出「上帝任务」
	// （单任务同时承担集成+多端兼容等多职责，违反叶子约束，会让单个 worker
	// 触达 tool cap）。检测到就再拆。非 preDecomposed 分支已在 decompose 循环内
	// 完成相同校验，且冲突时已自动重新分解，不再重复此处。
	if len(preDecomposed) > 0 {
		planTasks = e.splitOverloadedPlanTasks(planTasks, workdir, decomposerTimeout, leaderModel)
		// 源头机械门：输出文件唯一性在分解层校验（同名交付 = 分解问题，若两
		// 项任务交付同一文件，并行 worker 会互相覆盖）。冲突时 plan.json 不落盘、
		// Task 记录不创建、执行不进入——decompose 以明确错误终止，重新分解。
		if err := checkPlanTaskOutputConflicts(planTasks); err != nil {
			return nil, "", elabDur, decompDur, err
		}
	}

	// 编排机械约束（两个分支统一施加）：测试任务补依赖实现 batch、集成验证
	// 任务补依赖所有上游 batch。Leader 正确编排时全部 no-op；错误编排（测试
	// 与实现并行、验证先于实现）被修正，避免测试 worker 空转/验证半成品。
	if fixed, changed := enforcePlanTaskDependencies(planTasks); changed {
		if defaultTeamLog != nil {
			Log("plan", "plan: enforced dependency edges (test→impl / verification→all upstream)")
		}
		planTasks = fixed
	}

	// 最终计划（可能被 splitOverloadedPlanTasks 替换过任务/标题）才是 Task 记录
	// 的同源：createBatchesFromPlan 与恢复执行（recoverMasterBatches）都以它为准。
	// 必须在 split 之后写——否则 plan.json 与磁盘 Task 记录脱节，team_run_plan
	// 按 title 匹配不上而拒绝恢复（v21 串行根因：leader 被迫逐个 team_run）。
	// 对 preDecomposed 分支同样生效：它是唯一使 plan.json 存在的位置。
	e.writePlanJSON(masterTaskID, complexity, planTasks)

	return planTasks, complexity, elabDur, decompDur, nil
}

// createBatchesFromPlan is the TE's half of plan-and-run: the Leader hands its
// plan over to the TE, which groups the plan tasks into Batches and creates
// concrete Tasks in the store. The TE owns batch structure — the Leader never
// touches Batches directly.
func (e *TeamEngine) createBatchesFromPlan(planTasks []PlanTask, goal, workdir, masterTaskID, complexity string) ([]*Batch, error) {
	// Step 1: Group PlanTasks into batches by batch_id.
	type batchGroup struct {
		label       string
		dependsOn   []string
		planTasks   []PlanTask
		concurrency int
		maxCycles   int
	}
	batchMap := make(map[string]*batchGroup)
	batchOrder := make([]string, 0) // preserve insertion order

	for _, pt := range planTasks {
		batchID := pt.BatchID
		if batchID == "" {
			batchID = "default"
		}
		if _, ok := batchMap[batchID]; !ok {
			batchMap[batchID] = &batchGroup{
				label:       pt.BatchLabel,
				dependsOn:   []string(pt.DependsOnBatch),
				concurrency: pt.Concurrency,
				maxCycles:   pt.MaxCycles,
			}
			batchOrder = append(batchOrder, batchID)
		}
		batchMap[batchID].planTasks = append(batchMap[batchID].planTasks, pt)
	}

	// Step 2: Create Batch objects.
	var batches []*Batch
	for _, bid := range batchOrder {
		bg := batchMap[bid]
		concurrency := bg.concurrency
		if concurrency <= 0 {
			concurrency = e.teamMaxAgents()
		}
		maxCycles := bg.maxCycles
		if maxCycles <= 0 {
			maxCycles = e.Config.Batch.DefaultMaxCycles
		}
		batch := &Batch{
			ID:          bid,
			Label:       bg.label,
			DependsOn:   bg.dependsOn,
			Status:      BatchStatusPending,
			Concurrency: concurrency,
			MaxCycles:   maxCycles,
		}

		// Create tasks for this batch.
		for _, pt := range bg.planTasks {
			profile := ToolProfile(pt.Profile)
			task, err := e.CreateTask(
				pt.Title,
				pt.Description,
				AgentRole(pt.Role),
				profile,
				nil, // parent IDs now handled at batch level
				0,   // 0 = use NewTask default (9)
				workdir,
				pt.VerifierFocus, bid, masterTaskID,
			)
			if err != nil {
				return nil, fmt.Errorf("create subtask %s: %w", pt.Title, err)
			}
			task.Output = pt.Output
			task.BatchID = bid
			task.UseDW = pt.UseDW
			task.VerifierRole = pt.VerifierRole
			task.VerifyMode = verifyModeForTask(pt)
			task.AcceptanceCriteria = pt.AcceptanceCriteria
			task.MasterTaskID = masterTaskID
			task.Complexity = complexity
			task.UpstreamBatches = batch.DependsOn
			e.Store.UpdateTask(task.ID, map[string]interface{}{
				"batch_id":       bid,
				"master_task_id": masterTaskID,
				"output":         pt.Output,
				"complexity":     complexity,
			})
			batch.Tasks = append(batch.Tasks, task)
		}
		// Propagate UseDW from tasks to batch: if any task uses DW,
		// the entire batch gets DW pipeline execution (multi-verifier per task).
		for _, t := range batch.Tasks {
			if t.UseDW {
				batch.UseDW = true
				break
			}
		}
		batches = append(batches, batch)
	}

	if defaultTeamLog != nil {
		for _, batch := range batches {
			Log("plan", "plan: batch %s created — %d tasks", batch.LabelOrID(), len(batch.Tasks))
		}
		Log("plan", "plan: %d batches ready, starting execution", len(batches))
	}

	// 机械门:输出文件所有权唯一——同一个交付物只能有一个任务负责,并行写
	// 同名文件会互相覆盖、后完成者胜。计划非法在执行前直接拒绝,不让运行时撞车。
	var planTasksAll []*Task
	for _, batch := range batches {
		planTasksAll = append(planTasksAll, batch.Tasks...)
	}
	if err := checkOutputConflicts(planTasksAll); err != nil {
		return nil, err
	}

	// Write plan.md — structured overview of the goal and all batches/tasks.
	e.writePlanMarkdown(masterTaskID, goal, batches)

	// Notify dashboard that tasks have been created.
	e.fireEvent(TaskEvent{Type: EventStateChanged})

	return batches, nil
}

// runBatchesToCompletion executes one full pass over the plan: every batch runs
// to completion via topological dependency scheduling (independent batches
// concurrently; a failed batch blocks its downstream while sibling batches
// continue). Batches already passed in an earlier Cycle pre-complete (incremental
// re-run) and are not re-executed. Each batch's Status is updated in place.
// The three progress maps are owned by the caller so they survive across Cycles.
// Returns nil on success, or a cancellation error. It is shared by TeamCycle
// (first pass) and TeamCycle (subsequent Cycles), so both use the same execution
// substrate.
func (e *TeamEngine) runBatchesToCompletion(ctx context.Context, batches []*Batch, masterTaskID, workdir string, decomposerTimeout time.Duration, leaderModel string, completedBatches, passedBatches map[string]bool, completedBatchOutputs map[string]string, onBatchDone func()) error {
	// Progress heartbeat: every 30s a monitor-friendly summary line lands in
	// team_engine.log — batch statuses with task counts, elapsed time and the
	// accumulated token total — so a long run is observable live.
	runStarted := time.Now()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.logProgress(masterTaskID, batches, runStarted)
			}
		}
	}()

	escalator := NewEscalator().WithLoggers(e.Loggers)
	// stateMu guards the three shared progress maps plus saveCheckpoint, and also
	// batch.CycleCount / batch.Tasks / board rendering, which saveCheckpoint
	// and BuildBoardContent read across batches.
	stateMu := &sync.Mutex{}

	// Dependency graph: in-degree over existing deps, plus downstream list.
	batchByID := make(map[string]*Batch, len(batches))
	for _, b := range batches {
		batchByID[b.ID] = b
	}
	indeg := make(map[string]int, len(batches))
	dependents := make(map[string][]string)
	for _, b := range batches {
		for _, dep := range b.DependsOn {
			if _, ok := batchByID[dep]; !ok {
				continue // dangling dep — handled by markFailed below
			}
			indeg[b.ID]++
			dependents[dep] = append(dependents[dep], b.ID)
		}
	}

	// remaining counts batches not yet terminal (passed or failed). markFailed
	// marks a batch and its transitive downstream failed, so a failed batch
	// blocks downstream without aborting sibling batches.
	remaining := len(batches)
	// failed tracks which batches have had their failure propagated. It is keyed
	// independently of batch.Status because failure paths (worker cancel, RunBatch
	// error) set Status=Failed before the dispatcher observes the result — keying
	// on Status would skip propagation and deadlock the dispatcher on <-doneCh.
	failed := make(map[string]bool)
	var markFailed func(id string)
	markFailed = func(id string) {
		if failed[id] {
			return
		}
		failed[id] = true
		if b := batchByID[id]; b != nil {
			stateMu.Lock()
			b.Status = BatchStatusFailed
			stateMu.Unlock()
		}
		remaining--
		for _, down := range dependents[id] {
			markFailed(down)
		}
	}

	// Dangling deps make a batch unsatisfiable — fail it (and downstream).
	for _, b := range batches {
		for _, dep := range b.DependsOn {
			if _, ok := batchByID[dep]; !ok {
				markFailed(b.ID)
				break
			}
		}
	}

	// runBatch runs one batch to completion (DW or normal cycle loop) and
	// records its pass/fail state.  The scheduler reads passedBatches to decide
	// which downstream batches to unblock.
	runBatch := func(execCtx context.Context, batch *Batch) {
		// 批次达到终态(成功/失败/DW 完成)即推进 settle 的活跃性计时;
		// onBatchDone 为 nil 的传统路径(TeamCycle/测试)不受影响。
		if onBatchDone != nil {
			defer onBatchDone()
		}
		cycleLimit := batch.MaxCycles
		if cycleLimit <= 0 {
			cycleLimit = 1 // at minimum one cycle
		}

		if batch.UseDW {
			// DW pipeline execution: multi-verifier per task, no Leader review.
			e.runDWCycle(execCtx, batch, cycleLimit, masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches, escalator, workdir, decomposerTimeout, leaderModel, stateMu)
			// Cross-stage artifact passing for completed DW batch.
			stateMu.Lock()
			if completedBatches[batch.ID] {
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				if depArtifacts := e.buildStageArtifactContext(completedBatchOutputs, batch.DependsOn); depArtifacts != "" {
					for _, t := range batch.Tasks {
						updated := t.Description + depArtifacts
						e.Store.UpdateTask(t.ID, map[string]interface{}{"description": updated})
					}
				}
			}
			stateMu.Unlock()
			return
		}

		// Normal execution with Leader review cycle.
		batchStart := time.Now()
		defer func() {
			batch.TotalDuration = time.Since(batchStart).Seconds()
		}()

		for cycle := 0; cycle < cycleLimit; cycle++ {
			stateMu.Lock()
			batch.CycleCount = cycle + 1
			stateMu.Unlock()

			// Execute all tasks in this batch (parallel if concurrency > 0).
			if err := e.RunBatch(execCtx, batch); err != nil {
				BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
				stateMu.Lock()
				batch.Status = BatchStatusFailed
				e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				stateMu.Unlock()
				return
			}

			// Log suspended tasks (retries exhausted) for user attention.
			escalator.LogSuspendedTasks(batch, func(id string) (*Task, error) { return e.Store.GetTask(id) })

			// Write board.md after each batch cycle.
			stateMu.Lock()
			var boardContent string
			e.Store.WithReadLock(func() {
				boardContent = e.Whiteboard.BuildBoardContent(batches)
			})
			stateMu.Unlock()
			if boardContent != "" {
				_ = e.Whiteboard.WriteBoard(boardContent)
			}

			// Check for self-split children before declaring batch done.
			// 注意：children 拾取只有这一处——曾经在 cycle 顶部还有一个重复的
			// 拾取块，同一 cycle 内会把同一个子任务 append 两次，下一个 cycle
			// 并发启动同一任务两次（v15 实测：tool-cap 自拆出的 game-dom 子任务
			// 被并发执行 2 次，多烧 ~1M raw token，verifier 也跑了两遍）。
			anyNew := false
			stateMu.Lock()
			for _, t := range batch.Tasks {
				if children, _ := e.Store.ListTasksByParent(t.ID); len(children) > 0 {
					for _, c := range children {
						if c.State == TaskStatePending || c.State == TaskStateAssigned {
							c.BatchID = batch.ID
							_ = e.Store.UpdateTask(c.ID, map[string]interface{}{"batch_id": batch.ID, "master_task_id": t.MasterTaskID})
							batch.Tasks = append(batch.Tasks, c)
							anyNew = true
						}
					}
				}
			}
			stateMu.Unlock()
			if anyNew {
				if cycle == cycleLimit-1 {
					cycleLimit++ // extend limit for children
				}
				continue // run children in next cycle
			}

			// Batch done. Verifier already judged every task — skip Leader review,
			// but fail the batch if any task is not Done (retries exhausted →
			// suspended, or failed).
			stateMu.Lock()
			completedBatches[batch.ID] = true
			batchFailed := false
			for _, t := range batch.Tasks {
				st := e.Store.TaskState(t.ID)
				if st != TaskStateDone {
					batchFailed = true
					break
				}
			}
			if batchFailed {
				batch.Status = BatchStatusFailed
			} else {
				batch.Status = BatchStatusPassed
				passedBatches[batch.ID] = true
			}
			completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			stateMu.Unlock()
			BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
			return
		}
	}

	// Concurrent scheduler: a worker pool drains the ready queue; the main
	// goroutine owns the queue, in-degrees, and remaining count.
	type batchResult struct {
		id     string
		passed bool
	}
	workCh := make(chan string, len(batches))
	doneCh := make(chan batchResult, len(batches))

	concurrency := e.teamMaxAgents()
	if concurrency <= 0 || concurrency > len(batches) {
		concurrency = len(batches)
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range workCh {
				b := batchByID[id]
				// Cancel check before starting each batch.
				select {
				case <-ctx.Done():
					for _, t := range b.Tasks {
						if !t.State.IsTerminal() {
							_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
						}
					}
					stateMu.Lock()
					b.Status = BatchStatusFailed
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
					stateMu.Unlock()
					doneCh <- batchResult{id: id, passed: false}
					continue
				default:
				}
				runBatch(ctx, b)
				stateMu.Lock()
				passed := passedBatches[id]
				stateMu.Unlock()
				doneCh <- batchResult{id: id, passed: passed}
			}
		}()
	}

	// Incremental re-run (TeamCycle rounds): batches already passed in an
	// earlier Cycle pre-complete here — they unblock their dependents without
	// being re-executed, and their remaining-count contribution is consumed.
	for _, b := range batches {
		if b.Status == BatchStatusPassed {
			remaining--
			for _, down := range dependents[b.ID] {
				indeg[down]--
			}
		}
	}

	// Seed the ready queue with in-degree-0 batches (passed and failed skipped).
	for _, b := range batches {
		if b.Status != BatchStatusFailed && b.Status != BatchStatusPassed && indeg[b.ID] == 0 {
			workCh <- b.ID
		}
	}

	// Dispatch completion events until every batch is terminal. remaining is
	// decremented exactly once per batch: in the passed branch here, or inside
	// markFailed (which covers the failed batch and its blocked downstream).
	for remaining > 0 {
		res := <-doneCh
		if res.passed {
			remaining--
			for _, down := range dependents[res.id] {
				indeg[down]--
				if indeg[down] == 0 && !failed[down] {
					workCh <- down
				}
			}
		} else {
			markFailed(res.id)
		}
	}
	close(workCh)
	wg.Wait()

	// Cancellation: suspend any stragglers and report.
	if ctx.Err() != nil {
		for _, b := range batches {
			for _, t := range b.Tasks {
				if !t.State.IsTerminal() {
					_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
				}
			}
		}
		e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
		e.fireEvent(TaskEvent{Type: EventStateChanged})
		return fmt.Errorf("cancelled: %w", ctx.Err())
	}
	return nil
}

// collectBatchOutputs gathers output summaries from all completed tasks in a batch.
// Used for cross-stage artifact passing (场景4).
func (e *TeamEngine) collectBatchOutputs(batch *Batch) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n\n## Cross-Stage Context: Batch %s Outputs\n", batch.LabelOrID()))
	for _, t := range batch.Tasks {
		output, _ := e.Whiteboard.ReadOutput(t.ID)
		if len(output) == 0 {
			continue
		}
		preview := output
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		b.WriteString(fmt.Sprintf("\n### %s (%s)\n%s\n", t.Title, t.State, preview))
	}
	artifacts, _ := e.Whiteboard.ListArtifacts(batch.Tasks[0].ID)
	if len(artifacts) > 0 {
		b.WriteString(fmt.Sprintf("\nArtifacts: %v\n", artifacts))
	}
	return b.String()
}

// logRunSummary emits a per-stage timing table to team_engine.log at the end
// of a run (P2-timing).  It reuses batch.TotalDuration (set in runBatch /
// runDWCycle) plus the elaborate/decompose timings measured in TeamCycle, so a
// single glance shows which stage consumed the most wall-clock time.
func (e *TeamEngine) logRunSummary(runStart time.Time, elabDur, decompDur time.Duration, batches []*Batch) {
	total := time.Since(runStart).Seconds()
	pct := func(d float64) float64 {
		if total <= 0 {
			return 0
		}
		return d / total * 100
	}
	Log("plan", "=== RUN SUMMARY ===")
	Log("plan", "elaborate: %.1fs (%.0f%%)", elabDur.Seconds(), pct(elabDur.Seconds()))
	Log("plan", "decompose: %.1fs (%.0f%%)", decompDur.Seconds(), pct(decompDur.Seconds()))
	for _, b := range batches {
		Log("plan", "batch %s [%s]: %d tasks, cycles=%d, %.1fs (%.0f%%), %d tokens",
			b.LabelOrID(), b.Status, len(b.Tasks), b.CycleCount, b.TotalDuration, pct(b.TotalDuration), b.TotalTokens)
	}
	// Engine-wide total: worker/verifier batches PLUS the Leader (decompose
	// turn + drive turn) — the whole run's LLM cost, not only the batches。
	// 两种执行路径的计数器归属不同：engine-driven（TeamCycle）里 batch 与 leader
	// 共用同一 tokenTotal（此时 tokenTotal >= 批次之和）；leader-driven 里批次
	// token 由 execute 侧引擎实例累计，本实例 tokenTotal 只含 Leader 驱动轮
	// （此时 tokenTotal < 批次之和，v31_full 曾把 3.3M 显示成 85086）。
	// 按两者大小关系选取正确的总账，避免漏加或重复加。
	batchSum := 0
	for _, b := range batches {
		batchSum += b.TotalTokens
	}
	leaderTotal := e.tokenTotal()
	grand := leaderTotal
	if leaderTotal < batchSum {
		grand = batchSum + leaderTotal
	}
	Log("plan", "TOTAL: %.1fs · %d tokens (batches=%d leader=%d)", total, grand, batchSum, leaderTotal)
}

// buildStageArtifactContext generates context that injects previous batch
// outputs into the next batch's tasks, enabling true cross-stage pipeline
// flow where Stage 2's workers see Stage 1's deliverables.
func (e *TeamEngine) buildStageArtifactContext(cache map[string]string, dependsOn []string) string {
	if len(dependsOn) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## 📦 Previous Stage Deliverables\n")
	b.WriteString("The following outputs were produced by earlier stages. Reference them in your work:\n")
	for _, depID := range dependsOn {
		if output, ok := cache[depID]; ok && output != "" {
			b.WriteString(output)
		}
	}
	return b.String()
}

// buildCycleReport creates a CycleReport from a batch's current state.
func (e *TeamEngine) buildCycleReport(batch *Batch, cycleNumber int) *CycleReport {
	report := &CycleReport{
		BatchID:     batch.ID,
		BatchLabel:  batch.Label,
		CycleNumber: cycleNumber,
		Status:      batch.Status,
		BoardPath:   e.Whiteboard.BoardPath(),
		Deliverable: e.Whiteboard.DeliverablePath(),
	}

	for _, t := range batch.Tasks {
		summary := TaskSummary{
			ID:         t.ID,
			Title:      t.Title,
			Role:       string(t.Role),
			State:      t.State,
			RetryCount: t.RetryCount,
		}
		// Add a brief output preview if available.
		if output, err := e.Whiteboard.ReadOutput(t.ID); err == nil && len(output) > 0 {
			if len(output) > 100 {
				summary.OutputBrief = output[:100] + "..."
			} else {
				summary.OutputBrief = output
			}
		}
		report.Tasks = append(report.Tasks, summary)
	}

	return report
}

// buildPlanCycleReport builds a plan-level CycleReport from the full batch list
// after one pass over the plan. It aggregates every batch's task outcomes into
// a single report for the Leader's accept/reject decision.
func (e *TeamEngine) buildPlanCycleReport(batches []*Batch, cycleNumber int) *PlanCycleReport {
	report := &PlanCycleReport{
		CycleNumber: cycleNumber,
		BoardPath:   e.Whiteboard.BoardPath(),
		Deliverable: e.Whiteboard.DeliverablePath(),
	}
	allPassed := true
	for _, b := range batches {
		summary := BatchSummary{ID: b.ID, Label: b.Label, Status: b.Status}
		for _, t := range b.Tasks {
			ts := TaskSummary{
				ID:         t.ID,
				Title:      t.Title,
				Role:       string(t.Role),
				State:      t.State,
				RetryCount: t.RetryCount,
			}
			if output, err := e.Whiteboard.ReadOutput(t.ID); err == nil && len(output) > 0 {
				if len(output) > 100 {
					ts.OutputBrief = output[:100] + "..."
				} else {
					ts.OutputBrief = output
				}
			}
			summary.Tasks = append(summary.Tasks, ts)
		}
		if b.Status != BatchStatusPassed {
			allPassed = false
		}
		report.Batches = append(report.Batches, summary)
	}
	if allPassed {
		report.Status = BatchStatusPassed
	} else {
		report.Status = BatchStatusFailed
	}
	return report
}

// handleEscalation suspends pipeline execution and blocks until the user
// (or another agent via AgentChannel.ResolveEscalation) provides a decision.
//
// The engine writes a detailed escalation to board.md and the user's inbox,
// then blocks on EscalationManager.Escalate with a 30-minute timeout.
// If the timeout expires, the pipeline aborts.
func (e *TeamEngine) handleEscalation(batch *Batch, review *CycleReview) {
	req := EscalationRequest{
		BatchID:    batch.ID,
		BatchLabel: batch.Label,
		Reason:     review.Reason,
		Feedback:   review.Feedback,
		Cycle:      batch.CycleCount,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		TimeoutMin: 30,
	}

	// Write to board.md.
	boardEntry := fmt.Sprintf("\n## 🚨 ESCALATION — Batch %s\n\n"+
		"- **Reason**: %s\n- **Feedback**: %s\n- **Cycle**: %d\n- **Time**: %s\n"+
		"- **Status**: ⏳ Waiting for decision...\n"+
		"- **Resolve**: `whale team resolve %s continue|retry|abort|modify`\n\n",
		batch.LabelOrID(), review.Reason, review.Feedback, batch.CycleCount, req.CreatedAt, batch.ID)

	existing, _ := e.Whiteboard.ReadBoard()
	_ = e.Whiteboard.WriteBoard(existing + boardEntry)

	// Block until resolved (or timeout).
	decision, err := e.Escalation.Escalate(req)

	// Update board with result.
	resultEntry := fmt.Sprintf("  → **Resolved**: %s", decision)
	if err != nil {
		resultEntry = fmt.Sprintf("  → **Timeout**: %v", err)
	}
	existing, _ = e.Whiteboard.ReadBoard()
	_ = e.Whiteboard.WriteBoard(existing + resultEntry + "\n")
}

// runDWCycle executes a batch using DW pipeline mode: each task gets
// multi-verifier DW verification, there is no batch-level Leader review,
// and tasks complete independently (no barrier).  Only failed tasks are
// retried — passed tasks stay done.
func (e *TeamEngine) runDWCycle(
	ctx context.Context,
	batch *Batch,
	cycleLimit int,
	masterTaskID string,
	completedBatches map[string]bool,
	passedBatches map[string]bool,
	completedBatchOutputs map[string]string,
	batches []*Batch,
	escalator *Escalator,
	workdir string,
	decomposerTimeout time.Duration,
	leaderModel string,
	stateMu *sync.Mutex,
) {
	var prevFindings *CycleFindingsSet
	dryCount := 0
	batchStart := time.Now()
	defer func() {
		batch.TotalDuration = time.Since(batchStart).Seconds()
		BatchDone(batch.ID, string(batch.Status), batch.TotalDuration)
	}()

	for cycle := 0; cycle < cycleLimit; cycle++ {
		stateMu.Lock()
		batch.CycleCount = cycle + 1
		stateMu.Unlock()

		// Check cancellation between cycles.
		select {
		case <-ctx.Done():
			for _, t := range batch.Tasks {
				if !t.State.IsTerminal() {
					_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
				}
			}
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return
		default:
		}

		// Execute all tasks in parallel (each with DW verification if task.UseDW).
		if err := e.RunBatch(ctx, batch); err != nil {
			stateMu.Lock()
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			stateMu.Unlock()
			return
		}

		if e.Loggers != nil {
			e.Loggers.Engine("DW batch %s cycle %d/%d: status=%s", batch.LabelOrID(), cycle+1, cycleLimit, batch.Status)
		}

		// Loop-until-dry: collect findings, exit when no new ones for 2 cycles.
		currentFindings := e.collectCycleFindings(batch, cycle+1)
		if prevFindings != nil && !currentFindings.HasNewFindings(prevFindings) {
			dryCount++
			if dryCount >= 2 {
				if e.Loggers != nil {
					e.Loggers.Engine("DW batch %s: dry after %d cycles", batch.ID, cycle+1)
				}
				stateMu.Lock()
				completedBatches[batch.ID] = true
				if len(currentFindings.Findings) == 0 {
					batch.Status = BatchStatusPassed
					passedBatches[batch.ID] = true
				} else {
					batch.Status = BatchStatusFailed
				}
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				stateMu.Unlock()
				return
			}
		} else {
			dryCount = 0
		}
		prevFindings = currentFindings

		// Log suspended tasks (retries exhausted) for user attention.
		escalator.LogSuspendedTasks(batch, func(id string) (*Task, error) { return e.Store.GetTask(id) })

		// Write board after each cycle.
		stateMu.Lock()
		var boardContent string
		e.Store.WithReadLock(func() {
			boardContent = e.Whiteboard.BuildBoardContent(batches)
		})
		stateMu.Unlock()
		if boardContent != "" {
			_ = e.Whiteboard.WriteBoard(boardContent)
		}

		// Check if all tasks passed.  Only retry failed tasks; suspended tasks
		// (retries exhausted) fail the batch, not pass it.
		allPassed := true
		for _, t := range batch.Tasks {
			state := e.Store.TaskState(t.ID)
			if state == TaskStateFailed {
				allPassed = false
				_ = e.Store.ForceTransitionState(t.ID, TaskStateAssigned, "dw-retry")
			} else if state != TaskStateDone {
				allPassed = false
			}
		}

		if allPassed {
			stateMu.Lock()
			batch.Status = BatchStatusPassed
			completedBatches[batch.ID] = true
			passedBatches[batch.ID] = true
			completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			stateMu.Unlock()
			return
		}
	}

	// Max cycles reached — auto-accept whatever we have.
	stateMu.Lock()
	batch.Status = BatchStatusFailed
	completedBatches[batch.ID] = true
	completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
	e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
	stateMu.Unlock()
	if e.Loggers != nil {
		e.Loggers.Engine("DW batch %s: max cycles (%d) reached, auto-accepting", batch.ID, cycleLimit)
	}
}

// teamMaxAgents resolves the batch concurrency ceiling.  The team's
// config.yaml max_agents (when set) overrides the global batch.max_agents;
// otherwise the global default applies.
func (e *TeamEngine) teamMaxAgents() int {
	if e.team != nil && e.team.Config != nil && e.team.Config.MaxAgents > 0 {
		return e.team.Config.MaxAgents
	}
	return e.Config.Batch.MaxAgents
}

// RunBatch executes all tasks in a batch in parallel, respecting the
// configured concurrency limit.
func (e *TeamEngine) RunBatch(ctx context.Context, batch *Batch) error {
	batch.Status = BatchStatusRunning
	if defaultTeamLog != nil {
		defaultTeamLog.BatchStart(batch.ID, batch.Label, len(batch.Tasks), batch.CycleCount, batch.MaxCycles)
	}

	tasks := batch.Tasks
	if len(tasks) == 0 {
		return nil // empty batch is a no-op; Leader review decides outcome
	}

	concurrency := batch.Concurrency
	if concurrency <= 0 {
		concurrency = e.teamMaxAgents()
	}
	if concurrency <= 0 || concurrency > len(tasks) {
		concurrency = len(tasks) // cap at task count; 0 = unlimited in config
	}

	// runGroup 并行执行一组任务，受 concurrency 限流；ctx 取消时挂起未启动任务。
	runGroup := func(group []*Task) error {
		if len(group) == 0 {
			return nil
		}
		sem := make(chan struct{}, concurrency)
		type taskResult struct {
			taskID string
			ok     bool
			err    error
		}
		resultCh := make(chan taskResult, len(group))
		for _, task := range group {
			select {
			case <-ctx.Done():
				// Suspend all not-yet-launched tasks.
				for _, t := range group {
					if !t.State.IsTerminal() {
						_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
					}
				}
				batch.Status = BatchStatusFailed
				e.fireEvent(TaskEvent{Type: EventStateChanged})
				return ctx.Err()
			default:
			}
			sem <- struct{}{} // acquire semaphore (blocks if at limit)
			go func(t *Task) {
				defer func() { <-sem }() // release semaphore
				ok, err := e.RunTask(ctx, t.ID)
				resultCh <- taskResult{taskID: t.ID, ok: ok, err: err}
			}(task)
		}
		// Drain the semaphore (wait for all goroutines to finish).
		for i := 0; i < cap(sem); i++ {
			sem <- struct{}{}
		}
		// Drain result channel to unblock goroutines.
		close(resultCh)
		for range resultCh {
		}
		return nil
	}

	// 三阶段执行：报告类验收（审计/端到端清单）→ 实现/修复 → 测试。
	// 1) 报告类验收先跑：验收批次里 engineer 的修复任务以审计清单为输入；若
	//    fix 先于清单运行，会迫使它自己重新审计全仓（v31_full 4507e35b：
	//    59 轮 / 1.45M tokens 全花在替代审计上），既烧 token 又顺序倒挂。
	// 2) 实现组随后：互相独立的实现/修复任务并行进行。
	// 3) 测试最后：planner 偶尔把「实现 + 单元测试」拆成同 batch 的并行任务，
	//    测试任务会因实现产出尚未就绪而空转撞 tool cap；串行兜底消除这类空转。
	var auditGroup, implGroup, testGroup []*Task
	for _, t := range tasks {
		switch {
		case isTestTask(t):
			testGroup = append(testGroup, t)
		case isReportTask(t):
			auditGroup = append(auditGroup, t)
		default:
			implGroup = append(implGroup, t)
		}
	}
	if err := runGroup(auditGroup); err != nil {
		return err
	}
	if err := runGroup(implGroup); err != nil {
		return err
	}
	if err := runGroup(testGroup); err != nil {
		return err
	}

	// Real batch cost: per-task cumulative worker+verifier tokens, read back
	// from the store.  The old counter-delta (e.tokenTotal() - beforeTokens) is
	// only safe while batches run serially — once independent batches run
	// concurrently the deltas cross-contaminate and a batch is credited with
	// its siblings' tokens (v32: RUN SUMMARY batches=2802k vs real 1744k,
	// +61%).  Per-task stats accumulate in RunTask's UpdateTask calls.
	batch.TotalTokens = 0
	for _, t := range batch.Tasks {
		if cur, err := e.Store.GetTask(t.ID); err == nil && cur != nil {
			batch.TotalTokens += cur.WorkerTokens + cur.VerifierTokens
		}
	}
	return nil
}

// overloadedIntegrationMarkers 与 overloadedQaMarkers 用于检测 decompose 拆出的
// 「上帝任务」：单个任务同时承担「集成/联调」与「多端/兼容验证」两类职责，
// 违反「每个 concern 一个叶子任务」的约束，会让单个 worker 触达 tool cap。
var overloadedIntegrationMarkers = []string{"整合", "联调", "集成"}

// 「响应式」有意不放此列表：它是前端实现任务的高频特征词（任何响应式布局
// 任务都会写），而本列表的意图是「多端/兼容*验证*」职责信号——纯样式实现
// 的 desc 常同时含「集成」(引用句)+「响应式」(实现句)，放进列表会误判成
// 上帝任务（v26 style.css 被白拆一次、93s+13.3k tokens 换 1 个叶子）。
var overloadedQaMarkers = []string{"兼容", "浏览器", "破版", "多端", "chrome", "firefox", "safari", "edge"}

// isOverloadedTask 判断任务描述是否把集成工作与多端/兼容验证合并成了单个超重任务。
// 命中则应在执行前再拆成叶子，避免超重任务拖慢墙钟并触发 tool cap。
// 防误判（v48：2048 计划被白拆 3 次、~4 分钟+5 万 token）：计划模板要求任务注明
// 「本产出被集成验证任务引用」——这是引用句，不是本任务承担集成职责；「浏览器与 Node
// 双环境可用」是实现环境描述，不是多端兼容验证。引用句命中即排除，避免误拆。
func isOverloadedTask(desc string) bool {
	d := strings.ToLower(desc)
	hasIntegration := false
	for _, m := range overloadedIntegrationMarkers {
		// 引用句排除：desc 含「集成验证/集成验收/端到端验收」时，「集成」一词来自模板
		// 引用句（「本产出被集成验证任务引用」），不是本任务的整合/联调职责。
		if strings.Contains(d, m) && (strings.Contains(d, "集成验证") || strings.Contains(d, "集成验收") || strings.Contains(d, "端到端验收")) {
			continue
		}
		if strings.Contains(d, m) {
			hasIntegration = true
			break
		}
	}
	if !hasIntegration {
		return false
	}
	for _, m := range overloadedQaMarkers {
		if strings.Contains(d, m) {
			return true
		}
	}
	return false
}

// isTestTask 判断任务是否为「纯测试/验证类」任务，用于 RunBatch 的分阶段执行：
// 测试任务依赖实现产出，若与实现并行会因产出未就绪而空转撞 tool cap。
// 判定收紧为「纯测试」而非「产出含测试文件」：产出（全部条目）命中测试命名
// （.test./.spec./_test、test/、tests/、test_ 前缀）；产出完全未声明时才回退到
// 角色判定（qa/test）。
// 产出同时含实现与测试的任务（如 "game.js, game.test.js"）是自包含实现——
// 它的自测跑在自己的实现上，与同批其他实现任务无依赖，应归入实现组并行。
// QA 角色的「报告类验收」（产出 AUDIT_FINDINGS_*.md 等非测试文件）即测试命名
// 不命中 → 返回 false，由 isReportTask 接管（先于实现/修复运行）。
func isTestTask(t *Task) bool {
	matched := false
	for _, o := range strings.Split(strings.ToLower(t.Output), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if !isTestFileName(o) {
			// 混合产出（实现+测试）或报告类产出：不是纯测试任务。
			return false
		}
		matched = true
	}
	if matched {
		return true
	}
	// 产出完全未声明时回退到角色判定（qa/test 角色默认按测试任务处理）。
	role := strings.ToLower(string(t.Role))
	return strings.Contains(role, "qa") || strings.Contains(role, "test")
}

// isReportTask 判断任务是否为「报告类验收」：QA/test 角色产出的不是测试文件，
// 而是问题清单/验收报告（.md 等）。它与 isTestTask（纯测试，必须在实现产出就绪
// 后运行）不同——报告类验收是同位批次 fix 任务的依赖输入，必须先于实现/修复
// 任务运行（RunBatch 三阶段的第一阶段），否则 fix 没有清单可依据，只能自己
// 重复审计（v31_full batch 3：fix 先跑，59 轮 / 1.45M tokens 全花在自审替代上）。
func isReportTask(t *Task) bool {
	role := strings.ToLower(string(t.Role))
	if !strings.Contains(role, "qa") && !strings.Contains(role, "test") {
		return false
	}
	for _, o := range strings.Split(strings.ToLower(t.Output), ",") {
		o = strings.TrimSpace(o)
		if o == "" || isTestFileName(o) {
			continue
		}
		return true
	}
	return false
}

func isTestFileName(o string) bool {
	return strings.HasPrefix(o, "test/") || strings.HasPrefix(o, "tests/") || strings.HasPrefix(o, "test_") ||
		strings.Contains(o, ".test.") || strings.Contains(o, ".spec.") || strings.Contains(o, "_test")
}

// effectiveWorkerProfile resolves the worker's tool profile for a task.
// 报告/审计/验收类任务（只读清单/测试脚本运行，不产出源码）降级到 verify 面：
// read+shell+web，无 edit/write——工具 schema 是每轮固定成本（13 工具 ≈3.4k
// token/轮），审计任务不需要写工具，精简后每轮省 ~1.5-2k token，同时杜绝
// 越权改源码。单测/实现/fix 类任务保持原 profile（需要写产出）。
func effectiveWorkerProfile(task *Task) ToolProfile {
	if isReportTask(task) || isVerificationTask(task) {
		return ProfileVerify
	}
	return task.Profile
}

// effectiveWorkerBudget resolves the worker iteration budget for a task.
// 报告/审计类任务轮数压缩：脚本化后验收一般 8-15 轮；封顶 30 轮防止长会话
// 失控（v39 runtime QA 56 轮 / 1.24M tokens）。
func effectiveWorkerBudget(task *Task) (maxIters, maxCalls, maxTokens int) {
	if isReportTask(task) || isVerificationTask(task) {
		// E2E 验收要写脚本+跑+出报告，但 v45 集成验收在无约束下 40 轮/1.29M
		// tokens（反复改被验代码）。验收只验证不改（prompt 有【验收纪律】），
		// 25 轮/70 调用足够；静态审计（read+grep+清单）20 轮足够。
		if e2eTitleMarked(task.Title) {
			return 25, 70, 20000
		}
		return 20, 50, 18000
	}
	return iterationBudget(task.Complexity, false)
}

// isFixTask reports whether the task is a fix/regression task (title
// contains 修复/回归). 这类任务修改已交付源文件，语义复验是它们的主要成本
// 与质量关口——verifier 预算需按高风险档放宽（v43: 20 轮仍 cap 143s/442k）。
func isFixTask(task *Task) bool {
	title := strings.ToLower(task.Title)
	return strings.Contains(title, "修复") || strings.Contains(title, "回归")
}

// effectiveVerifierBudget resolves the semantic verifier's iteration budget.
// 修复/回归类任务的 verifier 读多文件+复验跑用例，20 轮屡次 cap（v41/v43）；
// 放宽到 32 轮/100 调用。其余档位维持 iterationBudget 的 verifier 分支。
func effectiveVerifierBudget(task *Task) (maxIters, maxCalls, maxTokens int) {
	if isFixTask(task) {
		return 32, 100, 26000
	}
	return iterationBudget(task.Complexity, true)
}

// taskMaySelfSplit reports whether a task may split itself into children
// (worker [SPLIT_PLAN] or tool-cap interruption). 报告/审计/验收类任务禁止拆分：
// 审计对象是交付物整体，拆分无意义且会递归膨胀（v40: cap 触顶→拆 4 子任务→
// 子任务又 cap→batch3 13 任务/8M token）；触顶时直接交给 verifier 判 FAIL。
func taskMaySelfSplit(task *Task) bool {
	if isReportTask(task) || isVerificationTask(task) {
		return false
	}
	return len(task.ParentIDs) == 0
}

// e2eTitleMarkers 命中「浏览器/运行时验收」类任务（标题或描述含端到端/运行
// 时/交互/浏览器）；命中时 worker prompt 追加【端到端验收规范】。静态审计等
// 纯只读标题不命中，不会收到浏览器引导。
var e2eTitleMarkers = []string{"端到端", "运行时", "交互", "浏览器"}

// e2eTitleMarked reports whether the task is an E2E/browser acceptance task.
// Title only — descriptions often mention browsers in negation (「无浏览器」),
// which would false-positive on the guidance.
func e2eTitleMarked(title string) bool {
	hay := strings.ToLower(title)
	for _, m := range e2eTitleMarkers {
		if strings.Contains(hay, m) {
			return true
		}
	}
	return false
}

// verificationTitleMarkers 匹配 planner 约定的「集成验证/端到端验收」任务标题
// （见 DecomposePrompt「跨任务引用闭环与集成验证」段）。
var verificationTitleMarkers = []string{"集成验证", "端到端验收", "集成验收"}

// isVerificationTask 判断任务是否为「纯验证/验收」任务——其产出是对上游产物的
// 验证结论（报告），而非可被重做修改的实现。这类任务 FAIL 后重试同一 worker
// 无意义（验证者不修复被验证对象，只会重复发现同一缺陷），应直接挂起避免空转。
func isVerificationTask(t *Task) bool {
	title := strings.ToLower(t.Title)
	for _, m := range verificationTitleMarkers {
		if strings.Contains(title, m) {
			return true
		}
	}
	return false
}

// decomposeTaskIntoLeaves 把一个（可能超重的）任务描述重新分解为叶子任务。
// 复用 Leader.Decompose：把任务描述当作新 goal 再走一次分解。
func (e *TeamEngine) decomposeTaskIntoLeaves(desc, workdir string, timeout time.Duration, model string) ([]PlanTask, error) {
	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team)
	return leader.Decompose(desc, workdir, timeout, model)
}

// splitOverloadedPlanTasks 对 decompose 产出的 plan 做后置校验：检测「上帝任务」
// （单个任务同时承担集成+多端兼容等多职责），命中则再拆成叶子任务并替换原任务。
// 拆分失败不阻塞主流程——保留原任务继续，交由运行时的 tool-cap 拆分兜底。
func (e *TeamEngine) splitOverloadedPlanTasks(tasks []PlanTask, workdir string, timeout time.Duration, model string) []PlanTask {
	var out []PlanTask
	for _, t := range tasks {
		if !isOverloadedTask(t.Description) {
			out = append(out, t)
			continue
		}
		children, err := e.decomposeTaskIntoLeaves(t.Description, workdir, timeout, model)
		if err != nil || len(children) == 0 {
			Log("plan", "plan: split overloaded task %q failed (err=%v children=%d), keep as-is", t.Title, err, len(children))
			out = append(out, t)
			continue
		}
		// v48 防拆了个寂寞：重新分解只产出 1 个叶子且与原任务同输出 = 没拆开。
		// 保留原任务（拆分调用已花掉一轮 LLM 时间，不能再把原任务换成一个等价任务，
		// 否则 title 漂移导致 plan.json 与磁盘 Task 记录脱节）。
		if len(children) == 1 && children[0].Output == t.Output {
			Log("plan", "plan: split overloaded task %q re-decomposed to same single output, keep as-is", t.Title)
			out = append(out, t)
			continue
		}
		for i := range children {
			children[i].BatchID = t.BatchID
			children[i].BatchLabel = t.BatchLabel
			// Leaves stay in the parent batch, so the batch-level dependency
			// must carry over too — dropping it makes a downstream batch
			// (e.g. integration verification) run in parallel with its upstream.
			children[i].DependsOnBatch = t.DependsOnBatch
		}
		// 兜底：叶子若互相冲突（LLM 拆出多列交付同一文件），回退原任务执行——
		// 让源头机械门在 execute 前终止整次运行比「不拆」更糟。
		if err := checkPlanTaskOutputConflicts(children); err != nil {
			Log("plan", "plan: split overloaded task %q leaves conflict (%v), keep as-is", t.Title, err)
			out = append(out, t)
			continue
		}
		Log("plan", "plan: split overloaded task %q into %d leaves", t.Title, len(children))
		out = append(out, children...)
	}
	return out
}

// logProgress writes a monitor-friendly progress summary into team_engine.log
// on the 30s heartbeat during a plan execution: per-batch status with task
// counts (terminal/total), elapsed wall time and the accumulated token total.
// Task states are read from the store so the line reflects reality even when
// the in-memory Task pointers are mid-transition.
func (e *TeamEngine) logProgress(masterTaskID string, batches []*Batch, started time.Time) {
	var b strings.Builder
	for _, bch := range batches {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		done, total := 0, len(bch.Tasks)
		for _, t := range bch.Tasks {
			if st := e.Store.TaskState(t.ID); st.IsTerminal() {
				done++
			}
		}
		fmt.Fprintf(&b, "%s[%s %d/%d]", bch.ID, bch.Status, done, total)
	}
	Log("progress", "master=%s elapsed=%s tokens=%d batches: %s",
		masterTaskID[:8], time.Since(started).Round(time.Second), e.tokenTotal(), b.String())
}
