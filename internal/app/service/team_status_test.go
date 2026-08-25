package service

import (
	"testing"
	"time"
)

// TestMaybeEmitTeamStatus_EmitsAfterTurn guards the team-run status line source:
// after a leader turn ends (POST /team submission), the service must emit an
// EventTeamStatus carrying the "team is working" text so the TUI can show the
// animated spinner line. Regression for "icon never appears".
func TestMaybeEmitTeamStatus_EmitsAfterTurn(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{app: app, events: make(chan Event, 4)}

	// 1. /team <goal> registers the inline master -> status text available.
	app.RegisterMasterForInlineForTest("master-1")

	// 2. turn ends -> maybeEmitTeamStatus -> EventTeamStatus on the wire.
	s.maybeEmitTeamStatus()

	select {
	case ev := <-s.events:
		if ev.Kind != EventTeamStatus {
			t.Fatalf("want EventTeamStatus, got %v", ev.Kind)
		}
		if ev.Text == "" {
			t.Fatalf("EventTeamStatus text empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for EventTeamStatus")
	}
}

// TestMaybeEmitTeamStatus_ThrottlesAndFinal guards the 60s throttle and the
// final ✅ event after the run finishes.
func TestMaybeEmitTeamStatus_ThrottlesAndFinal(t *testing.T) {
	app := newServiceTestApp(t, "")
	s := &Service{app: app, events: make(chan Event, 8)}
	app.RegisterMasterForInlineForTest("master-1")

	s.maybeEmitTeamStatus()
	<-s.events // drain first

	// second call within 60s: throttled, nothing new.
	s.maybeEmitTeamStatus()
	select {
	case ev := <-s.events:
		t.Fatalf("expected throttle, got %v", ev.Kind)
	case <-time.After(100 * time.Millisecond):
	}

	// run finished: final status line.
	app.FinishInlineRunForTest("master-1")
	s.teamStatusAt = time.Time{}
	s.maybeEmitTeamStatus()
	select {
	case ev := <-s.events:
		if ev.Kind != EventTeamStatus || ev.Text != "✅ 团队已完成" {
			t.Fatalf("want final event, got %v text=%q", ev.Kind, ev.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for final EventTeamStatus")
	}
}
