package team_engine

import (
	"context"
	"encoding/json"
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

	// Guard: only start from PENDING, ASSIGNED, or PRODUCED. Read the derived
	// file state, not the shared *Task.State field, so this read never races
	// with a concurrent refreshState write.
	if state != TaskStatePending && state != TaskStateAssigned && state != TaskStateProduced {
		e.mu.Unlock()
		return false, fmt.Errorf("task %q is in state %s; can only run from pending/assigned/produced", taskID, state)
	}

	// When resuming with existing output, skip produce and go straight to verification.
	skipProduce := state == TaskStateProduced

	// Step 1: Assign (only for pending tasks). AssignTask transitions the
	// store's in-memory state under fs.mu, so no direct field write here.
	if state == TaskStatePending {
		if err := e.AssignTask(taskID); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("assign task: %w", err)
		}
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

		// Collect upstream output file references (parents + done same-batch
		// siblings) as a lock-held value snapshot — see Store.UpstreamOutputs.
		upstreamRefs := e.Store.UpstreamOutputs(task)

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
					UsagePrompt:     resp.UsagePrompt,
					UsageCompletion: resp.UsageCompletion,
				}
				e.addTokens(resp.UsagePrompt, resp.UsageCompletion)
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
			// 产物会在任务 done 后复制到用户 workspace 根目录。用相对路径引用
			// 沙箱外文件（如 ../../game.js）在复制后层级改变会失效——要求用绝对路径。
			prompt += "\n\n【路径约束】你的产出文件会被复制到用户 workspace 根目录后交付。严禁用相对路径（如 ../../game.js）引用工作目录之外的文件，这类路径在复制后失效。若需引用其他任务的产出文件，请使用其绝对路径。"

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
			wIters, wCalls, wTokens := iterationBudget(task.Complexity, false)
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
				if !resp.Success {
					// The session died (timeout/crash) — drop it so the next
					// retry spawns a fresh session instead of reusing a dead one.
					e.closePersistentSession(workerKey)
				}
				result = &RunResult{
					SessionID:       resp.SessionID,
					ExitCode:        resp.ExitCode,
					Stdout:          resp.Output,
					Stderr:          resp.Diagnostic,
					DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
					Success:         resp.Success,
					PID:             resp.PID,
					UsagePrompt:     resp.UsagePrompt,
					UsageCompletion: resp.UsageCompletion,
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
					MaxIters:  wIters,
					MaxCalls:  wCalls,
					MaxTokens: effectiveMaxTokens(wTokens, ""),
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
					UsagePrompt:     resp.UsagePrompt,
					UsageCompletion: resp.UsageCompletion,
				}
			} else {
				// Fallback: normal spawn via Runner.
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker START role=%s", taskID[:8], task.Role)
				}
				result = e.Runner.RunWithContext(taskCtx, prompt, agentWorkdir, toolsStr, taskTimeout, wIters, wCalls, wTokens, liveOutput, onPID, onStdin)
			}
			Log("timing", "task %s worker done in %.1fs (success=%v)", taskID[:8], time.Since(workerStart).Seconds(), result.Success)
			if e.Loggers != nil {
				e.Loggers.Engine("task %s worker DONE in %.1fs (success=%v exit=%d)", taskID[:8], time.Since(workerStart).Seconds(), result.Success, result.ExitCode)
			}
			// Clean up nested .whale created by whale exec in the agent sandbox.
			e.addTokens(result.UsagePrompt, result.UsageCompletion)
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
				if childPlan, err := ParsePlanTasks(splitJSON); err == nil && len(childPlan) > 0 {
					if e.splitTaskIntoChildren(task, childPlan, workdir, "self-split into children") {
						return true, nil
					}
				}
			}

			// Tool-cap 触顶：worker 被 forceSummary 强制中断（任务过重，远超叶子
			// 规模）。把任务再拆成叶子，而不是让 verifier 对不完整 summary 判 PASS。
			const interruptedMarker = "This turn was auto-interrupted"
			if len(task.ParentIDs) == 0 && result.Success && strings.Contains(result.Stdout, interruptedMarker) {
				childPlan, err := e.decomposeTaskIntoLeaves(task.Description, workdir, time.Duration(e.Router.ResolveDecomposerTimeout())*time.Second, e.Router.ResolveModel("planner"))
				if err == nil && len(childPlan) > 0 {
					if e.splitTaskIntoChildren(task, childPlan, workdir, "split into children after tool cap") {
						return true, nil
					}
				} else {
					Log("task", "task: %s tool-cap hit but split failed (err=%v children=%d), fall through to verifier", task.ID[:8], err, len(childPlan))
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
				e.Loggers.LogAgent("worker", taskID, attempt+1, prompt, result.SystemPrompt, result.Stdout, result.ExitCode, time.Duration(result.DurationSeconds)*time.Second, nil)
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
		var skipVerifier bool

		// Non-worktree: the Worker produced in its sandbox out/ directory, but
		// task.Workdir still points at the original workspace (output is only
		// propagated there after a PASS). Point the Verifier at the sandbox so it
		// inspects the actual deliverables instead of an empty workspace.
		verifyWorkdir := task.Workdir
		if e.activeBranch(taskID) == "" {
			verifyWorkdir = filepath.Join(e.Whiteboard.TaskDir(taskID), "out")
		}

		// Transition to verifying (skip the separate checking phase —
		// the Verifier agent performs mechanical checks itself).
		e.mu.Lock()
		if err := e.Store.TransitionState(taskID, TaskStateVerifying, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to verifying: %w", err)
		}
		e.mu.Unlock()

		// 客观门（机械）：worker 交付的自动化测试先机械跑一遍。失败则跳过
		// LLM 验证器直接重试，省掉 300s+ 的验证器开销（验证器超时被杀是复测
		// 假 FAIL 的主因）。未检测到测试则跳过，交给 LLM 语义审查。
		if !task.UseDW {
			if ok, detail := runMechanicalVerify(verifyWorkdir); !ok {
				passed = false
				feedback = "交付物的自动化测试未通过（客观门）：\n" + detail
				if e.Loggers != nil {
					e.Loggers.Engine("task %s MECHANICAL GATE FAIL (skip LLM verifier)", taskID[:8])
				}
				goto verifyFailed
			}
		}

		// 回归风险抽样：机械客观门通过后，纯新建独立文件（未触及任何已存在
		// workspace 文件）且无关键 verifier 角色的任务跳过 LLM 语义 verifier，
		// 避免为独立页面文件付 worker+verifier 双份 LLM。客观门每次都跑。
		skipVerifier = e.shouldSkipVerifier(task, verifyWorkdir)

		if skipVerifier {
			passed = true
			feedback = "mechanical gate only（客观门通过，交付物为新建独立文件，跳过语义审查）"
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier SKIP (mechanical gate only, low regression risk)", taskID[:8])
			}
			// 写 verifier.md 以驱动 file-based state 到 done —— 等价于 verifier.Verify
			// 内部的 WriteVerifier 行为：跳过语义审查只是不付 LLM，产出证据照写。
			if err := e.Whiteboard.WriteVerifier(taskID, feedback); err != nil {
				Log("task", "task %s write verifier marker failed: %v", taskID[:8], err)
			}
		} else if task.UseDW {
			// Dynamic Workflow mode: N verifiers in parallel + Synthesizer.
			passed, _, feedback = e.runDWVerification(task)
		} else {
			verifierAgentName := e.resolveVerifierAgentName(task)
			verifierModel := ""
			if e.team != nil && e.team.Config != nil {
				verifierModel = e.team.Config.Model.VerifierDefault
			}
			verifyStart := time.Now()
			vIters, vCalls, vTokens := iterationBudget(task.Complexity, true)
			vKey := "verifier:" + taskID
			var verifyPromptTokens, verifyCompletionTokens int

			// Persistent Verifier session: reuse process across retries.
			if ws := e.persistentSessions[vKey]; ws != nil && e.shellSpawner != nil {
				if e.Loggers != nil {
					e.Loggers.Engine("task %s verifier CONTINUE session", taskID[:8])
				}
				v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName).WithWorkdir(verifyWorkdir)
				prompt := v.BuildPrompt(task)
				resp := e.shellSpawner.ContinueSession(ws, prompt)
				verifyPromptTokens, verifyCompletionTokens = resp.UsagePrompt, resp.UsageCompletion
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
					// Session died (timeout/crash) — drop it so the next retry
					// spawns a fresh verifier session.
					e.closePersistentSession(vKey)
				}
			} else if e.shellSpawner != nil {
				if e.Loggers != nil {
					e.Loggers.Engine("task %s verifier SPAWN persistent agent=%s", taskID[:8], verifierAgentName)
				}
				v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName).WithWorkdir(verifyWorkdir)
				prompt := v.BuildPrompt(task)
				req := SubagentRequest{
					Task:      prompt,
					Role:      "verifier",
					AgentName: verifierAgentName,
					Workdir:   verifyWorkdir,
					Timeout:   time.Duration(e.Router.ResolveTimeout(task.Role, true)) * time.Second,
					MaxIters:  vIters,
					MaxCalls:  vCalls,
					MaxTokens: effectiveMaxTokens(vTokens, verifierModel),
				}
				if verifierModel != "" {
					req.Model = verifierModel
				}
				ws, resp := e.shellSpawner.SpawnPersistent(context.Background(), req)
				if ws != nil {
					e.persistentSessions[vKey] = ws
				}
				verifyPromptTokens, verifyCompletionTokens = resp.UsagePrompt, resp.UsageCompletion
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
					// Session died (timeout/crash) — drop it so the next retry
					// spawns a fresh verifier session instead of reusing a dead one.
					e.closePersistentSession(vKey)
				}
			} else {
				// Fallback: normal spawn via Runner.
				v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName).WithWorkdir(verifyWorkdir)
				if e.Loggers != nil {
					e.Loggers.Engine("task %s verifier START agent=%s", taskID[:8], verifierAgentName)
				}
				passed, _, feedback, err = v.Verify(task)
				verifyPromptTokens, verifyCompletionTokens = v.LastPromptTokens, v.LastCompletionTokens
				verifyDur = time.Since(verifyStart)
				if err != nil {
					return false, fmt.Errorf("verifier error: %w", err)
				}
			}
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier DONE in %.1fs (pass=%v)", taskID[:8], verifyDur.Seconds(), passed)
			}
			e.addTokens(verifyPromptTokens, verifyCompletionTokens)
		}

		// Log verifier output for dashboard dialogue.
		if e.Loggers != nil {
			verdict := "PASS"
			if !passed {
				verdict = "FAIL"
			}
			var verifierPrompt, verifierSystemPrompt string
			if v != nil {
				verifierPrompt = v.LastPrompt
				verifierSystemPrompt = v.LastSystemPrompt
			} else {
				verifierPrompt = "mechanical"
			}
			e.Loggers.LogAgent("verifier", taskID, attempt+1, verifierPrompt, verifierSystemPrompt, fmt.Sprintf("[%s] %s", verdict, feedback), 0, verifyDur, nil)
			e.fireEvent(TaskEvent{Type: EventAgentLog, TaskID: taskID})
		}

		if passed {
			// Non-worktree: propagate BEFORE marking done so a mechanical re-check
			// can downgrade the verdict if the propagated deliverable is broken
			// (e.g. hardcoded sandbox-relative paths that no longer resolve after
			// copy — see #24).
			if e.activeBranch(taskID) == "" {
				if err := e.propagateTaskOutput(taskID, task.Workdir); err != nil {
					Log("task", "task %s output propagation failed: %v", taskID[:8], err)
				} else if ok, detail := runMechanicalVerify(task.Workdir); !ok {
					passed = false
					feedback = "交付物传播到 workspace 后机械验证失败（自动化测试/构建未通过）：\n" + detail
				}
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
				}

				// Write verify file — file-based completion proof.
				if task.Output != "" {
					os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(taskID), "verify.md"), []byte(feedback), 0644)
				}
				// Record lesson for future agents with the same role.
				e.recordLesson(task.Role, task.Title, truncateLesson(verifierFeedbackForWorker(feedback), 80))
				return true, nil
			}
			// passed=false after mechanical verification: fall through to retry.
		}

	verifyFailed:
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

		// Build retry state in local variables — never write the shared *Task
		// pointer outside the store lock (UpdateTask owns those writes under
		// fs.mu). prevFeedback captures the previous round's feedback before it
		// is overwritten, so stagnation detection compares two distinct rounds.
		newRetryCount := attempt + 1
		prevFeedback := task.VerifierFeedback
		desc := task.Description
		// Strip any previous verifier feedback blocks to prevent
		// prompt bloat across retries (context grows unboundedly).
		if idx := strings.Index(desc, "\n\n[VERIFIER FEEDBACK"); idx >= 0 {
			desc = desc[:idx]
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
		desc += fmt.Sprintf(
			"\n\n[VERIFIER FEEDBACK - Attempt %d]\n%s",
			newRetryCount, feedback,
		)

		if err := e.Store.UpdateTask(taskID, map[string]interface{}{
			"retry_count":       newRetryCount,
			"description":       desc,
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
			e.recordLesson(task.Role, task.Title, truncateLesson(verifierFeedbackForWorker(feedback), 80))
			return true, nil
		}
		// Stagnation: same findings 2 rounds → suspend.
		if attempt >= 2 {
			prevFindings := ParseFindings(prevFeedback)
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

		if newRetryCount >= task.MaxRetries {
			// Don't fail — the leader should re-decompose just this task.
			if err := e.Store.TransitionState(taskID, TaskStateSuspended, "retries exhausted — needs re-decomposition", feedback); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to suspended: %w", err)
			}
			e.mu.Unlock()
			return false, nil
		}

		// Reset state back to assigned for retry. TransitionState updates the
		// store's in-memory state under fs.mu, so no direct field write here.
		if err := e.Store.TransitionState(taskID, TaskStateAssigned, "", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("transition to assigned for retry: %w", err)
		}
		e.mu.Unlock()
	}

	return false, nil
}

// runMechanicalVerify 在传播后的 workdir 检测并运行标准测试/构建命令，
// 确认交付物真正可用。未检测到自动化测试时返回 passed=true（不误判）。
func runMechanicalVerify(workdir string) (passed bool, detail string) {
	name, args := detectTestCommand(workdir)
	if name == "" {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("%s %s\n%s", name, strings.Join(args, " "), string(out))
	}
	return true, ""
}

// outDeliverables walks outDir and reports how many files the worker produced
// and whether any of them already exist at the corresponding path in workdir —
// i.e. the worker modified an existing workspace file rather than creating a
// brand-new standalone deliverable.
func outDeliverables(outDir, workdir string) (count int, touches bool, err error) {
	walkErr := filepath.Walk(outDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// .whale is whale's own metadata/log dir (written by `whale exec` in
			// the sandbox), not a worker deliverable. Its relative path overlaps the
			// master engine's log under the workspace, so counting it as a touch
			// would misclassify every pure-new deliverable as high regression risk.
			if info.Name() == ".whale" {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		rel, relErr := filepath.Rel(outDir, path)
		if relErr != nil {
			return relErr
		}
		if _, statErr := os.Stat(filepath.Join(workdir, rel)); statErr == nil {
			touches = true
		}
		return nil
	})
	return count, touches, walkErr
}

// shouldSkipVerifier decides whether the LLM semantic verifier can be skipped
// after the mechanical gate passes. It is conservative: only brand-new
// standalone deliverables (touching no pre-existing workspace file) from
// mechanical roles are eligible. The mechanical gate (automated tests) always
// runs regardless; worktree and DW modes never skip.
func (e *TeamEngine) shouldSkipVerifier(task *Task, verifyWorkdir string) bool {
	if task.UseDW {
		return false
	}
	// Worktree mode edits the repo in place via a branch; regression assessment
	// needs a git diff — be conservative and always verify.
	if e.activeBranch(task.ID) != "" {
		return false
	}
	// Subjective/key roles (architecture, security, docs) always verify.
	if task.VerifierRole != "" {
		return false
	}
	// High regression risk: the deliverable touches an existing workspace file.
	// An empty out/ (worker produced nothing) also verifies — a missing
	// deliverable is not a low-risk standalone file.
	count, touches, err := outDeliverables(verifyWorkdir, task.Workdir)
	if err != nil || touches || count == 0 {
		return false
	}
	// No automated test command means the mechanical gate ran nothing — it
	// trivially "passes" and would release the deliverable with zero objective
	// check. Never skip semantic verification in that case.
	if name, _ := detectTestCommand(verifyWorkdir); name == "" {
		return false
	}
	return true
}

// detectTestCommand 在 workdir 检测标准自动化测试/构建命令，返回命令名与参数。
// 返回空 name 表示未检测到自动化测试。
func detectTestCommand(workdir string) (name string, args []string) {
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}

	// Go module：优先 go test。
	if exists(filepath.Join(workdir, "go.mod")) {
		return "go", []string{"test", "./..."}
	}
	// Node 测试文件（test/ 目录或根目录 *.test.js）。
	// 注意：必须用无参数的 `node --test`，让 Node 自动发现 test/ 与 *.test.js。
	// 传目录名（如 `node --test test`）会被 Node 26 当作模块路径去 require，
	// 报 MODULE_NOT_FOUND，导致客观门误判交付物失败。
	if matches, _ := filepath.Glob(filepath.Join(workdir, "test", "*.test.js")); len(matches) > 0 {
		return "node", []string{"--test"}
	}
	if matches, _ := filepath.Glob(filepath.Join(workdir, "*.test.js")); len(matches) > 0 {
		return "node", []string{"--test"}
	}
	// package.json 有非占位 test script。
	if pkg := filepath.Join(workdir, "package.json"); exists(pkg) {
		if script := npmTestScript(pkg); script != "" && !isNoopNpmTest(script) {
			return "npm", []string{"test"}
		}
	}
	return "", nil
}

// npmTestScript 读取 package.json 的 scripts.test。
func npmTestScript(pkgPath string) string {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Scripts["test"]
}

// isNoopNpmTest 判断 npm 默认占位 test script（未定义真实测试）。
func isNoopNpmTest(script string) bool {
	return strings.Contains(strings.ToLower(script), "no test specified")
}

// splitTaskIntoChildren 把一个已存在的任务拆成子任务（childPlan 来自
// self-split 的 [SPLIT_PLAN] 输出，或 tool-cap 触顶后的重新分解）。
// 返回 true 表示拆分成功且父任务已标记 done，调用者应 return true。
func (e *TeamEngine) splitTaskIntoChildren(task *Task, childPlan []PlanTask, workdir, reason string) bool {
	if len(childPlan) == 0 {
		return false
	}
	if defaultTeamLog != nil {
		Log("task", "task: %s %s into %d children", task.ID[:8], reason, len(childPlan))
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
		child.UpstreamBatches = task.UpstreamBatches
		_ = e.Store.UpdateTask(child.ID, map[string]interface{}{"batch_id": task.BatchID, "master_task_id": task.MasterTaskID})
		createdChildren = append(createdChildren, child)
		if defaultTeamLog != nil {
			Log("task", "task: %s child %s created", task.ID[:8], child.ID[:8])
		}
	}
	if len(createdChildren) == 0 {
		return false
	}
	e.writeTaskPlanJSON(task, createdChildren)
	// Mark parent as done (children carry the work forward). TransitionState
	// alone only flips the in-memory state; the file store derives "done" from
	// output.md + verifier.md, so persist a verifier.md or the batch done-check
	// re-reads the parent as "produced" and fails the batch.
	_ = e.Whiteboard.WriteVerifier(task.ID, "split into children: work carried forward")
	_ = e.Store.TransitionState(task.ID, TaskStateDone, "", reason)
	e.fireEvent(TaskEvent{Type: EventStateChanged})
	return true
}
