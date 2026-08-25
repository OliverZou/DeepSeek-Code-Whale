package team_engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTopologicalScheduling_FailedBatchDoesNotAbortSiblings verifies the
// P1-graph failure semantics: a batch with an unsatisfiable (dangling)
// dependency is marked Failed, but independent sibling batches still run to
// completion instead of triggering the old global abort.
func TestTopologicalScheduling_FailedBatchDoesNotAbortSiblings(t *testing.T) {
	workerOutput := "package main\n\nfunc main() {}\n"

	eng := newMockEngine(t, map[string]string{
		"software-engineer":  workerOutput,
		"software-architect": workerOutput,
		"verifier":           "TOOLS USED: read_file\nVERDICT: PASS\nEVIDENCE: ok\n## FINDINGS\n---json\n[]\n---",
	})
	defer eng.Close()

	workdir := t.TempDir()
	// 不写 go.mod：mock spawner 只把 worker 产出写到 output.md、不落盘到 workdir，
	// 写 go.mod 会让机械验证 `go test ./...` 在无 .go 文件时报
	// "no packages to test" 而误判任务失败。本测试只关注拓扑调度失败语义，
	// 不验证 Go 编译，故保持 workdir 无构建配置以跳过机械门。

	mt, err := eng.CreateMasterTask("three batches, one dangling dep", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "A", Description: "independent A", Output: "a.go", Role: "software-engineer", BatchID: "batch-a", BatchLabel: "A", MaxCycles: 1},
		{Title: "B", Description: "independent B", Output: "b.go", Role: "software-architect", BatchID: "batch-b", BatchLabel: "B", MaxCycles: 1},
		{Title: "C", Description: "depends on missing batch", Output: "c.go", Role: "software-engineer", BatchID: "batch-c", BatchLabel: "C", DependsOnBatch: FlexibleStringSlice{"ghost"}, MaxCycles: 1},
	}

	batches, err := eng.TeamCycle(context.Background(), "three batches", workdir, mt.ID, plan...)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}

	status := map[string]BatchStatus{}
	for _, b := range batches {
		status[b.ID] = b.Status
	}

	if status["batch-c"] != BatchStatusFailed {
		t.Errorf("batch-c: expected %s, got %s", BatchStatusFailed, status["batch-c"])
	}
	if status["batch-a"] != BatchStatusPassed {
		t.Errorf("batch-a: expected %s (sibling continues), got %s", BatchStatusPassed, status["batch-a"])
	}
	if status["batch-b"] != BatchStatusPassed {
		t.Errorf("batch-b: expected %s (sibling continues), got %s", BatchStatusPassed, status["batch-b"])
	}
}

// ---------------------------------------------------------------------------
// 拓扑调度行为契约（防回退）。用户拍板：TE 负责任务执行（并行/串行调度），
// Leader 异步等待。这组测试锁定调度器的行为：无依赖批次并发 overlap、
// 依赖批次顺序执行、批次内实现-测试两阶段、StartPlanRun 恢复执行。
// ---------------------------------------------------------------------------

// gateSpawner is a recording SubagentSpawner that can gate every spawn on a
// release channel. Entries are recorded BEFORE blocking, so tests observe
// which agents entered while the run is still in flight — the exact primitive
// needed to prove parallelism (not just ordering).
type gateSpawner struct {
	mu      sync.Mutex
	entries []SubagentRequest
	outputs map[string]string
	release chan struct{} // closed to unblock; spawns wait on it when block=true
	block   bool
}

func newGateSpawner(outputs map[string]string) *gateSpawner {
	return &gateSpawner{outputs: outputs}
}

func (g *gateSpawner) SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
	g.mu.Lock()
	g.entries = append(g.entries, req)
	block, release := g.block, g.release
	g.mu.Unlock()
	if block && release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return SubagentResponse{}, ctx.Err()
		}
	}
	out, ok := g.outputs[req.Role]
	if !ok {
		out = "mock output"
	}
	return SubagentResponse{Output: out, SessionID: "sess-" + req.Role, Success: true, ExitCode: 0}, nil
}

func (g *gateSpawner) snapshot() []SubagentRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]SubagentRequest(nil), g.entries...)
}

const gateWorkerOutput = "package main\n\nfunc main() {}\n"

const gatePassVerdict = "TOOLS USED: read_file\nVERDICT: PASS\nEVIDENCE: ok\n## FINDINGS\n---json\n[]\n---"

func gateOutputs() map[string]string {
	return map[string]string{
		"software-engineer":  gateWorkerOutput,
		"software-architect": gateWorkerOutput,
		"verifier":           gatePassVerdict,
	}
}

// waitEntries fails after 10s if fewer than n spawns have been recorded.
func waitEntries(t *testing.T, g *gateSpawner, n int) []SubagentRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := g.snapshot()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d spawns, got %d", n, len(got))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startTopoRun creates batches from the plan and runs one scheduler pass in the
// background, returning the batches and a channel that resolves when the pass
// finishes (nil = success).
func startTopoRun(eng *TeamEngine, plan []PlanTask, goal, workdir, masterID string) ([]*Batch, chan error) {
	batches, err := eng.createBatchesFromPlan(plan, goal, workdir, masterID, "")
	if err != nil {
		panic(err) // test setup failure — caller passes a pre-validated plan
	}
	done := make(chan error, 1)
	go func() {
		done <- eng.runBatchesToCompletion(context.Background(), batches, masterID, workdir, 0, "", map[string]bool{}, map[string]bool{}, map[string]string{}, nil)
	}()
	return batches, done
}

// TestTopologicalScheduling_ParallelBatches proves independent batches run
// concurrently: with the gate up, ALL THREE workers must enter before any
// release. A serial scheduler would deadlock on the first worker's entry and
// the wait times out — the test fails without ever releasing.
func TestTopologicalScheduling_ParallelBatches(t *testing.T) {
	g := newGateSpawner(gateOutputs())
	g.block, g.release = true, make(chan struct{})

	eng, err := New("", t.TempDir(), "", g)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("parallel batches", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "A", Description: "independent A", Output: "a.go", Role: "software-engineer", BatchID: "b1", BatchLabel: "A", MaxCycles: 1},
		{Title: "B", Description: "independent B", Output: "b.go", Role: "software-architect", BatchID: "b2", BatchLabel: "B", MaxCycles: 1},
		{Title: "C", Description: "independent C", Output: "c.go", Role: "software-engineer", BatchID: "b3", BatchLabel: "C", MaxCycles: 1},
	}
	batches, done := startTopoRun(eng, plan, "parallel", workdir, mt.ID)

	entries := waitEntries(t, g, 3)
	// Gate 未释放时三个 worker 全部进入 → 批次级并行。结构上此时也恰好只有
	// 三个 spawn（verifier 在 worker 返回后才 spawn，worker 全被 gate 挡住）。
	if len(entries) != 3 {
		t.Fatalf("expected exactly 3 worker spawns while gated, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Role == "verifier" {
			t.Errorf("verifier spawned while its worker is still gated: %+v", e.Role)
		}
	}

	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, b := range batches {
		if b.Status != BatchStatusPassed {
			t.Errorf("batch %s: expected %s, got %s", b.ID, BatchStatusPassed, b.Status)
		}
	}
}

// TestTopologicalScheduling_RespectsDependencyOrder proves a dependent batch
// never starts before its upstream completes: B depends on A, A's worker is
// gated, and B's worker must not enter before A's batch is released.
func TestTopologicalScheduling_RespectsDependencyOrder(t *testing.T) {
	g := newGateSpawner(gateOutputs())
	g.block, g.release = true, make(chan struct{})

	eng, err := New("", t.TempDir(), "", g)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("ordered batches", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "A", Description: "upstream A", Output: "a.go", Role: "software-engineer", BatchID: "b1", BatchLabel: "A", MaxCycles: 1},
		{Title: "B", Description: "downstream B", Output: "b.go", Role: "software-architect", BatchID: "b2", BatchLabel: "B", DependsOnBatch: FlexibleStringSlice{"b1"}, MaxCycles: 1},
	}
	batches, done := startTopoRun(eng, plan, "ordered", workdir, mt.ID)

	waitEntries(t, g, 1) // A's worker entered
	// A 的 worker 被 gate 挡住 → A 批次无法完成 → B 队列在 A 通过前不可见。
	// 等待一个 settle 窗口确认 B 没有越权入场（一旦回退为「并行忽略依赖」即捕获）。
	time.Sleep(300 * time.Millisecond)
	if got := g.snapshot(); len(got) != 1 {
		t.Fatalf("dependent batch started before upstream completed: %d spawns %+v", len(got), got)
	}

	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, b := range batches {
		if b.Status != BatchStatusPassed {
			t.Errorf("batch %s: expected %s, got %s", b.ID, BatchStatusPassed, b.Status)
		}
	}
}

// TestRunBatch_ImplementsBeforeTests proves the within-batch two-phase
// ordering: the test task (output test_*.js) must not spawn while the impl
// task's worker is still gated — tests depend on the impl output.
func TestRunBatch_ImplementsBeforeTests(t *testing.T) {
	g := newGateSpawner(gateOutputs())
	g.block, g.release = true, make(chan struct{})

	eng, err := New("", t.TempDir(), "", g)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("two-phase batch", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "Impl", Description: "write the game", Output: "game.js", Role: "software-engineer", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
		{Title: "Tests", Description: "test the game", Output: "test_game.js", Role: "software-architect", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
	}
	batches, done := startTopoRun(eng, plan, "two-phase", workdir, mt.ID)

	waitEntries(t, g, 1) // impl worker entered
	time.Sleep(300 * time.Millisecond)
	if got := g.snapshot(); len(got) != 1 {
		t.Fatalf("test task spawned before impl completed: %d spawns %+v", len(got), got)
	}

	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if status := batches[0].Status; status != BatchStatusPassed {
		t.Errorf("batch: expected %s, got %s", BatchStatusPassed, status)
	}

	// 完整顺序：impl worker 必须先于 test worker。
	entries := g.snapshot()
	implIdx, testIdx := -1, -1
	for i, e := range entries {
		switch e.Role {
		case "software-engineer":
			if implIdx == -1 {
				implIdx = i
			}
		case "software-architect":
			if testIdx == -1 {
				testIdx = i
			}
		}
	}
	if implIdx < 0 || testIdx < 0 {
		t.Fatalf("missing impl/test spawn: %+v", entries)
	}
	if testIdx < implIdx {
		t.Errorf("test task spawned (index %d) before impl task (index %d)", testIdx, implIdx)
	}
}

// TestRunBatch_ReportsBeforeFixes locks the acceptance-batch ordering: a QA
// report task (findings checklist) must spawn BEFORE the engineer fix task in
// the same batch. The old two-phase split classified any qa-role task as a
// "test task" and ran it in the serial second stage, so the fix task started
// first, re-audited the whole repo because the checklists did not exist yet
// (v31_full 4507e35b: 59 rounds / 1.45M tokens), and the batch order inverted.
func TestRunBatch_ReportsBeforeFixes(t *testing.T) {
	g := newGateSpawner(gateOutputs())
	g.block, g.release = true, make(chan struct{})

	eng, err := New("", t.TempDir(), "", g)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("acceptance batch", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "Audit", Description: "静态集成审计", Output: "AUDIT_FINDINGS_static.md", Role: "software-qa-engineer", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
		{Title: "Runtime", Description: "运行时与交互端到端验收", Output: "AUDIT_FINDINGS_runtime.md", Role: "software-qa-engineer", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
		{Title: "Fix", Description: "集成问题修复与端到端回归", Output: "FIX_REPORT.md", Role: "software-engineer", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
		{Title: "Tests", Description: "单元测试", Output: "test_game.js", Role: "software-architect", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
	}
	batches, done := startTopoRun(eng, plan, "acceptance", workdir, mt.ID)

	waitEntries(t, g, 2) // 两个 QA 报告任务先进入（第一阶段并行）
	time.Sleep(300 * time.Millisecond)
	if got := g.snapshot(); len(got) != 2 {
		t.Fatalf("fix/test spawned before reports completed: %d spawns %+v", len(got), got)
	}

	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if status := batches[0].Status; status != BatchStatusPassed {
		t.Errorf("batch: expected %s, got %s", BatchStatusPassed, status)
	}

	// 完整顺序：报告(qa) → 修复(engineer) → 测试(architect)。
	entries := g.snapshot()
	reportIdx, fixIdx, testIdx := -1, -1, -1
	for i, e := range entries {
		switch e.Role {
		case "software-qa-engineer":
			if reportIdx == -1 {
				reportIdx = i
			}
		case "software-engineer":
			if fixIdx == -1 {
				fixIdx = i
			}
		case "software-architect":
			if testIdx == -1 {
				testIdx = i
			}
		}
	}
	if reportIdx < 0 || fixIdx < 0 || testIdx < 0 {
		t.Fatalf("missing report/fix/test spawn: %+v", entries)
	}
	if fixIdx < reportIdx {
		t.Errorf("fix task spawned (index %d) before report task (index %d)", fixIdx, reportIdx)
	}
	if testIdx < fixIdx {
		t.Errorf("test task spawned (index %d) before fix task (index %d)", testIdx, fixIdx)
	}
}

// TestStartPlanRun_ExecutesRecoveredPlan proves the async execution ticket:
// a NEW engine instance (fresh store index) recovers the master's plan from
// disk, StartPlanRun returns immediately, and PlanRunStatus polls to
// completion. Duplicate submission while running is rejected.
func TestStartPlanRun_ExecutesRecoveredPlan(t *testing.T) {
	wbDir := t.TempDir()
	workdir := t.TempDir()

	// 阶段 1：Leader 分解轮的落盘（plan.json + batch 结构 + Task 记录）。
	eng, err := New("", wbDir, "", newGateSpawner(nil))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	mt, err := eng.CreateMasterTask("recovered plan", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}
	plan := []PlanTask{
		{Title: "Task A", Description: "impl A", Output: "a.go", Role: "software-engineer", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
		{Title: "Task B", Description: "impl B", Output: "b.go", Role: "software-architect", BatchID: "b2", BatchLabel: "B2", DependsOnBatch: FlexibleStringSlice{"b1"}, MaxCycles: 1},
	}
	if _, err := eng.createBatchesFromPlan(plan, "recovered plan", workdir, mt.ID, ""); err != nil {
		t.Fatalf("create batches: %v", err)
	}
	eng.writePlanJSON(mt.ID, "", plan)
	eng.Close()

	// 阶段 2：新实例（跨进程语义 — FileTaskStore 从磁盘重建索引）。
	g := newGateSpawner(gateOutputs())
	g.block, g.release = true, make(chan struct{})
	eng2, err := New("", wbDir, "", g)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng2.Close()

	ticket, err := eng2.StartPlanRun(context.Background(), mt.ID)
	if err != nil {
		t.Fatalf("start plan run: %v", err)
	}
	if !strings.Contains(ticket, "started in background") {
		t.Errorf("ticket = %q, want started-in-background notice", ticket)
	}

	// 执行在后台：worker 已入场（被 gate 挡住），Leader 拿到的只是票据。
	waitEntries(t, g, 1)
	// 执行中重复提交 → 明确拒绝（不另起一个 run）。
	if _, err := eng2.StartPlanRun(context.Background(), mt.ID); err == nil {
		t.Fatal("expected error for duplicate StartPlanRun while running")
	} else if !strings.Contains(err.Error(), "无需重复提交") {
		t.Errorf("duplicate error = %v, want already-running notice", err)
	}
	close(g.release)

	// 异步等待：轮询 PlanRunStatus 直到完成（Leader 等待机制的原语）。
	deadline := time.Now().Add(15 * time.Second)
	var view PlanRunView
	for {
		if v, ok := PlanRunStatus(mt.ID); ok && v.Done {
			view = v
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for plan run to finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if view.Err != nil {
		t.Fatalf("plan run error: %v", view.Err)
	}
	if !strings.Contains(view.Summary, "2/2 batches passed") {
		t.Errorf("summary = %q, want 2/2 passed", view.Summary)
	}
	if len(view.Batches) != 2 {
		t.Fatalf("view batches = %d, want 2", len(view.Batches))
	}
	for _, b := range view.Batches {
		if b.Status != BatchStatusPassed {
			t.Errorf("batch %s: expected %s, got %s", b.ID, BatchStatusPassed, b.Status)
		}
	}
	got, err := eng2.GetMasterTask(mt.ID)
	if err != nil {
		t.Fatalf("get master task: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("master status = %q, want done", got.Status)
	}
}

// TestStartPlanRun_ResolvesTeamForWorkers proves the team_run_plan
// reconstruction path: the tool rebuilds the engine WITHOUT SetTeam, so
// StartPlanRun must recover the team from the master record (Agent
// "team:label") and SetTeam it — otherwise worker spawns carry an empty
// AgentName and fall to the inline fallback definition (no .md persona/tools/
// model resolve). The team definition lives on disk (workdir/.whale/teams),
// exactly where DefaultTeamRoots discovers it.
func TestStartPlanRun_ResolvesTeamForWorkers(t *testing.T) {
	wbDir := t.TempDir()
	workdir := t.TempDir()

	// 阶段 1：Leader 分解轮带 team 落盘（CreateMasterTask 把 Agent 写进 master 记录）。
	tc := &TeamConfig{Label: "smoke-software", Roles: []string{"software-engineer", "software-architect"}}
	eng, err := New("", wbDir, "", newGateSpawner(nil))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	eng.SetTeam(tc)
	mt, err := eng.CreateMasterTask("team restore", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}
	plan := []PlanTask{
		{Title: "Task A", Description: "impl A", Output: "a.go", Role: "software-engineer", BatchID: "b1", BatchLabel: "B1", MaxCycles: 1},
	}
	if _, err := eng.createBatchesFromPlan(plan, "team restore", workdir, mt.ID, ""); err != nil {
		t.Fatalf("create batches: %v", err)
	}
	eng.writePlanJSON(mt.ID, "", plan)
	eng.Close()

	// team 定义必须能在 FindTeamInRoots 命中（全局 ~/.whale/teams 在 CI 不可用）。
	teamDir := filepath.Join(workdir, ".whale", "teams", "smoke-software")
	if err := os.MkdirAll(teamDir, 0755); err != nil {
		t.Fatalf("mkdir team dir: %v", err)
	}
	teamYAML := "label: smoke-software\ncategory: test\nleader:\n  role: software-team-lead\nroles:\n- software-engineer\n- software-architect\n"
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(teamYAML), 0644); err != nil {
		t.Fatalf("write team.yaml: %v", err)
	}

	// 阶段 2：team_run_plan 工具路径——重建的实例没有任何 team 配置。
	g := newGateSpawner(gateOutputs())
	g.block, g.release = true, make(chan struct{})
	eng2, err := New("", wbDir, "", g)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng2.Close()

	if _, err := eng2.StartPlanRun(context.Background(), mt.ID); err != nil {
		t.Fatalf("start plan run: %v", err)
	}

	// worker 已入场（gate 挡住）：它的 AgentName 必须由恢复的 team 解析出来,
	// 而不是走空 AgentName 的 inline fallback。
	entries := waitEntries(t, g, 1)
	w := entries[0]
	if w.AgentName != "software-engineer" {
		t.Errorf("worker AgentName = %q, want %q (team must be recovered from master)", w.AgentName, "software-engineer")
	}
	if w.Team == "" {
		t.Errorf("worker Team = %q, want non-empty (team must be recovered)", w.Team)
	}
	close(g.release)

	deadline := time.Now().Add(15 * time.Second)
	for {
		if v, ok := PlanRunStatus(mt.ID); ok && v.Done {
			if v.Err != nil {
				t.Fatalf("plan run error: %v", v.Err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for plan run to finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
