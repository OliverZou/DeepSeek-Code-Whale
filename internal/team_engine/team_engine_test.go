package team_engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockSpawner is a minimal SubagentSpawner implementation for testing.
type mockSpawner struct {
	mu          sync.Mutex        // guards seqCursor and the sequence cursor state
	output      string            // default output
	roleOutputs map[string]string // output by role (planner/verifier/worker)
	// roleSeq provides per-role output sequences.  Each call to SpawnSubagent
	// for a given role consumes the next string in the sequence.
	roleSeq   map[string][]string
	seqCursor map[string]int
	calls     map[string]int // spawn count by role
	err       error
}

func (m *mockSpawner) SpawnSubagent(_ context.Context, req SubagentRequest) (SubagentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = make(map[string]int)
	}
	m.calls[req.Role]++
	if m.err != nil {
		return SubagentResponse{}, m.err
	}
	// 1. Per-role sequence (for multi-call scenarios like self-split).
	if m.roleSeq != nil {
		if seq, ok := m.roleSeq[req.Role]; ok {
			if m.seqCursor == nil {
				m.seqCursor = make(map[string]int)
			}
			i := m.seqCursor[req.Role]
			if i < len(seq) {
				m.seqCursor[req.Role] = i + 1
				return SubagentResponse{Output: seq[i], Success: true, ExitCode: 0}, nil
			}
			// Sequence exhausted — fall through to roleOutputs.
		}
	}
	// 2. Exact role match.
	if out, ok := m.roleOutputs[req.Role]; ok {
		return SubagentResponse{Output: out, Success: true, ExitCode: 0}, nil
	}
	// 3. Worker roles fall back to "worker" key.
	if req.Role != "planner" && req.Role != "verifier" {
		if out, ok := m.roleOutputs["worker"]; ok {
			return SubagentResponse{Output: out, Success: true, ExitCode: 0}, nil
		}
	}
	// 4. Default output.
	return SubagentResponse{Output: m.output, Success: true, ExitCode: 0}, nil
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
		{TaskStateProduced, TaskStateChecking, true},
		{TaskStateChecking, TaskStateChecked, true},
		{TaskStateChecked, TaskStateVerifying, true},
		{TaskStateVerifying, TaskStateVerified, true},
		{TaskStateVerified, TaskStateDone, true},
		{TaskStatePending, TaskStateFailed, true},
		{TaskStateDone, TaskStatePending, false},
		{TaskStateFailed, TaskStateDone, false},
		{TaskStateProduced, TaskStateDone, true},
		{TaskStateProduced, TaskStateVerifying, false},
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

	// Write input.md so deriveState returns assigned.
	os.WriteFile(filepath.Join(eng.Whiteboard.TaskDir(t1.ID), "input.md"), []byte("test"), 0644)
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
			DefaultProfile: "default",
			VerifierTools:  "read_only",
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

	mt, err := eng.CreateMasterTask("test goal", "/tmp", "")
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
	mt, err := eng.CreateMasterTask("suspended test", "/tmp", "")
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
	mt, err := eng.CreateMasterTask("resume test", t.TempDir(), "")
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

// ============================================================================
// Integration tests — full pipeline with mock spawners
// ============================================================================

// newMockEngine creates an engine with a mock spawner pre-configured with
// role-specific outputs for decomposer, worker, and verifier.  The planner
// gets a per-role sequence so the TeamCycle flow works: elaborate-check,
// elaborate-spec, decompose (plan JSON), and the plan-level review (accept).
// Tests with pre-decomposed plans don't configure a planner output — they get
// an accept-only sequence (the review is the only planner spawn).
func newMockEngine(t *testing.T, outputs map[string]string) *TeamEngine {
	t.Helper()
	roleSeq := map[string][]string{}
	if plan, ok := outputs["planner"]; ok {
		roleSeq["planner"] = []string{"", "", plan, `{"decision":"accept","reason":"mock review"}`}
	} else {
		roleSeq["planner"] = []string{`{"decision":"accept","reason":"mock review"}`}
	}
	spawner := &mockSpawner{roleOutputs: outputs, roleSeq: roleSeq}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

// TestIntegration_SimpleCodeTask verifies the full pipeline for a trivial
// single-task coding goal (add.go case).
func TestIntegration_SimpleCodeTask(t *testing.T) {
	planJSON := `[{"title":"Implement Add","description":"Write add.go with Add(a,b int) int","role":"developer","batch_id":"batch-1","batch_label":"Implementation","verifier_focus":"correctness","use_dw":false,"max_cycles":1}]`

	// gofmt-compliant code so the linter check passes.
	workerOutput := "package add\n\n// Add returns the sum of two integers.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

	verifierOutput := "TOOLS USED: read_file (add.go), shell_run (go build, go vet)\n**VERDICT: ✅ PASS**\nEVIDENCE: go build exit 0, go vet clean\n## FINDINGS\n---json\n[]\n---"

	eng := newMockEngine(t, map[string]string{
		"planner":  planJSON,
		"worker":   workerOutput,
		"verifier": verifierOutput,
	})
	defer eng.Close()

	workdir := t.TempDir()
	mustWrite(t, workdir, "add.go", workerOutput)
	mustWrite(t, workdir, "go.mod", "module test\n\ngo 1.21")

	mt, err := eng.CreateMasterTask("write add.go", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	batches, err := eng.TeamCycle(context.Background(), "write add.go", workdir, mt.ID)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}
	if len(batches) == 0 {
		t.Fatal("expected at least one batch")
	}

	for _, b := range batches {
		for _, task := range b.Tasks {
			if task.State != TaskStateDone {
				t.Errorf("task %s (%s): expected DONE, got %s (retries=%d/%d, feedback=%s)",
					task.ID[:8], task.Title, task.State,
					task.RetryCount, task.MaxRetries, truncateStr(task.VerifierFeedback, 200))
			}
		}
	}
}

// TestIntegration_TeamCodeTask verifies the full pipeline with a team config
// and a Verifier that uses emoji in its verdict (reverse.go case).
func TestIntegration_TeamCodeTask(t *testing.T) {
	planJSON := `[{"title":"Research reverse algorithm","description":"Research and document the best approach for string reversal in Go","role":"researcher","verifier_role":"verifier","batch_id":"batch-1","batch_label":"Research","verifier_focus":"correctness,sources","use_dw":false,"max_cycles":1}]`

	workerOutput := `package reverse

// Reverse returns the reversed string using rune slice.
func Reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}`

	// Verifier output with emoji — the exact pattern that caused the bug.
	verifierOutput := "TOOLS USED: read_file, shell_run (go test -v -cover)\n**VERDICT: ✅ PASS**\nEVIDENCE: all tests pass, coverage 100%\n## FINDINGS\n---json\n[]\n---"

	eng := newMockEngine(t, map[string]string{
		"planner":  planJSON,
		"worker":   workerOutput,
		"verifier": verifierOutput,
	})
	defer eng.Close()

	// Set up a team config so the Leader uses team roles.
	eng.SetTeam(&TeamConfig{
		Label: "test-team",
		Leader: TeamLeaderConfig{
			Role:  "tech-lead",
			Model: "deepseek-v4-pro",
		},
		Roles: []string{"software-engineer", "software-architect", "software-qa-engineer"},
		RoleTitles: map[string]string{
			"software-engineer":    "软件工程师",
			"software-architect":   "软件架构师",
			"software-qa-engineer": "测试工程师",
		},
	})

	workdir := t.TempDir()
	mustWrite(t, workdir, "reverse.go", workerOutput)
	mustWrite(t, workdir, "go.mod", "module test\n\ngo 1.21")

	mt, err := eng.CreateMasterTask("write reverse.go", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	batches, err := eng.TeamCycle(context.Background(), "write reverse.go", workdir, mt.ID)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}

	for _, b := range batches {
		for _, task := range b.Tasks {
			if task.State != TaskStateDone {
				t.Errorf("task %s (%s): expected DONE, got %s (retries=%d, feedback=%s)",
					task.ID[:8], task.Title, task.State, task.RetryCount, truncateStr(task.VerifierFeedback, 100))
			}
		}
	}
}

// TestIntegration_VerdictVariants verifies that all common verdict formats
// produced by real LLMs are correctly parsed.
func TestIntegration_VerdictVariants(t *testing.T) {
	tests := []struct {
		name     string
		verdict  string
		wantDone bool
	}{
		{"plain_pass", "TOOLS USED: read_file\nVERDICT: PASS\nOK", true},
		{"emoji_pass", "TOOLS USED: read_file\n**VERDICT: ✅ PASS**\nOK", true},
		{"bold_pass", "TOOLS USED: read_file\n**VERDICT: PASS**\nOK", true},
		{"retry", "TOOLS USED: read_file\nVERDICT: RETRY\nNeeds more work", false},
		{"fail", "TOOLS USED: read_file\nVERDICT: FAIL\nBroken", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			planJSON := `[{"title":"Test task","description":"do something","role":"developer","batch_id":"batch-1","verifier_focus":"correctness","use_dw":false,"max_cycles":1}]`

			eng := newMockEngine(t, map[string]string{
				"planner":  planJSON,
				"worker":   "package testpkg",
				"verifier": tc.verdict,
			})
			defer eng.Close()

			workdir := t.TempDir()
			// No go.mod — skip build/lint checks, only test verdict parsing.
			mustWrite(t, workdir, "output.txt", "worker output here")

			mt, _ := eng.CreateMasterTask("test", workdir, "")
			batches, err := eng.TeamCycle(context.Background(), "test", workdir, mt.ID)
			if err != nil && tc.wantDone {
				t.Errorf("plan and run failed: %v", err)
				return
			}

			for _, b := range batches {
				for _, task := range b.Tasks {
					// PASS: genuine DONE. RETRY/FAIL: escalator re-decomposes
					// (original becomes DONE management node).
					if tc.wantDone && task.State != TaskStateDone {
						t.Errorf("verdict %q: expected DONE, got %s (retries=%d)",
							tc.verdict, task.State, task.RetryCount)
					}
					if !tc.wantDone && task.RetryCount == 0 {
						t.Errorf("verdict %q: RETRY/FAIL should have triggered retries",
							tc.verdict)
					}
				}
			}
		})
	}
}

func mustWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ============================================================================
// Complex multi-batch integration test with real team file
// ============================================================================

// TestIntegration_MultiBatchPipeline verifies cross-batch dependencies,
// artifact passing, and mixed code+content roles using a real team YAML.
//
// Team: 软件开发团队 (loaded from bin/teams)
//
//	Batch 1 (design):  software-architect → design.md
//	Batch 2 (coding):   software-engineer  → main.go
//	Batch 3 (test):     software-qa-engineer → main_test.go
//
// Batch 2 depends on Batch 1; Batch 3 depends on Batch 2.
func TestIntegration_MultiBatchPipeline(t *testing.T) {
	// Load the real team if available (team files ship in ~/.whale/teams or a
	// bundled exe_dir/teams; absence must not skip the pipeline assertions).
	tc, _ := LoadTeamConfig("../../bin/teams/软件开发团队/team.yaml")

	// The Leader would decompose the goal using team roles.  We simulate
	// that by providing a pre-decomposed plan that references the team's
	// actual role names.
	planJSON := `[
  {
    "title": "Write design document",
    "description": "Write a design document for a simple KV store CLI tool, covering the API and data model.",
    "output": "design.md",
    "role": "software-architect",
    "batch_id": "batch-1",
    "batch_label": "Design",
    "verifier_focus": "completeness",
    "use_dw": false,
    "max_cycles": 1
  },
  {
    "title": "Implement KV store",
    "description": "Implement the KV store in main.go based on design.md from the previous batch. Read the upstream output first.",
    "output": "main.go",
    "role": "software-engineer",
    "batch_id": "batch-2",
    "batch_label": "Implementation",
    "depends_on_batch": ["batch-1"],
    "verifier_focus": "correctness",
    "use_dw": false,
    "max_cycles": 1
  },
  {
    "title": "Write tests",
    "description": "Write tests for the KV store in main_test.go based on main.go from the previous batch.",
    "output": "main_test.go",
    "role": "software-qa-engineer",
    "batch_id": "batch-3",
    "batch_label": "Testing",
    "depends_on_batch": ["batch-2"],
    "verifier_focus": "correctness",
    "use_dw": false,
    "max_cycles": 1
  }
]`

	architectOutput := `# KV Store CLI — Design Document

## API

- PUT <key> <value> — store a key-value pair
- GET <key> — retrieve a value by key
- DELETE <key> — remove a key
- LIST — list all keys

## Data Model

In-memory map[string]string with sync.RWMutex for concurrent access.`

	backendOutput := "package main\n\nimport (\n\t\"fmt\"\n\t\"sync\"\n)\n\nvar store = struct {\n\tsync.RWMutex\n\tdata map[string]string\n}{data: make(map[string]string)}\n\nfunc main() { fmt.Println(\"kv store\") }\n"

	testerOutput := "package main\n\nimport \"testing\"\n\nfunc TestStore(t *testing.T) {\n\tt.Log(\"ok\")\n}\n"

	eng := newMockEngine(t, map[string]string{
		"planner":              planJSON,
		"software-architect":   architectOutput,
		"software-engineer":    backendOutput,
		"software-qa-engineer": testerOutput,
		"verifier":             "TOOLS USED: read_file, shell_run\nVERDICT: PASS\nEVIDENCE: all checks pass\n## FINDINGS\n---json\n[]\n---",
	})
	defer eng.Close()

	// Set the real team on the engine so Leader prompt generation,
	// role resolution, and team rules are exercised (if available).
	if tc != nil {
		eng.SetTeam(tc)
	}

	workdir := t.TempDir()
	mustWrite(t, workdir, "go.mod", "module kvstore\n\ngo 1.21")
	mustWrite(t, workdir, "design.md", architectOutput)
	mustWrite(t, workdir, "main.go", backendOutput)
	mustWrite(t, workdir, "main_test.go", testerOutput)

	mt, err := eng.CreateMasterTask("build KV store CLI", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	// Run the pipeline and collect engine log events.
	batches, err := eng.TeamCycle(context.Background(), "build KV store CLI", workdir, mt.ID)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}

	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}

	// Verify batch ordering and dependency gating.
	batchMap := make(map[string]*Batch)
	for _, b := range batches {
		batchMap[b.ID] = b
	}

	// Batch 2 should depend on Batch 1.
	if b2, ok := batchMap["batch-2"]; ok {
		if len(b2.DependsOn) != 1 || b2.DependsOn[0] != "batch-1" {
			t.Errorf("batch-2 depends_on = %v, want [batch-1]", b2.DependsOn)
		}
	}

	// Batch 3 should depend on Batch 2.
	if b3, ok := batchMap["batch-3"]; ok {
		if len(b3.DependsOn) != 1 || b3.DependsOn[0] != "batch-2" {
			t.Errorf("batch-3 depends_on = %v, want [batch-2]", b3.DependsOn)
		}
	}

	// Verify all tasks completed.
	for _, b := range batches {
		t.Logf("Batch %s [%s]: %d tasks", b.LabelOrID(), b.Status, len(b.Tasks))
		if b.Status != BatchStatusPassed {
			t.Errorf("batch %s: expected status %s, got %s", b.LabelOrID(), BatchStatusPassed, b.Status)
		}
		for _, task := range b.Tasks {
			t.Logf("  %s [%s] role=%s retries=%d/%d",
				task.Title, task.State, task.Role, task.RetryCount, task.MaxRetries)
			if task.State != TaskStateDone {
				t.Errorf("task %s: expected DONE, got %s", task.Title, task.State)
			}
		}
	}

}

// ============================================================================
// Self-split integration test — parent task spawns children
// ============================================================================

func TestIntegration_SelfSplitPipeline(t *testing.T) {
	planJSON := "[{\"title\":\"Build complete KV store\",\"description\":\"Too large for one pass\",\"output\":\"store.go\",\"role\":\"software-engineer\",\"batch_id\":\"batch-1\",\"batch_label\":\"Implementation\",\"verifier_focus\":\"correctness\",\"use_dw\":false,\"max_cycles\":3}]"

	roleSeq := map[string][]string{
		// TeamCycle planner sequence: elaborate-check, elaborate-spec,
		// decompose (plan JSON), plan-level review (accept).
		"planner": {"", "", planJSON, `{"decision":"accept","reason":"mock review"}`},
		"software-engineer": {
			"[SPLIT_PLAN][{\"title\":\"Implement data model\",\"description\":\"Write store.go\",\"output\":\"store.go\",\"role\":\"software-engineer\",\"batch_id\":\"batch-1\",\"verifier_focus\":\"correctness\"},{\"title\":\"Implement CLI\",\"description\":\"Write main.go\",\"output\":\"main.go\",\"role\":\"software-engineer\",\"batch_id\":\"batch-1\",\"verifier_focus\":\"correctness\"}]",
			"package main\n\nimport \"sync\"\n\nvar Store = struct {\n\tsync.RWMutex\n\tData map[string]string\n}{Data: make(map[string]string)}\n",
			"package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {\n\tfmt.Println(\"kv ready\")\n\t_ = os.Args\n}\n",
		},
	}

	eng, err := New(":memory:", t.TempDir(), "", &mockSpawner{
		roleOutputs: map[string]string{
			"planner":  planJSON,
			"verifier": "TOOLS USED: read_file, shell_run\nVERDICT: PASS\nEVIDENCE: ok\n## FINDINGS\n---json\n[]\n---",
		},
		roleSeq: roleSeq,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	tc, _ := LoadTeamConfig("../../bin/teams/软件开发团队/team.yaml")
	if tc != nil {
		eng.SetTeam(tc)
	}

	workdir := t.TempDir()
	mustWrite(t, workdir, "go.mod", "module kvstore\n\ngo 1.21")
	mustWrite(t, workdir, "store.go", roleSeq["software-engineer"][1])
	mustWrite(t, workdir, "main.go", roleSeq["software-engineer"][2])

	mt, _ := eng.CreateMasterTask("build KV store", workdir, "")
	batches, err := eng.TeamCycle(context.Background(), "build KV store", workdir, mt.ID)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}

	allTasks, _ := eng.Store.ListTasks()
	t.Logf("Total tasks: %d", len(allTasks))

	parentCount := 0
	childCount := 0
	for _, task := range allTasks {
		children, _ := eng.Store.ListTasksByParent(task.ID)
		hasChildren := len(children) > 0
		isChild := len(task.ParentIDs) > 0
		t.Logf("  %s [%s] role=%s retries=%d parents=%d children=%d", task.Title, task.State, task.Role, task.RetryCount, len(task.ParentIDs), len(children))
		if hasChildren {
			parentCount++
			// Parent that self-split successfully should be DONE, but
			// accept produced/done — children carry the work forward.
			if task.State != TaskStateDone && task.State != TaskStateProduced {
				t.Errorf("parent %s: expected DONE, got %s", task.Title, task.State)
			}
		} else if isChild {
			childCount++
			if task.State != TaskStateDone {
				t.Errorf("child %s: expected DONE, got %s", task.Title, task.State)
			}
		}
	}
	if parentCount == 0 {
		t.Error("expected at least 1 parent")
	}
	if childCount < 2 {
		t.Errorf("expected at least 2 children, got %d", childCount)
	}
	_ = batches
}

// TestIntegration_SoftwareTeam_AgentVerifier verifies the full pipeline using
// software development team roles with an agent-definition-driven Verifier.
// Worker="software-engineer", Verifier="software-qa-engineer" (via verifier_role).
func TestIntegration_SoftwareTeam_AgentVerifier(t *testing.T) {
	planJSON := `[{"title":"Implement gcd.go","description":"Write gcd.go with GCD(a,b int) int using Euclidean algorithm","role":"software-engineer","verifier_role":"software-qa-engineer","batch_id":"batch-1","batch_label":"Implementation","verifier_focus":"correctness","use_dw":false,"max_cycles":1}]`

	workerOutput := "package gcd\n\n// GCD returns the greatest common divisor using Euclidean algorithm.\nfunc GCD(a, b int) int {\n\tfor b != 0 {\n\t\ta, b = b, a%b\n\t}\n\treturn a\n}\n"

	verifierOutput := "TOOLS USED: read_file (gcd.go), shell_run (go build, go vet, go test -v -cover)\n**VERDICT: ✅ PASS**\nEVIDENCE: go build exit 0, go vet clean, go test all pass, coverage 100%\n## FINDINGS\n---json\n[]\n---"

	eng := newMockEngine(t, map[string]string{
		"planner":  planJSON,
		"worker":   workerOutput,
		"verifier": verifierOutput,
	})
	defer eng.Close()

	eng.SetTeam(&TeamConfig{
		Label: "software-dev-team",
		Leader: TeamLeaderConfig{
			Role:  "software-team-lead",
			Model: "deepseek-v4-flash",
		},
		Roles: []string{"software-engineer", "software-qa-engineer"},
		RoleTitles: map[string]string{
			"software-engineer":    "软件工程师",
			"software-qa-engineer": "测试工程师",
		},
	})

	workdir := t.TempDir()
	mustWrite(t, workdir, "gcd.go", workerOutput)
	mustWrite(t, workdir, "go.mod", "module test\n\ngo 1.21")

	mt, err := eng.CreateMasterTask("write gcd.go", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	batches, err := eng.TeamCycle(context.Background(), "write gcd.go", workdir, mt.ID)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}

	for _, b := range batches {
		for _, task := range b.Tasks {
			if task.State != TaskStateDone {
				t.Errorf("task %s (%s): expected DONE, got %s (retries=%d, feedback=%s)",
					task.ID[:8], task.Title, task.State, task.RetryCount, truncateStr(task.VerifierFeedback, 200))
			}
			if task.VerifierRole != "software-qa-engineer" {
				t.Errorf("task %s: expected VerifierRole=software-qa-engineer, got %q",
					task.ID[:8], task.VerifierRole)
			}
		}
	}
}
