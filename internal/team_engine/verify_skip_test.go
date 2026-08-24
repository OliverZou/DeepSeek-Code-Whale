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
