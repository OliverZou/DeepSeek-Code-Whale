package service

import (
	"context"
	"testing"
	"time"
)

// TestLocalSubmitTeamAbortPicker_EmitsPicker guards the busy-path team picker:
// /team abort (no args) submitted via the LOCAL submit queue (the path used
// while the leader turn is running) must emit EventTeamAbortPicker so the TUI
// can open the up/down selectable list. Regression for "list can only be seen,
// selecting does nothing" while working.
func TestLocalSubmitTeamAbortPicker_EmitsPicker(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{ctx: context.Background(), app: app, events: make(chan Event, 64)}

	s.handleLocalSubmit("/team abort")

	select {
	case ev := <-s.events:
		if ev.Kind != EventTeamAbortPicker {
			t.Fatalf("want EventTeamAbortPicker, got %v text=%q", ev.Kind, ev.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for EventTeamAbortPicker")
	}
}

// TestLocalSubmitTeamSessionPicker_EmitsPicker: /team session (no args) now
// reuses /subagent, so it must emit EventSubagentPicker.
func TestLocalSubmitTeamSessionPicker_EmitsPicker(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{ctx: context.Background(), app: app, events: make(chan Event, 64)}

	s.handleLocalSubmit("/team session")

	select {
	case ev := <-s.events:
		if ev.Kind != EventSubagentPicker {
			t.Fatalf("want EventSubagentPicker, got %v text=%q", ev.Kind, ev.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for EventSubagentPicker")
	}
}

// TestLocalSubmitSubagentPicker_EmitsPicker: /subagent must emit
// EventSubagentPicker.
func TestLocalSubmitSubagentPicker_EmitsPicker(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{ctx: context.Background(), app: app, events: make(chan Event, 64)}

	s.handleLocalSubmit("/subagent")

	select {
	case ev := <-s.events:
		if ev.Kind != EventSubagentPicker {
			t.Fatalf("want EventSubagentPicker, got %v text=%q", ev.Kind, ev.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for EventSubagentPicker")
	}
}
