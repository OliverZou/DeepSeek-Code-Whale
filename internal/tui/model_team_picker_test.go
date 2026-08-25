package tui

import (
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/usewhale/whale/internal/runtime/protocol"
)

// TestTeamAbortPicker_KeysMoveAndConfirm verifies the abort picker really reacts
// to up/down/enter (not just displays): EventTeamAbortPicker -> modeSessionPicker
// -> down moves selection -> enter arms confirm -> enter/y dispatches the abort.
func TestTeamAbortPicker_KeysMoveAndConfirm(t *testing.T) {
	m, intents := newModelWithDispatchSpy()

	// 1. abort picker event arrives with two runs.
	next, _ := m.Update(svcMsg(protocol.Event{
		Kind: protocol.EventTeamAbortPicker,
		TeamRuns: []protocol.RunPick{
			{MasterID: "aaaaaaaaaaaaaaaa", Status: "running", Goal: "game", Started: "09:00"},
			{MasterID: "bbbbbbbbbbbbbbbb", Status: "running", Goal: "docs", Started: "09:05"},
		},
	}))
	m = next.(model)
	if m.mode != modeSessionPicker || len(m.teamAbortRuns) != 2 {
		t.Fatalf("picker not open: mode=%v runs=%d", m.mode, len(m.teamAbortRuns))
	}

	// 2. down moves highlight to second item.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.teamAbortIdx != 1 {
		t.Fatalf("down did not move: idx=%d want 1", m.teamAbortIdx)
	}

	// 3. enter arms the confirm state (second press required).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if !m.teamAbortConfirm {
		t.Fatalf("enter did not arm confirm state")
	}

	// 4. y commits the abort for the selected run.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(model)
	if len(*intents) == 0 || (*intents)[0].Kind != protocol.IntentTeamAbort {
		t.Fatalf("abort intent not dispatched: %+v", *intents)
	}

	// 5. esc-cancel path: reopen and abort with esc twice keeps nothing dispatched.
	m2, i2 := newModelWithDispatchSpy()
	next2, _ := m2.Update(svcMsg(protocol.Event{
		Kind:     protocol.EventTeamAbortPicker,
		TeamRuns: []protocol.RunPick{{MasterID: "cccccccccccccccc", Status: "running"}},
	}))
	m2 = next2.(model)
	next2, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 = next2.(model)
	if m2.mode != modeChat {
		t.Fatalf("esc did not close picker: mode=%v", m2.mode)
	}
	if len(*i2) != 0 {
		t.Fatalf("esc dispatched intents: %+v", *i2)
	}
}

// TestTeamSessionPicker_KeysOpenSession verifies the member-session picker too:
// down + enter must dispatch IntentTeamSessionOpen for the highlighted member.
func TestTeamSessionPicker_KeysOpenSession(t *testing.T) {
	m, intents := newModelWithDispatchSpy()
	next, _ := m.Update(svcMsg(protocol.Event{
		Kind: protocol.EventTeamSessionPicker,
		TeamSessions: []protocol.SessionPick{
			{Role: "worker", Title: "实现核心逻辑", State: "done", SessionID: "sess-1"},
			{Role: "verifier", Title: "独立验证", State: "running", SessionID: "sess-2"},
		},
	}))
	m = next.(model)
	if m.mode != modeSessionPicker || len(m.teamSessions) != 2 {
		t.Fatalf("picker not open: mode=%v sessions=%d", m.mode, len(m.teamSessions))
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.teamPickIdx != 1 {
		t.Fatalf("down did not move: idx=%d want 1", m.teamPickIdx)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if len(*intents) == 0 || (*intents)[0].Kind != protocol.IntentTeamSessionOpen || (*intents)[0].Input != "sess-2" {
		t.Fatalf("session open not dispatched: %+v", *intents)
	}
}

// TestSubagentPicker_KeysSwitchSession verifies the /subagent picker: down +
// enter must dispatch IntentSelectSession with the selected subagent session id,
// reusing the main-session resume path.
func TestSubagentPicker_KeysSwitchSession(t *testing.T) {
	m, intents := newModelWithDispatchSpy()
	next, _ := m.Update(svcMsg(protocol.Event{
		Kind: protocol.EventSubagentPicker,
		Subagents: []protocol.SubagentPick{
			{SessionID: "parent-a--subagent-1", Role: "worker", Title: "实现核心逻辑", ParentTitle: "Alpha", Status: "running"},
			{SessionID: "parent-b--subagent-2", Role: "verifier", Title: "独立验证", ParentTitle: "Beta", Status: "running"},
		},
	}))
	m = next.(model)
	if m.mode != modeSessionPicker || len(m.subagentPicks) != 2 {
		t.Fatalf("picker not open: mode=%v subagents=%d", m.mode, len(m.subagentPicks))
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.subagentPickIdx != 1 {
		t.Fatalf("down did not move: idx=%d want 1", m.subagentPickIdx)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if len(*intents) == 0 || (*intents)[0].Kind != protocol.IntentSelectSession || (*intents)[0].SessionInput != "parent-b--subagent-2" {
		t.Fatalf("session switch not dispatched: %+v", *intents)
	}
}
