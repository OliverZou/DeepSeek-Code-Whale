package app

import (
	"fmt"
	"strings"

	"github.com/usewhale/whale/internal/runtime/protocol"
)

// TeamRunPicks returns the session team runs (masters) for the abort picker.
func (a *App) TeamRunPicks() []protocol.RunPick {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return nil
	}
	defer eng.Close()
	masters, _ := eng.Store.ListMasterTasksBySession(a.sessionID)
	if len(masters) == 0 {
		return nil
	}
	picks := make([]protocol.RunPick, 0, len(masters))
	for _, m := range masters {
		tasks, _ := eng.Store.ListTasksByMasterTask(m.ID)
		taskCount := len(tasks)
		goal := m.Goal
		if r := []rune(goal); len(r) > 50 {
			goal = string(r[:50]) + "…"
		}
		picks = append(picks, protocol.RunPick{MasterID: m.ID, Goal: goal, Status: m.Status, Started: fmt.Sprintf("%d 任务", taskCount)})
	}
	return picks
}

// StopSelectedRun stops the selected master from the abort picker.
func (a *App) StopSelectedRun(masterID string) string {
	return a.abortTeamRunBy(masterID)
}

// abortTeamRun implements "/team abort [masterID]": deterministic stop of
// the current (or a specified) run — no model round-trip needed.
func (a *App) abortTeamRun(sel string) string {
	return a.abortTeamRunBy(sel)
}

// abortTeamRunBy stops the current run (sel=="current"/"") or the master
// whose id starts with sel.
func (a *App) abortTeamRunBy(sel string) string {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return "团队引擎不可用: " + err.Error()
	}
	defer eng.Close()

	masters, _ := eng.Store.ListMasterTasksBySession(a.sessionID)
	if len(masters) == 0 {
		return "当前会话没有团队 run（先 /team <目标> 启动）"
	}
	mt := masters[len(masters)-1]
	if sel != "current" && sel != "" {
		for _, m := range masters {
			if strings.HasPrefix(m.ID, sel) {
				mt = m
				break
			}
		}
	}
	if !eng.StopRun(mt.ID) {
		if cur, _ := eng.Store.GetMasterTask(mt.ID); cur != nil {
			return fmt.Sprintf("master %s 没有运行中的任务（当前状态：%s）", short8(mt.ID), cur.Status)
		}
		return fmt.Sprintf("master %s 没有运行中的任务", short8(mt.ID))
	}
	return fmt.Sprintf("已停止全部任务（master %s）。已执行产出保留在工作区。", short8(mt.ID))
}

func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
