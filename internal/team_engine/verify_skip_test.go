package team_engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutDeliverables(t *testing.T) {
	t.Run("pure new", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		// Baseline taken before the worker ran, with no game.js in it → purely new.
		count, touches, err := outDeliverables(workdir, map[string]time.Time{}, nil)
		if err != nil {
			t.Fatalf("outDeliverables: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 deliverable, got %d", count)
		}
		if touches {
			t.Fatal("expected no existing-file touches for a pure-new deliverable")
		}
	})

	t.Run("touches existing", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		// Baseline records an older modtime for the same file → modified, not new.
		baseline := map[string]time.Time{"game.js": time.Now().Add(-time.Hour)}
		count, touches, err := outDeliverables(workdir, baseline, nil)
		if err != nil {
			t.Fatalf("outDeliverables: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 deliverable, got %d", count)
		}
		if !touches {
			t.Fatal("expected existing-file touch to be detected")
		}
	})

	t.Run("empty out", func(t *testing.T) {
		workdir := t.TempDir()
		count, touches, err := outDeliverables(workdir, map[string]time.Time{}, nil)
		if err != nil {
			t.Fatalf("outDeliverables: %v", err)
		}
		if count != 0 {
			t.Fatalf("expected 0 deliverables for empty workdir, got %d", count)
		}
		if touches {
			t.Fatal("expected no touches for empty workdir")
		}
	})

	t.Run("scoped to declared outputs", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		// 并发 sibling 的写入（style.css）不归本任务：scoped 统计只数声明文件，
		// 也不把它算成回归触碰。
		if err := os.WriteFile(filepath.Join(workdir, "style.css"), []byte("// y"), 0644); err != nil {
			t.Fatal(err)
		}
		count, touches, err := outDeliverables(workdir, map[string]time.Time{}, []string{"game.js"})
		if err != nil {
			t.Fatalf("outDeliverables: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 declared deliverable, got %d", count)
		}
		if touches {
			t.Fatal("sibling file must not count as a touch")
		}
	})

	t.Run("ignores .whale metadata", func(t *testing.T) {		workdir := t.TempDir()
		// whale writes metadata into .whale under the workspace; that path overlap
		// must not be treated as a regression touch — .whale is tool metadata,
		// not a deliverable.
		metaRel := filepath.Join(".whale", "team_tasks", "logs", "team_engine.log")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workdir, metaRel)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, metaRel), []byte("master log"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		count, touches, err := outDeliverables(workdir, map[string]time.Time{}, nil)
		if err != nil {
			t.Fatalf("outDeliverables: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 deliverable (excluding .whale), got %d", count)
		}
		if touches {
			t.Fatal("expected .whale metadata to be ignored, not counted as a touch")
		}
	})
}

func TestVerifyDepth(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	t.Run("pure new no role mechanical", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		// An automated test must be present for the mechanical gate to be
		// non-vacuous; otherwise mechanical depth is forbidden. The test file
		// must be THIS task's declared output — a sibling task's test file is
		// not this task's objective gate.
		if err := os.WriteFile(filepath.Join(workdir, "game.test.js"), []byte("// test"), 0644); err != nil {
			t.Fatal(err)
		}
		// Pre-worker baseline: nothing existed → all deliverables new.
		task := &Task{ID: "t1", Output: "game.js, game.test.js", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, map[string]time.Time{}); got != "mechanical" {
			t.Fatalf("expected mechanical depth for pure-new deliverable, got %q", got)
		}
	})

	t.Run("explicit verify_mode wins over heuristic", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		// Heuristic: no test command → semantic. Explicit mode must still win.
		task := &Task{ID: "tm1", VerifyMode: "mechanical", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, nil); got != "mechanical" {
			t.Fatalf("expected explicit mechanical depth, got %q", got)
		}
		// And explicit semantic on the same low-risk deliverable must not
		// collapse to mechanical.
		task.VerifyMode = "semantic"
		if got := eng.verifyDepth(task, workdir, nil); got != "semantic" {
			t.Fatalf("expected explicit semantic depth, got %q", got)
		}
	})

	t.Run("no automated tests semantic", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t1c", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, map[string]time.Time{}); got != "semantic" {
			t.Fatal("expected semantic depth when no automated test command is present")
		}
	})

	t.Run("empty out semantic", func(t *testing.T) {
		task := &Task{ID: "t1b", Workdir: t.TempDir()}
		if got := eng.verifyDepth(task, t.TempDir(), map[string]time.Time{}); got != "semantic" {
			t.Fatal("expected semantic depth when worker produced no deliverable")
		}
	})

	t.Run("touches existing semantic", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		// Baseline recorded the file with an older modtime → the worker modified
		// an existing file, regression risk, semantic depth.
		baseline := map[string]time.Time{"game.js": time.Now().Add(-time.Hour)}
		task := &Task{ID: "t2", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, baseline); got != "semantic" {
			t.Fatal("expected semantic depth when deliverable touches existing file")
		}
	})

	t.Run("no baseline semantic", func(t *testing.T) {
		// Resume-with-existing-output has no pre-worker baseline — new vs
		// modified files cannot be told apart, so stay conservative.
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t2b", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, nil); got != "semantic" {
			t.Fatal("expected semantic depth when baseline is missing")
		}
	})

	t.Run("verifier role semantic", func(t *testing.T) {
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t3", VerifierRole: "architect", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, map[string]time.Time{}); got != "semantic" {
			t.Fatal("expected semantic depth for key role even when pure-new")
		}
	})

	t.Run("DW semantic", func(t *testing.T) {
		task := &Task{ID: "t4", UseDW: true, Workdir: t.TempDir()}
		if got := eng.verifyDepth(task, t.TempDir(), nil); got != "semantic" {
			t.Fatal("expected semantic depth for DW tasks")
		}
	})

	t.Run("declared output missing semantic", func(t *testing.T) {
		// The workdir is non-empty and has a test gate, and nothing was touched
		// (touches=false, count>0) — all conditions for a mechanical SKIP. But the
		// declared deliverable game.js is absent, which must force semantic.
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "other.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, "game.test.js"), []byte("// test"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t1d", Output: "game.js", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, map[string]time.Time{}); got != "semantic" {
			t.Fatalf("expected semantic when declared deliverable is missing, got %q", got)
		}
	})

	t.Run("declared output present mechanical", func(t *testing.T) {
		// Declared deliverable + declared test gate exist + pure-new → mechanical.
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, "game.test.js"), []byte("// test"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t1e", Output: "game.js, game.test.js", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, map[string]time.Time{}); got != "mechanical" {
			t.Fatalf("expected mechanical when declared deliverable exists, got %q", got)
		}
	})

	t.Run("sibling test file is not the task's gate", func(t *testing.T) {
		// 任务只声明 game.js；工作区里的 game.test.js 属于另一个并发任务。
		// 它不是本任务的客观门——机械门必须属于本任务，否则会拿别家半成品
		// 测试制造假失败和注定失败的 retry（2048 v10 的 2.35M 教训）。
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, "game.test.js"), []byte("// test"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t1f", Output: "game.js", Workdir: workdir}
		if got := eng.verifyDepth(task, workdir, map[string]time.Time{}); got != "semantic" {
			t.Fatalf("expected semantic when only a sibling's test file exists, got %q", got)
		}
	})
}

func TestUpstreamOutputs_CrossBatch(t *testing.T) {
	store, err := NewFileTaskStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ws := t.TempDir() // workspace 根

	// batch1 前置任务：产出 game.js，已 Done。
	taskA := &Task{ID: "aaa", Title: "核心逻辑", Output: "game.js", Workdir: ws, MasterTaskID: "m1", BatchID: "1"}
	if err := store.InsertTask(taskA); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"output.md", "verify.md"} {
		if err := os.WriteFile(filepath.Join(store.taskDir("aaa"), name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// batch2 任务：应能拿到跨 batch 前置任务的产出（绝对路径）。
	taskB := &Task{ID: "bbb", Title: "接线", Output: "game.js", Workdir: ws, MasterTaskID: "m1", BatchID: "2", UpstreamBatches: []string{"1"}}
	if err := store.InsertTask(taskB); err != nil {
		t.Fatal(err)
	}

	refs := store.UpstreamOutputs(taskB)
	if len(refs) != 1 {
		t.Fatalf("expected 1 cross-batch upstream ref, got %d", len(refs))
	}
	if refs[0].Name != "核心逻辑" {
		t.Fatalf("expected upstream name 核心逻辑, got %q", refs[0].Name)
	}
	if want := filepath.Join(ws, "game.js"); refs[0].Path != want {
		t.Fatalf("expected absolute path %q, got %q", want, refs[0].Path)
	}
}

func TestUpstreamOutputs_NoDependencySkipsUnrelated(t *testing.T) {
	store, err := NewFileTaskStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ws := t.TempDir()

	// 同 master 下另一个已完成的任务，但当前任务不依赖它。
	taskA := &Task{ID: "aaa", Title: "无关前置", Output: "other.js", Workdir: ws, MasterTaskID: "m1", BatchID: "1"}
	if err := store.InsertTask(taskA); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"output.md", "verify.md"} {
		if err := os.WriteFile(filepath.Join(store.taskDir("aaa"), name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 当前任务没有任何上游 batch 依赖 → 不应看到无关任务的产出。
	taskB := &Task{ID: "bbb", Title: "独立任务", Output: "app.js", Workdir: ws, MasterTaskID: "m1", BatchID: "2"}
	if err := store.InsertTask(taskB); err != nil {
		t.Fatal(err)
	}

	refs := store.UpstreamOutputs(taskB)
	if len(refs) != 0 {
		t.Fatalf("expected 0 upstream refs for a task with no dependency, got %d", len(refs))
	}
}

func TestExtractUpstreamSection(t *testing.T) {
	inbox := "# 任务\n\n## 📋 任务描述\n\ndo the thing\n\n## 📥 上游产出（请先阅读）\n\n- [核心逻辑](/ws/game.js)\n\n## 📄 产出模板\n\nwrite js\n\n## 🧠 团队记忆\n\nrole memory\n\n## 🔄 上一轮审查反馈\n\nretry this\n"
	got := extractUpstreamSection(inbox)
	if !strings.Contains(got, "上游产出") {
		t.Fatalf("expected upstream section, got %q", got)
	}
	if strings.Contains(got, "产出模板") || strings.Contains(got, "团队记忆") || strings.Contains(got, "审查反馈") {
		t.Fatalf("leaked non-upstream section into verifier context: %q", got)
	}
	if !strings.Contains(got, "/ws/game.js") {
		t.Fatalf("expected upstream file ref, got %q", got)
	}
	if got2 := extractUpstreamSection("no upstream here"); got2 != "" {
		t.Fatalf("expected empty for missing upstream section, got %q", got2)
	}
}
