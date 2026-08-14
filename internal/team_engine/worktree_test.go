package team_engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the worktree git operations directly against a throwaway
// git repository. They do not depend on a spawner — only on git being present.

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// initGitRepo creates a fresh git repository with a base commit so HEAD exists.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "team-engine-test")
	writeFile(t, filepath.Join(dir, "base.txt"), "base\n")
	runGit(t, dir, "add", "base.txt")
	runGit(t, dir, "commit", "-m", "base commit")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newWorktreeEngine returns a minimal engine with worktree isolation enabled.
// The worktree git functions only touch worktreeEnabled/worktreeDir/activeTrees,
// so no Store/Whiteboard/Loggers setup is required.
func newWorktreeEngine(repo string) *TeamEngine {
	e := &TeamEngine{activeTrees: make(map[string]string)}
	e.EnableWorktree(repo)
	return e
}

func TestCommitAndMergeWorktree(t *testing.T) {
	repo := initGitRepo(t)
	e := newWorktreeEngine(repo)

	const taskID = "task0000000000000001"
	treePath, branch, err := e.createWorktree(taskID)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if branch == "" || treePath == "" {
		t.Fatalf("createWorktree returned empty branch/path")
	}
	defer e.cleanupWorktree(taskID)

	writeFile(t, filepath.Join(treePath, "feature.txt"), "feature\n")

	if err := e.commitWorktree(taskID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := e.mergeWorktree(taskID); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// The merged change must land in the main working tree.
	data, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
	if err != nil {
		t.Fatalf("merged file missing from main repo: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "feature" {
		t.Fatalf("merged content = %q, want %q", got, "feature")
	}
}

func TestCollectWorktreeDiff(t *testing.T) {
	repo := initGitRepo(t)
	e := newWorktreeEngine(repo)

	const taskID = "task0000000000000002"
	treePath, _, err := e.createWorktree(taskID)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	defer e.cleanupWorktree(taskID)

	writeFile(t, filepath.Join(treePath, "diff.txt"), "diff content here\n")
	if err := e.commitWorktree(taskID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	diff := e.collectWorktreeDiff(taskID)
	if !strings.Contains(diff, "diff content here") {
		t.Fatalf("diff missing committed content: %q", diff)
	}
}

func TestCleanupWorktree(t *testing.T) {
	repo := initGitRepo(t)
	e := newWorktreeEngine(repo)

	const taskID = "task0000000000000003"
	treePath, branch, err := e.createWorktree(taskID)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	writeFile(t, filepath.Join(treePath, "x.txt"), "x\n")
	if err := e.commitWorktree(taskID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	e.cleanupWorktree(taskID)

	if got := e.activeBranch(taskID); got != "" {
		t.Fatalf("activeBranch after cleanup = %q, want empty", got)
	}
	if _, err := os.Stat(treePath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be removed, stat err = %v", err)
	}
	if out := strings.TrimSpace(runGit(t, repo, "branch", "--list", branch)); out != "" {
		t.Fatalf("branch %s should be deleted, got %q", branch, out)
	}
}

func TestMergeConflictKeepsWorktree(t *testing.T) {
	repo := initGitRepo(t)
	// Add a conflict file to the base before branching.
	writeFile(t, filepath.Join(repo, "conflict.txt"), "base\n")
	runGit(t, repo, "add", "conflict.txt")
	runGit(t, repo, "commit", "-m", "add conflict file")

	e := newWorktreeEngine(repo)

	const taskID = "task0000000000000004"
	treePath, branch, err := e.createWorktree(taskID)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	// Worker edits the same file in the worktree.
	writeFile(t, filepath.Join(treePath, "conflict.txt"), "worktree change\n")
	if err := e.commitWorktree(taskID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Meanwhile main diverges on the same line.
	writeFile(t, filepath.Join(repo, "conflict.txt"), "main change\n")
	runGit(t, repo, "add", "conflict.txt")
	runGit(t, repo, "commit", "-m", "main change")

	if err := e.mergeWorktree(taskID); err == nil {
		t.Fatalf("expected merge conflict, got nil")
	}

	// Simulate the engine's abort path: restore a clean main tree.
	runGit(t, repo, "merge", "--abort")

	data, err := os.ReadFile(filepath.Join(repo, "conflict.txt"))
	if err != nil {
		t.Fatalf("read main conflict.txt: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "main change" {
		t.Fatalf("main not restored after abort: %q", got)
	}

	// The worktree branch and directory must be preserved for manual handling.
	if got := e.activeBranch(taskID); got != branch {
		t.Fatalf("activeBranch after conflict = %q, want %q", got, branch)
	}
	if _, err := os.Stat(treePath); err != nil {
		t.Fatalf("worktree dir should be preserved after conflict: %v", err)
	}
}
