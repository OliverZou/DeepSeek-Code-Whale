package team_engine

import (
	"testing"
)

// TestAffectedTaskIDs builds a 4-batch dependency graph:
//
//	b1 {t1}          (pending)
//	b2 {t2}  → b1    (pending)
//	b3 {t3}  → b1    (pending)
//	b4 {t4}  → b2,b3 (passed)
//	b5 {t5}          (pending, independent)
//
// and asserts the downstream closure for each change seed.
func TestAffectedTaskIDs(t *testing.T) {
	mkTask := func(id, batchID string) *Task {
		return &Task{ID: id, BatchID: batchID}
	}
	mkBatch := func(id string, deps []string, status BatchStatus, tasks ...*Task) *Batch {
		return &Batch{ID: id, DependsOn: deps, Status: status, Tasks: tasks}
	}
	batches := []*Batch{
		mkBatch("b1", nil, BatchStatusPending, mkTask("t1", "b1")),
		mkBatch("b2", []string{"b1"}, BatchStatusPending, mkTask("t2", "b2")),
		mkBatch("b3", []string{"b1"}, BatchStatusPending, mkTask("t3", "b3")),
		mkBatch("b4", []string{"b2", "b3"}, BatchStatusPassed, mkTask("t4", "b4")),
		mkBatch("b5", nil, BatchStatusPending, mkTask("t5", "b5")),
	}

	e := &TeamEngine{}

	// Changing t1 (b1) affects b1, its dependents b2+b3, and stops at passed b4.
	got := e.AffectedTaskIDs(batches, "t1")
	if len(got) != 3 || !containsAll(got, "t1", "t2", "t3") {
		t.Fatalf("change t1: got %v, want [t1 t2 t3]", got)
	}

	// Changing t4 (passed batch) resolves to nothing — passed batches are excluded.
	if got := e.AffectedTaskIDs(batches, "t4"); len(got) != 0 {
		t.Fatalf("change t4 (passed): got %v, want none", got)
	}

	// Changing t5 (independent batch) only touches its own batch.
	got = e.AffectedTaskIDs(batches, "t5")
	if len(got) != 1 || got[0] != "t5" {
		t.Fatalf("change t5: got %v, want [t5]", got)
	}

	// Unknown IDs are ignored.
	if got := e.AffectedTaskIDs(batches, "nope"); len(got) != 0 {
		t.Fatalf("unknown id: got %v, want none", got)
	}

	// No seeds → empty.
	if got := e.AffectedTaskIDs(batches); len(got) != 0 {
		t.Fatalf("no seeds: got %v, want none", got)
	}

	// Changing t2 affects b2 and its downstream b4 is passed so excluded.
	got = e.AffectedTaskIDs(batches, "t2")
	if len(got) != 1 || got[0] != "t2" {
		t.Fatalf("change t2: got %v, want [t2]", got)
	}
}

func containsAll(items []string, want ...string) bool {
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		seen[it] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}
