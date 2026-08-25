package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/usewhale/whale/internal/team_engine"
)

// teamEngineForGoal builds a TeamEngine bound to this app's workspace, using
// the native subagent adapter (and SessionOps) wired by the app runtime when
// present, with the shell spawner as the standalone fallback.
func (a *App) teamEngineForGoal() (*team_engine.TeamEngine, error) {
	dbPath := filepath.Join(a.workspaceRoot, ".whale", "team_engine.db")
	wbDir := filepath.Join(a.workspaceRoot, ".whale", "team_tasks")
	var spawner team_engine.SubagentSpawner = team_engine.NewShellSubagentSpawner()
	if fn := team_engine.DefaultSpawnFunc(); fn != nil {
		spawner = team_engine.NewFuncSpawner(fn)
	}
	eng, err := team_engine.New(dbPath, wbDir, "", spawner)
	if err != nil {
		return nil, err
	}
	if ops := team_engine.DefaultSessionOps(); ops != nil {
		eng.SetSessionOps(ops)
	}
	return eng, nil
}

// startTeamGoal runs the explicit --subagent mode (implemented in inline_leader.go).

// listTeamGoals renders the team runs started by this app session.
func (a *App) listTeamGoals() (string, error) {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return "", err
	}
	defer eng.Close()

	masters, err := eng.Store.ListMasterTasksBySession(a.sessionID)
	if err != nil {
		return "", err
	}
	if len(masters) == 0 {
		return "No team runs in this session. Start one with `/team <goal>` or `/team <goal> --team <name>`.", nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Team runs (%d):\n\n", len(masters)))
	for _, mt := range masters {
		tasks, _ := eng.ListTasksByMasterTask(mt.ID)
		state := "running"
		if mt.Status != "" {
			state = mt.Status
		}
		done := 0
		for _, t := range tasks {
			if t.State == team_engine.TaskStateDone {
				done++
			}
		}
		b.WriteString(fmt.Sprintf("- %s  %s  [%s]  %d/%d tasks done\n    goal: %s\n",
			mt.ID[:8], state, mt.ID, done, len(tasks), mt.Goal))
	}
	b.WriteString("\nDetail: `whale team status <id>` · artifacts: `.whale/team_tasks/<id>/`")
	return b.String(), nil
}
