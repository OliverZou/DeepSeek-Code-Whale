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
//
// 一次 team 任务全程一个 TeamEngine：会话内首次创建后注册为活动执行引擎，
// leader 的 team_run_plan 工具复用同一实例，而不是每次重建（重建丢配置、
// 双实例各记各的 token）。后续调用直接复用已注册的引擎。
func (a *App) teamEngineForGoal() (*team_engine.TeamEngine, error) {
	if eng := team_engine.DefaultRunEngine(); eng != nil {
		return eng, nil
	}
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
	team_engine.SetDefaultRunEngine(eng)
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

	masters, err := a.teamRunsForCurrentSession()
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
