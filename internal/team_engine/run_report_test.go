package team_engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteRunReport verifies the post-run dataset lands in
// <masterDir>/run_report.json with per-task stats (duration/tokens/retries/
// state) and batch-level summary — the machine-readable tail of the
// monitoring story (live progress is the 30s heartbeat log).
func TestWriteRunReport(t *testing.T) {
	eng, err := New(":memory:", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ws := t.TempDir()
	masterID := "deadbeef-1234-4321-90ab-abcdef012345"
	t1 := NewTask("t1", "task a", "write file", RoleDeveloper, "software-architect", 2, ws, nil, "b1", masterID)
	t2 := NewTask("t2", "task b", "check e2e", RoleTester, "", 0, ws, nil, "b1", masterID)
	for _, tk := range []*Task{t1, t2} {
		if err := eng.Store.InsertTask(tk); err != nil {
			t.Fatalf("insert task: %v", err)
		}
	}
	// Store shares the task pointers — stats written directly are visible to
	// GetTask in the report path just like UpdateTask writes.
	t1.WorkerDuration, t1.WorkerTokens = 42.5, 3500
	t1.VerifierDuration, t1.VerifierTokens = 18.2, 900
	t1.RetryCount = 1
	t1.ToolCalls, t1.TopTools = 26, "bash:18,read:7"
	t2.WorkerDuration, t2.WorkerTokens = 60.1, 4100

	batches := []*Batch{{ID: "b1", Label: "build", Status: BatchStatusPassed, Tasks: []*Task{t1, t2}}}
	started := time.Now().Add(-2 * time.Minute)
	eng.writeRunReport(masterID, "2048 game", "done", "1/1 batches passed, 0 failed", nil, started, batches, 0, 0, 0)

	data, err := os.ReadFile(filepath.Join(eng.Whiteboard.MasterDir(masterID), "run_report.json"))
	if err != nil {
		t.Fatalf("read run_report.json: %v", err)
	}
	var r runReport
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parse run_report.json: %v", err)
	}
	if r.Status != "done" || r.Summary != "1/1 batches passed, 0 failed" {
		t.Errorf("report status/summary = %q/%q", r.Status, r.Summary)
	}
	if len(r.Batches) != 1 || len(r.Batches[0].Tasks) != 2 {
		t.Fatalf("expected 1 batch with 2 tasks, got %d/%d", len(r.Batches), len(r.Batches[0].Tasks))
	}
	got := r.Batches[0].Tasks[0]
	if got.WorkerDuration != 42.5 || got.WorkerTokens != 3500 || got.VerifierDuration != 18.2 || got.VerifierTokens != 900 || got.RetryCount != 1 {
		t.Errorf("task t1 stats = %+v", got)
	}
	if got.ToolCalls != 26 || got.TopTools != "bash:18,read:7" {
		t.Errorf("task t1 tool probe = %d/%q, want 26/\"bash:18,read:7\"", got.ToolCalls, got.TopTools)
	}
	if got2 := r.Batches[0].Tasks[1]; got2.WorkerDuration != 60.1 || got2.WorkerTokens != 4100 {
		t.Errorf("task t2 stats = %+v", got2)
	}
	if r.WallSeconds < 110 || r.WallSeconds > 130 {
		t.Errorf("wall_seconds = %.1f, want ~120", r.WallSeconds)
	}
}
