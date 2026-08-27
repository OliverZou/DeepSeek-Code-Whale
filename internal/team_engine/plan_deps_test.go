package team_engine

import (
	"slices"
	"testing"
)

func pt(title, output, batch string, deps ...string) PlanTask {
	return PlanTask{
		Title:          title,
		Output:         output,
		BatchID:        batch,
		DependsOnBatch: FlexibleStringSlice(deps),
	}
}

func depsOf(tasks []PlanTask, title string) []string {
	for _, pt := range tasks {
		if pt.Title == title {
			return []string(pt.DependsOnBatch)
		}
	}
	return nil
}

func TestEnforcePlanTaskDependencies_TestTaskGetsImplDependency(t *testing.T) {
	// 测试任务与实现任务在不同 batch 且无依赖 → 补上。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("编写单元测试", "game.test.js", "2"),
	}
	fixed, changed := enforcePlanTaskDependencies(tasks)
	if !changed {
		t.Fatal("expected dependency to be enforced")
	}
	if got := depsOf(fixed, "编写单元测试"); !slices.Contains(got, "1") {
		t.Fatalf("test task deps = %v, want include batch 1", got)
	}
	// 原计划不被修改。
	if len(tasks[1].DependsOnBatch) != 0 {
		t.Fatal("input plan must not be mutated")
	}
}

func TestEnforcePlanTaskDependencies_SameBatchUntouched(t *testing.T) {
	// 测试与实现在同一 batch：RunBatch 三阶段保证顺序，不写自依赖。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("编写单元测试", "game.test.js", "1"),
	}
	fixed, changed := enforcePlanTaskDependencies(tasks)
	if changed {
		t.Fatal("same-batch test+impl must stay untouched")
	}
	if len(fixed[1].DependsOnBatch) != 0 {
		t.Fatalf("unexpected deps: %v", fixed[1].DependsOnBatch)
	}
}

func TestEnforcePlanTaskDependencies_AlreadyCorrectUntouched(t *testing.T) {
	// Leader 已正确声明依赖 → no-op。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("编写单元测试", "game.test.js", "2", "1"),
	}
	_, changed := enforcePlanTaskDependencies(tasks)
	if changed {
		t.Fatal("correct plan must be untouched")
	}
}

func TestEnforcePlanTaskDependencies_MergedTaskUntouched(t *testing.T) {
	// 核心+测试合并为同一任务（d21b618 规则）→ 无需干预。
	tasks := []PlanTask{
		pt("核心逻辑与测试", "game.js, game.test.js", "1"),
		pt("交互层", "game-dom.js", "2"),
	}
	_, changed := enforcePlanTaskDependencies(tasks)
	if changed {
		t.Fatal("merged test+impl task must be untouched")
	}
}

func TestEnforcePlanTaskDependencies_UnmatchedTestFileUntouched(t *testing.T) {
	// 找不到实现文件（basename 不匹配）→ 不强行干预。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("编写怪测试", "test/legacy.spec.js", "2"),
	}
	_, changed := enforcePlanTaskDependencies(tasks)
	if changed {
		t.Fatal("unmatched test file must be untouched")
	}
}

func TestEnforcePlanTaskDependencies_VerificationDependsOnAll(t *testing.T) {
	// 集成验证任务缺依赖 → 补全所有其他 batch。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("实现样式", "style.css", "2"),
		pt("集成验证与端到端验收", "report.md", "3"),
	}
	fixed, changed := enforcePlanTaskDependencies(tasks)
	if !changed {
		t.Fatal("expected verification deps to be enforced")
	}
	got := depsOf(fixed, "集成验证与端到端验收")
	if !slices.Contains(got, "1") || !slices.Contains(got, "2") {
		t.Fatalf("verification deps = %v, want include 1 and 2", got)
	}
	if slices.Contains(got, "3") {
		t.Fatal("verification must not depend on its own batch")
	}
}

func TestEnforcePlanTaskDependencies_VerificationPartialDepsCompleted(t *testing.T) {
	// Leader 已声明部分依赖 → 并集补全,已正确的保留。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("实现样式", "style.css", "2"),
		pt("集成验证与端到端验收", "report.md", "3", "1"),
	}
	fixed, changed := enforcePlanTaskDependencies(tasks)
	if !changed {
		t.Fatal("expected missing deps to be filled")
	}
	got := depsOf(fixed, "集成验证与端到端验收")
	if !slices.Contains(got, "1") || !slices.Contains(got, "2") {
		t.Fatalf("verification deps = %v, want include 1 and 2", got)
	}
}

func TestEnforcePlanTaskDependencies_NoCycleIntroduced(t *testing.T) {
	// 补全后依赖图无环,且原正确编排（验证已依赖所有上游）不变。
	tasks := []PlanTask{
		pt("实现核心逻辑", "game.js", "1"),
		pt("编写单元测试", "game.test.js", "2", "1"),
		pt("集成验证与端到端验收", "report.md", "3", "1", "2"),
	}
	fixed, changed := enforcePlanTaskDependencies(tasks)
	if changed {
		t.Fatal("already-correct plan must be untouched")
	}
	if hasPlanDependencyCycle(fixed) {
		t.Fatal("dependency cycle detected in corrected plan")
	}
}

func TestHasPlanDependencyCycle(t *testing.T) {
	if !hasPlanDependencyCycle([]PlanTask{
		pt("a", "a.js", "1"),
		pt("b", "b.js", "2", "1"),
		pt("c", "c.js", "1", "2"), // 1 ← 2 ← 1 环
	}) {
		t.Fatal("expected cycle detection on 1↔2")
	}
	if hasPlanDependencyCycle([]PlanTask{
		pt("a", "a.js", "1"),
		pt("b", "b.js", "2", "1"),
		pt("c", "c.js", "3", "2"),
	}) {
		t.Fatal("chain 1→2→3 must be acyclic")
	}
}

func TestImplementationForTestFile(t *testing.T) {
	cases := map[string]string{
		"game.test.js":        "game.js",
		"test/core.test.mjs":  "core.js",
		"spec/foo.test.js":    "foo.js",
		"game.js":             "",
		"legacy.spec.js":      "",
		"test/foo.js":         "",
	}
	for in, want := range cases {
		got, ok := implementationForTestFile(in)
		if want == "" {
			if ok {
				t.Fatalf("%s: expected no match, got %s", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Fatalf("%s: got %s/%v, want %s", in, got, ok, want)
		}
	}
}
