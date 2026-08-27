package team_engine

import (
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// 编排机械约束（防错误，不干预正确结果）
//
// 编排由 Leader 决定；但 TeamEngine 对可判定的「编排错误」施加机械约束，
// Leader 正确编排时全部为 no-op：
//
//  1. 测试任务依赖实现任务：拆开产出的测试文件（X.test.js）与实现文件
//     （X.js）若落在不同 batch 且无依赖，测试 worker 会在实现产出前空转、
//     读不到被测对象 → 撞 tool cap / 假失败。约束：测试 batch 补
//     depends_on 实现 batch。同一 batch 内的测试+实现无需干预——
//     RunBatch 的三阶段（audit → impl → test）已保证执行顺序。
//  2. 集成验证任务依赖所有上游：集成验证（标题含 集成验证/端到端验收/
//     集成验收）必须在全部实现完成后执行，否则验证的是半成品。
//     约束：其 depends_on_batch 补全为所有其他 batch 的并集。
// ---------------------------------------------------------------------------

// enforcePlanTaskDependencies 返回补全依赖后的计划副本（入参不被修改）。
// 返回 (tasks, changed)：changed 表示是否有任何依赖被补上。
func enforcePlanTaskDependencies(planTasks []PlanTask) ([]PlanTask, bool) {
	if len(planTasks) <= 1 {
		return planTasks, false
	}

	batchID := func(i int) string {
		bid := planTasks[i].BatchID
		if bid == "" {
			return "default"
		}
		return bid
	}

	// output basename → task index（实现文件定位）。
	outputOwner := map[string]int{}
	for i, pt := range planTasks {
		for _, o := range splitOutputEntries(pt.Output) {
			if _, dup := outputOwner[filepath.Base(o)]; !dup {
				outputOwner[filepath.Base(o)] = i
			}
		}
	}
	allBatches := map[string]bool{}
	for i := range planTasks {
		allBatches[batchID(i)] = true
	}

	changed := false
	out := append([]PlanTask(nil), planTasks...)

	// 追加不重复的 batch 依赖；同 batch 由 RunBatch 三阶段保证顺序，不写自依赖。
	addDep := func(i int, depBatch string) {
		if depBatch == "" || depBatch == batchID(i) {
			return
		}
		for _, d := range out[i].DependsOnBatch {
			if d == depBatch {
				return
			}
		}
		out[i].DependsOnBatch = append(out[i].DependsOnBatch, depBatch)
		changed = true
	}

	// 1) 测试任务 → 实现任务所在 batch。
	for i, pt := range out {
		for _, o := range splitOutputEntries(pt.Output) {
			impl, ok := implementationForTestFile(o)
			if !ok {
				continue
			}
			implTask, found := outputOwner[filepath.Base(impl)]
			if !found || implTask == i {
				continue // 找不到实现任务，或测试与实现同任务（合并产出，无需干预）
			}
			addDep(i, batchID(implTask))
			break
		}
	}

	// 2) 集成验证任务 → 所有其他 batch。
	for i, pt := range out {
		if !isVerificationTitle(pt.Title) {
			continue
		}
		for bid := range allBatches {
			addDep(i, bid)
		}
	}

	if !changed {
		return planTasks, false
	}
	// 防御：补全后依赖图不得有环（理论上不会——只加「测试→实现」「验证→
	// 所有」的单向边，实现/上游不会反向依赖测试/验证；有环则回退本次补全）。
	if hasPlanDependencyCycle(out) {
		return planTasks, false
	}
	return out, true
}

// implementationForTestFile 把测试文件名映射到对应实现文件名。
// game.test.js → game.js；test/core.test.mjs → core.js。非测试文件返回 !ok。
func implementationForTestFile(output string) (string, bool) {
	base := filepath.Base(output)
	for _, suffix := range []string{".test.js", ".test.mjs"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix) + ".js", true
		}
	}
	return "", false
}

// isVerificationTitle 判断任务标题是否为集成验证/端到端验收任务。
func isVerificationTitle(title string) bool {
	t := strings.ToLower(title)
	for _, m := range verificationTitleMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// hasPlanDependencyCycle 在 batch 依赖图上检测环（batch 粒度）。
func hasPlanDependencyCycle(planTasks []PlanTask) bool {
	batchID := func(i int) string {
		bid := planTasks[i].BatchID
		if bid == "" {
			return "default"
		}
		return bid
	}
	// batch 依赖邻接表。
	deps := map[string][]string{}
	for i, pt := range planTasks {
		bid := batchID(i)
		for _, d := range pt.DependsOnBatch {
			deps[bid] = append(deps[bid], d)
		}
	}
	// DFS 三色标记。
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(b string) bool
	visit = func(b string) bool {
		color[b] = gray
		for _, d := range deps[b] {
			switch color[d] {
			case gray:
				return true // 环
			case white:
				if visit(d) {
					return true
				}
			}
		}
		color[b] = black
		return false
	}
	for bid := range deps {
		if color[bid] == white && visit(bid) {
			return true
		}
	}
	return false
}
