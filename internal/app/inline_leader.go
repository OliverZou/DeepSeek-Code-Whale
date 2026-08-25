package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/usewhale/whale/internal/team_engine"
)

// createTeamMaster creates the master for either /team mode (inline or
// subagent) and returns it together with its artifact dir.
//
// The engine here is used only to CreateMasterTask (write master meta + goal to
// the whiteboard, which is on disk and survives Close). It is NOT the same
// engine that executes the run: inline mode drives the run from the leader
// session via team_* tools, and --subagent mode opens its own engine in
// startTeamGoal. Leaving this eng open leaks a TeamEngine (FileTaskStore,
// logger, active-cancel maps) per /team run the app session performs, so it
// must be closed before returning.
func (a *App) createTeamMaster(goal, teamName string) (*team_engine.MasterTask, string, error) {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return nil, "", fmt.Errorf("init engine: %w", err)
	}
	if teamName != "" {
		roots := team_engine.DefaultTeamRoots(a.workspaceRoot)
		tc, err := team_engine.FindTeamInRoots(roots, teamName)
		if err != nil {
			eng.Close()
			return nil, "", fmt.Errorf("load team %q: %w", teamName, err)
		}
		eng.SetTeam(tc)
	}
	masterTask, err := eng.CreateMasterTask(goal, a.workspaceRoot, a.sessionID)
	if err != nil {
		eng.Close()
		return nil, "", fmt.Errorf("create master task: %w", err)
	}
	eng.Close()
	dir := filepath.Join(a.workspaceRoot, ".whale", "team_tasks", masterTask.ID)
	return masterTask, dir, nil
}

// startTeamGoal runs the explicit --subagent mode (background subagent leader;
// the main session keeps working as before).
func (a *App) startTeamGoal(goal, teamName string) (string, error) {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return "", fmt.Errorf("init engine: %w", err)
	}
	masterTask, _, err := a.createTeamMaster(goal, teamName)
	if err != nil {
		eng.Close()
		return "", err
	}
	go func() {
		defer eng.Close()
		_, _ = eng.TeamCycle(context.Background(), goal, a.workspaceRoot, masterTask.ID)
	}()
	dir := filepath.Join(a.workspaceRoot, ".whale", "team_tasks", masterTask.ID)
	return fmt.Sprintf("👥 Team run started\n\nmaster:  %s\ngoal:    %s\nartifacts: %s\n\nRun /team list for status, or whale team status %s for detail.\nDeliverables land in %s/output.md when the run accepts.",
		masterTask.ID, goal, dir, masterTask.ID, dir), nil
}
