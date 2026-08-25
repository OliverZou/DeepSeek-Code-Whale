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
	got, _ := eng.Store.GetTask(task.ID)
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestDeleteMasterTaskCascades(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	mt, _ := eng.CreateMasterTask("cascade test", "/tmp", "")
	// Create 3 subtasks.
	for i := 0; i < 3; i++ {
		task, _ := eng.CreateTask("Sub", "subtask", RoleDeveloper, "", nil, 0, ".", "", "", "")
		task.MasterTaskID = mt.ID
		eng.Store.UpdateTask(task.ID, map[string]interface{}{"master_task_id": mt.ID})
	}

	// Delete the master task.
	if err := eng.DeleteMasterTask(mt.ID); err != nil {
		t.Fatalf("delete master task: %v", err)
	}

	// All subtasks should be gone.
	tasks, _ := eng.Store.ListTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 subtasks after cascade delete, got %d", len(tasks))
	}
	// Master task itself should be gone.
	got, _ := eng.Store.GetMasterTask(mt.ID)
	if got != nil {
		t.Error("master task should be deleted")
	}
}

func TestForceTransitionState(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Force", "Force transition", RoleDeveloper, "", nil, 0, ".", "", "", "")

	// With file-based state, DONE requires output.md + verify.md.
	dir := eng.Whiteboard.TaskDir(task.ID)
	os.WriteFile(filepath.Join(dir, "output.md"), []byte("done"), 0644)
	os.WriteFile(filepath.Join(dir, "verify.md"), []byte("passed"), 0644)

	// ForceTransitionState bypasses the transition table.
	if err := eng.Store.ForceTransitionState(task.ID, TaskStateDone, "forced"); err != nil {
		t.Fatalf("force transition: %v", err)
	}
	updated, _ := eng.Store.GetTask(task.ID)
	if updated.State != TaskStateDone {
		t.Errorf("expected DONE, got %s", updated.State)
	}
}

func TestGetTaskNonexistent(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.Store.GetTask("no-such-task")
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

	if err := eng.Store.UpdateTask(task.ID, map[string]interface{}{
		"description":    "Updated desc",
		"verifier_focus": "security",
		"max_retries":    5,
	}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	updated, _ := eng.Store.GetTask(task.ID)
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
		TaskStatePending:   TaskStateAssigned, // pending → assigned
		TaskStateAssigned:  TaskStateAssigned, // keep
		TaskStateProducing: TaskStateAssigned, // transient → assigned
		TaskStateProduced:  TaskStateAssigned, // transient → assigned
		TaskStateVerifying: TaskStateAssigned, // transient → assigned
		TaskStateVerified:  TaskStateDone,     // verifier passed, just need done confirm
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

func TestParseVerdict_Emoji(t *testing.T) {
	tests := []struct {
		output    string
		wantPass  bool
		wantRetry bool
	}{
		{"VERDICT: PASS", true, false},
		{"VERDICT: FAIL", false, false},
		{"VERDICT: RETRY", false, true},
		{"**VERDICT: ✅ PASS**", true, false},
		{"**VERDICT:** ✅ PASS", true, false},
		{"VERDICT:  PASS", true, false},
		{"VERDICT: ❌ FAIL", false, false},
		{"VERDICT:\nPASS", true, false},
		{"some text VERDICT: PASS more text", true, false},
		{"no verdict here", false, false},
	}
	for _, tc := range tests {
		gotPass, gotRetry := parseVerdict(tc.output)
		if gotPass != tc.wantPass || gotRetry != tc.wantRetry {
			t.Errorf("parseVerdict(%q) = {passed:%v, retry:%v}, want {passed:%v, retry:%v}",
				tc.output, gotPass, gotRetry, tc.wantPass, tc.wantRetry)
		}
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

func TestTeamMaxAgents(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	// No team configured → global default (Defaults.Batch.MaxAgents = 9).
	if got := eng.teamMaxAgents(); got != 9 {
		t.Fatalf("no team: got %d, want 9", got)
	}

	// Team without runtime config → still global default.
	eng.team = &TeamConfig{}
	if got := eng.teamMaxAgents(); got != 9 {
		t.Fatalf("team without config: got %d, want 9", got)
	}

	// Team with max_agents unset (0) → global default.
	eng.team = &TeamConfig{Config: &TeamRuntimeConfig{MaxAgents: 0}}
	if got := eng.teamMaxAgents(); got != 9 {
		t.Fatalf("team max_agents=0: got %d, want 9", got)
	}

	// Team overrides the global ceiling.
	eng.team = &TeamConfig{Config: &TeamRuntimeConfig{MaxAgents: 5}}
	if got := eng.teamMaxAgents(); got != 5 {
		t.Fatalf("team max_agents=5: got %d, want 5", got)
	}
}

// ============================================================================
// 验收标准（acceptance_criteria）— Worker 自检 + Verifier 独立验收共用
// ============================================================================

func TestWriteInboxFileAcceptanceCriteria(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("T", "desc", RoleDeveloper, "", nil, 0, ".", "", "", "")
	params := InboxParams{
		Title:              task.Title,
		Role:               string(task.Role),
		Description:        task.Description,
		AcceptanceCriteria: []string{"条件一", "条件二"},
	}
	if err := eng.Whiteboard.WriteInboxFile(task.ID, params); err != nil {
		t.Fatalf("write inbox: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(eng.Whiteboard.TaskDir(task.ID), "input.md"))
	if err != nil {
		t.Fatalf("read input.md: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "## ✅ 验收标准") {
		t.Errorf("inbox should contain 验收标准 section, got:\n%s", s)
	}
	for _, c := range params.AcceptanceCriteria {
		if !strings.Contains(s, c) {
			t.Errorf("inbox should list criterion %q", c)
		}
	}
}

func TestVerifierBuildPromptAcceptanceCriteria(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("T", "desc", RoleDeveloper, "", nil, 0, ".", "", "", "")
	task.AcceptanceCriteria = []string{"给定输入 → 期望输出"}
	eng.Whiteboard.WriteOutput(task.ID, "worker output")

	v := NewVerifier(eng.Whiteboard, eng.Runner, eng.Router, 0)
	prompt := v.BuildPrompt(task)

	if !strings.Contains(prompt, "ACCEPTANCE CRITERIA") {
		t.Errorf("verifier prompt should contain ACCEPTANCE CRITERIA section")
	}
	if !strings.Contains(prompt, "给定输入 → 期望输出") {
		t.Errorf("verifier prompt should list the criteria")
	}
	// The verifier is an inspector, not an editor: the prompt must forbid
	// writing files (no verify/ drop dir any more).
	if !strings.Contains(prompt, "禁止修改交付物或写入任何文件") {
		t.Errorf("verifier prompt should forbid writing files")
	}
	// The verification strategy must be delegated to the LLM per task
	// characteristics (行为原则 3), and the report must state the chosen
	// method — the engine does not pre-classify what/how to verify.
	if !strings.Contains(prompt, "多步骤链路") || !strings.Contains(prompt, "不可逆/高影响交付") {
		t.Errorf("verifier prompt should carry the task-characteristic strategy table")
	}
	if !strings.Contains(prompt, "METHOD:") {
		t.Errorf("verifier prompt should require a METHOD: line")
	}
}

// TestVerifierBuildPromptReReview locks the re-review contract: on a retry
// round the previous report is injected and the verifier is constrained to
// re-verify only 未通过/无法验证 items — pass items are not re-checked, so
// retry rounds do not re-pay the full verification cost.
func TestVerifierBuildPromptReReview(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("T", "desc", RoleDeveloper, "", nil, 0, ".", "", "", "")
	task.AcceptanceCriteria = []string{"criterion A"}
	eng.Whiteboard.WriteOutput(task.ID, "worker output")

	v := NewVerifier(eng.Whiteboard, eng.Runner, eng.Router, 0)
	prompt := v.BuildPrompt(task)
	if strings.Contains(prompt, "PREVIOUS VERIFICATION REPORT") {
		t.Errorf("first round must not carry a previous report")
	}

	v2 := NewVerifier(eng.Whiteboard, eng.Runner, eng.Router, 0).WithPreviousReport("VERDICT: FAIL\nISSUES: - criterion A failed")
	prompt2 := v2.BuildPrompt(task)
	if !strings.Contains(prompt2, "PREVIOUS VERIFICATION REPORT") || !strings.Contains(prompt2, "RE-REVIEW CONTRACT") {
		t.Errorf("re-review prompt must inject the previous report and the contract")
	}
	if !strings.Contains(prompt2, "只对上一轮标记为「未通过/无法验证」的验收点做复审") {
		t.Errorf("re-review contract must restrict scope to failed/unverifiable items")
	}
}

// TestDeriveState_VerifierReportAloneIsNotDone guards the false-green regression:
// a FAILED verification writes a verifier.md report (the verifier records its
// verdict there regardless of outcome), so a task dir with output.md +
// verifier.md but NO verify.md must NOT derive as done — otherwise a failed
// task survives batch reset as "done" and the batch is reported passed
// (run_report: 2/2 passed while the integration task's verdict was FAIL).
func TestDeriveState_VerifierReportAloneIsNotDone(t *testing.T) {
	dir := t.TempDir()
	store := &FileTaskStore{baseDir: t.TempDir()}

	mk := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// output + verifier.md (FAIL report) → NOT done (the regression).
	mk("output.md")
	mk("verifier.md")
	if got := store.DeriveState(dir); got == TaskStateDone {
		t.Fatalf("output.md + verifier.md (no verify.md) must NOT derive as done; got %s", got)
	}

	// output + verify.md → done (the correct marker).
	os.Remove(filepath.Join(dir, "verifier.md"))
	mk("verify.md")
	if got := store.DeriveState(dir); got != TaskStateDone {
		t.Fatalf("output.md + verify.md should derive as done; got %s", got)
	}
}
