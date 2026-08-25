package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/usewhale/whale/internal/core"
)

// newAllowlistSC builds a streamDispatchContext with a backed event channel,
// matching the read-before-edit gate tests.
func newAllowlistSC() *streamDispatchContext {
	return &streamDispatchContext{Events: make(chan AgentEvent, 8)}
}

func TestWithWriteAllowlist(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{workspaceRoot: dir}
	WithWriteAllowlist([]string{"game.js", "game.test.js"}, os.TempDir())(a)

	if a.writeAllowlist == nil {
		t.Fatal("allowlist must be non-nil after WithWriteAllowlist")
	}
	if !a.writeAllowlist[normalizeAllowlistPath("game.js", dir)] {
		t.Fatal("expected game.js in allowlist")
	}
	if !a.writeAllowlist[normalizeAllowlistPath("game.test.js", dir)] {
		t.Fatal("expected game.test.js in allowlist")
	}
	if len(a.writeExemptDirs) != 1 {
		t.Fatalf("expected 1 exempt dir, got %d", len(a.writeExemptDirs))
	}
}

func TestWithWriteAllowlistEmptyDisables(t *testing.T) {
	a := &Agent{}
	WithWriteAllowlist(nil)(a)
	if a.writeAllowlist != nil {
		t.Fatal("empty allowlist must disable the gate")
	}
}

func TestCheckWriteAllowlistGate(t *testing.T) {
	dir := t.TempDir()
	// An unrelated file that must NOT be writable by this worker.
	os.WriteFile(filepath.Join(dir, "other.js"), []byte("// other task's file\n"), 0644)
	// A dedicated exempt dir, kept OUTSIDE dir so the workspace files never
	// accidentally fall inside it (t.TempDir() lives under os.TempDir()).
	exempt := filepath.Join(t.TempDir(), "exempt")

	t.Run("declared output allowed", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		WithWriteAllowlist([]string{"game.js"}, exempt)(a)
		var results []core.ToolResult
		if a.checkWriteAllowlistGate(context.Background(), newAllowlistSC(), tc("edit", map[string]any{"file_path": "game.js"}), &results) {
			t.Fatal("writing own declared output must be allowed")
		}
	})
	t.Run("other task file blocked", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		WithWriteAllowlist([]string{"game.js"}, exempt)(a)
		var results []core.ToolResult
		if !a.checkWriteAllowlistGate(context.Background(), newAllowlistSC(), tc("write", map[string]any{"file_path": "other.js"}), &results) {
			t.Fatal("writing another task's file must be blocked")
		}
		if results[len(results)-1].Code != "write_not_in_allowlist" {
			t.Fatalf("wrong code %q", results[len(results)-1].Code)
		}
	})
	t.Run("exempt temp dir allowed", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		WithWriteAllowlist([]string{"game.js"}, exempt)(a)
		var results []core.ToolResult
		exemptFile := filepath.Join(exempt, "verify_game.js")
		if a.checkWriteAllowlistGate(context.Background(), newAllowlistSC(), tc("write", map[string]any{"file_path": exemptFile}), &results) {
			t.Fatal("writing to the exempt dir must be allowed")
		}
	})
	t.Run("no allowlist means no restriction", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		var results []core.ToolResult
		if a.checkWriteAllowlistGate(context.Background(), newAllowlistSC(), tc("write", map[string]any{"file_path": "other.js"}), &results) {
			t.Fatal("no allowlist must never block")
		}
	})
}

func TestCheckShellWriteGate(t *testing.T) {
	dir := t.TempDir()

	t.Run("read-only shell allowed", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		WithWriteAllowlist([]string{"game.js"}, os.TempDir())(a)
		var results []core.ToolResult
		if a.checkShellWriteGate(context.Background(), newAllowlistSC(), tc("shell_run", map[string]any{"command": "cat game.js"}), &results) {
			t.Fatal("read-only shell must be allowed")
		}
	})
	t.Run("redirection shell blocked", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		WithWriteAllowlist([]string{"game.js"}, os.TempDir())(a)
		var results []core.ToolResult
		if !a.checkShellWriteGate(context.Background(), newAllowlistSC(), tc("shell_run", map[string]any{"command": "echo x > other.js"}), &results) {
			t.Fatal("shell that writes must be blocked")
		}
		if results[len(results)-1].Code != "shell_write_not_allowed" {
			t.Fatalf("wrong code %q", results[len(results)-1].Code)
		}
	})
	t.Run("verification shell allowed", func(t *testing.T) {
		// node --check / node <file> / git diff are how a worker verifies its
		// deliverable; blocking them used to burn ~9 tool rounds and explode the
		// prompt budget via history replay. They must be allowed.
		a := &Agent{workspaceRoot: dir}
		WithWriteAllowlist([]string{"game.js"}, os.TempDir())(a)
		var results []core.ToolResult
		for _, cmd := range []string{"node --check game.js", "node game.test.js", "git diff", "python --version"} {
			if a.checkShellWriteGate(context.Background(), newAllowlistSC(), tc("shell_run", map[string]any{"command": cmd}), &results) {
				t.Fatalf("verification shell %q must be allowed", cmd)
			}
		}
	})
	t.Run("no allowlist means shell unrestricted", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir}
		var results []core.ToolResult
		if a.checkShellWriteGate(context.Background(), newAllowlistSC(), tc("shell_run", map[string]any{"command": "echo x > other.js"}), &results) {
			t.Fatal("no allowlist must never block shell")
		}
	})
}
