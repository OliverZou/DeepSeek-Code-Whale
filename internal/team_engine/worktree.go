package team_engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnableWorktree activates git worktree-based isolation for coding tasks.
// repoPath should be the root of the git repository.
func (e *TeamEngine) EnableWorktree(repoPath string) {
	e.worktreeEnabled = true
	e.worktreeDir = repoPath
}

// createWorktree creates a git worktree for a coding task.
// Returns the worktree path and branch name.
func (e *TeamEngine) createWorktree(taskID string) (string, string, error) {
	if !e.worktreeEnabled || e.worktreeDir == "" {
		return "", "", fmt.Errorf("worktree not enabled")
	}
	shortID := taskID[:8]
	branchName := "team-" + shortID
	treePath := filepathJoin(e.worktreeDir, ".whale", "worktrees", branchName)

	// Use `git worktree add` directly.
	cmd := exec.Command("git", "worktree", "add", treePath, "-b", branchName)
	cmd.Dir = e.worktreeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Try without -b (branch may already exist).
		cmd2 := exec.Command("git", "worktree", "add", treePath, branchName)
		cmd2.Dir = e.worktreeDir
		out, err = cmd2.CombinedOutput()
		if err != nil {
			return "", "", fmt.Errorf("git worktree add: %s: %w", string(out), err)
		}
	}

	e.mu.Lock()
	e.activeTrees[taskID] = branchName
	e.mu.Unlock()
	return treePath, branchName, nil
}

// collectWorktreeDiff generates a git diff for a coding task's worktree.
// collectWorktreeDiff generates a git diff for a coding task's worktree.
// Call after commitWorktree so the diff captures the committed worker changes.
func (e *TeamEngine) collectWorktreeDiff(taskID string) string {
	branch := e.activeBranch(taskID)
	if branch == "" || e.worktreeDir == "" {
		return ""
	}

	// Diff the worktree branch against the main repo's current HEAD (the base
	// the worktree was created from). The team engine merges worktrees
	// serially, so the main HEAD is still the original base here.
	baseCmd := exec.Command("git", "rev-parse", "HEAD")
	baseCmd.Dir = e.worktreeDir
	baseOut, err := baseCmd.Output()
	if err != nil {
		return ""
	}
	base := strings.TrimSpace(string(baseOut))

	cmd := exec.Command("git", "diff", base+".."+branch)
	cmd.Dir = e.worktreeDir
	out, _ := cmd.Output()
	diff := string(out)

	if strings.TrimSpace(diff) == "" {
		return "(no diff — no changes detected)"
	}
	return diff
}

// cleanupWorktree removes a worktree directory and branch.
func (e *TeamEngine) cleanupWorktree(taskID string) {
	e.mu.Lock()
	branch, ok := e.activeTrees[taskID]
	delete(e.activeTrees, taskID)
	e.mu.Unlock()

	if !ok || branch == "" || e.worktreeDir == "" {
		return
	}

	treePath := filepathJoin(e.worktreeDir, ".whale", "worktrees", branch)
	// Remove the worktree.
	cmd := exec.Command("git", "worktree", "remove", treePath, "--force")
	cmd.Dir = e.worktreeDir
	cmd.Run()
	// Remove the branch.
	delCmd := exec.Command("git", "branch", "-D", branch)
	delCmd.Dir = e.worktreeDir
	_ = delCmd.Run()
}

// activeBranch returns the worktree branch for a task without consuming the
// entry. Used for reuse detection, diff, merge, and cleanup.
func (e *TeamEngine) activeBranch(taskID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeTrees[taskID]
}

// commitWorktree commits the worker's working-tree changes in its isolated
// worktree so they can be diffed and merged as a real commit. Returns nil when
// there is nothing to commit. --no-verify skips untrusted pre-commit hooks
// (see AGENTS.md: hooks are untrusted input that may run shell commands).
func (e *TeamEngine) commitWorktree(taskID string) error {
	branch := e.activeBranch(taskID)
	if branch == "" || e.worktreeDir == "" {
		return nil
	}
	wtPath := filepath.Join(e.worktreeDir, ".whale", "worktrees", branch)

	add := exec.Command("git", "add", "-A")
	add.Dir = wtPath
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s: %w", string(out), err)
	}

	commit := exec.Command("git", "commit", "-m", "team task "+taskID[:8], "--no-verify")
	commit.Dir = wtPath
	out, err := commit.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "nothing to commit") {
		return fmt.Errorf("git commit: %s: %w", string(out), err)
	}
	return nil
}

// mergeWorktree merges the task's worktree branch back into the main repo.
// Uses --no-ff so the merge commit keeps the team task traceable. Returns an
// error on conflict/failure; the caller decides whether to abort.
func (e *TeamEngine) mergeWorktree(taskID string) error {
	branch := e.activeBranch(taskID)
	if branch == "" || e.worktreeDir == "" {
		return nil
	}
	cmd := exec.Command("git", "merge", branch, "--no-ff", "--no-edit", "-m", "team task "+taskID[:8])
	cmd.Dir = e.worktreeDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git merge: %s: %w", string(out), err)
	}
	return nil
}

// isWorktreeEligibleRole returns true if the role modifies code and benefits from worktree isolation.
func isWorktreeEligibleRole(role AgentRole) bool {
	switch role {
	case RoleDeveloper, RoleTester:
		return true
	default:
		return false
	}
}

// filepathJoin joins path elements with the OS-specific separator.
func filepathJoin(elem ...string) string {
	return strings.Join(elem, string(os.PathSeparator))
}
