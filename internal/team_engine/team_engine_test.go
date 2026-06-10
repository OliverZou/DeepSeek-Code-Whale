package team_engine

import (
	"context"
	"strings"
	"testing"
)

// mockSpawner is a minimal SubagentSpawner implementation for testing.
type mockSpawner struct {
	output string
	err    error
}

func (m *mockSpawner) SpawnSubagent(_ context.Context, req SubagentRequest) (SubagentResponse, error) {
	if m.err != nil {
		return SubagentResponse{}, m.err
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

	task, err := eng.CreateTask("Build API", "Write a FastAPI app", RoleDeveloper, "", nil, 0, ".", "")
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

	retrieved, err := eng.DB.GetTask(task.ID)
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

	task, err := eng.CreateTask("Research", "Find information", RoleResearcher, "", nil, 0, ".", "sources")
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
		{TaskStatePending, TaskStateFailed, false},
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

	task, _ := eng.CreateTask("Test", "Cancel test", RoleDeveloper, "", nil, 0, ".", "")
	if err := eng.CancelTask(task.ID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	updated, _ := eng.DB.GetTask(task.ID)
	if updated.State != TaskStateFailed {
		t.Errorf("expected FAILED, got %s", updated.State)
	}
}

func TestCancelTerminalTask(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Test", "Terminal cancel", RoleDeveloper, "", nil, 0, ".", "")
	// Manually transition to a terminal state via valid path.
	_ = eng.DB.TransitionState(task.ID, TaskStateAssigned, "", "")
	_ = eng.DB.TransitionState(task.ID, TaskStateFailed, "manually failed", "")

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

	task, _ := eng.CreateTask("Feedback", "Original", RoleDeveloper, "", nil, 0, ".", "")
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
	updated, _ := eng.DB.GetTask(task.ID)
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
		{TaskStateAssigned, 10},
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

	eng.CreateTask("Task 1", "First", RoleDeveloper, "", nil, 0, ".", "")
	eng.CreateTask("Task 2", "Second", RoleResearcher, "", nil, 0, ".", "")

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

	t1, _ := eng.CreateTask("Task 1", "First", RoleDeveloper, "", nil, 0, ".", "")
	t2, _ := eng.CreateTask("Task 2", "Second", RoleResearcher, "", nil, 0, ".", "")

	// Transition t1 to ASSIGNED.
	eng.DB.TransitionState(t1.ID, TaskStateAssigned, "", "")

	pending, _ := eng.ListTasksByState(TaskStatePending)
	if len(pending) != 1 {
		t.Errorf("expected 1 pending task, got %d", len(pending))
	}

	assigned, _ := eng.ListTasksByState(TaskStateAssigned)
	if len(assigned) != 1 {
		t.Errorf("expected 1 assigned task, got %d", len(assigned))
	}

	// Cancel t2 so it's FAILED
	eng.CancelTask(t2.ID)

	failed, _ := eng.ListTasksByState(TaskStateFailed)
	if len(failed) != 1 {
		t.Errorf("expected 1 failed task, got %d", len(failed))
	}
}

// ---------------------------------------------------------------------------
// Whiteboard integration
// ---------------------------------------------------------------------------

func TestWhiteboardInit(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("WB Test", "Whiteboard test", RoleDeveloper, "", nil, 0, ".", "")

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

	task, _ := eng.CreateTask("WB RW", "Read/Write test", RoleDeveloper, "", nil, 0, ".", "")
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
	if got := r.ResolveTimeout(RoleDeveloper, true); got != 120 {
		t.Errorf("expected 120 (verifier), got %d", got)
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

	task, _ := eng.CreateTask("Kill Test", "To be killed", RoleDeveloper, "", nil, 0, ".", "")
	if err := eng.Kill(nil, task.ID); err != nil {
		t.Fatalf("kill task: %v", err)
	}

	updated, _ := eng.DB.GetTask(task.ID)
	if updated.State != TaskStateFailed {
		t.Errorf("expected FAILED, got %s", updated.State)
	}
}

func TestAgentChannelAbort(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Abort Test", "To be aborted", RoleDeveloper, "", nil, 0, ".", "")
	if err := eng.Abort(nil, task.ID); err != nil {
		t.Fatalf("abort task: %v", err)
	}

	updated, _ := eng.DB.GetTask(task.ID)
	if updated.State != TaskStateFailed {
		t.Errorf("expected FAILED, got %s", updated.State)
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

	task, _ := eng.CreateTask("Prompt Test", "Original", RoleDeveloper, "", nil, 0, ".", "")

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

	task, _ := eng.CreateTask("Inbox Test", "Test inbox", RoleDeveloper, "", nil, 0, ".", "")

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

	task, _ := eng.CreateTask("Ctx Test", "Test context", RoleDeveloper, "", nil, 0, ".", "")

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
