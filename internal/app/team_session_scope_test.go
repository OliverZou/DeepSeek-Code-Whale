package app

import (
	"testing"

	"github.com/usewhale/whale/internal/team_engine"
)

// TestTeamSessionPicks_ScopedToCurrentRun guards the /team session scope fix:
// the member-session picker must list only the CURRENT run's tasks (the master
// this app session registered), not tasks belonging to another (stale) master
// that may exist in the same workspace. Regression for /team session loading a
// stale run's sessions (D:\src\whale_test_2048_v45_ab) and /team abort showing
// an empty list — both caused by ListMasterTasksBySession(a.sessionID) resolving
// to the wrong run (or none) under a mismatched session id.
func TestTeamSessionPicks_ScopedToCurrentRun(t *testing.T) {
	ws := t.TempDir()
	a := &App{sessionID: "sess-current", workspaceRoot: ws}
	a.leaderProgressState = &leaderProgress{masters: map[string]bool{}}

	eng, err := a.teamEngineForGoal()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	// A stale run with its own task — must NOT appear in the current-run picker.
	staleMaster, derr := eng.CreateMasterTask("stale run", ws, "sess-OTHER")
	if derr != nil {
		t.Fatalf("create stale master: %v", derr)
	}
	if _, derr := eng.CreateTask("stale task", "desc", team_engine.RoleDeveloper, "", nil, 0, ws, "", "b1", staleMaster.ID); derr != nil {
		t.Fatalf("create stale task: %v", derr)
	}

	// The CURRENT run, registered inline, with its own task.
	curMaster, derr := eng.CreateMasterTask("current run 2048", ws, "sess-current")
	if derr != nil {
		t.Fatalf("create current master: %v", derr)
	}
	curTask, derr := eng.CreateTask("game.js", "desc", team_engine.RoleDeveloper, "", nil, 0, ws, "", "b1", curMaster.ID)
	if derr != nil {
		t.Fatalf("create current task: %v", derr)
	}
	a.leaderProgressState.masters[curMaster.ID] = true
	eng.Close()

	picks := a.TeamSessionPicks()
	if len(picks) != 1 {
		t.Fatalf("TeamSessionPicks = %d picks, want 1 (only the current run's task)", len(picks))
	}
	if picks[0].TaskID != curTask.ID {
		t.Fatalf("pick task = %s, want current run task %s (%s)", picks[0].TaskID, curTask.ID, curTask.Title)
	}
}

// TestTeamRunPicks_ScopedToCurrentRun guards /team abort: the run picker must
// resolve the current run even when the session id does not match, so the user
// can actually stop it (previously it listed empty because
// ListMasterTasksBySession returned nothing).
func TestTeamRunPicks_ScopedToCurrentRun(t *testing.T) {
	ws := t.TempDir()
	a := &App{sessionID: "sess-current", workspaceRoot: ws}
	a.leaderProgressState = &leaderProgress{masters: map[string]bool{}}

	eng, err := a.teamEngineForGoal()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	curMaster, derr := eng.CreateMasterTask("current run", ws, "sess-current")
	if derr != nil {
		t.Fatalf("create master: %v", derr)
	}
	a.leaderProgressState.masters[curMaster.ID] = true
	eng.Close()

	picks := a.TeamRunPicks()
	if len(picks) != 1 {
		t.Fatalf("TeamRunPicks = %d picks, want 1", len(picks))
	}
	if picks[0].MasterID != curMaster.ID {
		t.Fatalf("pick master = %s, want current run master %s", picks[0].MasterID, curMaster.ID)
	}
}
