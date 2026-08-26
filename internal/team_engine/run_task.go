package team_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

	// Suspended tasks are resumable (state machine: suspended → pending). The
	// re-run through team_run is the resume path — without it a suspended task
	// (v24: integration-verification task whose headless-Chrome E2E false-
	// failed and hit the tool-iteration cap) could never be re-settled and
	// stayed a permanent leftover for the Leader's review turn.
	if state == TaskStateSuspended {
		if err := e.Store.TransitionState(taskID, TaskStatePending, "resumed by re-run", ""); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("resume suspended task: %w", err)
		}
		state = TaskStatePending
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
	// Workspace snapshot before the worker runs; on resume (skipProduce) it
	// stays nil and verifyDepth falls back to the conservative semantic depth.
	var baseline map[string]time.Time

	// Step 1: Assign (only for pending tasks). AssignTask transitions the
	// store's in-memory state under fs.mu, so no direct field write here.
	if state == TaskStatePending {
		if err := e.AssignTask(taskID); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("assign task: %w", err)
		}
	}
	e.mu.Unlock()

	// 工具探针：跨 attempt 累计（重试多轮都计入），progress 回调来自后台
	// goroutine，需加锁。toolWaitMS 累计工具执行等待时长（墙钟归因：
	// 分得清“LLM 在想”还是“工具在等”，浏览器/脚本等待是长尾主源）。
	var (
		toolProbeMu sync.Mutex
		toolEvents  int
		toolHist    = map[string]int{}
		toolWaitMS  int64
	)

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
		if !skipProduce {
			// Read team template and memory.
			template := e.readTeamTemplate(task.Output)
			memory, _ := e.BuildMemoryContext(task.Role, task.Title)

			// Write structured inbox.md.
			inboxParams := InboxParams{
				Title:              task.Title,
				Role:               string(task.Role),
				Description:        task.Description,
				Output:             task.Output,
				AcceptanceCriteria: task.AcceptanceCriteria,
				UpstreamOutputs:    upstreamRefs,
				Template:           template,
				Memory:             memory,
				AllowSelfSplit:     taskMaySelfSplit(task),
				RetryFeedback:      task.VerifierFeedback,
			}
			if err := e.Whiteboard.WriteInboxFile(task.ID, inboxParams); err != nil {
				return false, fmt.Errorf("write inbox: %w", err)
			}

			// Agent prompt: reference inbox.md. The working directory IS the
			// user's workspace — deliverables are written in place (no sandbox,
			// no copy-back stage).
			inboxPath := filepath.Join(e.Whiteboard.TaskDir(task.ID), "input.md")
			// Empty workdir must NEVER fall back to cwd: the worker writes
			// deliverables directly into its workdir, and "." would resolve to
			// the process cwd (the repo root in smoke runs), polluting the
			// host checkout. An empty workdir is a task-configuration bug;
			// surface it instead of guessing.
			if strings.TrimSpace(task.Workdir) == "" {
				return false, fmt.Errorf("task %s has empty workdir; refusing to run into process cwd", taskID[:8])
			}
			workdir := task.Workdir
			if wd, err := filepath.Abs(workdir); err != nil {
				return false, fmt.Errorf("resolve workdir: %w", err)
			} else {
				workdir = wd
				// Write back so the verify phase (verifyWorkdir = task.Workdir)
				// resolves against the same absolute deliverable dir instead of
				// the raw relative task.Workdir (a "." would silently depend on
				// the engine process cwd).
				task.Workdir = wd
			}
			prompt := fmt.Sprintf("[Role: %s]\n\n工作目录: %s\n任务文件: %s\n产出: %s\n\n将产出文件直接写在你的工作目录（用户 workspace）下。只汇报实际完成的内容，不虚构数字。交付前对照任务文件里的「验收标准」逐项自检，确认完整性与正确性。",
				task.Role, workdir, inboxPath, task.Output)
			if len(task.ParentIDs) == 0 {
				prompt += "\n\n如果任务过大无法一次完成，在产出开头输出 [SPLIT_PLAN] 拆分。"
			}
			// 产出直接落在用户 workspace,不再有 sandbox+复制环节:临时/验证脚本
			// 若写在工作目录会留在交付区域污染交付物,必须放系统临时目录。
			prompt += "\n\n【过程文件】执行用的临时脚本（测试脚手架、验证脚本、运行数据）请写入系统临时目录，不要作为交付文件留在工作目录下。同一工作区其他任务的产出文件可直接用相对路径引用。"
			// 验收/审计纪律：只验证不改——跨任务改写交付文件会引入契约漂移
			// （v45: 集成验收 1.29M tokens 反复修改 game-dom.js，verifier 判 FAIL）。
			if isReportTask(task) || isVerificationTask(task) {
				prompt += "\n\n【验收纪律】本任务是验证/审计：只验证、不改写。除非任务描述明确要求你产出新文件，否则禁止修改或覆盖其它任务的交付文件；发现问题在报告中如实记录并交 Leader 决策，不要自行修复被测代码。"
			}
			// 成本纪律：每轮工具调用都会重发全部会话历史，轮数与工具结果大小直接
			// 决定总 token（v31_full fix 任务 59 轮/1.45M tokens 的教训）。同一文件
			// 只读一次、优先 grep 定位、不做范围外工作，可显著压低轮数与上下文增长。
			prompt += "\n\n【成本纪律】只读必要的文件，同一文件在本次会话中最多读取一次；需要定位时优先用 grep 搜索，不要整读大文件；只做任务要求的改动，不要重构、重排或重写其它任务的产出。任务描述要求依据的清单/报告（如 AUDIT_FINDINGS_*.md）若已在工作区，先读取再动手；若不存在则如实说明，不要自行编造或代做其它任务的工作。交付完成即总结退出，不要追加额外检索。\n\n【集中读全，再动手】开始实现前把这件任务所需的全部上游/契约/相关文件一次性完整读完（大文件按 offset/limit 分段读完，不要只读头尾），在此基础上建立完整上下文再动手；不要边实现边逐文件补读。只是把分散读取集中到开头——每个该读的文件仍要完整读，绝不为了省轮次而漏读或只读部分。"
			// 端到端验收任务：脚本化 + 环境复用——v34 实测 40 轮 739s 的 QA 会话里
			// 446s（61%）耗在 4 个「重装/启动浏览器+逐条验」的 shell 等待轮；一次
			// 脚本覆盖全部验收点、复用系统已装环境，可把轮数与墙钟同时砍半以上。
			if e2eTitleMarked(task.Title) {
				prompt += "\n\n【端到端验收规范】用浏览器/运行时验收时：\n1. 写一个**小型**临时脚本（Node + Playwright 或等价，≤120 行）一次运行，逐项输出 PASS/FAIL 与证据；脚本调试最多 2 轮，仍不通过就降级：改用最小可行验证（加载无错误 + 键盘移动 + 分数/持久化这三个核心点），或直接以已有证据完成判定并如实标注未验证项。\n2. 本机已安装 Playwright 与 Chrome/Chromium（用户目录 ms-playwright 缓存与 Google Chrome），直接复用并优先 channel:'chrome'；**禁止重新下载或安装浏览器/包**。\n3. **交付物先写后调**：每完成一个阶段就把已有证据写入产出文件（如 AUDIT_FINDINGS_*.md）；工具轮数预算将尽时，用已有证据落盘报告，**绝不允许以'计划文本/设计文档'作为产出**。\n4. 报告即证据：引用命令输出/断言结果原文；不要输出超长设计叙述。\n5. 脚本放系统临时目录；禁止逐条交互式重验。"
			}

			// Baseline snapshot for regression-risk assessment: compare workspace
			// file modtimes AFTER the worker runs to distinguish new deliverables
			// from overwritten/modified existing files.
			baseline = snapshotWorkdir(workdir)

			// State: assigned → producing (under lock).
			e.mu.Lock()
			if err := e.Store.TransitionState(taskID, TaskStateProducing, "", ""); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to producing: %w", err)
			}
			e.mu.Unlock()
			e.fireEvent(TaskEvent{Type: EventStateChanged, TaskID: taskID, NewState: string(TaskStateProducing)})

			// Agent runs directly in the user workspace — deliverables materialize
			// in place, no copy-back. task.Workdir is the deliverable location.
			agentWorkdir := workdir
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

			// 工具面保持任务原 profile：v40 曾把报告任务降到 verify 面（无 write），
			// 实测 LLM 反复尝试不可用工具→撞 tool cap→触发自拆递归（batch3 变成
			// 13 任务/8M token）。读+写+shell 保持原样，靠预算封顶与禁自拆控制成本。
			toolNames := ProfileToToolNames(task.Profile)
			toolsStr := strings.Join(toolNames, ",")

			// Live streaming: write each agent output line to whiteboard in real-time.
			// 工具探针：进度回调累计事件与工具直方图（探针声明在 attempt 循环外）。
			liveOutput := func(status, summary, toolName string, toolDurationMS int64) {
				toolProbeMu.Lock()
				toolEvents++
				if toolName != "" {
					toolHist[toolName]++
				}
				if toolDurationMS > 0 {
					toolWaitMS += toolDurationMS
				}
				toolProbeMu.Unlock()
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
			wIters, wCalls, wTokens := effectiveWorkerBudget(task)
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
			workerStart := time.Now()
			var result *RunResult

			// On retry with a recorded session, prompt the existing member
			// session with the Verifier feedback instead of re-spawning —
			// preserves the Worker's failure context (partial work, exploration
			// state) across retries. First attempt (or no SessionOps wired)
			// falls through to a fresh spawn.
			var continued bool
			if ops := e.getSessionOps(); ops != nil {
				if sessionID := e.Store.SessionID(taskID); sessionID != "" {
					continued = true
					fbPrompt := verifierFeedbackForWorker(task.VerifierFeedback)
					if e.Loggers != nil {
						e.Loggers.Engine("task %s worker CONTINUE session %s (retry=%d)", taskID[:8], sessionID, attempt)
					}
					resp, err := ops.Continue(taskCtx, sessionID, fbPrompt)
					if err != nil {
						result = &RunResult{
							SessionID: sessionID,
							ExitCode:  -1,
							Stderr:    fmt.Sprintf("continue session: %v", err),
							Success:   false,
						}
					} else {
						result = &RunResult{
							SessionID:       resp.SessionID,
							ExitCode:        resp.ExitCode,
							Stdout:          resp.Output,
							Stderr:          resp.Diagnostic,
							DurationSeconds: round(time.Since(workerStart).Seconds(), 2),
							Success:         resp.Success,
							UsagePrompt:     resp.UsagePrompt,
							UsageCompletion: resp.UsageCompletion,
							SystemPrompt:    resp.SystemPrompt,
							PID:             resp.PID,
						}
					}
				}
			}
			if !continued {
				// First attempt: normal spawn via Runner.
				if e.Loggers != nil {
					e.Loggers.Engine("task %s worker START role=%s", taskID[:8], task.Role)
				}
				// System-level write boundary: the worker may only write its
				// declared outputs (+ system temp dir for verification scripts).
				// This prevents a worker overwriting another task's deliverable
				// in the shared workdir (engine-level invariant, not a hint).
				allowlist := splitOutputEntries(task.Output)
				exemptDirs := []string{os.TempDir()}
				result = e.Runner.RunWithContext(taskCtx, prompt, agentWorkdir, toolsStr, taskTimeout, wIters, wCalls, wTokens, allowlist, exemptDirs, liveOutput, onPID, onStdin)
			}
			Log("timing", "task %s worker done in %.1fs (success=%v)", taskID[:8], time.Since(workerStart).Seconds(), result.Success)
			if e.Loggers != nil {
				e.Loggers.Engine("task %s worker DONE in %.1fs (success=%v exit=%d)", taskID[:8], time.Since(workerStart).Seconds(), result.Success, result.ExitCode)
			}
			// Clean up nested .whale created by whale exec inside the agent's
			// workdir — it would otherwise clutter the user's workspace.
			// The engine's own team_tasks lives under the same .whale when the
			// agent runs in the workspace (b.root == workdir) — never delete it:
			// it holds the plan.json, task records and masters of the run.
			e.addTokens(result.UsagePrompt, result.UsageCompletion)
			if entries, err := os.ReadDir(filepath.Join(agentWorkdir, ".whale")); err == nil {
				for _, entry := range entries {
					if entry.Name() == "team_tasks" {
						continue
					}
					_ = os.RemoveAll(filepath.Join(agentWorkdir, ".whale", entry.Name()))
				}
			}
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

			// Persist subagent session ID + run stats. Per-task token numbers feed
			// the heartbeat logs and run_report.json for post-run analysis.
			statsFields := map[string]interface{}{
				"worker_duration_seconds": result.DurationSeconds,
				"worker_tokens":           result.UsagePrompt + result.UsageCompletion,
			}
			if result.SessionID != "" {
				statsFields["session_id"] = result.SessionID
			}
			// 工具探针落盘：累计事件数 + 最重 Top2 工具 + 工具等待时长。
			toolProbeMu.Lock()
			if toolEvents > 0 {
				statsFields["tool_calls"] = toolEvents
				if toolWaitMS > 0 {
					statsFields["tool_wait_seconds"] = float64(toolWaitMS) / 1000
				}
				type kv struct {
					k string
					n int
				}
				hist := make([]kv, 0, len(toolHist))
				for k, n := range toolHist {
					hist = append(hist, kv{k, n})
				}
				sort.Slice(hist, func(i, j int) bool { return hist[i].n > hist[j].n })
				top := make([]string, 0, min(2, len(hist)))
				for i := 0; i < min(2, len(hist)); i++ {
					top = append(top, fmt.Sprintf("%s:%d", hist[i].k, hist[i].n))
				}
				statsFields["top_tools"] = strings.Join(top, ",")
			}
			toolProbeMu.Unlock()
			_ = e.Store.UpdateTask(task.ID, statsFields)
			if e.Loggers != nil {
				e.Loggers.Engine("task %s worker tokens prompt=%d completion=%d (run total=%d)", taskID[:8], result.UsagePrompt, result.UsageCompletion, e.tokenTotal())
			}

			// Self-split: only top-level tasks (no parents) can split.
			// Children must complete without further splitting.
			const splitMarker = "[SPLIT_PLAN]"
			if taskMaySelfSplit(task) && result.Success && strings.Contains(result.Stdout, splitMarker) {
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
			if taskMaySelfSplit(task) && result.Success && strings.Contains(result.Stdout, interruptedMarker) {
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

			// Delivery manifest: log which files the worker added or modified
			// in the deliverable workdir, comparing against the pre-worker
			// baseline snapshot taken in Phase 1. Worktree mode is excluded —
			// the git diff above already records its change set.
			if baseline != nil && !hasWorktree {
				if changed, err := changedDeliverables(workdir, baseline); err == nil && len(changed) > 0 {
					Log("whiteboard", "task %s delivered %d file(s): %s", task.ID[:8], len(changed), strings.Join(changed, ", "))
					// 所有权交叉校验（v47/Q8）：实际交付必须是任务 Output 声明的文件；
					// worker 写了别人家文件（并行覆盖隐患）记录为交付异常并警告。
					if unexpected := unexpectedDeliverables(task, workdir, changed); len(unexpected) > 0 {
						Log("whiteboard", "task %s WARN: delivered files outside declared output: %s (ownership anomaly)", task.ID[:8], strings.Join(unexpected, ", "))
						if e.Loggers != nil {
							e.Loggers.Engine("task %s ownership anomaly: extra files %s not in output %q", task.ID[:8], strings.Join(unexpected, ", "), task.Output)
						}
					}
					// 交付清单落盘：完成事件带出，leader 汇报渲染可点击链接。
					if werr := e.Whiteboard.WriteDelivered(task.ID, changed); werr != nil {
						Log("whiteboard", "task %s write delivered.txt failed: %v", task.ID[:8], werr)
					}
				}
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

		// ---- Phase 2: Verifying ----------------------------------------
		var passed bool
		var feedback string
		var verifyDur time.Duration
		var v *Verifier

		// Deliverables were written directly into task.Workdir — verify there.
		verifyWorkdir := task.Workdir

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
			if ok, detail := runMechanicalVerify(verifyWorkdir, verifyWorkdir); !ok {
				passed = false
				feedback = "交付物的自动化测试未通过（客观门）：\n" + detail
				if e.Loggers != nil {
					e.Loggers.Engine("task %s MECHANICAL GATE FAIL (skip LLM verifier)", taskID[:8])
				}
				goto verifyFailed
			}
		}

		// 深度按需：Leader 在分解时判定（verify_mode，验证环节恒有）；未判定
		// （auto）时由启发式回退。mechanical = 客观门 + worker 自检（不调 LLM
		// 验证器）；semantic = 独立系统验证器交叉验证。
		if e.verifyDepth(task, verifyWorkdir, baseline) == "mechanical" {
			passed = true
			feedback = "mechanical gate only（客观门通过，验证深度=机械，无语义审查）"
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier SKIP (mechanical depth, low regression risk)", taskID[:8])
			}
			// 写 verifier.md 以驱动 file-based state 到 done —— 等价于 verifier.Verify
			// 内部的 WriteVerifier 行为：机械深度只是不付 LLM，产出证据照写。
			if err := e.Whiteboard.WriteVerifier(taskID, feedback); err != nil {
				Log("task", "task %s write verifier marker failed: %v", taskID[:8], err)
			}
			_ = e.Store.UpdateTask(taskID, map[string]interface{}{"verdict": "MECHANICAL"})
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
			var verifyPromptTokens, verifyCompletionTokens int

			// Normal spawn via Runner. 系统 verifier 定义（verifier.md）由 adapter
			// 特判注入，不查团队/项目/用户 agent 文件.
			v = NewVerifier(e.Whiteboard, e.Runner, e.Router, 0, verifierModel).WithAgentName(verifierAgentName).WithWorkdir(verifyWorkdir)
			// 复审轮：把上一轮验证报告注入 prompt，verifier 只复审未通过/无法
			// 验证项，避免全量重验的重复 token（协作协议：不重复验证已通过项）。
			// 条件只看 feedback 是否存在：Leader 重派被 suspend 的验证任务时
			// resume 路径从 attempt=0 重跑，但 VerifierFeedback 已归档——首轮
			// 也必须是复审轮（v44 缺口）。
			if task.VerifierFeedback != "" {
				v = v.WithPreviousReport(task.VerifierFeedback)
			}
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier START agent=%s", taskID[:8], verifierAgentName)
			}
			passed, _, feedback, err = v.Verify(task)
			verifyPromptTokens, verifyCompletionTokens = v.LastPromptTokens, v.LastCompletionTokens
			verifyDur = time.Since(verifyStart)
			if err != nil {
				return false, fmt.Errorf("verifier error: %w", err)
			}
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier DONE in %.1fs (pass=%v)", taskID[:8], verifyDur.Seconds(), passed)
			}
			// 验证结论事件：leader 叙述“严过关完成独立验证，判定 PASS/FAIL”。
			verdict := "FAIL"
			if passed {
				verdict = "PASS"
			}
			e.fireEvent(TaskEvent{Type: EventVerifierResult, TaskID: taskID, Title: task.Title, Data: verdict})
			e.addTokens(verifyPromptTokens, verifyCompletionTokens)
			verifyStats := map[string]interface{}{
				"verifier_duration_seconds": verifyDur.Seconds(),
				"verifier_tokens":           verifyPromptTokens + verifyCompletionTokens,
			}
			// 事后分析轨迹：验证结论 + 验证者 cap 中断次数。
			if passed {
				verifyStats["verdict"] = "PASS"
			} else {
				verifyStats["verdict"] = "FAIL"
			}
			if v.LastCapRetries > 0 {
				verifyStats["verifier_cap_count"] = v.LastCapRetries
			}
			_ = e.Store.UpdateTask(taskID, verifyStats)
			if e.Loggers != nil {
				e.Loggers.Engine("task %s verifier tokens prompt=%d completion=%d (run total=%d)", taskID[:8], verifyPromptTokens, verifyCompletionTokens, e.tokenTotal())
			}
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

			// Write verify file — file-based completion proof.  Unconditional:
			// verify.md is the PASS-only marker that deriveState keys on (it
			// must NOT be gated on Output, else a passed task with empty Output
			// would leave only verifier.md — which is also written on FAIL — and
			// be re-derived as not-done).  The verifier report lives in
			// verifier.md and stays readable either way.
			os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(taskID), "verify.md"), []byte(feedback), 0644)
			return true, nil
		}

	verifyFailed:
		// Verifier 自身被 tool cap 中断（输出 missing "auto-interrupted"）≠
		// 交付物缺陷：打回 worker 会陷入“verifier 中断→worker 重做→verifier
		// 又中断”循环（v41: fix 3 次重试 / 2.1M tokens）。挂起保留报告，交
		// Leader/用户升级（与 isVerificationTask 的“无法验证”处理一致）。
		if isVerifierCapInterrupt(feedback) {
			_ = e.Store.UpdateTask(taskID, map[string]interface{}{"verdict": "CAP-INTERRUPT"})
			if err := e.Store.TransitionState(taskID, TaskStateSuspended, "verifier interrupted by tool cap — cannot verify", feedback); err != nil {
				return false, err
			}
			return false, nil
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

		// 纯验证/验收任务的 FAIL 是「被验证对象有缺陷」的有效结论，不是本任务
		// 交付物本身的问题。重试同一验证 worker 只会重复发现同一缺陷（验证者不
		// 修复代码），空转浪费 token。直接挂起，保留验证报告与缺陷清单，交
		// Leader/用户决策修复。
		if isVerificationTask(task) {
			if err := e.Store.TransitionState(taskID, TaskStateSuspended, "verification task reported defects — needs upstream fix", feedback); err != nil {
				e.mu.Unlock()
				return false, fmt.Errorf("transition to suspended: %w", err)
			}
			e.mu.Unlock()
			return false, nil
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
		// Feedback itself is carried ONLY in task.VerifierFeedback (the
		// input.md "上一轮审查反馈" section) — it must not also be appended to
		// Description, else the same content is injected twice per retry and
		// tokens balloon (2048 case: identical [VERIFIER FEEDBACK] + re-review
		// blocks reaching 1.4M tokens).
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
		// Tag the feedback with its attempt so the worker still sees how many
		// rounds elapsed, without duplicating it into the description.
		feedback = fmt.Sprintf("[Attempt %d]\n%s", newRetryCount, feedback)

		if err := e.Store.UpdateTask(taskID, map[string]interface{}{
			"retry_count":       newRetryCount,
			"description":       desc,
			"verifier_feedback": feedback,
		}); err != nil {
			e.mu.Unlock()
			return false, fmt.Errorf("update task for retry: %w", err)
		}

		// 记录失败教训：仅当 verifier 给出结构化 findings 时，供同角色
		// 后续 worker 参考。PASS 无教训价值，不记录。
		if len(ParseFindings(feedback)) > 0 {
			e.recordLesson(task.Role, task.Title, truncateLesson(verifierFeedbackForWorker(feedback), 80))
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

// runMechanicalVerify 在交付物的 workdir/outDir 检测并运行标准测试/构建命令，
// 确认交付物真正可用。未检测到自动化测试时返回 passed=true（不误判）。
//
// 验证范围是「交付物」而非整个 workspace：机械验证目标从 worker 的产物目录
// （outDir）推断——outDir 根或直接子目录含 go.mod 表示交付了独立 module，
// 只在对应 workdir 路径内验证。这避免在仓库根跑全量 go test ./...，把与本
// 任务无关的既有失败（如 whale 仓库自身的预存 Windows 失败）误判为交付物
// 失败、触发无关 retry 并把上万行日志灌入任务描述（冒烟实测 input.md 膨胀
// 到 12.7MB）。无独立 module 交付时回退到 workdir 本身——改动现有文件的
// 任务需要在整个 workspace 验证回归。
func runMechanicalVerify(workdir, outDir string) (passed bool, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	targets := mechanicalTargets(workdir, outDir)
	// Log the resolved targets — the gate must never silently fall back to the
	// process cwd (repo root); if workdir ever mis-resolves, the log shows it.
	if len(targets) == 0 {
		Log("task", "mechverify: no targets (workdir=%q) - skipping mechanical check", workdir)
		return true, ""
	}
	Log("task", "mechverify: workdir=%q outDir=%q targets=%v", workdir, outDir, targets)

	var failures []string
	for _, target := range targets {
		name, args := detectTestCommand(target)
		if name == "" {
			// 该目标无自动化测试——交给 LLM 语义审查，不误判。
			continue
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = target
		out, err := cmd.CombinedOutput()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s\n%s", name, strings.Join(args, " "), truncateMechanicalDetail(string(out))))
		}
	}
	if len(failures) == 0 {
		return true, ""
	}
	return false, strings.Join(failures, "\n---\n")
}

// mechanicalTargets 返回机械验证应执行的目录列表。交付的独立 module 以
// outDir（worker 产物暂存）为唯一证据：outDir 内根/直接子目录含 go.mod 的
// 目录才是本任务的交付物；其余 workspace 路径（仓库根、既有 module）都不
// 属于验证范围。无独立 module 交付时回退到 workdir（改动现有文件的任务）。
// 空 workdir 返回空列表——机械验证不接触任何目录（空 workdir 是任务配置
// bug，若回退到 "." 会解析到进程 cwd=仓库根，把仓库自身的失败算进交付物）。
func mechanicalTargets(workdir, outDir string) []string {
	if strings.TrimSpace(workdir) == "" {
		return nil
	}
	var rels []string
	if _, err := os.Stat(filepath.Join(outDir, "go.mod")); err == nil {
		rels = append(rels, ".")
	}
	if entries, err := os.ReadDir(outDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if _, err := os.Stat(filepath.Join(outDir, e.Name(), "go.mod")); err == nil {
				rels = append(rels, e.Name())
			}
		}
	}
	if len(rels) == 0 {
		return []string{workdir}
	}
	targets := make([]string, 0, len(rels))
	for _, rel := range rels {
		targets = append(targets, filepath.Join(workdir, rel))
	}
	return targets
}

// mechanicalGateAvailable 报告 outDir 中是否存在可执行的客观测试门。
// mechanical 深度必须有一个真实测试门，否则机械门 trivially pass 会在零
// 客观检查下放行交付物（verifyDepth 的 auto 启发式依赖它）。
func mechanicalGateAvailable(outDir string) bool {
	for _, target := range mechanicalTargets(outDir, outDir) {
		if name, _ := detectTestCommand(target); name != "" {
			return true
		}
	}
	return false
}

// truncateMechanicalDetail 截断机械验证失败输出，防止上万行测试日志经
// retry feedback 灌入任务描述造成 input 膨胀（冒烟实测 12.7MB）。保留头
// 部失败摘要即可，完整输出在 tool-result/引擎日志可见。
func truncateMechanicalDetail(out string) string {
	const keep = 8192
	if len(out) <= keep {
		return out
	}
	// 按合法 UTF-8 边界截断（一个字符最多回退 4 字节）。
	end := keep
	for end > 0 && !utf8.ValidString(out[:end]) {
		end--
	}
	return out[:end] + "\n…(output truncated)"
}

// snapshotWorkdir records rel path → modtime for every file under workdir,
// skipping Whale state (.whale), VCS internals (.git) and dependency dirs.
// RunTask takes the snapshot just before spawning the worker; outDeliverables
// compares against it afterwards to detect modifications of existing files
// (regression risk) versus purely new deliverables.
func snapshotWorkdir(workdir string) map[string]time.Time {
	snap := map[string]time.Time{}
	_ = filepath.Walk(workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".whale" || info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, relErr := filepath.Rel(workdir, path); relErr == nil {
			snap[rel] = info.ModTime()
		}
		return nil
	})
	return snap
}

// outDeliverables counts files in the workdir and reports whether the worker
// modified an existing file (regression risk) — compares each file's modtime
// against the pre-worker baseline snapshot. .whale/.git/node_modules are
// whale's own state / VCS internals / dependencies, never deliverables.
func outDeliverables(workdir string, baseline map[string]time.Time) (count int, touches bool, err error) {
	walkErr := filepath.Walk(workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".whale" || info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		rel, relErr := filepath.Rel(workdir, path)
		if relErr != nil {
			return relErr
		}
		if mod, ok := baseline[rel]; ok && !mod.Equal(info.ModTime()) {
			touches = true
		}
		return nil
	})
	return count, touches, walkErr
}

// changedDeliverables lists the files the worker added or modified in workdir —
// entries whose modtime differs from the pre-worker baseline snapshot (new
// files have no baseline entry). Walk visits entries in lexical order, so the
// list is deterministic. .whale/.git/node_modules are never deliverables.
func changedDeliverables(workdir string, baseline map[string]time.Time) ([]string, error) {
	var changed []string
	err := filepath.Walk(workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".whale" || info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(workdir, path)
		if relErr != nil {
			return relErr
		}
		if mod, ok := baseline[rel]; !ok || !mod.Equal(info.ModTime()) {
			changed = append(changed, rel)
		}
		return nil
	})
	return changed, err
}

// verifyDepth decides the verification depth for a task after the mechanical
// gate passes. The verification link is always present（环节恒有）; only the
// depth varies: "mechanical" = objective gate + worker self-check (no LLM
// verifier), "semantic" = independent system verifier cross-check.
//
// The Leader decides the depth at decompose time (task.VerifyMode); an unset
// value (auto) falls back to a conservative heuristic — any signal of
// subjectivity or regression risk promotes to semantic. The mechanical gate
// (automated tests/build) always runs regardless of depth.
func (e *TeamEngine) verifyDepth(task *Task, verifyWorkdir string, baseline map[string]time.Time) string {
	switch task.VerifyMode {
	case "mechanical", "semantic":
		return task.VerifyMode
	}
	// Auto heuristics.
	if task.UseDW {
		return "semantic"
	}
	// Worktree mode edits the repo in place via a branch; regression assessment
	// needs a git diff — be conservative and always verify.
	if e.activeBranch(task.ID) != "" {
		return "semantic"
	}
	// Subjective/key roles (architecture, security, docs) always verify.
	if task.VerifierRole != "" {
		return "semantic"
	}
	// Declared deliverables must actually exist. A worker that failed to produce
	// its promised output is the worst failure mode — but the workdir-wide count
	// (count>0) and the no-regression flag (touches=false) would let it slip
	// through as a low-risk mechanical SKIP. A missing declared file forces
	// semantic depth so the system verifier cross-checks instead, not at the
	// cost of a false PASS via the degenerate SKIP path.
	if !declaredOutputsPresent(task, verifyWorkdir) {
		return "semantic"
	}
	// High regression risk: the deliverable touches an existing workspace file.
	// An empty workdir (worker produced nothing) also verifies — a missing
	// deliverable is not a low-risk standalone file. Without a baseline (resume
	// with existing output) new vs modified files cannot be told apart, so stay
	// conservative.
	count, touches, err := outDeliverables(verifyWorkdir, baseline)
	if err != nil || touches || count == 0 || baseline == nil {
		return "semantic"
	}
	// No automated test command means the mechanical gate ran nothing — it
	// trivially "passes" and would release the deliverable with zero objective
	// check. Never go mechanical-only without an objective gate.
	if !mechanicalGateAvailable(verifyWorkdir) {
		return "semantic"
	}
	return "mechanical"
}

// declaredOutputsPresent 校验 task.Output 声明的每个交付文件是否真实存在于
// workdir. 批量任务把多个交付物写在同一 workdir,声明文件缺失(worker 没产出承诺
// 的东西)是最重的失败——即使 workdir 非空、也没触碰旧文件(touches=false),也不该
// 被 mechanical 深度(SKIP)放行,必须提升 semantic 由系统验证器交叉审查。
// task.Output 以逗号分隔多个条目;为空声明时视为无需校验(交由 outDeliverables
// 的 count 判断)。父任务聚合产物等非 worker 直接写的路径不做硬判,缺失也仅升
// semantic 不致误 fail。
func declaredOutputsPresent(task *Task, workdir string) bool {
	out := strings.TrimSpace(task.Output)
	if out == "" {
		return true
	}
	for _, o := range strings.Split(out, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		p := filepath.Clean(o)
		if !filepath.IsAbs(p) {
			p = filepath.Join(workdir, p)
		}
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// verifyModeForTask resolves the effective verification depth handed down from
// a PlanTask. verify_mode wins; a legacy verifier_role (old plans predating
// verify_mode) maps to "semantic" because a Leader-assigned verifier denoted a
// subjective/key task. Empty result means auto (engine heuristic).
func verifyModeForTask(pt PlanTask) string {
	switch pt.VerifyMode {
	case "mechanical", "semantic":
		return pt.VerifyMode
	}
	if strings.TrimSpace(pt.VerifierRole) != "" {
		return "semantic"
	}
	return ""
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
		child.VerifyMode = verifyModeForTask(pt)
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
	// output.md + verify.md (verify.md is the PASS-only marker; verifier.md is
	// the verifier's report and is written on FAIL too, so it must NOT drive
	// done). Persist both or the batch done-check re-reads the parent as
	// "produced" and fails the batch.
	_ = e.Whiteboard.WriteVerifier(task.ID, "split into children: work carried forward")
	_ = os.WriteFile(filepath.Join(e.Whiteboard.TaskDir(task.ID), "verify.md"), []byte("split into children: work carried forward"), 0644)
	_ = e.Store.TransitionState(task.ID, TaskStateDone, "", reason)
	e.fireEvent(TaskEvent{Type: EventStateChanged})
	return true
}
