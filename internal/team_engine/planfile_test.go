package team_engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPlanFile_RoundTrip locks the plan.json persistence shape: tasks
// written by writePlanJSON (including verify mode/focus, acceptance criteria,
// batch label) must round-trip through LoadPlanFile, and the complexity hint
// must survive — a lost complexity silently widens worker budgets to the
// default, skewing A/B comparisons.
func TestLoadPlanFile_RoundTrip(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("round trip", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{
			Title:              "Implement game.js core",
			Description:        "move/merge/win logic",
			Role:               "software-engineer",
			Output:             "game.js, game.core.test.js",
			BatchID:            "b1",
			BatchLabel:         "核心逻辑",
			DependsOnBatch:     FlexibleStringSlice{},
			VerifyMode:         "semantic",
			VerifierFocus:      "correctness",
			AcceptanceCriteria: []string{"move merges equals", "game over detected"},
		},
		{
			Title:          "E2E acceptance",
			Description:    "run the game in browser",
			Role:           "software-qa-engineer",
			Output:         "AUDIT_FINDINGS_runtime.md",
			BatchID:        "b2",
			DependsOnBatch: FlexibleStringSlice{"b1"},
		},
	}
	eng.writePlanJSON(mt.ID, "medium", plan)

	path := filepath.Join(eng.Whiteboard.MasterDir(mt.ID), "plan.json")
	complexity, tasks, err := LoadPlanFile(path)
	if err != nil {
		t.Fatalf("load plan file: %v", err)
	}
	if complexity != "medium" {
		t.Errorf("complexity = %q, want medium", complexity)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	if tasks[0].VerifyMode != "semantic" || tasks[0].VerifierFocus != "correctness" {
		t.Errorf("verify mode/focus lost: %+v", tasks[0])
	}
	if tasks[0].BatchLabel != "核心逻辑" {
		t.Errorf("batch label lost: %q", tasks[0].BatchLabel)
	}
	if len(tasks[0].AcceptanceCriteria) != 2 {
		t.Errorf("acceptance criteria lost: %+v", tasks[0].AcceptanceCriteria)
	}
	if len(tasks[1].DependsOnBatch) != 1 || tasks[1].DependsOnBatch[0] != "b1" {
		t.Errorf("depends_on lost: %+v", tasks[1].DependsOnBatch)
	}

	// Historical top-level array format must keep loading.
	arrPath := filepath.Join(t.TempDir(), "plan.json")
	arrData, _ := json.Marshal(plan)
	if err := os.WriteFile(arrPath, arrData, 0644); err != nil {
		t.Fatalf("write legacy plan: %v", err)
	}
	complexity2, tasks2, err := LoadPlanFile(arrPath)
	if err != nil {
		t.Fatalf("load legacy array plan: %v", err)
	}
	if complexity2 != "" || len(tasks2) != 2 {
		t.Errorf("legacy load = complexity %q tasks %d, want empty / 2", complexity2, len(tasks2))
	}
}

// TestUpdateTaskTokenStatsAccumulate locks the per-task token bookkeeping:
// repeated UpdateTask calls for worker/verifier stats must accumulate (retry
// rounds count twice) and persist into meta.json so run_report and RUN SUMMARY
// read the true totals instead of a counter delta (which cross-contaminates
// when batches run in parallel).
func TestUpdateTaskTokenStatsAccumulate(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	mt, err := eng.CreateMasterTask("stats", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}
	task, err := eng.CreateTask("T", "desc", RoleDeveloper, ProfileDefault, nil, 3, t.TempDir(), "", "b1", mt.ID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := eng.Store.UpdateTask(task.ID, map[string]interface{}{
		"worker_tokens":           100,
		"worker_duration_seconds": 5.5,
	}); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if err := eng.Store.UpdateTask(task.ID, map[string]interface{}{
		"worker_tokens":             50,
		"verifier_tokens":           20,
		"verifier_duration_seconds": 2.0,
	}); err != nil {
		t.Fatalf("update 2: %v", err)
	}

	cur, err := eng.Store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if cur.WorkerTokens != 150 {
		t.Errorf("worker tokens = %d, want 150 (accumulated)", cur.WorkerTokens)
	}
	if cur.VerifierTokens != 20 || cur.VerifierDuration != 2.0 {
		t.Errorf("verifier stats = %d/%v, want 20/2.0", cur.VerifierTokens, cur.VerifierDuration)
	}

	// Persisted: meta.json carries the accumulated values.
	metaRaw, err := os.ReadFile(filepath.Join(eng.Whiteboard.TaskDir(task.ID), "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	if !strings.Contains(string(metaRaw), `"worker_tokens": 150`) {
		t.Errorf("meta.json missing accumulated worker_tokens: %s", metaRaw)
	}
}

// TestRunLeaderDriven_PlanFile proves the plan-file path: the Leader session
// is bootstrapped with ONE minimal spawn (no elaborate, no decompose — the
// plan comes from the file), the drive turn still runs on that session, and
// no LLM decomposition prompt ever reaches the spawner.
func TestRunLeaderDriven_PlanFile(t *testing.T) {
	spawner := &mockSpawner{sessionID: "sess-leader-1"}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	finalReport := "plan-file delivery report"
	ops := &mockSessionOps{resp: SubagentResponse{Output: finalReport, Success: true, ExitCode: 0}}
	eng.SetSessionOps(ops)

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("plan file goal", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "Task A", Description: "write add.go", Role: string(RoleDeveloper), Output: "add.go", BatchID: "b1", BatchLabel: "Build"},
	}
	report, err := eng.RunLeaderDriven(context.Background(), "plan file goal", workdir, mt.ID,
		WithPreDecomposedPlan(plan, "simple"))
	if err != nil {
		t.Fatalf("run leader driven with plan file: %v", err)
	}
	if report != finalReport {
		t.Errorf("report = %q, want %q", report, finalReport)
	}

	// Exactly one planner spawn (the bootstrap) and it must be a one-shot
	// minimal turn — no elaborate check, no decompose prompt.
	spawner.mu.Lock()
	reqs := append([]SubagentRequest(nil), spawner.reqs...)
	spawner.mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("planner spawns = %d, want 1 (bootstrap only)", len(reqs))
	}
	boot := reqs[0]
	if boot.Role != "planner" || boot.MaxIters != 15 || boot.MaxCalls != 40 {
		t.Errorf("bootstrap req = role %q iters %d calls %d, want planner/15/40 (review-round budget)", boot.Role, boot.MaxIters, boot.MaxCalls)
	}
	if !strings.Contains(boot.Task, "等待编排指令") {
		t.Errorf("bootstrap must be a minimal acknowledgement, got: %s", boot.Task)
	}
	if strings.Contains(boot.Task, "产出文件") || strings.Contains(boot.Task, "evaluate the following") {
		t.Errorf("bootstrap must not carry a decomposition prompt: %s", boot.Task)
	}
	if !reflectDeepEqual(boot.OrchestrationTools, OrchestrationToolNames) {
		t.Errorf("bootstrap OrchestrationTools = %v, want %v", boot.OrchestrationTools, OrchestrationToolNames)
	}

	// Drive + review turns ran on the bootstrapped session (2 Continues).
	ops.mu.Lock()
	contSeq := append([]string(nil), ops.contSeq...)
	ops.mu.Unlock()
	if len(contSeq) != 2 {
		t.Fatalf("Continue calls = %d, want 2 (submit + review turns)", len(contSeq))
	}

	// Master output delivered + master done.
	outPath := filepath.Join(eng.Whiteboard.MasterDir(mt.ID), "output.md")
	raw, err := os.ReadFile(outPath)
	if err != nil || string(raw) != finalReport {
		t.Fatalf("master output missing: %v", err)
	}
}

// TestE2ETitleMarked locks the E2E guidance trigger: browser/runtime
// acceptance titles get the script-first guidance; static audits do not.
func TestE2ETitleMarked(t *testing.T) {
	cases := []struct {
		name  string
		title string
		desc  string
		want  bool
	}{
		{"runtime acceptance", "运行时与交互/持久化端到端验收", "浏览器自动化打开 index.html", true},
		{"fix and regression", "集成问题修复与端到端回归", "修复后再次打开 index.html 确认", true},
		{"static audit", "静态集成审计：引用路径/脚本加载/命名契约/加载顺序", "读取工作区源文件核对", false},
		{"unit test", "编写核心逻辑单元测试", "无浏览器", false},
	}
	for _, c := range cases {
		if got := e2eTitleMarked(c.title); got != c.want {
			t.Errorf("%s: e2eTitleMarked = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEffectiveWorkerBudgetAndSelfSplit locks the report-task cost guards:
// QA findings/E2E acceptance tasks get a capped iteration budget (30 iters,
// v39 runtime QA 56 rounds / 1.24M tokens), and report/verification tasks are
// FORBIDDEN from self-splitting — the v40 run showed tool-cap → split recursion
// (batch3 13 tasks / 8M tokens) when report tasks were allowed to split.
func TestEffectiveWorkerBudgetAndSelfSplit(t *testing.T) {
	reportTask := &Task{Role: AgentRole("software-qa-engineer"), Output: "AUDIT_FINDINGS_runtime.md", Title: "运行时与交互/持久化端到端验收", Complexity: "medium"}
	if iters, _, _ := effectiveWorkerBudget(reportTask); iters != 25 {
		t.Errorf("E2E report task iters = %d, want 25", iters)
	}
	if taskMaySelfSplit(reportTask) {
		t.Errorf("report task must not self-split")
	}

	auditTask := &Task{Role: AgentRole("software-qa-engineer"), Output: "AUDIT_FINDINGS_static.md", Title: "静态集成审计：引用路径/脚本加载/命名契约/加载顺序", Complexity: "medium"}
	if iters, _, _ := effectiveWorkerBudget(auditTask); iters != 20 {
		t.Errorf("static audit iters = %d, want 20", iters)
	}

	implTask := &Task{Role: "software-engineer", Output: "game.js, game.core.test.js", Title: "实现游戏核心逻辑", Complexity: "medium"}
	if iters, _, _ := effectiveWorkerBudget(implTask); iters != 30 {
		t.Errorf("impl task iters = %d, want 30 (medium budget)", iters)
	}
	if !taskMaySelfSplit(implTask) {
		t.Errorf("impl task may self-split")
	}

	unitTestTask := &Task{Role: AgentRole("software-qa-engineer"), Output: "game.test.js", Title: "编写单元测试"}
	if !taskMaySelfSplit(unitTestTask) {
		t.Errorf("unit-test task may self-split (not a report task)")
	}

	// fix 任务：语义 verifier 走高预算（32 轮）——v41/v43 的 20 轮 cap 循环。
	fixTask := &Task{Role: "software-engineer", Output: "FIX_REPORT.md", Title: "集成问题修复与端到端回归", Complexity: "medium"}
	if !isFixTask(fixTask) {
		t.Errorf("fix task not recognized")
	}
	if iters, calls, _ := effectiveVerifierBudget(fixTask); iters != 32 || calls != 100 {
		t.Errorf("fix verifier budget = %d/%d, want 32/100", iters, calls)
	}
	auditV := &Task{Role: AgentRole("software-qa-engineer"), Output: "AUDIT_FINDINGS_static.md", Title: "静态集成审计", Complexity: "medium"}
	if iters, _, _ := effectiveVerifierBudget(auditV); iters != 20 {
		t.Errorf("audit verifier budget = %d, want 20 (medium)", iters)
	}
}

func reflectDeepEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
