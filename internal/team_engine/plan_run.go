package team_engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Pipeline: Plan → Batches → Tasks (with parallelism)
// ---------------------------------------------------------------------------

// PlanAndRun uses the Leader to decompose a goal into batches of subtasks
// and runs them respecting batch-level dependencies with parallelism.
//
// Architecture:
//
//	Leader     → plan ([]PlanTask with batch_id)
//	Engine     → group by batch_id → []Batch
//	           → for each Batch (in dependency order):
//	               → spawn all tasks in batch in parallel (goroutine pool)
//	               → wait for all tasks to finish (sync.WaitGroup)
//	               → gate: all PASS → next batch, any FAIL → abort
//	           → write board.md + deliverable.md
//
// PlanAndRun accepts optional pre-decomposed planTasks.  When provided, the
// internal decompose step is skipped and the pre-computed plan is used directly.
func (e *TeamEngine) PlanAndRun(ctx context.Context, goal, workdir, masterTaskID string, preDecomposed ...PlanTask) ([]*Batch, error) {
	runStart := time.Now()
	var elabDur, decompDur time.Duration

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

	var planTasks []PlanTask
	var complexity string
	if len(preDecomposed) > 0 {
		planTasks = preDecomposed
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
		var err error
		if defaultTeamLog != nil {
			Log("plan", "plan: complexity=%s", complexity)
		}
		leader.WithComplexity(complexity)
		decompStart := time.Now()
		planTasks, err = leader.Decompose(elaboratedGoal, workdir, decomposerTimeout, leaderModel)
		decompDur = time.Since(decompStart)
		if err != nil {
			if defaultTeamLog != nil {
				Log("plan", "plan: decompose FAIL: %v", err)
			}
			return nil, fmt.Errorf("decompose goal: %w", err)
		}
		if len(planTasks) == 0 {
			if defaultTeamLog != nil {
				Log("plan", "plan: decompose EMPTY")
			}
			return nil, fmt.Errorf("plan is empty")
		}
		if defaultTeamLog != nil {
			Log("plan", "plan: decompose OK: %d tasks in %d batches", len(planTasks), countBatches(planTasks))
		}
		e.writePlanJSON(masterTaskID, planTasks)
	}

	// 后置校验：decompose 可能拆出「上帝任务」（单任务同时承担集成+多端兼容
	// 等多职责，违反叶子约束，会让单个 worker 触达 tool cap）。检测到就再拆。
	planTasks = e.splitOverloadedPlanTasks(planTasks, workdir, decomposerTimeout, leaderModel)

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
			task.AcceptanceCriteria = pt.AcceptanceCriteria
			task.MasterTaskID = masterTaskID
			task.Complexity = complexity
			task.UpstreamBatches = batch.DependsOn
			e.Store.UpdateTask(task.ID, map[string]interface{}{
				"batch_id":       bid,
				"master_task_id": masterTaskID,
				"output":         pt.Output,
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

	// Write plan.md — structured overview of the goal and all batches/tasks.
	e.writePlanMarkdown(masterTaskID, goal, batches)

	// Notify dashboard that tasks have been created.
	e.fireEvent(TaskEvent{Type: EventStateChanged})

	// Step 3: Execute batches with topological dependency scheduling —
	//         independent batches run concurrently; a failed batch blocks its
	//         downstream while sibling batches continue (Bug 1 + P1-graph).
	escalator := NewEscalator().WithLoggers(e.Loggers)
	completedBatches := make(map[string]bool)
	passedBatches := make(map[string]bool) // only pass; controls dep gating
	completedBatchOutputs := make(map[string]string)
	// stateMu guards the three shared maps above plus saveCheckpoint, and also
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

			// Pick up self-split children for the next cycle.
			stateMu.Lock()
			for _, t := range batch.Tasks {
				children, err := e.Store.ListTasksByParent(t.ID)
				if err == nil && len(children) > 0 {
					for _, child := range children {
						if child.State == TaskStatePending || child.State == TaskStateAssigned {
							_ = e.Store.UpdateTask(child.ID, map[string]interface{}{"batch_id": batch.ID, "master_task_id": t.MasterTaskID})
							batch.Tasks = append(batch.Tasks, child)
						}
					}
				}
			}
			stateMu.Unlock()

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
				case <-execCtx.Done():
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
				runBatch(execCtx, b)
				stateMu.Lock()
				passed := passedBatches[id]
				stateMu.Unlock()
				doneCh <- batchResult{id: id, passed: passed}
			}
		}()
	}

	// Seed the ready queue with in-degree-0 batches.
	for _, b := range batches {
		if b.Status != BatchStatusFailed && indeg[b.ID] == 0 {
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
	if execCtx.Err() != nil {
		for _, b := range batches {
			for _, t := range b.Tasks {
				if !t.State.IsTerminal() {
					_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
				}
			}
		}
		e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
		e.fireEvent(TaskEvent{Type: EventStateChanged})
		e.logRunSummary(runStart, elabDur, decompDur, batches)
		return batches, fmt.Errorf("cancelled: %w", ctx.Err())
	}

	// Step 4: Write final deliverable.md.
	if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
		_ = e.Whiteboard.WriteDeliverable(delContent)
	}

	// Step 5: Leader final summary — collect all outputs and produce
	// a user-facing summary of what was accomplished.
	e.assembleParentOutputs(batches)
	e.writeMasterOutput(goal, batches, workdir)

	e.logRunSummary(runStart, elabDur, decompDur, batches)
	return batches, nil
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
// runDWCycle) plus the elaborate/decompose timings measured in PlanAndRun, so a
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
	totalTokens := 0
	for _, b := range batches {
		totalTokens += b.TotalTokens
		Log("plan", "batch %s [%s]: %d tasks, cycles=%d, %.1fs (%.0f%%), %d tokens",
			b.LabelOrID(), b.Status, len(b.Tasks), b.CycleCount, b.TotalDuration, pct(b.TotalDuration), b.TotalTokens)
	}
	Log("plan", "TOTAL: %.1fs · %d tokens", total, totalTokens)
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
// configured concurrency limit.// RunBatch executes all tasks in a batch in parallel, respecting the
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

	beforeTokens := e.tokenTotal()

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

	// 两阶段执行：先跑非测试任务（实现/产出），再跑测试任务。planner 偶尔会把
	// 「实现 + 单元测试」拆成同 batch 的并行任务，测试任务会因实现产出尚未就绪
	// 而空转撞 tool cap；串行兜底消除这类空转。
	var implGroup, testGroup []*Task
	for _, t := range tasks {
		if isTestTask(t) {
			testGroup = append(testGroup, t)
		} else {
			implGroup = append(implGroup, t)
		}
	}
	if err := runGroup(implGroup); err != nil {
		return err
	}
	if err := runGroup(testGroup); err != nil {
		return err
	}

	// Real batch cost: tokens accumulated by worker/verifier spawns in RunTask
	// since this batch started.  The delta snapshot is safe because batches run
	// serially (P1-graph will introduce concurrent batches and must revisit this).
	batch.TotalTokens = e.tokenTotal() - beforeTokens
	return nil
}

// overloadedIntegrationMarkers 与 overloadedQaMarkers 用于检测 decompose 拆出的
// 「上帝任务」：单个任务同时承担「集成/联调」与「多端/兼容验证」两类职责，
// 违反「每个 concern 一个叶子任务」的约束，会让单个 worker 触达 tool cap。
var overloadedIntegrationMarkers = []string{"整合", "联调", "集成"}
var overloadedQaMarkers = []string{"兼容", "浏览器", "响应式", "破版", "多端", "chrome", "firefox", "safari", "edge"}

// isOverloadedTask 判断任务描述是否把集成工作与多端/兼容验证合并成了单个超重任务。
// 命中则应在执行前再拆成叶子，避免超重任务拖慢墙钟并触发 tool cap。
func isOverloadedTask(desc string) bool {
	d := strings.ToLower(desc)
	hasIntegration := false
	for _, m := range overloadedIntegrationMarkers {
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

// isTestTask 判断任务是否为「测试/验证类」任务，用于 RunBatch 的两阶段执行：
// 测试任务依赖实现产出，若与实现并行会因产出未就绪而空转撞 tool cap。
// 依据产出路径命名（.test./.spec./_test、test/ 前缀）或角色（qa/test）识别。
func isTestTask(t *Task) bool {
	out := strings.ToLower(t.Output)
	if strings.Contains(out, ".test.") || strings.Contains(out, ".spec.") || strings.Contains(out, "_test") {
		return true
	}
	if strings.HasPrefix(out, "test/") || strings.HasPrefix(out, "tests/") || strings.HasPrefix(out, "test_") {
		return true
	}
	role := strings.ToLower(string(t.Role))
	return strings.Contains(role, "qa") || strings.Contains(role, "test")
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
		for i := range children {
			children[i].BatchID = t.BatchID
			children[i].BatchLabel = t.BatchLabel
		}
		Log("plan", "plan: split overloaded task %q into %d leaves", t.Title, len(children))
		out = append(out, children...)
	}
	return out
}
