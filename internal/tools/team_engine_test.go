package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/team_engine"
)

// teamEngineTestHelper creates a Toolset with a temp workspace
// and pre-creates the .whale directory so the SQLite DB can be created.
// Returns a cleanup function that should be called at the end of the test.
func teamEngineTestHelper(t *testing.T) (*Toolset, context.Context, func()) {
	t.Helper()
	dir := t.TempDir()
	ts, err := NewToolset(dir)
	if err != nil {
		t.Fatalf("new toolset: %v", err)
	}
	// Pre-create .whale dir so NewDB can write the file.
	if err := os.MkdirAll(filepath.Join(ts.root, ".whale"), 0755); err != nil {
		t.Fatalf("mkdir .whale: %v", err)
	}
	// Return a cleanup that forces SQLite connections closed before TempDir
	// removal.  We do NOT call ts.Close — it has no Close method — but we
	// delete the .whale directory eagerly so TempDir does not trip on locked
	// WAL files on Windows.
	cleanup := func() {
		os.RemoveAll(filepath.Join(dir, ".whale"))
	}
	return ts, context.Background(), cleanup
}

// fullUUIDLen is the standard UUID v4 string length (36 chars).
const fullUUIDLen = 36

// isFullUUID reports whether s looks like a standard UUID v4 string.
func isFullUUID(s string) bool {
	if len(s) != fullUUIDLen {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// teamCreateTool — verify the returned ID is the full UUID, not truncated
// ---------------------------------------------------------------------------

func TestTeamCreateTool_ReturnsFullUUID(t *testing.T) {
	ts, ctx, cleanup := teamEngineTestHelper(t)
	defer cleanup()

	tool := ts.teamCreateTool()
	result, err := tool.fn(ctx, core.ToolCall{
		ID:   "1",
		Name: "team_create",
		Input: `{
			"title": "Test Task",
			"description": "A test task for UUID verification",
			"role": "developer"
		}`,
	})
	if err != nil {
		t.Fatalf("team_create failed: %v", err)
	}
	if result.Outcome == core.OutcomeFailure {
		t.Fatalf("team_create returned error: %s", result.ModelText)
	}

	// Output format: "Created task {UUID}: {title} [{state}]"
	// The UUID ends before ": ".
	const prefix = "Created task "
	if !strings.HasPrefix(result.ModelText, prefix) {
		t.Fatalf("unexpected output format: %s", result.ModelText)
	}
	rest := strings.TrimPrefix(result.ModelText, prefix)
	colonIdx := strings.Index(rest, ": ")
	if colonIdx < 0 {
		t.Fatalf("cannot find colon separator in: %s", result.ModelText)
	}
	id := rest[:colonIdx]

	if !isFullUUID(id) {
		t.Errorf("team_create returned truncated ID (%q, len=%d); want full 36-char UUID",
			id, len(id))
	} else {
		t.Logf("Got full UUID: %s", id)
	}
}

// ---------------------------------------------------------------------------
// teamListTool — verify listed task IDs are full UUIDs
// ---------------------------------------------------------------------------

func TestTeamListTool_ShowsFullUUID(t *testing.T) {
	ts, ctx, cleanup := teamEngineTestHelper(t)
	defer cleanup()

	// Create a task first so list has something to show.
	createTool := ts.teamCreateTool()
	cr, err := createTool.fn(ctx, core.ToolCall{
		ID:   "1",
		Name: "team_create",
		Input: `{
			"title": "List Test",
			"description": "Task for list UUID test",
			"role": "developer"
		}`,
	})
	if err != nil || cr.Outcome == core.OutcomeFailure {
		t.Fatalf("create failed: %v / %s", err, cr.ModelText)
	}

	// Now list
	listTool := ts.teamListTool()
	result, err := listTool.fn(ctx, core.ToolCall{ID: "2", Name: "team_list", Input: `{}`})
	if err != nil {
		t.Fatalf("team_list failed: %v", err)
	}
	if result.Outcome == core.OutcomeFailure {
		t.Fatalf("team_list returned error: %s", result.ModelText)
	}

	// Every line should have a full UUID visible (not truncated).
	lines := strings.Split(strings.TrimSpace(result.ModelText), "\n")
	if len(lines) == 0 {
		t.Fatal("team_list returned no output")
	}
	for i, line := range lines {
		// skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Format: "- {full-uuid} [{state}] {progress}% {title}"
		// Find the first space after the leading "- " to locate the ID.
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "+ ") {
			continue
		}
		afterDash := strings.TrimLeft(line, "-+ ")
		// The ID is the next token (until the first space after it).
		parts := strings.SplitN(afterDash, " ", 2)
		if len(parts) < 2 {
			continue
		}
		id := parts[0]
		if !isFullUUID(id) {
			t.Errorf("line %d: ID %q (len=%d) is not a full UUID; line=%q",
				i, id, len(id), line)
		}
	}
}

// ---------------------------------------------------------------------------
// teamStatusTool — verify full round-trip: create → status with full UUID
// ---------------------------------------------------------------------------

func TestTeamStatusTool_WithFullUUID(t *testing.T) {
	ts, ctx, cleanup := teamEngineTestHelper(t)
	defer cleanup()

	// Create a task and capture its ID.
	createTool := ts.teamCreateTool()
	cr, err := createTool.fn(ctx, core.ToolCall{
		ID:   "1",
		Name: "team_create",
		Input: `{
			"title": "Status Test",
			"description": "Task for status UUID test",
			"role": "developer"
		}`,
	})
	if err != nil || cr.Outcome == core.OutcomeFailure {
		t.Fatalf("create failed: %v / %s", err, cr.ModelText)
	}
	id := extractIDFromCreateOutput(t, cr.ModelText)

	// Query status with the full UUID.
	statusTool := ts.teamStatusTool()
	result, err := statusTool.fn(ctx, core.ToolCall{
		ID:   "2",
		Name: "team_status",
		Input: `{"task_id": "` + id + `"}`,
	})
	if err != nil {
		t.Fatalf("team_status failed: %v", err)
	}
	if result.Outcome == core.OutcomeFailure {
		t.Fatalf("team_status returned error: %s", result.ModelText)
	}

	// The status output should start with "Task: {full-uuid}".
	taskLine := firstLine(result.ModelText)
	if !strings.Contains(taskLine, id) {
		t.Errorf("status output missing full UUID %q; first line: %q", id, taskLine)
	} else {
		t.Logf("Status found task: %s", taskLine)
	}
}

// ---------------------------------------------------------------------------
// teamStatusTool — verify that short (truncated) ID returns "not found"
// ---------------------------------------------------------------------------

func TestTeamStatusTool_WithShortID_ReturnsNotFound(t *testing.T) {
	ts, ctx, cleanup := teamEngineTestHelper(t)
	defer cleanup()

	statusTool := ts.teamStatusTool()
	result, err := statusTool.fn(ctx, core.ToolCall{
		ID:   "1",
		Name: "team_status",
		Input: `{"task_id": "aaaaaaaa"}`,
	})
	if err != nil {
		t.Fatalf("team_status failed: %v", err)
	}
	if !result.IsError() {
		t.Fatal("expected error for short/non-existent ID, but got success")
	}
	if !strings.Contains(result.ModelText, "not found") {
		t.Errorf("expected 'not found' error, got: %s", result.ModelText)
	}
}

// ---------------------------------------------------------------------------
// teamRunTool — verify full round-trip: create → run with full UUID
// ---------------------------------------------------------------------------

func TestTeamRunTool_WithFullUUID(t *testing.T) {
	ts, ctx, cleanup := teamEngineTestHelper(t)
	defer cleanup()

	// Install a mock spawner so team_run does not launch a real shell process
	// (which would hold a Windows file lock on the DB during cleanup).
	ts.SetTeamEngineSpawnFunc(func(ctx context.Context, req team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
		return team_engine.SubagentResponse{Output: "mock ok", Success: true, ExitCode: 0}, nil
	})

	// Create a task and capture its ID.
	createTool := ts.teamCreateTool()
	cr, err := createTool.fn(ctx, core.ToolCall{
		ID:   "1",
		Name: "team_create",
		Input: `{
			"title": "Run Test",
			"description": "Simple echo task",
			"role": "developer"
		}`,
	})
	if err != nil || cr.Outcome == core.OutcomeFailure {
		t.Fatalf("create failed: %v / %s", err, cr.ModelText)
	}
	id := extractIDFromCreateOutput(t, cr.ModelText)

	// Run with full UUID — should resolve the task (no "not found").
	runTool := ts.teamRunTool()
	result, err := runTool.fn(ctx, core.ToolCall{
		ID:   "2",
		Name: "team_run",
		Input: `{"task_id": "` + id + `"}`,
	})
	if err != nil {
		t.Fatalf("team_run call failed: %v", err)
	}
	if strings.Contains(result.ModelText, "not found") {
		t.Errorf("team_run reported 'not found' for valid UUID; output: %s", result.ModelText)
	}
	t.Logf("team_run result: %s", result.ModelText)
}

// ---------------------------------------------------------------------------
// teamRunTool — verify short ID returns "not found"
// ---------------------------------------------------------------------------

func TestTeamRunTool_WithShortID_ReturnsNotFound(t *testing.T) {
	ts, ctx, cleanup := teamEngineTestHelper(t)
	defer cleanup()

	runTool := ts.teamRunTool()
	result, err := runTool.fn(ctx, core.ToolCall{
		ID:   "1",
		Name: "team_run",
		Input: `{"task_id": "bbbbbbbb"}`,
	})
	if err != nil {
		t.Fatalf("team_run call failed: %v", err)
	}
	if !result.IsError() {
		t.Fatal("expected error for short/non-existent ID, but got success")
	}
	if !strings.Contains(result.ModelText, "not found") {
		t.Errorf("expected 'not found' error, got: %s", result.ModelText)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractIDFromCreateOutput(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Created task "
	if !strings.HasPrefix(output, prefix) {
		t.Fatalf("unexpected create output format: %s", output)
	}
	rest := strings.TrimPrefix(output, prefix)
	// Format: "{UUID}: {title} [{state}]" — the UUID ends before ": ".
	colonIdx := strings.Index(rest, ": ")
	if colonIdx < 0 {
		t.Fatalf("cannot find colon separator in: %s", output)
	}
	id := rest[:colonIdx]
	if !isFullUUID(id) {
		t.Fatalf("extracted ID %q is not a full UUID from: %s", id, output)
	}
	return id
}

func firstLine(s string) string {
	idx := strings.Index(s, "\n")
	if idx < 0 {
		return s
	}
	return s[:idx]
}
