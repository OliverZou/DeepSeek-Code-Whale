package tui

import (
	"strings"
	"testing"

	"github.com/usewhale/whale/internal/runtime/protocol"
)

// TestTeamStatusLine_EventStartsSpinner locks the visible team-run status line:
// EventTeamStatus -> active + spinner ticker -> frames advance -> render includes
// a spinner glyph. Regression guard for the "icon never appears" report.
func TestTeamStatusLine_EventStartsSpinner(t *testing.T) {
	m, _ := newModelWithDispatchSpy()

	// 1. status event arrives
	_ = m.handleTeamStatusEvent("团队正在干活：安排任务分工…")
	if !m.teamStatusActive {
		t.Fatalf("teamStatusActive = false, want true after event")
	}
	if m.teamStatusLine == "" {
		t.Fatalf("teamStatusLine empty after event")
	}

	// 2. render contains spinner glyph + text
	line := m.renderTeamStatusLine(80)
	if !strings.Contains(line, "团队正在干活") {
		t.Fatalf("status line missing text: %q", line)
	}
	if !strings.Contains(line, teamSpinFrames[m.teamSpinFrame%len(teamSpinFrames)]) {
		t.Fatalf("status line missing spinner glyph: %q", line)
	}

	// 3. tick advances the frame and keeps ticking while active
	next, cmd := updateTestModel(t, m, teamSpinTickMsg{})
	if next.teamSpinFrame == m.teamSpinFrame {
		t.Fatalf("spin frame did not advance after tick")
	}
	if cmd == nil {
		t.Fatalf("expected tick cmd to continue while active")
	}

	// 4. final (✅) event stops the spinner (delivered via the real service-event path)
	finalMod, _ := next.handleServiceUpdate([]protocol.Event{{Kind: protocol.EventTeamStatus, Text: "✅ 团队已完成"}})
	final := finalMod.(model)
	if final.teamStatusActive {
		t.Fatalf("teamStatusActive = true after final event")
	}
	// 完结态不再永久占用底部状态行（用户反馈：后续对话中不应再显示）。
	if final.teamStatusFinal != "" {
		t.Fatalf("teamStatusFinal should be empty after completion (must not stick), got %q", final.teamStatusFinal)
	}
	if strings.Contains(final.renderTeamStatusLine(80), teamSpinFrames[0]) {
		t.Fatalf("spinner still shown after final event")
	}
	if line := final.renderTeamStatusLine(80); line != "" {
		t.Fatalf("bottom status line should be empty after completion event, got %q", line)
	}
	// 完结提示必须作为对话里的一条消息进入 transcript，随对话滚动消失。
	found := false
	for _, msg := range final.transcript {
		if strings.Contains(msg.Text, "✅ 团队已完成") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("completion notice was not appended to transcript: %+v", final.transcript)
	}
}

// TestTeamStatusLine_BottomPartsContainsStatus guards the render placement: the
// team status line lives in bottomPartsBeforeInput (above composer), so it is
// visible even while the leader turn is idle.
func TestTeamStatusLine_BottomPartsContainsStatus(t *testing.T) {
	m, _ := newModelWithDispatchSpy()
	_ = m.handleTeamStatusEvent("团队正在干活：任务已开工")

	parts := m.bottomPartsBeforeInput(100)
	found := false
	for _, part := range parts {
		if strings.Contains(part, "团队正在干活") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("team status line not rendered in bottomPartsBeforeInput: %+v", parts)
	}
}

// tick command must actually schedule work (tea.Tick) — verify it is non-nil
// and carries the right payload type when invoked.
func TestTeamSpinCmd_ProducesTickMsg(t *testing.T) {
	cmd := teamSpinCmd()
	if cmd == nil {
		t.Fatalf("teamSpinCmd() returned nil")
	}
	msg := cmd()
	if _, ok := msg.(teamSpinTickMsg); !ok {
		t.Fatalf("teamSpinCmd() produced %T, want teamSpinTickMsg", msg)
	}
}
