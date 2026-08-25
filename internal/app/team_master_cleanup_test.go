package app

import (
	"testing"
)

// TestCreateTeamMaster_ClosesEngineButPersistsMaster guards the per-`/team`-run
// engine leak fix: createTeamMaster builds a TeamEngine purely to CreateMasterTask
// (write master meta + goal to the whiteboard), and must Close it before
// returning. The master must still be readable from disk afterwards — a fresh
// engine (teamEngineForGoal) must see the same master, proving Close does not
// drop the persisted index.
func TestCreateTeamMaster_ClosesEngineButPersistsMaster(t *testing.T) {
	ws := t.TempDir()
	a := &App{sessionID: "sess-team-cleanup", workspaceRoot: ws}

	master, dir, err := a.createTeamMaster("build a tiny app", "")
	if err != nil {
		t.Fatalf("createTeamMaster: %v", err)
	}
	if master == nil || master.ID == "" {
		t.Fatalf("createTeamMaster returned empty master")
	}
	if dir == "" {
		t.Fatalf("createTeamMaster returned empty artifact dir")
	}

	// A fresh engine bound to the same workspace must see the persisted master.
	eng2, err := a.teamEngineForGoal()
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	defer eng2.Close()

	got, err := eng2.Store.GetMasterTask(master.ID)
	if err != nil {
		t.Fatalf("get master after close: %v", err)
	}
	if got == nil {
		t.Fatalf("master %s not found on disk after engine close", master.ID)
	}
	if got.Goal != "build a tiny app" {
		t.Fatalf("master.Goal = %q, want %q", got.Goal, "build a tiny app")
	}
}

// TestCreateTeamMaster_RunsPersistAcrossEngineInstances verifies the same run
// is visible to the session (team run picker) after createTeamMaster, i.e. it
// is registered under the app session id and survives engine close/reopen.
func TestCreateTeamMaster_RunsPersistAcrossEngineInstances(t *testing.T) {
	ws := t.TempDir()
	a := &App{sessionID: "sess-team-cleanup-2", workspaceRoot: ws}

	_, _, err := a.createTeamMaster("another goal", "")
	if err != nil {
		t.Fatalf("createTeamMaster: %v", err)
	}

	eng2, err := a.teamEngineForGoal()
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	defer eng2.Close()

	masters, err := eng2.Store.ListMasterTasksBySession("sess-team-cleanup-2")
	if err != nil {
		t.Fatalf("list masters by session: %v", err)
	}
	if len(masters) == 0 {
		t.Fatalf("expected the created run to be listable by session after engine close")
	}
}
