package team_engine

import (
	"context"
	"strings"
	"testing"
)

// mockSpawner is a minimal SubagentSpawner implementation for testing.
type mockSpawner struct {
	output   string            // default output
	roleOutputs map[string]string // output by role
	err      error
}

func (m *mockSpawner) SpawnSubagent(_ context.Context, req SubagentRequest) (SubagentResponse, error) {
	if m.err != nil {
		return SubagentResponse{}, m.err
	}
	// Return role-specific output if available.
	if out, ok := m.roleOutputs[req.Role]; ok {
		return SubagentResponse{
			Output:   out,
			Success:  true,
			ExitCode: 0,
		}, nil
	}
	return SubagentResponse{
		Output:   m.output,
		Success:  true,
		ExitCode: 0,
	}, nil
}

func newTestEngine(t *testing.T) *TeamEngine {
	t.Helper()
	spawner := &mockSpawner{output: "mock output"}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

// ---------------------------------------------------------------------------
// CreateTask
// ---------------------------------------------------------------------------

func TestCreateTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.CreateTask("Build API", "Write a FastAPI app", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.ID == "" {
		t.Error("task ID should not be empty")
	}
	if task.State != TaskStatePending {
		t.Errorf("expected PENDING, got %s", task.State)
	}
	if task.Title != "Build API" {
		t.Errorf("expected 'Build API', got %s", task.Title)
	}

	retrieved, err := eng.Store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if retrieved == nil {
		t.Fatal("task should exist")
	}
	if retrieved.Title != task.Title {
		t.Errorf("expected title %q, got %q", task.Title, retrieved.Title)
	}
}

func TestCreateTaskWithProfile(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.CreateTask("Research", "Find information", RoleResearcher, "", nil, 0, ".", "sources", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.VerifierFocus != "sources" {
		t.Errorf("expected verifier_focus 'sources', got %q", task.VerifierFocus)
	}
}

// ---------------------------------------------------------------------------
// State transitions
// ---------------------------------------------------------------------------

func TestValidTransitions(t *testing.T) {
	tests := []struct {
		from, to TaskState
		valid    bool
	}{
		{TaskStatePending, TaskStateAssigned, true},
		{TaskStateAssigned, TaskStateProducing, true},
		{TaskStateProducing, TaskStateProduced, true},
		{TaskStateProduced, TaskStateVerifying, true},
		{TaskStateVerifying, TaskStateVerified, true},
		{TaskStateVerified, TaskStateDone, true},
		{TaskStatePending, TaskStateFailed, true},
		{TaskStateDone, TaskStatePending, false},
		{TaskStateFailed, TaskStateDone, false},
		{TaskStateProduced, TaskStateDone, true}, // skip verification
	}

	for _, tc := range tests {
		got := CanTransition(tc.from, tc.to)
		if got != tc.valid {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.valid)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	if !TaskStateFailed.IsTerminal() {
		t.Error("FAILED should be terminal")
	}
	if !TaskStateDone.IsTerminal() {
		t.Error("DONE should be terminal")
	}
	if TaskStatePending.IsTerminal() {
		t.Error("PENDING should not be terminal")
	}
	if TaskStateProducing.IsTerminal() {
		t.Error("PRODUCING should not be terminal")
	}
}

// ---------------------------------------------------------------------------
// CancelTask
// ---------------------------------------------------------------------------

func TestCancelTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Test", "Cancel test", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err := eng.CancelTask(task.ID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	updated, _ := eng.Store.GetTask(task.ID)
	if updated.State != TaskStateSuspended {
		t.Errorf("expected SUSPENDED, got %s", updated.State)
	}
}

func TestCancelTerminalTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Test", "Terminal cancel", RoleDeveloper, "", nil, 0, ".", "", "", "")
	// Manually transition to a terminal state via valid path.
	_ = eng.Store.TransitionState(task.ID, TaskStateAssigned, "", "")
	_ = eng.Store.TransitionState(task.ID, TaskStateFailed, "manually failed", "")

	if err := eng.CancelTask(task.ID); err == nil {
		t.Error("expected error when cancelling terminal task")
	}
}

func TestCancelNonexistent(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	if err := eng.CancelTask("no-such"); err == nil {
		t.Error("expected error for nonexistent task")
	}
}

// ---------------------------------------------------------------------------
// SendFeedback
// ---------------------------------------------------------------------------

func TestSendFeedback(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Feedback", "Original", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err := eng.SendFeedback(task.ID, "Please add input validation"); err != nil {
		t.Fatalf("send feedback: %v", err)
	}

	// SendFeedback writes the message to the task's inbox (agent-to-human
	// equal-rights channel), not directly to the description.
	messages, err := eng.Whiteboard.ReadInbox(task.ID)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(messages) == 0 {
		t.Fatal("expected at least 1 inbox message")
	}
	if !strings.Contains(messages[0].Content, "Please add input validation") {
		t.Errorf("inbox message should contain feedback text, got: %s", messages[0].Content)
	}
	if messages[0].From != "human" {
		t.Errorf("expected sender 'human', got %q", messages[0].From)
	}

	// Also verify the verifier_feedback field was updated.
	updated, _ := eng.Store.GetTask(task.ID)
	if !strings.Contains(updated.VerifierFeedback, "Please add input validation") {
		t.Errorf("verifier_feedback should contain the message, got: %s", updated.VerifierFeedback)
	}
}

func TestSendFeedbackNonexistent(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	if err := eng.SendFeedback("no-such", "msg"); err == nil {
		t.Error("expected error for nonexistent task")
	}
}

// ---------------------------------------------------------------------------
// GetProgress
// ---------------------------------------------------------------------------

func TestGetProgress(t *testing.T) {
	tests := []struct {
		state TaskState
		pct   int
	}{
		{TaskStatePending, 0},
		{TaskStateAssigned, 0},
		{TaskStateProducing, 25},
		{TaskStateProduced, 50},
		{TaskStateVerifying, 65},
		{TaskStateVerified, 85},
		{TaskStateDone, 100},
		{TaskStateFailed, 100},
	}

	for _, tc := range tests {
		if got := GetProgress(tc.state); got != tc.pct {
			t.Errorf("GetProgress(%s) = %d, want %d", tc.state, got, tc.pct)
		}
	}
}

// ---------------------------------------------------------------------------
// ListTasks
// ---------------------------------------------------------------------------

func TestListTasks(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	eng.CreateTask("Task 1", "First", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.CreateTask("Task 2", "Second", RoleResearcher, "", nil, 0, ".", "", "", "")

	tasks, err := eng.ListTasks()
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestListTasksByState(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	t1, _ := eng.CreateTask("Task 1", "First", RoleDeveloper, "", nil, 0, ".", "", "", "")
	t2, _ := eng.CreateTask("Task 2", "Second", RoleResearcher, "", nil, 0, ".", "", "", "")

	// Transition t1 to ASSIGNED.
	eng.Store.TransitionState(t1.ID, TaskStateAssigned, "", "")

	pending, _ := eng.ListTasksByState(TaskStatePending)
	if len(pending) != 1 {
		t.Errorf("expected 1 pending task, got %d", len(pending))
	}

	assigned, _ := eng.ListTasksByState(TaskStateAssigned)
	if len(assigned) != 1 {
		t.Errorf("expected 1 assigned task, got %d", len(assigned))
	}

	// Cancel t2 so it's SUSPENDED
	eng.CancelTask(t2.ID)

	suspended, _ := eng.ListTasksByState(TaskStateSuspended)
	if len(suspended) != 1 {
		t.Errorf("expected 1 suspended task, got %d", len(suspended))
	}
}

// ---------------------------------------------------------------------------
// Whiteboard integration
// ---------------------------------------------------------------------------

func TestWhiteboardInit(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("WB Test", "Whiteboard test", RoleDeveloper, "", nil, 0, ".", "", "", "")

	if err := eng.Whiteboard.InitTask(task.ID, task.Description); err != nil {
		t.Fatalf("init whiteboard: %v", err)
	}

	input, err := eng.Whiteboard.ReadInput(task.ID)
	if err != nil {
		t.Fatalf("read input: %v", err)
	}
	if input != task.Description {
		t.Errorf("expected input %q, got %q", task.Description, input)
	}
}

func TestWhiteboardWriteRead(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("WB RW", "Read/Write test", RoleDeveloper, "", nil, 0, ".", "", "", "")
	eng.Whiteboard.InitTask(task.ID, "input")

	if err := eng.Whiteboard.WriteOutput(task.ID, "worker output"); err != nil {
		t.Fatalf("write output: %v", err)
	}

	output, _ := eng.Whiteboard.ReadOutput(task.ID)
	if output != "worker output" {
		t.Errorf("expected 'worker output', got %q", output)
	}

	if err := eng.Whiteboard.WriteVerifier(task.ID, "verifier result"); err != nil {
		t.Fatalf("write verifier: %v", err)
	}

	verifier, _ := eng.Whiteboard.ReadVerifier(task.ID)
	if verifier != "verifier result" {
		t.Errorf("expected 'verifier result', got %q", verifier)
	}
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

func TestRouterResolveProfile(t *testing.T) {
	cfg := &Config{
		Routing: RoutingConfig{
			DefaultProfile:  "default",
			VerifierTools:   "read_only",
			RoleMap: map[string]RoleEntry{
				"developer":  {Profile: "default", Timeout: 600},
				"reviewer":   {Profile: "read_only", Timeout: 600},
				"researcher": {Profile: "research", Timeout: 900},
			},
		},
	}
	r := NewRouter(cfg)

	tests := []struct {
		role       AgentRole
		isVerifier bool
		want       ToolProfile
	}{
		{RoleDeveloper, false, ProfileDefault},
		{RoleReviewer, false, ProfileReadOnly},
		{RoleResearcher, false, ProfileResearch},
		{RoleDeveloper, true, ProfileReadOnly}, // verifier always read_only
	}

	for _, tc := range tests {
		got := r.ResolveProfile(tc.role, "", tc.isVerifier)
		if got != tc.want {
			t.Errorf("ResolveProfile(%s, verifier=%v) = %s, want %s", tc.role, tc.isVerifier, got, tc.want)
		}
	}
}

func TestRouterResolveTimeout(t *testing.T) {
	cfg := &Config{
		Routing: RoutingConfig{
			RoleMap: map[string]RoleEntry{
				"developer":  {Profile: "default", Timeout: 600},
				"researcher": {Profile: "research", Timeout: 900},
			},
		},
	}
	r := NewRouter(cfg)

	if got := r.ResolveTimeout(RoleDeveloper, false); got != 600 {
		t.Errorf("expected 600, got %d", got)
	}
	if got := r.ResolveTimeout(RoleResearcher, false); got != 900 {
		t.Errorf("expected 900, got %d", got)
	}
	if got := r.ResolveTimeout(RoleDeveloper, true); got != 300 {
		t.Errorf("expected 300 (verifier default), got %d", got)
	}
}

func TestProfileToToolNames(t *testing.T) {
	defaultTools := ProfileToToolNames(ProfileDefault)
	if len(defaultTools) == 0 {
		t.Error("default profile should have tools")
	}

	readOnly := ProfileToToolNames(ProfileReadOnly)
	for _, tool := range readOnly {
		if tool == "write" || tool == "shell_run" {
			t.Errorf("read_only profile should not include %q", tool)
		}
	}
}

// ---------------------------------------------------------------------------
// isSubstantive
// ---------------------------------------------------------------------------

func TestIsSubstantive(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"short", false},
		{strings.Repeat("a", 200), true},
		{"I will do this. I plan to do that. " + strings.Repeat("a", 100), true},
		{"I will I will I will plan to 准备 计划 将", false},
	}

	for _, tc := range tests {
		got := isSubstantive(tc.input)
		if got != tc.want {
			t.Errorf("isSubstantive(%q) = %v, want %v", tc.input[:min(len(tc.input), 30)], got, tc.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// AgentChannel - prompt/spawn/abort/kill
// ---------------------------------------------------------------------------

func TestAgentChannelKill(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Kill Test", "To be killed", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err := eng.Kill(nil, task.ID); err != nil {
		t.Fatalf("kill task: %v", err)
	}

	updated, _ := eng.Store.GetTask(task.ID)
	if updated.State != TaskStateSuspended {
		t.Errorf("expected SUSPENDED, got %s", updated.State)
	}
}

func TestAgentChannelAbort(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Abort Test", "To be aborted", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err := eng.Abort(nil, task.ID); err != nil {
		t.Fatalf("abort task: %v", err)
	}

	updated, _ := eng.Store.GetTask(task.ID)
	if updated.State != TaskStateSuspended {
		t.Errorf("expected SUSPENDED, got %s", updated.State)
	}
}

func TestAgentChannelSpawn(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	spawned, err := eng.Spawn(nil, SpawnRequest{
		Title:       "Spawned",
		Description: "Spawned by channel",
		Role:        RoleResearcher,
		Profile:     ProfileResearch,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if spawned.Title != "Spawned" {
		t.Errorf("expected 'Spawned', got %q", spawned.Title)
	}
	if spawned.State != TaskStatePending {
		t.Errorf("expected PENDING, got %s", spawned.State)
	}
}

func TestAgentChannelPrompt(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Prompt Test", "Original", RoleDeveloper, "", nil, 0, ".", "", "", "")

	// Fire-and-forget prompt.
	reply, err := eng.Prompt(nil, PromptRequest{
		ToTaskID: task.ID,
		From:     "human",
		Content:  "Please add error handling",
		Sync:     false,
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if reply != nil {
		t.Error("expected nil reply for async prompt")
	}

	// Verify message landed in inbox.
	messages, _ := eng.Whiteboard.ReadInbox(task.ID)
	if len(messages) != 1 {
		t.Fatalf("expected 1 inbox message, got %d", len(messages))
	}
	if messages[0].Content != "Please add error handling" {
		t.Errorf("unexpected content: %s", messages[0].Content)
	}
	if messages[0].From != "human" {
		t.Errorf("expected from 'human', got %q", messages[0].From)
	}
}

// ---------------------------------------------------------------------------
// Message bus
// ---------------------------------------------------------------------------

func TestWriteAndReadInbox(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Inbox Test", "Test inbox", RoleDeveloper, "", nil, 0, ".", "", "", "")

	msg1 := NewMessage(task.ID, "human", "First message", "")
	msg2 := NewMessage(task.ID, "agent:worker-1", "Second message", "")

	if err := eng.Whiteboard.WriteMessage(task.ID, msg1); err != nil {
		t.Fatalf("write message 1: %v", err)
	}
	if err := eng.Whiteboard.WriteMessage(task.ID, msg2); err != nil {
		t.Fatalf("write message 2: %v", err)
	}

	messages, err := eng.Whiteboard.ReadInbox(task.ID)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	// Build a set of contents to verify both messages arrived.
	contents := make(map[string]bool)
	senders := make(map[string]bool)
	for _, m := range messages {
		contents[m.Content] = true
		senders[m.From] = true
	}
	if !contents["First message"] {
		t.Error("expected 'First message' in inbox")
	}
	if !contents["Second message"] {
		t.Error("expected 'Second message' in inbox")
	}
	if !senders["human"] {
		t.Error("expected sender 'human'")
	}
	if !senders["agent:worker-1"] {
		t.Error("expected sender 'agent:worker-1'")
	}
}

func TestBuildInboxContext(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Ctx Test", "Test context", RoleDeveloper, "", nil, 0, ".", "", "", "")

	// No messages → empty context.
	ctx, err := eng.Whiteboard.BuildInboxContext(task.ID)
	if err != nil {
		t.Fatalf("build inbox context: %v", err)
	}
	if ctx != "" {
		t.Errorf("expected empty context, got: %s", ctx)
	}

	// With messages → non-empty context.
	msg := NewMessage(task.ID, "human", "Add validation", "")
	eng.Whiteboard.WriteMessage(task.ID, msg)

	ctx, err = eng.Whiteboard.BuildInboxContext(task.ID)
	if err != nil {
		t.Fatalf("build inbox context: %v", err)
	}
	if !strings.Contains(ctx, "📨") {
		t.Error("context should contain inbox indicator")
	}
	if !strings.Contains(ctx, "Add validation") {
		t.Error("context should contain message content")
	}
}

// ---------------------------------------------------------------------------
// Suspend / Resume / Checkpoint
// ---------------------------------------------------------------------------

func TestSaveAndLoadCheckpoint(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	mt, err := eng.CreateMasterTask("test goal", "/tmp")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	// Save a checkpoint.
	checkJSON := `{"completed_batches":["batch-1"],"batch_cycles":{"batch-1":1}}`
	if err := eng.Store.SaveMasterTaskProgress(mt.ID, checkJSON); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// Reload and verify.
	loaded, err := eng.Store.GetMasterTaskProgress(mt.ID)
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if loaded != checkJSON {
		t.Errorf("checkpoint mismatch:\ngot:  %s\nwant: %s", loaded, checkJSON)
	}
}

func TestSuspendAndResumeTransition(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.CreateTask("Test", "Resume test", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Transition to assigned then suspend (simulating Kill).
	eng.Store.TransitionState(task.ID, TaskStateAssigned, "", "")
	if err := eng.CancelTask(task.ID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	updated, _ := eng.Store.GetTask(task.ID)
	if updated.State != TaskStateSuspended {
		t.Fatalf("expected SUSPENDED, got %s", updated.State)
	}

	// Verify IsResumable.
	if !updated.State.IsResumable() {
		t.Error("suspended state should be resumable")
	}

	// Resume transition.
	if err := eng.Store.TransitionState(task.ID, TaskStatePending, "resumed", ""); err != nil {
		t.Fatalf("resume transition: %v", err)
	}
	updated, _ = eng.Store.GetTask(task.ID)
	if updated.State != TaskStatePending {
		t.Errorf("expected PENDING after resume, got %s", updated.State)
	}
}

func TestListSuspendedMasterTasks(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	// Create a master task with one subtask.
	mt, err := eng.CreateMasterTask("suspended test", "/tmp")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}
	task, err := eng.CreateTask("Sub", "subtask", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err != nil {
		t.Fatalf("create subtask: %v", err)
	}
	task.MasterTaskID = mt.ID
	eng.Store.UpdateTaskMasterTaskID(task.ID, mt.ID)

	// No suspended tasks yet.
	suspended, err := eng.ListSuspendedMasterTasks()
	if err != nil {
		t.Fatalf("list suspended: %v", err)
	}
	if len(suspended) != 0 {
		t.Errorf("expected 0 suspended, got %d", len(suspended))
	}

	// Suspend the subtask.
	task.State = TaskStateSuspended
	eng.Store.TransitionState(task.ID, TaskStateSuspended, "suspended", "")

	suspended, err = eng.ListSuspendedMasterTasks()
	if err != nil {
		t.Fatalf("list suspended: %v", err)
	}
	if len(suspended) != 1 {
		t.Fatalf("expected 1 suspended master task, got %d", len(suspended))
	}
	if suspended[0].ID != mt.ID {
		t.Errorf("expected master task %s, got %s", mt.ID, suspended[0].ID)
	}
}

func newResumeTestEngine(t *testing.T) *TeamEngine {
	t.Helper()
	// Mock that returns valid accept JSON for the planner (review cycle),
	// and "ok" for the worker.
	spawner := &mockSpawner{
		roleOutputs: map[string]string{
			"planner":  `{"decision":"accept","reason":"good","feedback":""}`,
			"worker":   "task output ok",
			"verifier": "VERDICT: PASS\ndetails: looks good",
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

func TestResumeMasterTask(t *testing.T) {
	t.Skip("TODO: update for file-store behavior — needs output files for done tasks")
	eng := newResumeTestEngine(t)
	defer eng.Close()

	// Create master task with two subtasks, one already done, one suspended.
	mt, err := eng.CreateMasterTask("resume test", t.TempDir())
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	t1, _ := eng.CreateTask("Done task", "This task is done", RoleDeveloper, "", nil, 0, ".", "", "", "")
	t1.MasterTaskID = mt.ID
	t1.BatchID = "batch-research"
	eng.Store.UpdateTaskMasterTaskID(t1.ID, mt.ID)
	eng.Store.UpdateTask(t1.ID, map[string]interface{}{"batch_id": "batch-research"})
	eng.Store.TransitionState(t1.ID, TaskStateAssigned, "", "")
	eng.Store.TransitionState(t1.ID, TaskStateDone, "", "")

	t2, _ := eng.CreateTask("Suspended task", "This task got killed", RoleDeveloper, "", nil, 0, ".", "", "", "")
	t2.MasterTaskID = mt.ID
	t2.BatchID = "batch-coding"
	eng.Store.UpdateTaskMasterTaskID(t2.ID, mt.ID)
	eng.Store.UpdateTask(t2.ID, map[string]interface{}{"batch_id": "batch-coding"})
	eng.Store.TransitionState(t2.ID, TaskStateAssigned, "", "")
	eng.Store.TransitionState(t2.ID, TaskStateSuspended, "killed", "")

	// Save checkpoint: batch-research done, batch-coding not yet started.
	checkJSON := `{"completed_batches":["batch-research"],"batch_cycles":{"batch-research":1}}`
	eng.Store.SaveMasterTaskProgress(mt.ID, checkJSON)

	// List suspended.
	suspended, err := eng.ListSuspendedMasterTasks()
	if err != nil {
		t.Fatalf("list suspended: %v", err)
	}
	if len(suspended) != 1 {
		t.Fatalf("expected 1 suspended master task, got %d", len(suspended))
	}

	// Resume — will run the suspended task via RunBatch -> RunTask -> mock spawner.
	batches, err := eng.ResumeMasterTask(context.Background(), mt.ID, "resume test", t.TempDir())
	if err != nil {
		t.Fatalf("resume master task: %v", err)
	}

	// --- Debug: print final states ---
	t.Logf("batches returned: %d", len(batches))
	for _, b := range batches {
		t.Logf("  batch %s: status=%s, %d tasks", b.ID, b.Status, len(b.Tasks))
		for _, task := range b.Tasks {
			st, _ := eng.Store.GetTask(task.ID)
			t.Logf("    task %s: state=%s (title=%s)", task.ID, st.State, task.Title)
		}
	}

	// Check t2 (the suspended-then-resumed task) reached done or failed.
	updated2, _ := eng.Store.GetTask(t2.ID)
	t.Logf("t2 final state: %s", updated2.State)
	if updated2.State != TaskStateDone && updated2.State != TaskStateFailed {
		t.Errorf("t2 expected DONE or FAILED after resume, got %s", updated2.State)
	}

	if len(batches) == 0 {
		t.Error("expected at least one batch after resume")
	}
}
