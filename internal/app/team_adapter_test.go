package app

import (
	"reflect"
	"testing"

	"github.com/usewhale/whale/internal/tasks"
)

func TestPermissionForRole(t *testing.T) {
	// verifier is intentionally auto (not read_only): it needs shell.run to
	// execute tests/linters for tool-grounded verification. Its toolset is
	// still constrained to the verify profile (read + shell, no write) in the
	// adapter fallback.
	readOnly := []string{"planner", "reviewer", "researcher", "evaluator", "synthesizer"}
	for _, role := range readOnly {
		if got := permissionForRole(role); got != tasks.AgentPermissionReadOnly {
			t.Errorf("permissionForRole(%q) = %q, want read_only", role, got)
		}
	}
	for _, role := range []string{"worker", "frontend-dev", "developer", "verifier", ""} {
		if got := permissionForRole(role); got != tasks.AgentPermissionAuto {
			t.Errorf("permissionForRole(%q) = %q, want auto", role, got)
		}
	}
}

func TestTeamToolsToCapabilities(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  []string
	}{
		{
			name:  "default profile collapses to read+write+shell",
			tools: []string{"read_file", "list_dir", "grep", "search_files", "edit", "write", "apply_patch", "shell_run", "shell_wait", "shell_cancel", "write_stdin"},
			want:  []string{"shell.run", "terminal.write", "workspace.read", "workspace.write"},
		},
		{
			name:  "read-only profile maps web + read",
			tools: []string{"read_file", "list_dir", "grep", "search_files", "web_search", "web_fetch", "fetch"},
			want:  []string{"web.fetch", "web.search", "workspace.read"},
		},
		{
			name:  "verify profile keeps shell but not write",
			tools: []string{"read_file", "list_dir", "grep", "search_files", "shell_run", "shell_wait", "shell_cancel", "web_search", "web_fetch", "fetch"},
			want:  []string{"shell.run", "web.fetch", "web.search", "workspace.read"},
		},
		{
			name:  "apply_patch collapses into workspace.write",
			tools: []string{"apply_patch"},
			want:  []string{"workspace.write"},
		},
		{
			name:  "unknown tools are ignored",
			tools: []string{"bogus_tool"},
			want:  nil,
		},
		{
			name:  "empty input",
			tools: nil,
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := teamToolsToCapabilities(tc.tools)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("teamToolsToCapabilities(%v) = %v, want %v", tc.tools, got, tc.want)
			}
		})
	}
}
