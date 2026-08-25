package team_engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockSessionOps is a recording SessionOps stub for leader-driven tests.
type mockSessionOps struct {
	mu          sync.Mutex
	cont        map[string]string // sessionID → drive task (Continue calls)
	contSeq     []string          // Continue task texts in call order
	promptCalls map[string]string // sessionID → task (Prompt calls)
	resp        SubagentResponse  // default Continue/Prompt response
}

func (m *mockSessionOps) Prompt(_ context.Context, sessionID, task string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.promptCalls == nil {
		m.promptCalls = map[string]string{}
	}
	m.promptCalls[sessionID] = task
	return m.resp.Output, nil
}

func (m *mockSessionOps) Continue(_ context.Context, sessionID, task string) (SubagentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cont == nil {
		m.cont = map[string]string{}
	}
	m.cont[sessionID] = task
	m.contSeq = append(m.contSeq, task)
	return m.resp, nil
}

func (m *mockSessionOps) Fork(_ context.Context, sessionID string) (string, error) { return "", nil }
func (m *mockSessionOps) Summarize(_ context.Context, sessionID string) (string, error) {
	return "", nil
}
func (m *mockSessionOps) Abort(_ context.Context, sessionID string) error { return nil }
func (m *mockSessionOps) Kill(_ context.Context, sessionID string) error  { return nil }

// leaderPlanJSON is a minimal parseable decompose result for one batch.
const leaderPlanJSON = `{"tasks":[
	{"title":"Task A","description":"write add.go","role":"developer","batch_id":"b1","batch_label":"Build","depends_on_index":-1,"output":"add.go"},
	{"title":"Task B","description":"write add_test.go","role":"developer","batch_id":"b1","batch_label":"Build","depends_on_index":-1,"output":"add_test.go"}
]}`

// completeVerdictJSON is a COMPLETE completeness verdict — elaboration exits
// early (all 6 dimensions OK), so the second planner spawn is the decompose.
const completeVerdictJSON = `{"verdict":"COMPLETE","dimensions":[
	{"name":"scope","status":"OK"},{"name":"interface","status":"OK"},
	{"name":"behaviour","status":"OK"},{"name":"quality","status":"OK"},
	{"name":"dependencies","status":"OK"},{"name":"constraints","status":"OK"}
]}`

// TestRunLeaderDriven_DrivesTeam verifies the P2 two-turn shape:
//   - turn 1: leader decompose spawn carries OrchestrationToolNames and the
//     session ID is persisted on the master task;
//   - turn 2: SessionOps.Continue is called on that session with the drive
//     prompt (no new spawn);
//   - the drive report is returned, delivered as master output.md, and the
//     master task is marked done.
func TestRunLeaderDriven_DrivesTeam(t *testing.T) {
	spawner := &mockSpawner{
		sessionID: "sess-leader-1",
		roleSeq: map[string][]string{
			"planner": {completeVerdictJSON, leaderPlanJSON},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	finalReport := "final delivery report"
	ops := &mockSessionOps{
		resp: SubagentResponse{Output: finalReport, Success: true, ExitCode: 0},
	}
	eng.SetSessionOps(ops)

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("write a tiny calculator", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	report, err := eng.RunLeaderDriven(context.Background(), "write a tiny calculator", workdir, mt.ID)
	if err != nil {
		t.Fatalf("run leader driven: %v", err)
	}
	if report != finalReport {
		t.Errorf("report = %q, want %q", report, finalReport)
	}

	// Turn 1: the decompose spawn is lean (no persona/tools — v45): the
	// one-shot task split is pure text; the leader session records no
	// orchestration tools and the review cycle runs as an independent subagent.
	if len(spawner.reqs) < 2 {
		t.Fatalf("expected 2 planner spawns, got %d", len(spawner.reqs))
	}
	decomposeReq := spawner.reqs[1]
	if decomposeReq.Role != "planner" {
		t.Fatalf("expected planner decompose req, got role %q", decomposeReq.Role)
	}
	if len(decomposeReq.OrchestrationTools) != 0 {
		t.Errorf("decompose OrchestrationTools = %v, want none (lean split)", decomposeReq.OrchestrationTools)
	}
	// The decompose spawn itself is the only new spawn — no team_run-style
	// engine step was driven by the engine between turns.
	if got := spawner.calls["developer"]; got != 0 {
		t.Errorf("engine must not spawn workers itself in leader-driven mode, got %d developer spawns", got)
	}

	// Turn 2 + Turn 3: Continue on the leader session with the submit
	// instruction, then (after the engine-side wait) the review instruction.
	if got := len(ops.contSeq); got != 2 {
		t.Fatalf("Continue calls = %d, want 2 (submit + review); got %v", got, ops.contSeq)
	}
	submit, review := ops.contSeq[0], ops.contSeq[1]
	for _, want := range []string{"write a tiny calculator", "team_run_plan", "master_task_id: " + mt.ID} {
		if !strings.Contains(submit, want) {
			t.Errorf("submit prompt missing %q", want)
		}
	}
	if !strings.Contains(submit, "不要轮询") {
		t.Errorf("submit prompt must forbid polling (waiting is engine-side, not an LLM loop)")
	}
	if strings.Contains(submit, "逐任务执行") {
		t.Errorf("submit prompt must not tell the Leader to walk tasks one by one (TE owns execution; it must submit the whole plan via team_run_plan)")
	}
	for _, want := range []string{"write a tiny calculator", "team_result", "team_feedback", "master_task_id: " + mt.ID} {
		if !strings.Contains(review, want) {
			t.Errorf("review prompt missing %q", want)
		}
	}
	// The mock never submits via team_run_plan → waitPlanRunSettled reports the
	// run never started; the review prompt replays that outcome.
	if !strings.Contains(review, "未正常结算") {
		t.Errorf("review prompt must replay the unsettled outcome, got: %q", review)
	}

	// Leader session persisted on the master task.
	got, err := eng.GetMasterTask(mt.ID)
	if err != nil {
		t.Fatalf("get master task: %v", err)
	}
	if got.LeaderSessionID != "sess-leader-1" {
		t.Errorf("LeaderSessionID = %q, want %q", got.LeaderSessionID, "sess-leader-1")
	}
	if got.Status != "done" {
		t.Errorf("master status = %q, want done", got.Status)
	}

	// Deliverable: the leader's report IS master output.md (under the master
	// dir, not the whiteboard root).
	out, err := os.ReadFile(filepath.Join(eng.Whiteboard.MasterDir(mt.ID), "output.md"))
	if err != nil {
		t.Fatalf("read output.md: %v", err)
	}
	if string(out) != finalReport {
		t.Errorf("output.md = %q, want %q", string(out), finalReport)
	}
	if _, err := os.Stat(filepath.Join(eng.Whiteboard.BaseDir(), "output.md")); err == nil {
		t.Errorf("output.md must NOT be written at the whiteboard root (pre-P2 bug)")
	}
}

// TestRunLeaderDriven_ContinueFailure verifies that a failed drive turn marks
// the master task failed and surfaces the error (still returning the report).
func TestRunLeaderDriven_ContinueFailure(t *testing.T) {
	spawner := &mockSpawner{
		sessionID: "sess-leader-2",
		roleSeq: map[string][]string{
			"planner": {completeVerdictJSON, leaderPlanJSON},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ops := &mockSessionOps{
		resp: SubagentResponse{Output: "partial", Success: false, ExitCode: 1},
	}
	eng.SetSessionOps(ops)

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("failing drive", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	report, err := eng.RunLeaderDriven(context.Background(), "failing drive", workdir, mt.ID)
	if err == nil {
		t.Fatal("expected error for failed drive turn")
	}
	if report != "partial" {
		t.Errorf("report = %q, want the partial output", report)
	}
	got, err := eng.GetMasterTask(mt.ID)
	if err != nil {
		t.Fatalf("get master task: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("master status = %q, want failed", got.Status)
	}
}

// TestRunLeaderDriven_NoSessionOps verifies the explicit error when no
// SessionOps is wired (shell/standalone fallback has no continuation).
func TestRunLeaderDriven_NoSessionOps(t *testing.T) {
	spawner := &mockSpawner{
		sessionID: "sess-leader-3",
		roleSeq: map[string][]string{
			"planner": {completeVerdictJSON, leaderPlanJSON},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("no ops", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	if _, err := eng.RunLeaderDriven(context.Background(), "no ops", workdir, mt.ID); err == nil {
		t.Fatal("expected error when SessionOps is missing")
	} else if !strings.Contains(err.Error(), "SessionOps") {
		t.Errorf("error = %v, want SessionOps mention", err)
	}
	got, err := eng.GetMasterTask(mt.ID)
	if err != nil {
		t.Fatalf("get master task: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("master status = %q, want failed", got.Status)
	}
}

// TestRunLeaderDriven_SettledReview verifies the engine-side wait path: when
// the plan run is already settled (PlanRunState done — like a team_run_plan
// that completed), the review prompt replays the execution summary instead of
// the "never started" error, and the leader's report is delivered.
func TestRunLeaderDriven_SettledReview(t *testing.T) {
	spawner := &mockSpawner{
		sessionID: "sess-leader-4",
		roleSeq: map[string][]string{
			"planner": {completeVerdictJSON, leaderPlanJSON},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	finalReport := "final delivery report with deliverables"
	ops := &mockSessionOps{
		resp: SubagentResponse{Output: finalReport, Success: true, ExitCode: 0},
	}
	eng.SetSessionOps(ops)

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("settled review", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	// Simulate team_run_plan having completed: the plan run settles with a
	// summary before the review turn is dispatched.
	state := &PlanRunState{}
	state.finish(true, nil, "1/2 batches passed, 1 failed", nil)
	planRuns.Store(mt.ID, state)

	report, err := eng.RunLeaderDriven(context.Background(), "settled review", workdir, mt.ID)
	if err != nil {
		t.Fatalf("run leader driven: %v", err)
	}
	if report != finalReport {
		t.Errorf("report = %q, want %q", report, finalReport)
	}
	if got := len(ops.contSeq); got != 2 {
		t.Fatalf("Continue calls = %d, want 2", got)
	}
	if !strings.Contains(ops.contSeq[1], "1/2 batches passed, 1 failed") {
		t.Errorf("review prompt must replay the executed summary, got: %q", ops.contSeq[1])
	}
}
