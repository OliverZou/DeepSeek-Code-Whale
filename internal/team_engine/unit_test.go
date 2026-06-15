package team_engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// DB operations
// ============================================================================

func TestDeleteTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("ToDelete", "Will be deleted", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err := eng.DeleteTask(task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	// Verify it's gone.
	got, _ := eng.DB.GetTask(task.ID)
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestDeleteMasterTaskCascades(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	mt, _ := eng.CreateMasterTask("cascade test", "/tmp")
	// Create 3 subtasks.
	for i := 0; i < 3; i++ {
		task, _ := eng.CreateTask("Sub", "subtask", RoleDeveloper, "", nil, 0, ".", "", "", "")
		task.MasterTaskID = mt.ID
		eng.DB.UpdateTask(task.ID, map[string]interface{}{"master_task_id": mt.ID})
	}

	// Delete the master task.
	if err := eng.DeleteMasterTask(mt.ID); err != nil {
		t.Fatalf("delete master task: %v", err)
	}

	// All subtasks should be gone.
	tasks, _ := eng.DB.ListTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 subtasks after cascade delete, got %d", len(tasks))
	}
	// Master task itself should be gone.
	got, _ := eng.DB.GetMasterTask(mt.ID)
	if got != nil {
		t.Error("master task should be deleted")
	}
}

func TestForceTransitionState(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Force", "Force transition", RoleDeveloper, "", nil, 0, ".", "", "", "")

	// Normal CanTransition from PENDING to DONE is not valid.
	if CanTransition(TaskStatePending, TaskStateDone) {
		t.Skip("PENDING→DONE is now valid, skipping force test")
	}

	// ForceTransitionState bypasses the transition table.
	if err := eng.DB.ForceTransitionState(task.ID, TaskStateDone, "forced"); err != nil {
		t.Fatalf("force transition: %v", err)
	}
	updated, _ := eng.DB.GetTask(task.ID)
	if updated.State != TaskStateDone {
		t.Errorf("expected DONE, got %s", updated.State)
	}
}

func TestGetTaskNonexistent(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.DB.GetTask("no-such-task")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task != nil {
		t.Errorf("expected nil for nonexistent task, got %+v", task)
	}
}

func TestUpdateTaskFields(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Update", "Original desc", RoleDeveloper, "", nil, 0, ".", "", "", "")

	if err := eng.DB.UpdateTask(task.ID, map[string]interface{}{
		"description":     "Updated desc",
		"verifier_focus":  "security",
		"max_retries":     5,
	}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	updated, _ := eng.DB.GetTask(task.ID)
	if updated.Description != "Updated desc" {
		t.Errorf("description = %q, want 'Updated desc'", updated.Description)
	}
	if updated.VerifierFocus != "security" {
		t.Errorf("verifier_focus = %q, want 'security'", updated.VerifierFocus)
	}
	if updated.MaxRetries != 5 {
		t.Errorf("max_retries = %d, want 5", updated.MaxRetries)
	}
}

// ============================================================================
// Models — CycleFindingsSet / HasNewFindings (loop-until-dry core)
// ============================================================================

func TestHasNewFindings(t *testing.T) {
	prev := &CycleFindingsSet{
		Cycle: 1,
		Findings: []Finding{
			{ID: "bug-1", Title: "NPE", Severity: "critical"},
			{ID: "bug-2", Title: "leak", Severity: "major"},
		},
	}

	// Same findings — no new.
	curr := &CycleFindingsSet{
		Cycle: 2,
		Findings: []Finding{
			{ID: "bug-2", Title: "leak", Severity: "major"},
			{ID: "bug-1", Title: "NPE", Severity: "critical"},
		},
	}
	if curr.HasNewFindings(prev) {
		t.Error("same findings should NOT report as new")
	}

	// One new finding.
	curr.Findings = append(curr.Findings, Finding{ID: "bug-3", Title: "crash", Severity: "critical"})
	if !curr.HasNewFindings(prev) {
		t.Error("should report new finding when ID is new")
	}

	// Current is empty.
	curr.Findings = nil
	if curr.HasNewFindings(prev) {
		t.Error("empty current should NOT report as having new findings")
	}

	// Previous is nil (first cycle).
	if !(&CycleFindingsSet{Cycle: 1, Findings: []Finding{{ID: "x"}}}).HasNewFindings(nil) {
		t.Error("first cycle with findings should report as new")
	}
	if (&CycleFindingsSet{Cycle: 1}).HasNewFindings(nil) {
		t.Error("first cycle with no findings should NOT report as new")
	}
}

func TestResetForResume(t *testing.T) {
	tests := map[TaskState]TaskState{
		TaskStateSuspended: TaskStatePending,
		TaskStateFailed:    TaskStatePending,
		TaskStateDone:      TaskStateDone,     // keep done
		TaskStatePending:   TaskStateAssigned,  // pending → assigned
		TaskStateAssigned:  TaskStateAssigned,  // keep
		TaskStateProducing: TaskStateAssigned,  // transient → assigned
		TaskStateProduced:  TaskStateAssigned,  // transient → assigned
		TaskStateVerifying: TaskStateAssigned,  // transient → assigned
		TaskStateVerified:  TaskStateAssigned,  // verified → assigned for retry
	}
	for from, want := range tests {
		got := ResetForResume(from)
		if got != want {
			t.Errorf("ResetForResume(%s) = %s, want %s", from, got, want)
		}
	}
}

func TestBatchLabelOrID(t *testing.T) {
	b := &Batch{ID: "batch-123", Label: "Research Phase"}
	if b.LabelOrID() != "Research Phase" {
		t.Error("should return label when set")
	}
	b.Label = ""
	if b.LabelOrID() != "batch-123" {
		t.Error("should return ID when label is empty")
	}
}

func TestIsContentRole(t *testing.T) {
	if !RoleResearcher.IsContentRole() {
		t.Error("researcher should be content role")
	}
	if !RoleWriter.IsContentRole() {
		t.Error("writer should be content role")
	}
	if !RoleSynthesizer.IsContentRole() {
		t.Error("synthesizer should be content role")
	}
	if RoleDeveloper.IsContentRole() {
		t.Error("developer should NOT be content role")
	}
}

// ============================================================================
// Verifier — pure functions
// ============================================================================

func TestParseFindings(t *testing.T) {
	input := `VERDICT: FAIL
FINDINGS:
[{"id": "prd-s3-missing", "title": "Missing section 3", "severity": "major", "evidence": "PRD lacks deployment section"},
{"id": "api-auth-weak", "title": "Weak auth", "severity": "critical", "evidence": "No rate limiting on /login"}]
SYNTHESIS: needs work`
	findings := ParseFindings(input)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[0].ID != "prd-s3-missing" {
		t.Errorf("first finding ID = %q", findings[0].ID)
	}
	if findings[1].Severity != "critical" {
		t.Errorf("second finding severity = %q", findings[1].Severity)
	}
}

func TestParseFindingsInline(t *testing.T) {
	// Single finding JSON object on its own line — ParseFindings only
	// handles JSON arrays after "FINDINGS:", not bare objects.
	input := "VERDICT: FAIL\nFINDINGS:\nnot a json array\nSYNTHESIS: needs work"
	findings := ParseFindings(input)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for non-JSON-array, got %d", len(findings))
	}
}

func TestParseFindingsNone(t *testing.T) {
	findings := ParseFindings("VERDICT: PASS\nAll good.")
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
	findings = ParseFindings("")
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty input, got %d", len(findings))
	}
}

func TestExtractVerdict(t *testing.T) {
	tests := []struct {
		output    string
		passCount int
		total     int
		wantPass  bool
		wantRetry bool
	}{
		{"VERDICT: PASS", 1, 2, true, false},
		{"VERDICT: FAIL", 0, 2, false, false},
		{"VERDICT: RETRY", 0, 2, false, true},
		{"no verdict text", 2, 3, true, false},  // majority pass
		{"no verdict text", 1, 3, false, false}, // majority fail
	}
	for _, tc := range tests {
		v := extractVerdict(tc.output, tc.passCount, tc.total)
		if v.passed != tc.wantPass || v.retry != tc.wantRetry {
			t.Errorf("extractVerdict(%q, %d, %d) = {passed:%v, retry:%v}, want {passed:%v, retry:%v}",
				tc.output, tc.passCount, tc.total, v.passed, v.retry, tc.wantPass, tc.wantRetry)
		}
	}
}

func TestVerdictLabel(t *testing.T) {
	if verdictLabel(true, false) != "PASS" {
		t.Error("true,false should be PASS")
	}
	if verdictLabel(false, false) != "FAIL" {
		t.Error("false,false should be FAIL")
	}
	if verdictLabel(false, true) != "RETRY" {
		t.Error("false,true should be RETRY")
	}
}

// ============================================================================
// Escalator — re-decomposition logic
// ============================================================================

func TestEscalatorStuckTasks(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	// Create tasks and set their state in DB.
	t1, _ := eng.CreateTask("Normal", "ok", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.DB.UpdateTask(t1.ID, map[string]interface{}{"retry_count": 2})
	eng.DB.TransitionState(t1.ID, TaskStateAssigned, "", "")
	eng.DB.TransitionState(t1.ID, TaskStateProducing, "", "")
	// t1 is producing, retries not exhausted → not stuck

	t2, _ := eng.CreateTask("Stuck", "stuck", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.DB.UpdateTask(t2.ID, map[string]interface{}{"retry_count": 3, "max_retries": 3})
	eng.DB.TransitionState(t2.ID, TaskStateAssigned, "", "")
	eng.DB.ForceTransitionState(t2.ID, TaskStateSuspended, "retries exhausted")
	// t2 is suspended with retries exhausted → stuck

	t3, _ := eng.CreateTask("Done", "done", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.DB.TransitionState(t3.ID, TaskStateAssigned, "", "")
	eng.DB.TransitionState(t3.ID, TaskStateDone, "", "")
	// t3 is done → not stuck

	// Re-read from DB to get current state.
	t1, _ = eng.DB.GetTask(t1.ID)
	t2, _ = eng.DB.GetTask(t2.ID)
	t3, _ = eng.DB.GetTask(t3.ID)

	batch := &Batch{ID: "test", Tasks: []*Task{t1, t2, t3}}
	esc := NewEscalator(nil) // planner not needed for StuckTasks

	stuck := esc.StuckTasks(batch, func(id string) (*Task, error) {
		return eng.DB.GetTask(id)
	})
	if len(stuck) != 1 {
		t.Fatalf("expected 1 stuck task, got %d", len(stuck))
	}
	if stuck[0].ID != t2.ID {
		t.Errorf("expected stuck task %s, got %s", t2.ID, stuck[0].ID)
	}
}

func TestEscalatorNoStuckTasks(t *testing.T) {
	batch := &Batch{ID: "test"}
	esc := NewEscalator(nil)
	stuck := esc.StuckTasks(batch, nil)
	if len(stuck) != 0 {
		t.Errorf("expected 0 stuck tasks for empty batch, got %d", len(stuck))
	}
}

// ============================================================================
// Whiteboard — cleanup
// ============================================================================

func TestWhiteboardCleanupTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Cleanup", "Will be cleaned", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.Whiteboard.InitTask(task.ID, "input")
	eng.Whiteboard.WriteOutput(task.ID, "output")
	eng.Whiteboard.WriteVerifier(task.ID, "verifier")

	// Verify files exist.
	taskDir := eng.Whiteboard.TaskDir(task.ID)
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		t.Fatal("task dir should exist before cleanup")
	}

	// Cleanup.
	if err := eng.Whiteboard.CleanupTask(task.ID); err != nil {
		t.Fatalf("cleanup task: %v", err)
	}

	// Verify directory is gone.
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Error("task dir should be removed after cleanup")
	}
}

func TestWhiteboardCleanupMasterTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("SubClean", "Sub task", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.Whiteboard.InitTask(task.ID, "input")

	// Write board and deliverable.
	eng.Whiteboard.WriteBoard("board content")
	eng.Whiteboard.WriteDeliverable("deliverable content")

	// Verify they exist.
	if content, _ := eng.Whiteboard.ReadBoard(); content == "" {
		t.Fatal("board should exist before cleanup")
	}

	// Cleanup.
	eng.Whiteboard.CleanupMasterTask("master-1", []string{task.ID})

	// Verify board and task dir are gone.
	if content, _ := eng.Whiteboard.ReadBoard(); content != "" {
		t.Error("board should be removed after cleanup")
	}
	if _, err := os.Stat(eng.Whiteboard.TaskDir(task.ID)); !os.IsNotExist(err) {
		t.Error("task dir should be removed")
	}
}

func TestWhiteboardBoardPath(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	boardPath := eng.Whiteboard.BoardPath()
	if !strings.HasSuffix(boardPath, filepath.Join("board.md")) {
		t.Errorf("board path should end with board.md, got %s", boardPath)
	}

	delPath := eng.Whiteboard.DeliverablePath()
	if !strings.HasSuffix(delPath, filepath.Join("deliverable.md")) {
		t.Errorf("deliverable path should end with deliverable.md, got %s", delPath)
	}
}

// ============================================================================
// Config / Router — edge cases
// ============================================================================

func TestNewTaskDefaults(t *testing.T) {
	task := NewTask("id-1", "Test", "desc", RoleDeveloper, "", 0, ".", nil, "", "")
	if task.MaxRetries != 3 {
		t.Errorf("default max_retries should be 3, got %d", task.MaxRetries)
	}
	if task.State != TaskStatePending {
		t.Errorf("default state should be PENDING, got %s", task.State)
	}
	if task.Profile != ProfileDefault {
		t.Errorf("default profile should be 'default', got %s", task.Profile)
	}
}

func TestNewTaskWithProfile(t *testing.T) {
	task := NewTask("id-2", "Test", "desc", RoleResearcher, ProfileResearch, 5, "/work", nil, "batch-1", "mt-1")
	if task.MaxRetries != 5 {
		t.Errorf("max_retries should be 5, got %d", task.MaxRetries)
	}
	if task.Profile != ProfileResearch {
		t.Errorf("profile should be 'research', got %s", task.Profile)
	}
	if task.Workdir != "/work" {
		t.Errorf("workdir should be '/work', got %s", task.Workdir)
	}
	if task.BatchID != "batch-1" {
		t.Errorf("batch_id should be 'batch-1', got %s", task.BatchID)
	}
	if task.MasterTaskID != "mt-1" {
		t.Errorf("master_task_id should be 'mt-1', got %s", task.MasterTaskID)
	}
}
