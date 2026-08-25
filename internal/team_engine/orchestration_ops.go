package team_engine

import (
	"fmt"
	"sort"
)
// leaderDriveSubmitPrompt is the turn-2 instruction handed to the Leader's
// session after the decompose turn. The plan exists and the TE already turned
// it into executable batches — this turn the Leader only hands it over (one
// team_run_plan call) and stops. Waiting is deliberately NOT the Leader's job:
// an LLM polling loop burns the session's tool-iteration budget (repetitive
// team_list calls get storm_blocked, then the cap auto-interrupts the turn —
// v22's failure mode, which killed both the drive turn and its report). The
// engine waits on the plan run, then the review turn replays the outcome.
func leaderDriveSubmitPrompt(goal, workdir, masterTaskID string) string {
	return fmt.Sprintf(`这是你的提交轮。上一轮你已把目标分解为计划;TeamEngine 已把它转成可执行的批次。

## 目标
%s

## 团队任务
master_task_id: %s
工作目录: %s

## 你这一轮只做一件事
调用一次 team_run_plan(master_task_id)把整个计划提交给 TE。TE 负责全部调度——无依赖的批次并行、有依赖的批次串行、每个任务完整走完产出→验证→机械门→传播。调用立即返回执行票据。

提交完成后直接简短回复(例如「已提交,等待执行结算」)结束本轮:
- 不要重复提交;
- 不要轮询(引擎正在前台等待结算,不需要你来刷进度);
- 不要逐个 team_run 包办任务(执行是 TE 的职责,等待与轮询同样是)。`, goal, masterTaskID, workdir)
}

// leaderDriveReviewPrompt is the turn-3 instruction handed back to the Leader
// after the engine's plan run settled: it replays the execution outcome, and
// the Leader reviews / redispatches failed tasks (team_run is synchronous, the
// result comes back in the call) and writes the final report.
func leaderDriveReviewPrompt(goal, workdir, masterTaskID, result string) string {
	return fmt.Sprintf(`这是你的评审交付轮。上一轮提交的计划执行已由 TE 结算。

## 目标
%s

## 团队任务
master_task_id: %s
工作目录: %s

## 执行结果
%s

## 你的职责
1. 用 team_list / team_status 核实各任务的最终状态。
2. 失败任务:先 team_feedback(task_id + 反馈)说明问题,再 team_run 单跑重派(team_run 同步执行到该任务结束,结果当场返回)。
3. 全部结算后:team_result / team_output 汇总各任务产出,输出最终报告;有未修好的失败如实汇报。
4. 若执行结果显示计划执行未启动/未结算,用 team_list 核实后如实汇报原因,不要伪造结论。

## 最终报告格式(你的最后回复)
1. 目标完成度总结(完成的验证链条:每项任务的验证结论)
2. 交付物清单(路径、文件)
3. 遗留问题(若有)
4. 一句总结

注意:不要调用 team_execute / team_compose(这两个是发起者的一键入口,你作为驱动的 leader 用它们会与自身循环冲突)。`, goal, masterTaskID, workdir, result)
}



// AffectedTaskIDs returns the downstream closure of the batches containing the
// seeded task IDs: the seed tasks plus every task in batches that depend on a
// touched batch. Traversal stops at passed batches — their tasks are not
// re-executed, only re-reviewed. Unknown IDs and batches already passed are
// ignored. Used by the Leader to compute the blast radius of a change before
// redispatching.
func (e *TeamEngine) AffectedTaskIDs(batches []*Batch, seedTaskIDs ...string) []string {
	if len(seedTaskIDs) == 0 {
		return nil
	}
	seed := map[string]bool{}
	for _, id := range seedTaskIDs {
		seed[id] = true
	}
	affected := map[string]bool{}
	touchedBatches := map[string]bool{}
	for _, b := range batches {
		if b.Status == BatchStatusPassed {
			continue
		}
		for _, t := range b.Tasks {
			if seed[t.ID] {
				touchedBatches[b.ID] = true
				affected[t.ID] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, b := range batches {
			if b.Status == BatchStatusPassed || touchedBatches[b.ID] {
				continue
			}
			hasDep := false
			for _, dep := range b.DependsOn {
				if touchedBatches[dep] {
					hasDep = true
					break
				}
			}
			if hasDep {
				touchedBatches[b.ID] = true
				for _, t := range b.Tasks {
					affected[t.ID] = true
				}
				changed = true
			}
		}
	}
	if len(affected) == 0 {
		return nil
	}
	out := make([]string, 0, len(affected))
	for id := range affected {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// OrchestrationToolNames is the tool subset the team's Leader carries to drive
// the team. The Leader is an ordinary sessioned subagent — no engine-side
// special-casing — so these are plain tool names chosen through the native
// subagent's tool selector (Runner.ParentTools already carries the full
// toolset, and the app adapter passes team_* names through untouched).
//
// The Leader drives its own loop in two turns: turn 1 is the decompose spawn
// (planning happens inside the spawn itself — the Leader is not given a
// team_plan tool; tool selectors referencing a tool that does not exist in the
// parent toolset would fail the spawn), turn 2 is a Continue with the drive
// instruction. In that loop the callable set is: team_run (execute a task
// through produce→verify→done), team_status / team_list (observe),
// team_feedback (send review feedback), team_result / team_output (read
// deliverables) — plus the six session primitives so it can address member
// sessions exactly like the user can. team_execute and team_compose are
// deliberately excluded: they trigger the legacy engine-driven loop (TeamCycle)
// and belong to the initiator, not to the session that is itself the driver.
var OrchestrationToolNames = []string{
	"team_run",
	"team_run_plan",
	"team_status",
	"team_list",
	"team_feedback",
	"team_result",
	"team_output",
	"team_prompt",
	"team_spawn",
	"team_abort",
	"team_kill",
	"team_summarize",
	"team_fork",
}
