package team_engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyDirSkipsWhaleState reproduces bug #2: a worker subprocess runs inside
// a sandbox out/ dir, where its App init (app_new.go) calls
// SetLogger(NewTeamLog(workspaceRoot)), creating an empty
// .whale/team_tasks/logs/team_engine.log. When propagateTaskOutput copies out/
// back to the workspace, copyDir must NOT overwrite the master's team_engine.log
// (which holds the full run timeline) with the sandbox's empty one.
func TestCopyDirSkipsWhaleState(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Worker sandbox: empty team_engine.log + a real deliverable.
	sandboxLog := filepath.Join(src, ".whale", "team_tasks", "logs", "team_engine.log")
	if err := os.MkdirAll(filepath.Dir(sandboxLog), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sandboxLog, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "game.js"), []byte("// deliverable"), 0644); err != nil {
		t.Fatal(err)
	}

	// Master workspace: team_engine.log with prior timeline content.
	masterLog := filepath.Join(dst, ".whale", "team_tasks", "logs", "team_engine.log")
	if err := os.MkdirAll(filepath.Dir(masterLog), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masterLog, []byte("[batch] 1 done\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	// Deliverable must be propagated.
	if _, err := os.Stat(filepath.Join(dst, "game.js")); err != nil {
		t.Fatalf("deliverable not copied: %v", err)
	}

	// Master's team_engine.log must be untouched.
	got, err := os.ReadFile(masterLog)
	if err != nil {
		t.Fatalf("read master log: %v", err)
	}
	if string(got) != "[batch] 1 done\n" {
		t.Fatalf("master team_engine.log was clobbered: got %q", string(got))
	}
}
