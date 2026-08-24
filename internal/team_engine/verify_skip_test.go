package team_engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutDeliverables(t *testing.T) {
	t.Run("pure new", func(t *testing.T) {
		outDir := t.TempDir()
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(outDir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		count, touches, err := outDeliverables(outDir, workdir)
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
		outDir := t.TempDir()
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "game.js"), []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		count, touches, err := outDeliverables(outDir, workdir)
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
		outDir := t.TempDir()
		count, touches, err := outDeliverables(outDir, t.TempDir())
		if err != nil {
			t.Fatalf("outDeliverables: %v", err)
		}
		if count != 0 {
			t.Fatalf("expected 0 deliverables for empty out, got %d", count)
		}
		if touches {
			t.Fatal("expected no touches for empty out")
		}
	})

	t.Run("ignores .whale metadata", func(t *testing.T) {
		outDir := t.TempDir()
		workdir := t.TempDir()
		// whale exec writes its own engine log into out/.whale; the master engine
		// writes the same relative path under the workspace. This path overlap must
		// not be treated as a regression touch — .whale is tool metadata, not a
		// deliverable.
		metaRel := filepath.Join(".whale", "team_tasks", "logs", "team_engine.log")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workdir, metaRel)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, metaRel), []byte("master log"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(outDir, metaRel)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, metaRel), []byte(""), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		count, touches, err := outDeliverables(outDir, workdir)
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

func TestShouldSkipVerifier(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	t.Run("pure new no role skips", func(t *testing.T) {
		outDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(outDir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t1", UseDW: false, VerifierRole: "", Workdir: t.TempDir()}
		if !eng.shouldSkipVerifier(task, outDir) {
			t.Fatal("expected skip for pure-new mechanical deliverable")
		}
	})

	t.Run("empty out does not skip", func(t *testing.T) {
		task := &Task{ID: "t1b", UseDW: false, VerifierRole: "", Workdir: t.TempDir()}
		if eng.shouldSkipVerifier(task, t.TempDir()) {
			t.Fatal("expected verify when worker produced no deliverable")
		}
	})

	t.Run("touches existing does not skip", func(t *testing.T) {
		outDir := t.TempDir()
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "game.js"), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "game.js"), []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t2", UseDW: false, VerifierRole: "", Workdir: workdir}
		if eng.shouldSkipVerifier(task, outDir) {
			t.Fatal("expected verify when deliverable touches existing file")
		}
	})

	t.Run("verifier role does not skip", func(t *testing.T) {
		outDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(outDir, "game.js"), []byte("// x"), 0644); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: "t3", UseDW: false, VerifierRole: "architect", Workdir: t.TempDir()}
		if eng.shouldSkipVerifier(task, outDir) {
			t.Fatal("expected verify for key role even when pure-new")
		}
	})

	t.Run("DW does not skip", func(t *testing.T) {
		task := &Task{ID: "t4", UseDW: true, VerifierRole: "", Workdir: t.TempDir()}
		if eng.shouldSkipVerifier(task, t.TempDir()) {
			t.Fatal("expected verify for DW tasks")
		}
	})
}
