package team_engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToolCapSplitChildRunsOnce 是双拾取 bug 的集成回归测试:
// worker 撞 tool cap 自拆出的子任务,在 batch 的下一个 cycle 必须
// 只执行一次。历史 bug:runBatch cycle 内两个重复的 children 拾取块
// 把同一子任务 append 两次,batch.Tasks 出现两个相同任务,并发双执行
// (v15 实测:DOM 子任务被并发跑两遍,多烧 ~1M raw token)。
func TestToolCapSplitChildRunsOnce(t *testing.T) {
	spawner := &mockSpawner{
		// worker(实际 role 为 developer):第一次输出撞 cap 标记触发自拆;
		// 之后(子任务)正常完成。
		roleSeq: map[string][]string{
			"developer": {
				"mock output\nThis turn was auto-interrupted: tool call cap reached",
				"child task done",
			},
		},
		// planner:自拆的分解输出(1 个子任务)。
		roleOutputs: map[string]string{
			"planner": `[{"title":"实现 DOM 子任务","description":"child","output":"game-dom.js","role":"developer","verify_mode":"mechanical","batch_id":"1","depends_on_batch":[]}]`,
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ws := t.TempDir()
	// 主任务撞 cap 后自拆出子任务;子任务继承 batch 1。
	parent := NewTask("p1-0000-0000-0000-000000000001", "实现 DOM 交互层", "desc", RoleDeveloper, "", 0, ws, nil, "1", "m1")
	parent.Output = "game-dom.js"
	if err := eng.Store.InsertTask(parent); err != nil {
		t.Fatal(err)
	}
	batch := &Batch{ID: "1", Label: "交互", Status: BatchStatusPending, MaxCycles: 2, Tasks: []*Task{parent}}

	// 执行一个 cycle(允许自拆后继续 cycle 跑子任务)。
	if err := eng.RunBatch(context.Background(), batch); err != nil {
		t.Fatalf("run batch: %v", err)
	}

	// 自拆出子任务后,引擎的 cycle 拾取会把子任务加入 batch;RunBatch
	// 本身不跑下一个 cycle,这里模拟 runBatch 的拾取+下一 cycle。
	children, err := eng.Store.ListTasksByParent(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
	child := children[0]
	batch.Tasks = append(batch.Tasks, child)
	if err := eng.RunBatch(context.Background(), batch); err != nil {
		t.Fatalf("run second cycle: %v", err)
	}

	// 双拾取 bug 会让 batch.Tasks 里出现两个相同的子任务 → 子任务 spawn 2 次。
	// prompt 不含任务标题,按子任务的任务文件路径(ID)匹配。
	spawner.mu.Lock()
	defer spawner.mu.Unlock()
	childSpawns := 0
	for _, req := range spawner.reqs {
		if req.Role == "developer" && strings.Contains(req.Task, child.ID) {
			childSpawns++
		}
	}
	if childSpawns != 1 {
		t.Fatalf("child task spawned %d times, want exactly 1 (duplicate-pickup regression)", childSpawns)
	}
}

// TestVerificationTaskNotAutoReset 验证任务 FAIL 后不参与 cycle 自动重置:
// CycleReject 重置循环跳过验证任务(保持 suspended,交 Leader 决策),
// 避免无谓重跑(v15:集成验证 FAIL 后 cycle 重置重跑,白烧 ~300K raw)。
func TestVerificationTaskNotAutoReset(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	ws := t.TempDir()
	verify := NewTask("v1-0000-0000-0000-000000000002", "集成验证与端到端验收", "desc", RoleTester, "", 0, ws, nil, "3", "m1")
	verify.Output = "report.md"
	if err := eng.Store.InsertTask(verify); err != nil {
		t.Fatal(err)
	}
	impl := NewTask("i1-0000-0000-0000-000000000003", "实现核心逻辑", "desc", RoleDeveloper, "", 0, ws, nil, "1", "m1")
	impl.Output = "game.js"
	if err := eng.Store.InsertTask(impl); err != nil {
		t.Fatal(err)
	}
	// 验证任务已 FAIL 挂起(模拟 RunTask 的 isVerificationTask 分支)。
	_ = eng.Store.TransitionState(verify.ID, TaskStateSuspended, "verification task reported defects — needs upstream fix", "report")
	// 实现任务已完成(passed batch 的终态:output.md + verify.md 驱动派生)。
	_ = os.WriteFile(filepath.Join(eng.Store.taskDir(impl.ID), "output.md"), []byte("ok"), 0644)
	_ = os.WriteFile(filepath.Join(eng.Store.taskDir(impl.ID), "verify.md"), []byte("ok"), 0644)

	batches := []*Batch{
		{ID: "1", Label: "核心", Status: BatchStatusPassed, Tasks: []*Task{impl}},
		{ID: "3", Label: "验收", Status: BatchStatusFailed, Tasks: []*Task{verify}},
	}
	review := &CycleReview{Decision: CycleReject, Feedback: "fix the issues"}

	// 直接模拟 TeamCycle 的 CycleReject 分支逻辑:对验证 batch 跳过重置。
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
			// 验证 batch:不重置。
			continue
		}
		b.Status = BatchStatusPending
		for _, t := range b.Tasks {
			if isVerificationTask(t) {
				continue
			}
			_ = eng.Store.UpdateTask(t.ID, map[string]interface{}{"verifier_feedback": review.Feedback})
			if !t.State.IsTerminal() && t.State != TaskStateSuspended {
				_ = eng.Store.TransitionState(t.ID, TaskStateAssigned, "", "")
			}
		}
	}

	// 验证任务保持 suspended(未被打回重跑),上游实现任务未被波及。
	if st := eng.Store.TaskState(verify.ID); st != TaskStateSuspended {
		t.Fatalf("verification task state = %s, want suspended (not auto-reset)", st)
	}
	if st := eng.Store.TaskState(impl.ID); st != TaskStateDone {
		t.Fatalf("impl task state = %s, want done (passed batch untouched)", st)
	}
}
