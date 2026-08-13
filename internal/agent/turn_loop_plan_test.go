package agent

import (
	"context"
	"strings"
	"testing"
)

func TestSessionFilesChanged(t *testing.T) {
	msgs := []Message{
		{
			SessionID: "s1",
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{
				{Name: "edit", Input: `{"file_path":"foo.go"}`},
				{Name: "write", Input: `{"file_path":"bar.go"}`},
				{Name: "read_file", Input: `{"file_path":"baz.go"}`},
			},
		},
	}
	changed := sessionFilesChanged(msgs)
	if len(changed) != 2 || !changed["foo.go"] || !changed["bar.go"] || changed["baz.go"] {
		t.Fatalf("unexpected changed set: %v", changed)
	}
}

func TestCheckIncompletePlanUntouchedFiles(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStore()
	// A pending step declaring a target file that has not been touched yet.
	_, _ = st.Create(ctx, Message{
		SessionID: "s1",
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{
			{Name: "update_plan", Input: `{"plan":[{"step":"Add auth middleware","status":"in_progress","files":["middleware.go"]}]}`},
		},
	})

	a := &Agent{store: st}
	got := a.checkIncompletePlan(ctx, "s1")
	if !strings.Contains(got, "Untouched target file") {
		t.Fatalf("expected untouched target file warning, got %q", got)
	}
}

func TestCheckIncompletePlanStepTouchedIsComplete(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStore()
	_, _ = st.Create(ctx, Message{
		SessionID: "s1",
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{
			{Name: "update_plan", Input: `{"plan":[{"step":"Add auth middleware","status":"in_progress","files":["middleware.go"]}]}`},
		},
	})
	// The pending step's target file has been edited, so no untouched warning.
	_, _ = st.Create(ctx, Message{
		SessionID: "s1",
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{
			{Name: "edit", Input: `{"file_path":"middleware.go"}`},
		},
	})

	a := &Agent{store: st}
	got := a.checkIncompletePlan(ctx, "s1")
	if strings.Contains(got, "Untouched target file") {
		t.Fatalf("did not expect untouched warning when target file was edited, got %q", got)
	}
}

func TestHasPendingPlanSteps(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStore()
	_, _ = st.Create(ctx, Message{
		SessionID: "s1",
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{
			{Name: "update_plan", Input: `{"plan":[{"step":"A","status":"completed"},{"step":"B","status":"in_progress"}]}`},
		},
	})
	a := &Agent{store: st}
	if !a.hasPendingPlanSteps(ctx, "s1") {
		t.Fatal("expected pending plan steps")
	}
}

func TestHasPendingPlanStepsAllComplete(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStore()
	_, _ = st.Create(ctx, Message{
		SessionID: "s1",
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{
			{Name: "update_plan", Input: `{"plan":[{"step":"A","status":"completed"}]}`},
		},
	})
	a := &Agent{store: st}
	if a.hasPendingPlanSteps(ctx, "s1") {
		t.Fatal("expected no pending plan steps when all completed")
	}
}

func TestIncompleteWorkReminderWithPendingPlan(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStore()
	_, _ = st.Create(ctx, Message{
		SessionID: "s1",
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{
			{Name: "update_plan", Input: `{"plan":[{"step":"A","status":"in_progress"}]}`},
		},
	})
	a := &Agent{store: st}
	if got := a.incompleteWorkReminder(ctx, "s1"); got == "" {
		t.Fatal("expected incomplete work reminder for pending plan")
	}
}

func TestIncompleteWorkReminderEmpty(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStore()
	a := &Agent{store: st}
	if got := a.incompleteWorkReminder(ctx, "s1"); got != "" {
		t.Fatalf("expected empty reminder, got %q", got)
	}
}
