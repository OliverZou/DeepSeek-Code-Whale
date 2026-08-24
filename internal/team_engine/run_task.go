package team_engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Core lifecycle
// ---------------------------------------------------------------------------

// RunTask runs a single task through the full produce→verify→done lifecycle.
//
// Steps:
//  1. pending → assigned
//  2. Write whiteboard input
//  3. assigned → producing, spawn Worker via SubagentSpawner
//  4. Worker done → produced (write output to whiteboard)
//  5. produced → verifying, spawn Verifier
//  6. Verifier PASS → verified → done
//  7. Verifier FAIL → increment retry, append feedback, loop to step 3
//  8. Retry budget exhausted → failed
//
// Returns true if the task reached done, false otherwise.
func (e *TeamEngine) RunTask(ctx context.Context, taskID string) (bool, error) {
	// Phase 0: Initial state check and assignment (under lock).
	if defaultTeamLog != nil {
		Log("task", "task: %s start", taskID[:8])
	}
	e.mu.Lock()
	task, err := e.Store.GetTask(taskID)
	if err != nil {
		e.mu.Unlock()
		return false, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q not found", taskID)
	}

	// File-based completion: task-dir output + verifier → skip execution.
	// State is derived from files, not read from meta.json.
	dir := e.Whiteboard.TaskDir(taskID)
	state := e.Store.DeriveState(dir)
	if state == TaskStateDone {
		e.mu.Unlock()
		Log("task", "task: %s skipped (done: output+verify exist)", taskID[:8])
		return true, nil
	}

	// Guard: only start from PENDING, ASSIGNED, or PRODUCED.
	if task.State != TaskStatePending && task.State != TaskStateAssigned && task.State != TaskStateProduced {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q is in state %s; can only run from pending/assigned/produced", taskID, task.State)
	}

	// When resuming with existing output, skip produce and go straight to verification.
	skipProduce := task.State == TaskStateProduced

	// Step 1: Assign (only for pending tasks).
	if task.State == TaskStatePending {
		if err := e.AssignTask(taskID); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("assign task: %w", err)
		}
		task.State = TaskStateAssigned
	}
	e.mu.Unlock()

	// Main retry loop.
	for attempt := 0; attempt < task.MaxRetries; attempt++ {
		// Check for cancellation before each retry.
		select {
		case <-ctx.Done():
			_ = e.Store.ForceTransitionState(taskID, TaskStateSuspended, "cancelled-by-user")
			e.fireEvent(TaskEvent{Type: EventStateChanged})
			return false, ctx.Err()
		default:
		}
		// ---- Phase 1: Producing ----------------------------------------
		// ---- Phase 1: Producing ----------------------------------------
		// Build inbox.md with file-path references, not inline content.
		// Agent reads inbox.md → does work → writes output file.

		// Collect upstream output file references from parent tasks.
		var upstreamRefs []UpstreamRef
		for _, pid := range task.ParentIDs {
			if pt, ptErr := e.Store.GetTask(pid); ptErr == nil && pt != nil && pt.Output != "" {
				upstreamRefs = append(upstreamRefs, UpstreamRef{Name: pt.Title, Path: pt.Output})
			}
		}
		// Also include done same-batch task outputs.
		if task.BatchID != "" && task.MasterTaskID != "" {
			batchTasks, _ := e.Store.ListTasksByMasterTask(task.MasterTaskID)
			for _, bt := range batchTasks {
				if bt.BatchID == task.BatchID && bt.ID != task.ID && bt.State == TaskStateDone && bt.Output != "" {
					upstreamRefs = append(upstreamRefs, UpstreamRef{Name: bt.Title, Path: bt.Output})
				}
			}
		}

		// ---- Phase 1: Producing (skip if resuming with existing output) --
		workerKey := "worker:" + taskID
		if !skipProduce {
			// Persistent session retry: send Verifier feedback to existing
			// Worker session instead of re-spawning.
			if ws := e.persistentSessions[workerKey]; ws != nil && e.shellSpawner != nil {
				fbPrompt := verifierFeedbackForWorker(task.VerifierFeedback)
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker CONTINUE retry=%d", taskID[:8], attempt)
				}
				workerStart := time.Now()
				resp := e.shellSpawner.ContinueSession(ws, fbPrompt)
				contResult := &RunResult{
					SessionID:       resp.SessionID,
					ExitCode:        resp.ExitCode,
					Stdout:          resp.Output,
					Stderr:          resp.Diagnostic,
					DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
					Success:         resp.Success,
					PID:             resp.PID,
				}
				Log("timing", "task %s worker continue done in %.1fs", taskID[:8], time.Since(workerStart).Seconds())
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker CONTINUE DONE in %.1fs (success=%v)", taskID[:8], time.Since(workerStart).Seconds(), resp.Success)
				}
				if contResult.Stdout != "" {
					e.Whiteboard.WriteOutput(task.ID, contResult.Stdout)
				}
				goto verifyPhase
			}

			// Read team template and memory.
			template := e.readTeamTemplate(task.Output)
			memory, _ := e.BuildMemoryContext(task.Role, task.Title)

			// Write structured inbox.md.
			inboxParams := InboxParams{
				Title:           task.Title,
				Role:            string(task.Role),
				Description:     task.Description,
				Output:          task.Output,
				UpstreamOutputs: upstreamRefs,
				Template:        template,
				Memory:          memory,
				AllowSelfSplit:  len(task.ParentIDs) == 0,
				RetryFeedback:   task.VerifierFeedback,
			}
			if err := e.Whiteboard.WriteInboxFile(task.ID, inboxParams); err != nil {
				return false, fmt.Errorf("write inbox: %w", err)
			}

			// Agent prompt: reference inbox.md, working dir is out/.
			inboxPath := filepath.Join(e.Whiteboard.TaskDir(task.ID), "input.md")
			outDir := filepath.Join(e.Whiteboard.TaskDir(task.ID), "out")
			prompt := fmt.Sprintf("[Role: %s]\n\n工作目录: %s\n任务文件: %s\n产出: %s\n\n将产出文件写在你的工作目录下。只汇报实际完成的内容，不虚构数字。",
				task.Role, outDir, inboxPath, task.Output)
			if len(task.ParentIDs) == 0 {
				prompt += "\n\n如果任务过大无法一次完成，在产出开头输出 [SPLIT_PLAN] 拆分。"
			}

			// State: assigned → producing (under lock).
			e.mu.Lock()
			if err := e.Store.TransitionState(taskID, TaskStateProducing, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to producing: %w", err)
			}
			e.mu.Unlock()
			e.fireEvent(TaskEvent{Type: EventStateChanged, TaskID: taskID, NewState: string(TaskStateProducing)})

			workdir := task.Workdir
			if workdir == "" {
				workdir = "."
			}
			// Agent runs in a sandboxed out/ directory.
			// task.Workdir keeps the original workspace for checker/build reference.
			agentWorkdir := filepath.Join(e.Whiteboard.TaskDir(taskID), "out")
			os.MkdirAll(agentWorkdir, 0755)

			// Coding Harness (场景2): create or reuse an isolated git worktree
			// for coding tasks. On retry the same worktree is reused so the
			// Worker keeps accumulating changes on one branch.
			var hasWorktree bool
			if e.worktreeEnabled && isWorktreeEligibleRole(task.Role) {
				branch := e.activeBranch(task.ID)
				if branch == "" {
					wtPath, newBranch, err := e.createWorktree(task.ID)
					if err == nil {
						branch = newBranch
						// Append worktree info to prompt only on first creation.
						prompt += fmt.Sprintf("\n\n## Git Worktree: %s\nYou are working in an isolated git branch `%s`. All changes are safe.\nWhen finished, describe what you changed.",
							wtPath, branch)
					}
				}
				if branch != "" {
					wtPath := filepath.Join(e.worktreeDir, ".whale", "worktrees", branch)
					agentWorkdir = wtPath
					task.Workdir = wtPath
					task.ArtifactPath = branch
					hasWorktree = true
				}
			}

			toolNames := ProfileToToolNames(task.Profile)
			toolsStr := strings.Join(toolNames, ",")

			// Live streaming: write each agent output line to whiteboard in real-time.
			liveOutput := func(status, summary, toolName string) {
				if summary != "" {
					if err := e.Whiteboard.AppendTaskOutput(task.ID,
						fmt.Sprintf("[%s] %s: %s", status, toolName, summary)); err != nil {
						// Best-effort streaming; don't fail on write error.
					}
				}
			}

			// Resolve per-role timeout from config instead of using the
			// hardcoded default.  Research/deep-analysis roles need more
			// time (900-1800s) than the default 300s.
			taskTimeout := time.Duration(e.Router.ResolveTimeout(task.Role, false)) * time.Second
			// Register a per-task cancel so Close() / Kill() can immediately
			// abort running subagents instead of waiting for them to finish.
			taskCtx, taskCancel := context.WithCancel(ctx)
			e.mu.Lock()
			e.activeCancels[taskID] = taskCancel
			e.mu.Unlock()
			// onPID callback: track the OS process ID as soon as it starts,
			// so the stop button can kill it during execution.
			onPID := func(pid int) {
				if pid > 0 {
					e.mu.Lock()
					e.activeAgents[taskID] = pid
					e.mu.Unlock()
				}
			}
			onStdin := func(w io.WriteCloser) {
				e.mu.Lock()
				e.activeStdinWriters[taskID] = w
				e.mu.Unlock()
			}
			workerKey := "worker:" + taskID
			workerStart := time.Now()
			var result *RunResult

			// Persistent session: on retry, send Verifier feedback to the
			// existing Worker session instead of restarting from scratch.
			if ws := e.persistentSessions[workerKey]; ws != nil && e.shellSpawner != nil {
				fbPrompt := verifierFeedbackForWorker(task.VerifierFeedback)
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker CONTINUE session (retry=%d)", taskID[:8], attempt)
				}
				resp := e.shellSpawner.ContinueSession(ws, fbPrompt)
				result = &RunResult{
					SessionID:       resp.SessionID,
					ExitCode:        resp.ExitCode,
					Stdout:          resp.Output,
					Stderr:          resp.Diagnostic,
					DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
					Success:         resp.Success,
					PID:             resp.PID,
				}
			} else if e.shellSpawner != nil {
				// First attempt: spawn a persistent Worker session.
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker SPAWN persistent role=%s", taskID[:8], task.Role)
				}
				req := SubagentRequest{
					Task:      prompt,
					Role:      string(task.Role),
					Model:     "",
					Tools:     toolNames,
					Workdir:   agentWorkdir,
					Timeout:   taskTimeout,
					MaxIters:  80,
					MaxCalls:  200,
					MaxTokens: effectiveMaxTokens(0, ""),
					OnPID:     onPID,
				}
				ws, resp := e.shellSpawner.SpawnPersistent(context.Background(), req)
				if ws != nil {
					e.persistentSessions[workerKey] = ws
				}
				result = &RunResult{
					SessionID:       resp.SessionID,
					ExitCode:        resp.ExitCode,
					Stdout:          resp.Output,
					Stderr:          resp.Diagnostic,
					DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
					Success:         resp.Success,
					PID:             resp.PID,
				}
			} else {
				// Fallback: normal spawn via Runner.
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker START role=%s", taskID[:8], task.Role)
				}
				result = e.Runner.RunWithContext(taskCtx, prompt, agentWorkdir, toolsStr, taskTimeout, liveOutput, onPID, onStdin)
			}
			Log("timing", "task %s worker done in %.1fs (success=%v)", taskID[:8], time.Since(workerStart).Seconds(), result.Success)
			if e.Loggers != nil {
				e.Loggers.Engine("task %s worker DONE in %.1fs (success=%v exit=%d)", taskID[:8], time.Since(workerStart).Seconds(), result.Success, result.ExitCode)
			}
			// Clean up nested .whale created by whale exec in the agent sandbox.
			_ = os.RemoveAll(filepath.Join(agentWorkdir, ".whale"))
			e.mu.Lock()
			delete(e.activeCancels, taskID)
			delete(e.activeAgents, taskID)
			delete(e.activeStdinWriters, taskID)
			e.mu.Unlock()
			taskCancel()

			if !result.Success {
				errorContent := fmt.Sprintf("\nERROR (exit %d):\n%s", result.ExitCode, result.Stderr)
				if err := e.Whiteboard.AppendOutput(task.ID, errorContent); err != nil {
					return false, fmt.Errorf("write error output: %w", err)
				}
				// Write the partial stdout anyway.
				if result.Stdout != "" {
					if err := e.Whiteboard.WriteOutput(task.ID, result.Stdout); err != nil {
						return false, fmt.Errorf("write partial stdout: %w", err)
					}
				}
			} else {
				if err := e.Whiteboard.WriteOutput(task.ID, result.Stdout); err != nil {
					return false, fmt.Errorf("write worker output: %w", err)
				}
			}
			if defaultTeamLog != nil {
				defaultTeamLog.WorkerDone(task.ID, result.DurationSeconds, result.ExitCode, len(result.Stdout), result.Success)
			}

			// Persist subagent session ID for traceability.
			if result.SessionID != "" {
				_ = e.Store.UpdateTask(task.ID, map[string]interface{}{"session_id": result.SessionID})
			}

			// Self-split: only top-level tasks (no parents) can split.
			// Children must complete without further splitting.
			const splitMarker = "[SPLIT_PLAN]"
			if len(task.ParentIDs) == 0 && result.Success && strings.Contains(result.Stdout, splitMarker) {
				idx := strings.Index(result.Stdout, splitMarker)
				splitJSON := result.Stdout[idx+len(splitMarker):]
				childPlan, err := ParsePlanTasks(splitJSON)
				if err == nil && len(childPlan) > 0 {
					if defaultTeamLog != nil {
						Log("task", "task: %s self-split into %d children", task.ID[:8], len(childPlan))
					}
					var createdChildren []*Task
					for _, pt := range childPlan {
						child, err := e.CreateTask(pt.Title, pt.Description, task.Role, task.Profile, []string{task.ID}, 0, workdir, pt.VerifierFocus, task.BatchID, task.MasterTaskID)
						if err != nil {
							if defaultTeamLog != nil {
								Log("task", "task: %s child create failed: %v", task.ID[:8], err)
							}
							continue
						}
						child.Output = pt.Output
						child.BatchID = task.BatchID
						child.MasterTaskID = task.MasterTaskID
						_ = e.Store.UpdateTask(child.ID, map[string]interface{}{"batch_id": task.BatchID, "master_task_id": task.MasterTaskID})
						createdChildren = append(createdChildren, child)
						if defaultTeamLog != nil {
							Log("task", "task: %s child %s created", task.ID[:8], child.ID[:8])
						}
					}
					e.writeTaskPlanJSON(task, createdChildren)
					// Mark parent as done (children carry the work forward).
					_ = e.Store.TransitionState(taskID, TaskStateDone, "", "self-split into children")
					e.fireEvent(TaskEvent{Type: EventStateChanged})
					return true, nil
				}
			}

			// Coding Harness: commit then collect git diff after worker completes.
			if hasWorktree {
				_ = e.commitWorktree(task.ID)
				diff := e.collectWorktreeDiff(task.ID)
				diffContent := fmt.Sprintf("\n\n## git diff\n```diff\n%s\n```", diff)
				if err := e.Whiteboard.AppendOutput(task.ID, diffContent); err != nil {
					// Best-effort; don't fail on diff write error.
				}
				// Save diff as a deliverable artifact.
				_ = e.Whiteboard.CopyArtifact(task.ID, task.ID[:8]+".diff", diffContent)
			}

			// Log worker output for dashboard dialogue.
			if e.Loggers != nil {
				e.Loggers.LogAgent("worker", taskID, attempt+1, prompt, result.Stdout, result.ExitCode, time.Duration(result.DurationSeconds)*time.Second, nil)
				e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: taskID})
			}

			// State: producing → produced (under lock).
			e.mu.Lock()
			if err := e.Store.TransitionState(taskID, TaskStateProduced, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to produced: %w", err)
			}
			e.mu.Unlock()
		} // end if !skipProduce

		// Reset for subsequent retry iterations.
		skipProduce = false

	verifyPhase:
		// ---- Phase 2: Verifying ----------------------------------------
		var passed bool
		var feedback string
		var verifyDur time.Duration
		var v *Verifier

		// Transition to verifying (skip the separate checking phase —
		// the Verifier agent performs mechanical checks itself).
		e.mu.Lock()
		if err := e.Store.TransitionState(taskID, TaskStateVerifying, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to verifying: %w", err)
		}
		e.mu.Unlock()

		if task.UseDW {
			// Dynamic Workflow mode: N verifiers in parallel + Synthesizer.
			passed, _, feedback = e.runDWVerification(task)
		} else {
			verifierAgentName := e.resolveVerifierAgentName(task)
			verifierModel := ""
			if e.team != nil && e.team.Config != nil {
				verifierModel = e.team.Config.Model.VerifierDefault
			}
			verifyStart := time.Now()
			vKey := "verifier:" + taskID

			// Persistent Verifier session: reuse process across retries.
			if ws := e.persistentSessions[vKey]; ws != nil && e.shellSpawner != nil {
				if e.Loggers != nil {
					e.Loggers.Engine("task %s verifier CONTINUE session", taskID[:8])
				}
				v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName)
				prompt := v.BuildPrompt(task)
				resp := e.shellSpawner.ContinueSession(ws, prompt)
				verifyDur = time.Since(verifyStart)
				if resp.Success {
					passed, _ = parseVerdict(resp.Output)
					feedback = resp.Output
				} else {
					passed = false
					feedback = resp.Diagnostic
					if feedback == "" {
						feedback = resp.Output
					}
				}
			} else if e.shellSpawner != nil {
				if e.Loggers != nil {
					e.Loggers.Engine("task %s verifier SPAWN persistent agent=%s", taskID[:8], verifierAgentName)
				}
				v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName)
				prompt := v.BuildPrompt(task)
				req := SubagentRequest{
					Task:      prompt,
					Role:      "verifier",
					AgentName: verifierAgentName,
					Workdir:   task.Workdir,
					Timeout:   time.Duration(e.Router.ResolveTimeout(task.Role, true)) * time.Second,
					MaxIters:  15,
					MaxCalls:  50,
					MaxTokens: effectiveMaxTokens(0, verifierModel),
				}
				if verifierModel != "" {
					req.Model = verifierModel
				}
				ws, resp := e.shellSpawner.SpawnPersistent(context.Background(), req)
				if ws != nil {
					e.persistentSessions[vKey] = ws
				}
				verifyDur = time.Since(verifyStart)
				if resp.Success {
					passed, _ = parseVerdict(resp.Output)
					feedback = resp.Output
				} else {
					passed = false
					feedback = resp.Diagnostic
					if feedback == "" {
						feedback = resp.Output
					}
				}
			} else {
				// Fallback: normal spawn via Runner.
				v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName)
				if e.Loggers != nil {
					e.Loggers.Engine("task %s verifier START agent=%s", taskID[:8], verifierAgentName)
				}
				passed, _, feedback, err = v.Verify(task)
				verifyDur = time.Since(verifyStart)
				if err != nil {
					return false, fmt.Errorf("verifier error: %w", err)
				}
			}
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier DONE in %.1fs (pass=%v)", taskID[:8], verifyDur.Seconds(), passed)
			}
		}

		// Log verifier output for dashboard dialogue.
		if e.Loggers != nil {
			verdict := "PASS"
			if !passed {
				verdict = "FAIL"
			}
			var verifierPrompt string
			if v != nil {
				verifierPrompt = v.LastPrompt
			} else {
				verifierPrompt = "mechanical"
			}
			e.Loggers.LogAgent("verifier", taskID, attempt+1, verifierPrompt, fmt.Sprintf("[%s] %s", verdict, feedback), 0, verifyDur, nil)
			e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: taskID})
		}

		if passed {
			e.mu.Lock()
			if err := e.Store.TransitionState(taskID, TaskStateVerified, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to verified: %w", err)
			}
			if err := e.Store.TransitionState(taskID, TaskStateDone, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to done: %w", err)
			}
			if err := e.Store.UpdateTask(taskID, map[string]interface{}{
				"retry_count": attempt,
			}); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("update retry count: %w", err)
			}
			e.mu.Unlock()

			// Coding Harness: merge the worktree branch back into the main repo
			// now that verification passed.
			if e.activeBranch(taskID) != "" {
				if err := e.mergeWorktree(taskID); err != nil {
					// Merge conflict/failure: abort to restore the main repo and
					// keep the worktree branch for manual inspection. The diff
					// artifact already holds the worker's changes.
					abort := exec.Command("git", "merge", "--abort")
					abort.Dir = e.worktreeDir
					_ = abort.Run()
					Log("worktree", "task %s merge failed (branch kept): %v", taskID[:8], err)
				} else {
					e.cleanupWorktree(taskID)
				}
			} else {
				// Non-worktree: the Worker ran in a sandboxed out/ directory.
				// Copy its results back into the task's workdir so the user's
				// project actually reflects the completed work.
				if err := e.propagateTaskOutput(taskID, task.Workdir); err != nil {
					Log("task", "task %s output propagation failed: %v", taskID[:8], err)
				}
			}

			// Write verify file — file-based completion proof.
			if task.Output != "" {
				os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(taskID), "verify.md"), []byte(feedback), 0644)
			}
			// Record lesson for future agents with the same role.
			e.recordLesson(task.Role, task.Title, truncateLesson(feedback, 80))
			return true, nil
		}

		// Verification failed — prepare retry.
		e.mu.Lock()
		task, err = e.Store.GetTask(taskID)
		if err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("re-get task for retry: %w", err)
		}
		if task == nil {
			e.mu.Unlock()
			return false, fmt.Errorf("task %q disappeared", taskID)
		}

		task.RetryCount = attempt + 1
		task.VerifierFeedback = feedback
		// Strip any previous verifier feedback blocks to prevent
		// prompt bloat across retries (context grows unboundedly).
		if idx := strings.Index(task.Description, "\n\n[VERIFIER FEEDBACK"); idx >= 0 {
			task.Description = task.Description[:idx]
		}
		// Guard: empty feedback from a lazy verifier wastes Worker time.
		// Generate a meaningful default so the Worker knows what to fix.
		if len(strings.TrimSpace(feedback)) < 30 {
			feedback = "Previous attempt did not pass verification. " +
				"Please re-read the original task requirements carefully and " +
				"ensure ALL requirements are met. Verify your output is " +
				"complete — no truncated code, all functions implemented, " +
				"and the deliverable compiles or passes basic checks."
		}
		task.Description += fmt.Sprintf(
			"\n\n[VERIFIER FEEDBACK - Attempt %d]\n%s",
			task.RetryCount, feedback,
		)

		if err := e.Store.UpdateTask(taskID, map[string]interface{}{
			"retry_count":       task.RetryCount,
			"description":       task.Description,
			"verifier_feedback": feedback,
		}); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("update task for retry: %w", err)
		}

		// 0 issues → pass immediately.
		currCount := len(ParseFindings(feedback))
		if currCount == 0 && hasVerdictMarkers(feedback) {
			e.mu.Unlock()
			e.closePersistentSession("worker:" + taskID)
			if task.Output != "" {
				os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(taskID), "verify.md"), []byte(feedback), 0644)
			}
			e.recordLesson(task.Role, task.Title, truncateLesson(feedback, 80))
			return true, nil
		}
		// Stagnation: same findings 2 rounds → suspend.
		if attempt >= 2 {
			prevFindings := ParseFindings(task.VerifierFeedback)
			currFindings := ParseFindings(feedback)
			if findingsAreSame(prevFindings, currFindings) {
				if err := e.Store.TransitionState(taskID, TaskStateSuspended, "same findings 2 rounds — early re-decomposition", feedback); err != nil {
					e.mu.Unlock()
					return false, fmt.Errorf("transition to suspended: %w", err)
				}
				e.mu.Unlock()
				return false, nil
			}
		}

		if task.RetryCount >= task.MaxRetries {
			// Don't fail — the leader should re-decompose just this task.
			if err := e.Store.TransitionState(taskID, TaskStateSuspended, "retries exhausted — needs re-decomposition", feedback); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to suspended: %w", err)
			}
			e.mu.Unlock()
			return false, nil
		}

		// Reset state back to assigned for retry.
		if err := e.Store.TransitionState(taskID, TaskStateAssigned, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to assigned for retry: %w", err)
		}
		e.mu.Unlock()

		task.State = TaskStateAssigned
	}

	return false, nil
}
