package team_engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestComplexityPersists covers the leader-decision plumbing: complexity must
// survive the store round-trip (UpdateTask → meta.json → GetTask), otherwise
// every task silently falls back to the complex-tier iteration budget
// (80 iters) and long debug spirals run uncapped (2048 v11 教训).
func TestComplexityPersists(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.CreateTask("Core logic", "write game.js", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := eng.Store.UpdateTask(task.ID, map[string]interface{}{"complexity": "medium"}); err != nil {
		t.Fatalf("update complexity: %v", err)
	}

	// Round-trip through GetTask (reads from the in-memory copy synced with
	// meta.json) — the persisted value must survive.
	updated, err := eng.Store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Complexity != "medium" {
		t.Fatalf("complexity = %q, want medium", updated.Complexity)
	}
	// And it must drive the worker budget (30 iters, not 80).
	iters, calls, _ := effectiveWorkerBudget(updated)
	if iters != 30 || calls != 80 {
		t.Fatalf("budget = %d/%d, want 30/80 for medium", iters, calls)
	}
}

// TestComplexityPersistsOnInsert covers InsertTask (the file-store entry
// point) writing complexity into meta.json.
func TestComplexityPersistsOnInsert(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task := NewTask("t-cx", "Task", "desc", RoleDeveloper, "", 2, t.TempDir(), nil, "b1", "m1")
	task.Complexity = "simple"
	if err := eng.Store.InsertTask(task); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	got, err := eng.Store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Complexity != "simple" {
		t.Fatalf("complexity = %q, want simple", got.Complexity)
	}
	iters, _, _ := effectiveWorkerBudget(got)
	if iters != 20 {
		t.Fatalf("simple budget iters = %d, want 20", iters)
	}
}

// TestTokenSplitAccounting covers the cache hit/miss accounting that feeds
// effective_tokens in the run report: raw prompt sums are dominated by cached
// history replay and massively overstate real cost.
func TestTokenSplitAccounting(t *testing.T) {
	eng, err := New(":memory:", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	// 1000 prompt tokens: 800 from prefix cache, 200 full-price; 200 completion.
	eng.addTokens(1000, 200, 800, 200)
	hit, miss, completion := eng.usageSplit()
	if hit != 800 || miss != 200 || completion != 200 {
		t.Fatalf("split = %d/%d/%d, want 800/200/200", hit, miss, completion)
	}
	if got := eng.tokenTotal(); got != 1200 {
		t.Fatalf("raw total = %d, want 1200", got)
	}
	// effective = miss + completion + hit/31 (DeepSeek hit ≈ 1/31 of miss).
	if got := eng.effectiveTokens(); got != 200+200+800/31 {
		t.Fatalf("effective = %d, want %d", got, 200+200+800/31)
	}
}

// TestEffectiveTokensFallsBackToRaw covers spawners that record no cache
// split (e.g. the lite spawner): the report must fall back to the raw total
// instead of showing a misleading zero.
func TestEffectiveTokensFallsBackToRaw(t *testing.T) {
	eng, err := New(":memory:", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	eng.addTokens(500, 100, 0, 0)
	if got := eng.effectiveTokens(); got != 600 {
		t.Fatalf("effective = %d, want raw fallback 600", got)
	}
}

// TestRunReportTokenSplit verifies the report carries the raw/cache/effective
// token columns that let post-run analysis tell replay volume from real cost.
// Totals are aggregated from the store's per-task persisted split plus the
// caller-supplied leader split — two engine instances (leader/execute) share
// the store, so local counters alone understate the run.
func TestRunReportTokenSplit(t *testing.T) {
	eng, err := New(":memory:", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ws := t.TempDir()
	masterID := "deadbeef-1234-4321-90ab-abcdef012345"
	t1 := NewTask("t1", "task a", "write file", RoleDeveloper, "", 2, ws, nil, "b1", masterID)
	if err := eng.Store.InsertTask(t1); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	// Worker: prompt 10K (9K hit / 1K miss) + 2K completion; verifier: 1K hit.
	if err := eng.Store.UpdateTask(t1.ID, map[string]interface{}{
		"worker_prompt_hit":    9000,
		"worker_prompt_miss":   1000,
		"worker_completion":    2000,
		"worker_tokens":        12000,
		"verifier_prompt_hit":  1000,
		"verifier_prompt_miss": 200,
		"verifier_completion":  100,
		"verifier_tokens":      1300,
	}); err != nil {
		t.Fatalf("update task split: %v", err)
	}

	batches := []*Batch{{ID: "b1", Label: "build", Status: BatchStatusPassed, Tasks: []*Task{t1}}}
	started := time.Now().Add(-time.Minute)
	// Leader turns: 3K hit / 500 miss / 100 completion.
	eng.writeRunReport(masterID, "2048 game", "done", "ok", nil, started, batches, 3000, 500, 100)

	data, err := os.ReadFile(filepath.Join(eng.Whiteboard.MasterDir(masterID), "run_report.json"))
	if err != nil {
		t.Fatalf("read run_report.json: %v", err)
	}
	var r runReport
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parse run_report.json: %v", err)
	}
	// raw = worker 12000 + verifier 1300 + leader 3600 = 16900.
	if r.TotalTokens != 16900 {
		t.Errorf("total_tokens = %d, want 16900", r.TotalTokens)
	}
	if r.PromptCacheHitTokens != 9000+1000+3000 || r.PromptCacheMissTokens != 1000+200+500 || r.CompletionTokens != 2000+100+100 {
		t.Errorf("split = %d/%d/%d, want 13000/1700/2200", r.PromptCacheHitTokens, r.PromptCacheMissTokens, r.CompletionTokens)
	}
	if want := 1700 + 2200 + 13000/31; r.EffectiveTokens != want {
		t.Errorf("effective_tokens = %d, want %d", r.EffectiveTokens, want)
	}
}

// TestDeclaredChangedFiltersManifest verifies the delivery manifest only
// lists the task's declared outputs — sibling files changed concurrently are
// not this task's deliverables.
func TestDeclaredChangedFiltersManifest(t *testing.T) {
	task := &Task{ID: "t1", Output: "game.js, test/core.test.js"}
	changed := []string{"game.js", "test/core.test.js", "style.css"}
	got := declaredChanged(task, changed)
	if len(got) != 2 || got[0] != "game.js" || got[1] != "test/core.test.js" {
		t.Fatalf("declaredChanged = %v, want [game.js test/core.test.js]", got)
	}
}

// TestUnexpectedDeliverablesExcluding verifies the ownership-anomaly warning
// drops files that are sibling tasks' declared outputs (their concurrent
// writes misattributed by the per-task baseline snapshot).
func TestUnexpectedDeliverablesExcluding(t *testing.T) {
	task := &Task{ID: "t1", Output: "game.js"}
	changed := []string{"game.js", "style.css", "scratch.txt"}
	exclude := map[string]bool{"style.css": true}
	got := unexpectedDeliverablesExcluding(task, ".", changed, exclude)
	if len(got) != 1 || got[0] != "scratch.txt" {
		t.Fatalf("unexpected = %v, want [scratch.txt]", got)
	}
}

// TestSiblingDeclaredOutputs verifies the exclusion set comes from the other
// tasks of the same master run.
func TestSiblingDeclaredOutputs(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	ws := t.TempDir()
	for _, tk := range []*Task{
		NewTask("t1", "core", "d", RoleDeveloper, "", 2, ws, nil, "b1", "m1"),
		NewTask("t2", "ui", "d", RoleDeveloper, "", 2, ws, nil, "b1", "m1"),
	} {
		tk.Output = map[string]string{"t1": "game.js", "t2": "style.css"}[tk.ID]
		if err := eng.Store.InsertTask(tk); err != nil {
			t.Fatalf("insert task: %v", err)
		}
	}
	t1, _ := eng.Store.GetTask("t1")
	exclude := eng.siblingDeclaredOutputs(t1)
	if len(exclude) != 1 || !exclude["style.css"] {
		t.Fatalf("sibling outputs = %v, want {style.css}", exclude)
	}
	if _, ok := exclude["game.js"]; ok {
		t.Fatal("own output must not be in the sibling set")
	}
}
