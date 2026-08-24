package app

import (
	"reflect"
	"strings"
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

func TestStripWorkbuddySections(t *testing.T) {
	cases := []struct {
		name    string
		prompt  string
		want    []string // substrings that must remain
		notWant []string // substrings that must be gone
	}{
		{
			name: "engineer keeps coding standards, drops input handoff and collaboration",
			prompt: `## Core Identity
- Role: Engineer

## Input

You will receive a Design Doc.

## Coding Process

Write code.

## Code Writing Standards

COMPLETE CODE.

## 团队协作（回传机制）

Use SendMessage.`,
			want:    []string{"## Core Identity", "## Coding Process", "## Code Writing Standards"},
			notWant: []string{"## Input", "You will receive", "团队协作", "SendMessage"},
		},
		{
			name: "team-lead truncates orchestration protocol after the collaboration section",
			prompt: `# 主理人

You orchestrate.

## 团队成员

| 成员 | 职责 |

## 团队协作机制（铁律）

TeamCreate 铁律。

## 工作流路由（CRITICAL）

快速模式。

## ⚡ 快速模式

Skip PRD.`,
			want:    []string{"# 主理人", "## 团队成员"},
			notWant: []string{"团队协作机制", "TeamCreate", "工作流路由", "快速模式"},
		},
		{
			name: "qa keeps testing standards, drops smart routing and round control",
			prompt: `## Testing Process

### 1. Analyze

Read code.

### 2. Write Test Cases

Write tests.

### 3. Run Tests and Smart Routing

#### Smart Routing Decision

Send To: Engineer.

## Test Round Control

MAX 2 ROUNDS.

## Test Writing Standards

Arrange-Act-Assert.

## Test Report Format

# Test Report.

## 团队协作

SendMessage.`,
			want:    []string{"## Testing Process", "### 1. Analyze", "### 2. Write Test Cases", "## Test Writing Standards"},
			notWant: []string{"### 3. Run Tests", "Smart Routing", "Test Round Control", "Test Report Format", "团队协作", "SendMessage"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripWorkbuddySections(tc.prompt)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("stripWorkbuddySections should keep %q, got:\n%s", w, got)
				}
			}
			for _, n := range tc.notWant {
				if strings.Contains(got, n) {
					t.Errorf("stripWorkbuddySections should drop %q, got:\n%s", n, got)
				}
			}
		})
	}
}
