package app

import (
	"testing"
	"time"

	"github.com/usewhale/whale/internal/team_engine"
)

func TestLeaderProgress_OnlyInlineMastersAndThrottled(t *testing.T) {
	a := &App{}
	a.leaderProgressInit()
	var sinkVisible, sinkHidden string
	a.SetInlineLeaderTurnSink(func(v, h string) { sinkVisible, sinkHidden = v, h })
	a.registerInlineMaster("master-1")

	sinkVisible, sinkHidden = "", ""
	a.onTeamEngineEvent(team_engine.TaskEvent{Type: team_engine.EventStateChanged, MasterID: "master-1", TaskID: "12345678", Title: "实现核心逻辑", NewState: "done", Workdir: "w", Deliverables: []string{"game.js"}})
	if sinkVisible != "" || sinkHidden == "" || !strContains(sinkHidden, "实现核心逻辑") || !strContains(sinkHidden, "完成") || !strContains(sinkHidden, "- [game.js]") || !strContains(sinkHidden, "w\\game.js") {
		t.Errorf("expected leader narration on done, got vis=%q hidden=%q", sinkVisible, sinkHidden)
	}

	// Throttle: a second event within 30s must not re-trigger.
	_ = time.Now()
	sinkHidden = ""
	a.onTeamEngineEvent(team_engine.TaskEvent{Type: team_engine.EventStateChanged, MasterID: "master-1", TaskID: "87654321", Title: "集成验收", NewState: "failed"})
	if sinkHidden != "" {
		t.Errorf("throttle failed: second event delivered: %q", sinkHidden)
	}

	// Reset throttle window and verify failure narration wording.
	a.leaderProgressState.mu.Lock()
	a.leaderProgressState.lastNotify["master-1"] = time.Now().Add(-time.Minute)
	a.leaderProgressState.mu.Unlock()
	a.onTeamEngineEvent(team_engine.TaskEvent{Type: team_engine.EventStateChanged, MasterID: "master-1", TaskID: "87654321", Title: "集成验收", NewState: "failed"})
	if !strContains(sinkHidden, "失败") {
		t.Errorf("failure narration missing 失败: %q", sinkHidden)
	}

	// Non-inline master: no narration.
	sinkHidden = ""
	a.onTeamEngineEvent(team_engine.TaskEvent{Type: team_engine.EventStateChanged, MasterID: "other-master", TaskID: "11111111", Title: "x", NewState: "done"})
	if sinkHidden != "" {
		t.Errorf("non-inline master narrated: %q", sinkHidden)
	}
}

func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
