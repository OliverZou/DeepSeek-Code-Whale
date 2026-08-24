package team_engine

import (
	"context"
	"strings"
	"testing"
)

// passVerdict is a verifier output that parseVerdict reads as PASS. It is long
// enough (>= 100 chars, "TOOLS USED:" followed by a real tool name) to avoid
// isLazyVerdict re-spawning the verifier.
const passVerdict = "TOOLS USED: read_file (inspected output), list_dir (verified files present), shell_run (ran tests)\n" +
	"VERDICT: PASS\n" +
	"EVIDENCE: The worker output is complete and correct; the deliverable implements every requirement.\n" +
	"ISSUES:\n- none\n\n" +
	"## FINDINGS (structured JSON)\n---json\n[]\n---\n"

// failVerdict is a verifier output that parseVerdict reads as FAIL with one
// non-empty finding, forcing a retry. (An empty FINDINGS array auto-passes, so
// the FAIL must carry at least one finding to actually trigger a retry.)
const failVerdict = "TOOLS USED: read_file (inspected output)\n" +
	"VERDICT: FAIL\n" +
	"EVIDENCE: The implementation is missing the required function.\n" +
	"ISSUES:\n- missing required function\n\n" +
	"## FINDINGS (structured JSON)\n---json\n[{\"id\":\"f1\",\"title\":\"missing function\",\"severity\":\"major\",\"evidence\":\"not found in output\"}]\n---\n"

func newRunTaskEngine(t *testing.T, spawner SubagentSpawner) *TeamEngine {
	t.Helper()
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

func TestRunTaskPassesEndToEnd(t *testing.T) {
	spawner := &mockSpawner{
		roleOutputs: map[string]string{
			"worker":   "implemented feature",
			"verifier": passVerdict,
		},
	}
	eng := newRunTaskEngine(t, spawner)
	defer eng.Close()

	task, err := eng.CreateTask("Build feature", "Write the feature", RoleDeveloper, "", nil, 1, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ok, err := eng.RunTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if !ok {
		t.Fatalf("expected task to reach done, got ok=false")
	}

	got, err := eng.Store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.State != TaskStateDone {
		t.Fatalf("expected done, got %s", got.State)
	}
}

func TestRunTaskVerifierFailThenRetry(t *testing.T) {
	spawner := &mockSpawner{
		roleSeq: map[string][]string{
			"verifier": {failVerdict, passVerdict},
		},
		roleOutputs: map[string]string{
			"worker": "implemented feature",
		},
	}
	eng := newRunTaskEngine(t, spawner)
	defer eng.Close()

	task, err := eng.CreateTask("Build feature", "Write the feature", RoleDeveloper, "", nil, 2, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ok, err := eng.RunTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if !ok {
		t.Fatalf("expected task to pass on retry, got ok=false")
	}

	got, _ := eng.Store.GetTask(task.ID)
	if got.State != TaskStateDone {
		t.Fatalf("expected done after retry, got %s", got.State)
	}
	if got.RetryCount != 1 {
		t.Fatalf("expected 1 retry, got %d", got.RetryCount)
	}
}

func TestRunTaskRetryExhaustedSuspends(t *testing.T) {
	spawner := &mockSpawner{
		roleOutputs: map[string]string{
			"worker":   "incomplete feature",
			"verifier": failVerdict,
		},
	}
	eng := newRunTaskEngine(t, spawner)
	defer eng.Close()

	task, err := eng.CreateTask("Build feature", "Write the feature", RoleDeveloper, "", nil, 1, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ok, err := eng.RunTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if ok {
		t.Fatalf("expected task to suspend, got ok=true")
	}

	got, _ := eng.Store.GetTask(task.ID)
	if got.State != TaskStateSuspended {
		t.Fatalf("expected suspended, got %s", got.State)
	}
}

func TestRunTaskWorktreeMergeCleanup(t *testing.T) {
	repo := initGitRepo(t)
	spawner := &mockSpawner{
		roleOutputs: map[string]string{
			"worker":   "implemented in worktree",
			"verifier": passVerdict,
		},
	}
	eng := newRunTaskEngine(t, spawner)
	eng.EnableWorktree(repo)
	defer eng.Close()

	task, err := eng.CreateTask("Worktree task", "Write code in an isolated worktree", RoleDeveloper, "", nil, 1, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ok, err := eng.RunTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if !ok {
		t.Fatalf("expected worktree task to reach done, got ok=false")
	}

	// Merge succeeded, so the worktree branch and activeTrees entry must be gone.
	if got := eng.activeBranch(task.ID); got != "" {
		t.Fatalf("activeBranch after merge = %q, want empty", got)
	}
	branch := "team-" + task.ID[:8]
	if out := strings.TrimSpace(runGit(t, repo, "branch", "--list", branch)); out != "" {
		t.Fatalf("branch %s should be deleted after merge, got %q", branch, out)
	}
}
