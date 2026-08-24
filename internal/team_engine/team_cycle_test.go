package team_engine

import (
	"context"
	"testing"
)

// newCycleEngine builds an engine whose planner role sequences the
// plan-level review decisions (decompose is skipped via preDecomposed, so the
// planner spawns are all ReviewPlanCycle calls).
func newCycleEngine(t *testing.T, plannerSeq []string) (*TeamEngine, *mockSpawner) {
	t.Helper()
	workerOutput := "package main\n\nfunc main() {}\n"
	spawner := &mockSpawner{
		roleOutputs: map[string]string{
			"software-engineer":  workerOutput,
			"software-architect": workerOutput,
			"verifier":           "TOOLS USED: read_file\nVERDICT: PASS\nEVIDENCE: ok\n## FINDINGS\n---json\n[]\n---",
		},
		roleSeq: map[string][]string{
			"planner": plannerSeq,
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng, spawner
}

// TestTeamCycle_AcceptFirstCycle verifies the happy path: the Leader accepts
// the plan-level report of the first Cycle and the team delivers.
func TestTeamCycle_AcceptFirstCycle(t *testing.T) {
	eng, spawner := newCycleEngine(t, []string{
		`{"decision":"accept","reason":"all good"}`,
	})
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("one batch", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "A", Description: "independent A", Output: "a.go", Role: "software-engineer", BatchID: "batch-a", BatchLabel: "A", MaxCycles: 1},
	}

	batches, err := eng.TeamCycle(context.Background(), "one batch", workdir, mt.ID, plan...)
	if err != nil {
		t.Fatalf("team cycle: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	if batches[0].Status != BatchStatusPassed {
		t.Errorf("batch-a: expected %s, got %s", BatchStatusPassed, batches[0].Status)
	}
	if got := spawner.calls["planner"]; got != 1 {
		t.Errorf("expected 1 plan review, got %d", got)
	}
	if got := spawner.calls["software-engineer"]; got != 1 {
		t.Errorf("expected 1 worker spawn, got %d", got)
	}
}

// TestTeamCycle_RejectSkipsPassedBatches verifies the incremental re-run:
// after the Leader rejects the first Cycle, the second Cycle re-runs only the
// not-passed batches — the passed batch's worker is not spawned again.
func TestTeamCycle_RejectSkipsPassedBatches(t *testing.T) {
	eng, spawner := newCycleEngine(t, []string{
		`{"decision":"reject","reason":"try again","feedback":"improve"}`,
		`{"decision":"accept","reason":"good now"}`,
	})
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("two batches", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "A", Description: "independent A", Output: "a.go", Role: "software-engineer", BatchID: "batch-a", BatchLabel: "A", MaxCycles: 1},
		{Title: "C", Description: "depends on missing batch", Output: "c.go", Role: "software-engineer", BatchID: "batch-c", BatchLabel: "C", DependsOnBatch: FlexibleStringSlice{"ghost"}, MaxCycles: 1},
	}

	batches, err := eng.TeamCycle(context.Background(), "two batches", workdir, mt.ID, plan...)
	if err != nil {
		t.Fatalf("team cycle: %v", err)
	}

	status := map[string]BatchStatus{}
	for _, b := range batches {
		status[b.ID] = b.Status
	}
	if status["batch-a"] != BatchStatusPassed {
		t.Errorf("batch-a: expected %s, got %s", BatchStatusPassed, status["batch-a"])
	}
	if status["batch-c"] != BatchStatusFailed {
		t.Errorf("batch-c: expected %s, got %s", BatchStatusFailed, status["batch-c"])
	}

	// Two Cycles → two plan reviews.
	if got := spawner.calls["planner"]; got != 2 {
		t.Errorf("expected 2 plan reviews, got %d", got)
	}
	// batch-a ran exactly once — the second Cycle skipped the passed batch.
	// batch-c never spawns a worker (dangling dep fails it immediately).
	if got := spawner.calls["software-engineer"]; got != 1 {
		t.Errorf("expected 1 worker spawn (passed batch skipped in Cycle 2), got %d", got)
	}
}
