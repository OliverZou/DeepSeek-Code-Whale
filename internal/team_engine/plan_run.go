package team_engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
		elaboratedGoal, elabErr := leader.Elaborate(goal, workdir, time.Duration(e.Router.ResolveDecomposerTimeout())*time.Second, leaderModel)
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
		planTasks, err = leader.Decompose(elaboratedGoal, workdir, decomposerTimeout, leaderModel)
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
				dependsOn:   pt.DependsOnBatch,
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
			task.MasterTaskID = masterTaskID
			e.Store.UpdateTask(task.ID, map[string]interface{}{
				"batch_id":       bid,
				"master_task_id": masterTaskID,
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

	// Step 3: Execute batches in order with dependency gating + CycleReport
	//         + cross-stage artifact passing (场景4).
	escalator := NewEscalator().WithLoggers(e.Loggers)
	completedBatches := make(map[string]bool)
	passedBatches := make(map[string]bool) // only CycleAccept; controls dep gating
	// Track completed batch outputs for cross-stage injection.
	completedBatchOutputs := make(map[string]string)

	for _, batch := range batches {
		// Check batch dependencies — use passedBatches so a failed batch
		// does NOT unblock downstream batches (Bug 1 fix).
		allDepsPassed := true
		for _, depID := range batch.DependsOn {
			if !passedBatches[depID] {
				allDepsPassed = false
				break
			}
		}
		if !allDepsPassed {
			batch.Status = BatchStatusFailed
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			return batches, fmt.Errorf("batch %s: dependency %v not satisfied", batch.ID, batch.DependsOn)
		}

		// Check for cancellation between batches.
		select {
		case <-execCtx.Done():
			// Suspend all non-terminal tasks so the dashboard shows them as
			// stopped rather than still running.
			for _, b := range batches {
				for _, t := range b.Tasks {
					if !t.State.IsTerminal() {
						_ = e.Store.ForceTransitionState(t.ID, TaskStateSuspended, "cancelled-by-user")
					}
				}
			}
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return batches, fmt.Errorf("cancelled: %w", ctx.Err())
		default:
		}

		// Run this batch with cycle support.
		cycleLimit := batch.MaxCycles
		if cycleLimit <= 0 {
			cycleLimit = 1 // at minimum one cycle
		}

		if batch.UseDW {
			// DW pipeline execution: multi-verifier per task, no Leader review.
			e.runDWCycle(ctx, batch, cycleLimit, masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches, escalator, workdir, decomposerTimeout, leaderModel)
			// Cross-stage artifact passing for completed DW batch.
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
		} else {
			// Normal execution with Leader review cycle.
			var prevFindings *CycleFindingsSet
			dryCount := 0
			batchStart := time.Now()

		batchCycleLoop:
			for cycle := 0; cycle < cycleLimit; cycle++ {
				batch.CycleCount = cycle + 1

				// Execute all tasks in this batch (parallel if concurrency > 0).
				if err := e.RunBatch(execCtx, batch); err != nil {
					BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
					e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
					return batches, fmt.Errorf("run batch %s cycle %d: %w", batch.ID, cycle, err)
				}

				// Pick up self-split children for the next cycle.
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

				// Loop-until-dry: collect findings, exit when no new ones for 2 cycles.
				currentFindings := e.collectCycleFindings(batch, cycle+1)
				if prevFindings != nil && !currentFindings.HasNewFindings(prevFindings) {
					dryCount++
					if dryCount >= 2 {
						if e.Loggers != nil {
							e.Loggers.Engine("batch %s: dry after %d cycles", batch.ID, cycle+1)
						}
						completedBatches[batch.ID] = true
						if len(currentFindings.Findings) == 0 {
							batch.Status = BatchStatusPassed
						} else {
							batch.Status = BatchStatusFailed
						}
						completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
						e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
						BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
						break batchCycleLoop
					}
				} else {
					dryCount = 0
				}
				prevFindings = currentFindings

				// Check for tasks needing re-decomposition (retries exhausted).
				newTasks, recount := escalator.LogSuspendedTasks(batch, masterTaskID, workdir, decomposerTimeout, leaderModel,
					func(id string) (*Task, error) { return e.Store.GetTask(id) },
					func(pt PlanTask, batchID, mtID string, parentIDs []string) (*Task, error) {
						profile := ToolProfile(pt.Profile)
						task, err := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, parentIDs, 0, workdir, pt.VerifierFocus, batchID, mtID)
						if err != nil {
							return nil, err
						}
						task.BatchID = batchID
						task.UseDW = pt.UseDW
						task.VerifierRole = pt.VerifierRole
						_ = e.Store.UpdateTask(task.ID, map[string]interface{}{"batch_id": batchID, "master_task_id": mtID})
						return task, nil
					},
					func(taskID string, state TaskState, reason string) error {
						return e.Store.ForceTransitionState(taskID, state, reason)
					},
				)
				// Pick up new re-decomposed tasks for the dashboard.
				if len(newTasks) > 0 {
					_ = e.Store.Checkpoint()
				}
				// Add new child tasks to the batch so RunBatch picks them up.
				batch.Tasks = append(batch.Tasks, newTasks...)
				// If new child tasks were created by the escalator, extend the
				// cycle limit and skip Leader review — child tasks must run
				// first before the Leader can judge the batch.
				if recount > 0 {
					if cycle == cycleLimit-1 {
						cycleLimit++
					}
					continue // skip Leader review, run child tasks next cycle
				}

				// Write board.md after each batch cycle.
				if boardContent := e.Whiteboard.BuildBoardContent(batches); boardContent != "" {
					_ = e.Whiteboard.WriteBoard(boardContent)
				}

				// Check for self-split children before declaring batch done.
				anyNew := false
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
				if anyNew {
					if cycle == cycleLimit-1 {
						cycleLimit++ // extend limit for children
					}
					continue // run children in next cycle
				}

				// Batch done. Verifier already judged every task — skip Leader review.
				completedBatches[batch.ID] = true
				passedBatches[batch.ID] = true
				batch.Status = BatchStatusPassed
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
				break batchCycleLoop
			}
		}
	}

	// Step 4: Write final deliverable.md.
	if delContent := e.Whiteboard.BuildDeliverableContent(batches); delContent != "" {
		_ = e.Whiteboard.WriteDeliverable(delContent)
	}

	// Step 5: Leader final summary — collect all outputs and produce
	// a user-facing summary of what was accomplished.
	e.assembleParentOutputs(batches)
	e.writeMasterOutput(goal, batches, workdir)

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
) {
	var prevFindings *CycleFindingsSet
	dryCount := 0
	batchStart := time.Now()
	defer func() {
		BatchDone(batch.ID, string(batch.Status), time.Since(batchStart).Seconds())
	}()

	for cycle := 0; cycle < cycleLimit; cycle++ {
		batch.CycleCount = cycle + 1

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
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
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
				completedBatches[batch.ID] = true
				if len(currentFindings.Findings) == 0 {
					batch.Status = BatchStatusPassed
				} else {
					batch.Status = BatchStatusFailed
				}
				completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
				e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
				return
			}
		} else {
			dryCount = 0
		}
		prevFindings = currentFindings

		// Escalator: re-decompose stuck tasks (retries exhausted).
		newTasks, recount := escalator.LogSuspendedTasks(batch, masterTaskID, workdir, decomposerTimeout, leaderModel,
			func(id string) (*Task, error) { return e.Store.GetTask(id) },
			func(pt PlanTask, batchID, mtID string, parentIDs []string) (*Task, error) {
				profile := ToolProfile(pt.Profile)
				task, err := e.CreateTask(pt.Title, pt.Description, AgentRole(pt.Role), profile, parentIDs, 0, workdir, pt.VerifierFocus, batchID, mtID)
				if err != nil {
					return nil, err
				}
				task.BatchID = batchID
				task.UseDW = pt.UseDW
				task.VerifierRole = pt.VerifierRole
				_ = e.Store.UpdateTask(task.ID, map[string]interface{}{"batch_id": batchID, "master_task_id": mtID})
				return task, nil
			},
			func(taskID string, state TaskState, reason string) error {
				return e.Store.ForceTransitionState(taskID, state, reason)
			},
		)
		batch.Tasks = append(batch.Tasks, newTasks...)
		// Flush WAL so dashboard sees new re-decomposed tasks.
		if len(newTasks) > 0 {
			_ = e.Store.Checkpoint()
		}
		if recount > 0 {
			if cycle == cycleLimit-1 {
				cycleLimit++
			}
			continue // skip further processing, run child tasks next cycle
		}

		// Write board after each cycle.
		if boardContent := e.Whiteboard.BuildBoardContent(batches); boardContent != "" {
			_ = e.Whiteboard.WriteBoard(boardContent)
		}

		// Check if all tasks passed.  Only retry failed tasks (not suspended —
		// those are handled by Escalator).
		allPassed := true
		for _, t := range batch.Tasks {
			current, err := e.Store.GetTask(t.ID)
			if err != nil {
				continue
			}
			if current == nil {
				continue
			}
			if current.State == TaskStateFailed {
				allPassed = false
				_ = e.Store.ForceTransitionState(t.ID, TaskStateAssigned, "dw-retry")
			} else if current.State != TaskStateDone && current.State != TaskStateSuspended {
				allPassed = false
			}
		}

		if allPassed {
			batch.Status = BatchStatusPassed
			completedBatches[batch.ID] = true
			completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
			e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
			return
		}
	}

	// Max cycles reached — auto-accept whatever we have.
	batch.Status = BatchStatusFailed
	completedBatches[batch.ID] = true
	completedBatchOutputs[batch.ID] = e.collectBatchOutputs(batch)
	e.saveCheckpoint(masterTaskID, completedBatches, passedBatches, completedBatchOutputs, batches)
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

	// Use a semaphore channel to limit concurrency.
	sem := make(chan struct{}, concurrency)
	type taskResult struct {
		taskID string
		ok     bool
		err    error
	}
	resultCh := make(chan taskResult, len(tasks))

	for _, task := range tasks {
		select {
		case <-ctx.Done():
			// Suspend all not-yet-launched tasks.
			for _, t := range tasks {
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

	// Estimate batch cost: worker + verifier tokens per task × rounds.
	for _, t := range batch.Tasks {
		batch.TotalTokens += (t.RetryCount + 1) * 10000 // ~10K tokens per worker+verifier round
	}
	return nil
}
