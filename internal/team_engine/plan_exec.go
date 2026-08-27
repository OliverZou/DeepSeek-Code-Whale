package team_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// plan_exec.go — 「TE 负责任务执行，Leader 异步等待」的原语。
//
// Leader 只做决策：分解（plan）→ 提交执行（team_run_plan）→ 轮询状态 →
// 评审 → 失败重派。执行本身（批次按依赖图并行/串行、每任务
// produce→verify→机械门→传播）全部由 TeamEngine 在后台完成。
// StartPlanRun 立即返回执行票据，不阻塞在结果上。

// PlanRunState 是一次计划执行的生命周期。它在进程内共享（sync.Map），
// 使团队工具调用（每次重建 engine 实例）之间仍能查到同一执行的状态。
type PlanRunState struct {
	mu      sync.Mutex
	running bool
	done    bool
	err     error
	summary string // 完成摘要（通过/失败计数）
	batches []*Batch
	// lastProgress 是最后一次批次级推进（任一批次成功/失败）的时间，用于
	// 活跃性判定：只要计划在执行，批次会不断推进，等待就不设总时限；停滞超过
	// planRunStallTimeout（任务层超时也兜不住的引擎侧卡死）才判定挂死。
	lastProgress time.Time
}

// PlanRunView is the lock-free snapshot of a plan execution.
type PlanRunView struct {
	Running      bool
	Done         bool
	Err          error
	Summary      string
	Batches      []*Batch
	LastProgress time.Time
}

func (s *PlanRunState) view() PlanRunView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := PlanRunView{Running: s.running, Done: s.done, Err: s.err, Summary: s.summary, LastProgress: s.lastProgress}
	if s.done {
		v.Batches = s.batches // 仅完成时携带批次结果，便于展示
	}
	return v
}

func (s *PlanRunState) finish(execDone bool, err error, summary string, batches []*Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = execDone
	s.running = !execDone
	s.err = err
	s.summary = summary
	s.batches = batches
}

// recordProgress stamps the last batch-level progress: the started state
// (planRuns.Store) covers the pre-first-batch window; runBatchesToCompletion
// calls it after each batch reaches a terminal state.
func (s *PlanRunState) recordProgress() {
	s.mu.Lock()
	s.lastProgress = time.Now()
	s.mu.Unlock()
}

var planRuns sync.Map // masterTaskID → *PlanRunState

// PlanRunStatus returns the in-process execution status for a master task.
// bool is false when no execution is known (never started or started in a
// different process — fall back to team_list/team_status over the store).
func PlanRunStatus(masterTaskID string) (PlanRunView, bool) {
	v, ok := planRuns.Load(masterTaskID)
	if !ok {
		return PlanRunView{}, false
	}
	return v.(*PlanRunState).view(), true
}

// StartPlanRun restores the master's plan (plan.json + tasks on disk) and
// hands it to the TeamEngine's topological scheduler in the background. It
// returns immediately with an execution ticket; the caller polls PlanRunStatus
// or team_list/team_status to observe progress.
//
// The engine instance is owned by the execution from here on: it is closed
// when the run finishes (the tool wrapper must therefore NOT defer Close on
// the success path).
func (e *TeamEngine) StartPlanRun(ctx context.Context, masterTaskID string) (string, error) {
	if v, ok := planRuns.Load(masterTaskID); ok {
		st := v.(*PlanRunState)
		st.mu.Lock()
		running := st.running
		st.mu.Unlock()
		if running {
			return "", fmt.Errorf("计划已在运行（master=%s）——无需重复提交，等待进度通知即可", masterTaskID[:8])
		}
	}

	master, err := e.GetMasterTask(masterTaskID)
	if err != nil || master == nil {
		return "", fmt.Errorf("master task %s not found", masterTaskID)
	}
	workdir := master.WorkspacePath
	if strings.TrimSpace(workdir) == "" {
		return "", fmt.Errorf("master %s has no workspace_path; refusing to execute into cwd", masterTaskID[:8])
	}

	// team_run_plan 工具每次调用重建 engine 实例,新建时不挂 team 配置;但
	// 执行(worker/verifier spawn)需要 team 做 role→AgentName 解析,否则成员
	// 走 inline fallback,丢 .md 定义(persona/工具/模型)。master 记录里
	// CreateMasterTask 已落 Agent("team:label"),这里从它恢复。
	if e.team == nil {
		if label := strings.TrimPrefix(master.Agent, "team:"); label != master.Agent && label != "" {
			if tc, err := FindTeamInRoots(DefaultTeamRoots(master.WorkspacePath), label); err == nil {
				e.SetTeam(tc)
			} else {
				Log("plan-run", "master %s team %q not found: %v", masterTaskID[:8], label, err)
			}
		}
	}

	if err := e.Store.UpdateMasterTaskStatus(masterTaskID, "running"); err != nil {
		return "", fmt.Errorf("mark master running: %w", err)
	}

	state := &PlanRunState{running: true, lastProgress: time.Now()}
	planRuns.Store(masterTaskID, state)
	runStarted := time.Now()
	execCtx, cancel := context.WithCancel(context.Background())
	// 注册 master cancel（用户喊停 → StopRun 生效；RunLeaderDriven 早已注册）。
	e.mu.Lock()
	e.masterTaskCancels[masterTaskID] = cancel
	e.mu.Unlock()

	// v46 异步化：计划准备（缺 plan.json 时先分解，约 1-3 分钟）与执行全部后台
	// 化——team_run_plan 立即返回票据，leader turn 不被同步阻塞（用户不再看到
	// 一条长 Running）；分解完成/失败通过事件通知（进展桥注入 leader 叙述）。
	go func() {
		defer e.Close() // the run owns this engine instance
		defer func() {
			e.mu.Lock()
			delete(e.masterTaskCancels, masterTaskID)
			e.mu.Unlock()
		}()
		batches, err := e.recoverMasterBatches(masterTaskID, master.Goal, workdir)
		if err != nil || len(batches) == 0 {
			// 无 plan.json（inline leader 首次提交）：先异步分解再执行。
			if _, derr := e.EnsureMasterPlan(context.Background(), masterTaskID); derr != nil {
				state.finish(false, derr, "decompose failed", nil)
				_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
				e.fireEvent(TaskEvent{Type: EventLeaderLog, MasterID: masterTaskID, Title: "计划分解失败：" + derr.Error()})
				cancel()
				return
			}
			if e.Loggers != nil {
				e.Loggers.Engine("master %s plan decomposed in background", masterTaskID[:8])
			}
			batches, err = e.recoverMasterBatches(masterTaskID, master.Goal, workdir)
			if err != nil || len(batches) == 0 {
				state.finish(false, err, "plan empty after decompose", nil)
				_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
				cancel()
				return
			}
		}
		if cerr := checkBatchOutputConflicts(batches); cerr != nil {
			state.finish(false, cerr, "output conflicts", nil)
			_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
			cancel()
			return
		}
		// 计划就绪：通知 leader（进展桥注入叙述：计划已就绪，团队开工）。
		// Data 带上真实分工（batch/任务/成员角色/产出文件）——leader 转述时凭事实说话，
		// 不瞎编（v48：无内容时 leader 只能编造分工与执行进度）。
		planDigest := formatPlanDigestForLeader(batches)
		e.fireEvent(TaskEvent{Type: EventLeaderLog, MasterID: masterTaskID, Title: "计划已就绪", Data: planDigest})
		decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
		leaderModel := e.Router.ResolveModel("planner")
		maxCycles := e.Config.Batch.DefaultMaxCycles
		if maxCycles <= 0 {
			maxCycles = 3
		}
		completedBatches := map[string]bool{}
		passedBatches := map[string]bool{}
		completedOutputs := map[string]string{}
		leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team)
		err = e.runBatchesToCompletion(
			execCtx, batches, masterTaskID, workdir,
			decomposerTimeout, leaderModel,
			completedBatches, passedBatches, completedOutputs,
			func() { state.recordProgress() },
		)
		// 失败批不直接终局：给 Leader 一轮 review 决策（reject → feedback 重跑，
		// 至多 maxCycles 轮）；escalate/超轮 → 保持失败交付（v45: 批次 fail 直接结算导致
		// leader 的 team_run 重派与结算竞争、重派无效）。
		for cycle := 1; err == nil && !allBatchesPassed(batches) && cycle < maxCycles; cycle++ {
			report := e.buildPlanCycleReport(batches, cycle)
			review, rerr := leader.ReviewPlanCycle(master.Goal, report, workdir, decomposerTimeout, leaderModel)
			if rerr != nil {
				Log("plan-run", "master %s cycle review failed: %v (keep current state)", masterTaskID[:8], rerr)
				break
			}
			if review.Decision != CycleReject && review.Decision != CycleEscalated {
				break
			}
			Log("plan-run", "master %s cycle %d rejected: resetting failed batches", masterTaskID[:8], cycle)
			resetAny := false
			for _, b := range batches {
				if b.Status == BatchStatusPassed {
					continue
				}
				// 验证任务不参与自动重置:FAIL 的缺陷属于上游实现,重跑验证
				// 任务只会重复发现同一缺陷(v15/v16/v17:集成验证 FAIL 后
				// cycle 重置重跑,白烧 ~300K raw token)。保持 suspended 与
				// 缺陷报告,交 Leader 决策(其 review 轮可驱动上游修复)。
				hasNonVerification := false
				for _, t := range b.Tasks {
					if !isVerificationTask(t) {
						hasNonVerification = true
						break
					}
				}
				if !hasNonVerification {
					Log("plan-run", "master %s verification batch %s kept failed (defects belong upstream — Leader decides)", masterTaskID[:8], b.ID)
					continue
				}
				b.Status = BatchStatusPending
				resetAny = true
				for _, t := range b.Tasks {
					if isVerificationTask(t) {
						continue // 保留 suspended 与缺陷报告
					}
					e.SendFeedback(t.ID, review.Feedback)
					resetAny = true
					if t.State == TaskStateFailed || t.State == TaskStateSuspended {
						_ = e.Store.TransitionState(t.ID, TaskStatePending, "cycle reject retry", "")
					} else if !t.State.IsTerminal() {
						_ = e.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
					}
				}
			}
			// 无可重置任务(所有未通过 batch 都是验证缺陷):重跑不会改变任何
			// 状态,继续 review 循环只会空转 LLM 决策轮(v19:kept failed 后
			// 每轮 review 一次调用直到 maxCycles)。直接结束,缺陷报告保留,
			// Leader 的最终 review 轮可驱动上游修复。
			if !resetAny {
				Log("plan-run", "master %s cycle %d: no resettable tasks (verification defects belong upstream) — stopping review loop", masterTaskID[:8], cycle)
				break
			}
			if err = e.runBatchesToCompletion(
				execCtx, batches, masterTaskID, workdir,
				decomposerTimeout, leaderModel,
				completedBatches, passedBatches, completedOutputs,
				func() { state.recordProgress() },
			); err != nil {
				break
			}
		}
		execDone := err == nil && allBatchesPassed(batches)
		summary := summarizeBatches(batches)
		if !execDone {
			summary = fmt.Sprintf("execution error: %v (partial %s)", err, summary)
			_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "failed")
		} else {
			_ = e.Store.UpdateMasterTaskStatus(masterTaskID, "done")
		}
		state.finish(execDone, err, summary, batches)
		// 运行统计落盘：结束后在 master 目录写 run_report.json（每任务耗时/token/
		// 重试/终态），供事后优化分析。批次失败（调度完成）err==nil；执行错误/取消
		// err!=nil——状态优先按 err 判。
		status := "done"
		failedBatches := 0
		for _, b := range batches {
			if b.Status == BatchStatusFailed {
				failedBatches++
			}
		}
		if failedBatches > 0 {
			status = "failed"
		}
		if err != nil {
			status = "error"
		}
		e.writeRunReport(masterTaskID, master.Goal, status, summary, err, runStarted, batches, 0, 0, 0)
		// 运行完结事件：leader 收尾叙述（进展桥据此停止后续注入）。
		e.fireEvent(TaskEvent{Type: EventLeaderLog, MasterID: masterTaskID, Title: "运行已完结（" + status + "）"})
		cancel()
		e.mu.Lock()
		delete(e.masterTaskCancels, masterTaskID)
		e.mu.Unlock()
	}()

	return fmt.Sprintf("🧭 plan & execution started in background (master=%s): decompose if needed, then run — I will report progress", masterTaskID[:8]), nil
}

func summarizeBatches(batches []*Batch) string {
	passed, failed := 0, 0
	for _, b := range batches {
		switch b.Status {
		case BatchStatusPassed:
			passed++
		case BatchStatusFailed:
			failed++
		}
	}
	return fmt.Sprintf("%d/%d batches passed, %d failed", passed, len(batches), failed)
}

// allBatchesPassed reports whether every batch reached the passed status.
func allBatchesPassed(batches []*Batch) bool {
	for _, b := range batches {
		if b.Status != BatchStatusPassed {
			return false
		}
	}
	return true
}

// recoverMasterBatches rebuilds the master's batches from the disk state:
// plan.json (writePlanJSON, <whiteboard>/<masterID>/plan.json — the same file
// the daemon DAG snapshot reads) is the authoritative task plan; the actual
// Task records (with their real IDs, states, retries) were created by
// createBatchesFromPlan and survive in the file store. Tasks are matched to
// plan entries by (batch_id, title) — the same titles the planner produced.
// Note: Store.GetMasterTaskProgress is NOT the plan — it reads the checkpoint
// (masters/<id>/plan.json, completed/passed batches) written by saveCheckpoint.
func (e *TeamEngine) recoverMasterBatches(masterTaskID, goal, workdir string) ([]*Batch, error) {
	data, err := os.ReadFile(filepath.Join(e.Whiteboard.MasterDir(masterTaskID), "plan.json"))
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return nil, fmt.Errorf("no plan.json for master %s — the master has no decomposition on disk", masterTaskID[:8])
	}
	var plan struct {
		Tasks []struct {
			Title              string   `json:"title"`
			Description        string   `json:"description"`
			Role               string   `json:"role"`
			Output             string   `json:"output"`
			BatchID            string   `json:"batch_id"`
			BatchLabel         string   `json:"batch_label,omitempty"`
			DependsOn          []string `json:"depends_on,omitempty"`
			AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(data), &plan); err != nil {
		return nil, fmt.Errorf("parse plan.json: %w", err)
	}

	tasks, err := e.ListTasksByMasterTask(masterTaskID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	byTitle := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		byTitle[t.Title] = t
	}

	byBatch := map[string]*Batch{}
	var order []string
	for _, pt := range plan.Tasks {
		bid := pt.BatchID
		if bid == "" {
			bid = "default"
		}
		batch, ok := byBatch[bid]
		if !ok {
			batch = &Batch{ID: bid, Label: pt.BatchLabel, DependsOn: pt.DependsOn}
			byBatch[bid] = batch
			order = append(order, bid)
		}
		task, ok := byTitle[pt.Title]
		if !ok {
			return nil, fmt.Errorf("plan task %q (batch %s) not found on disk — plan.json and task records are out of sync; a fresh decompose is required", pt.Title, bid)
		}
		batch.Tasks = append(batch.Tasks, task)
	}

	var batches []*Batch
	for _, bid := range order {
		batches = append(batches, byBatch[bid])
	}
	return batches, nil
}

// planRunStallTimeout is the stall threshold for the settle wait: a plan run
// keeps stamping progress as each batch reaches a terminal state (success or
// failure), so as long as progress moves the wait has no total deadline —
// legitimate long runs (several slow serial batches) never get cut. Only a
// stall beyond this threshold (an engine-side hang neither the per-task
// timeouts nor runBatch can recover from) is declared hung and handed back to
// the leader as unsettled. Configured slow roles default to 1800s per task, so
// 60m covers up to two worst-case serial batches plus verifier slack.
const planRunStallTimeout = 60 * time.Minute

// waitPlanRunSettled blocks until the plan execution started by team_run_plan
// (StartPlanRun) settles — done or failed — or until a stall indicates a hang.
// The team_run_plan tool is synchronous, so by the end of the leader's submit
// turn StartPlanRun has either registered the run in planRuns or failed
// outright: no state means the leader never submitted (or submission failed),
// so report that immediately instead of burning the whole wait. The wait is
// engine-side on purpose — an LLM polling loop would burn the session's
// tool-iteration budget (repetitive team_list calls get storm_blocked, then
// the cap auto-interrupts the turn, and the CLI process exits killing the run).
// There is no total deadline: each batch reaching a terminal state stamps
// progress, so a still-advancing run is never cut; the stall path only fires
// when per-task timeouts and runBatch both failed to turn a hang into a
// terminal state.
func (e *TeamEngine) waitPlanRunSettled(masterTaskID string) (PlanRunView, bool, error) {
	start := time.Now()
	lastBeat := start
	for {
		v, ok := PlanRunStatus(masterTaskID)
		if !ok {
			return PlanRunView{}, false, fmt.Errorf("plan execution for master %s was never started — the leader did not submit via team_run_plan", masterTaskID[:8])
		}
		if v.Done {
			return v, true, nil
		}
		if !v.LastProgress.IsZero() && time.Since(v.LastProgress) > planRunStallTimeout {
			return v, false, fmt.Errorf("plan execution for master %s made no batch progress for %s", masterTaskID[:8], planRunStallTimeout)
		}
		if defaultTeamLog != nil && time.Since(lastBeat) >= 30*time.Second {
			lastBeat = time.Now()
			Log("leader", "waiting for plan run to settle (elapsed %s)", time.Since(start).Round(time.Second))
		}
		time.Sleep(2 * time.Second)
	}
}

// reviewResultForMaster turns the wait outcome into the replay text for the
// leader's review turn.
func reviewResultForMaster(view PlanRunView, settled bool, waitErr error) string {
	if waitErr != nil {
		return fmt.Sprintf("⚠️ 计划执行未正常结算: %v", waitErr)
	}
	if view.Err != nil {
		return fmt.Sprintf("⚠️ 执行出错: %v (部分结算: %s)", view.Err, view.Summary)
	}
	return view.Summary
}

// formatPlanDigestForLeader renders the ready plan as a short human-readable
// assignment digest for the inline-leader event (`“计划已就绪” Data): who does what
// (batches -> tasks -> role -> output files). The leader narration must quote facts,
// not invent them.
func formatPlanDigestForLeader(batches []*Batch) string {
	var b strings.Builder
	for _, bch := range batches {
		b.WriteString("批次 " + bch.LabelOrID() + "：")
		for i, t := range bch.Tasks {
			if i > 0 {
				b.WriteString("；")
			}
			b.WriteString(t.Title + "（" + string(t.Role) + " → " + t.Output + "）")
		}
	}
	return b.String()
}
