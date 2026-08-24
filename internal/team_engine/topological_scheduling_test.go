package team_engine

import (
	"context"
	"testing"
)

// TestTopologicalScheduling_FailedBatchDoesNotAbortSiblings verifies the
// P1-graph failure semantics: a batch with an unsatisfiable (dangling)
// dependency is marked Failed, but independent sibling batches still run to
// completion instead of triggering the old global abort.
func TestTopologicalScheduling_FailedBatchDoesNotAbortSiblings(t *testing.T) {
	workerOutput := "package main\n\nfunc main() {}\n"

	eng := newMockEngine(t, map[string]string{
		"software-engineer":  workerOutput,
		"software-architect": workerOutput,
		"verifier":           "TOOLS USED: read_file\nVERDICT: PASS\nEVIDENCE: ok\n## FINDINGS\n---json\n[]\n---",
	})
	defer eng.Close()

	workdir := t.TempDir()
	// 不写 go.mod：mock spawner 只把 worker 产出写到 output.md、不落盘到 out/，
	// 写 go.mod 会让 propagate 后的机械验证 `go test ./...` 在无 .go 文件时报
	// "no packages to test" 而误判任务失败。本测试只关注拓扑调度失败语义，
	// 不验证 Go 编译，故保持 workdir 无构建配置以跳过机械门。

	mt, err := eng.CreateMasterTask("three batches, one dangling dep", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	plan := []PlanTask{
		{Title: "A", Description: "independent A", Output: "a.go", Role: "software-engineer", BatchID: "batch-a", BatchLabel: "A", MaxCycles: 1},
		{Title: "B", Description: "independent B", Output: "b.go", Role: "software-architect", BatchID: "batch-b", BatchLabel: "B", MaxCycles: 1},
		{Title: "C", Description: "depends on missing batch", Output: "c.go", Role: "software-engineer", BatchID: "batch-c", BatchLabel: "C", DependsOnBatch: FlexibleStringSlice{"ghost"}, MaxCycles: 1},
	}

	batches, err := eng.TeamCycle(context.Background(), "three batches", workdir, mt.ID, plan...)
	if err != nil {
		t.Fatalf("plan and run: %v", err)
	}

	status := map[string]BatchStatus{}
	for _, b := range batches {
		status[b.ID] = b.Status
	}

	if status["batch-c"] != BatchStatusFailed {
		t.Errorf("batch-c: expected %s, got %s", BatchStatusFailed, status["batch-c"])
	}
	if status["batch-a"] != BatchStatusPassed {
		t.Errorf("batch-a: expected %s (sibling continues), got %s", BatchStatusPassed, status["batch-a"])
	}
	if status["batch-b"] != BatchStatusPassed {
		t.Errorf("batch-b: expected %s (sibling continues), got %s", BatchStatusPassed, status["batch-b"])
	}
}
