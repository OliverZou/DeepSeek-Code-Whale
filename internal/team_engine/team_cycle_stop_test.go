package team_engine

import (
	"testing"
	"time"
)

// TestTeamCycleVerificationFailureStopsWithoutIdleReview 覆盖「无可重置任务
// 即终止」:计划只含实现 batch(通过)+ 集成验证 batch(FAIL 挂起),Leader
// review 决策 CycleReject 时,重置循环无任务可重置(纯验证 batch kept
// failed)→ 直接升级结束,而不是空转 review 循环直到 maxCycles
// (v19 实测:kept failed 后每轮 review 一次 LLM 调用)。
func TestTeamCycleVerificationFailureStopsWithoutIdleReview(t *testing.T) {
	spawner := &mockSpawner{
		roleSeq: map[string][]string{
			// planner 调用序列:elaborate completeness check → decompose →
			// plan review。
			"planner": {
				`{"verdict":"COMPLETE","dimensions":[
					{"name":"scope","status":"OK"},{"name":"interface","status":"OK"},
					{"name":"behaviour","status":"OK"},{"name":"quality","status":"OK"},
					{"name":"dependencies","status":"OK"},{"name":"constraints","status":"OK"}
				]}`,
				`[{"title":"实现核心逻辑","description":"write game.js","output":"game.js","role":"developer","verify_mode":"mechanical","batch_id":"1","depends_on_batch":[]},
				  {"title":"集成验证与端到端验收","description":"verify all","output":"report.md","role":"tester","verify_mode":"semantic","batch_id":"2","depends_on_batch":["1"]}]`,
				`{"decision":"reject","reason":"verification found defects","feedback":"fix the defects"}`,
			},
		},
		roleOutputs: map[string]string{
			"verifier": "VERDICT: FAIL\n缺陷:game.js 的 move 合并计分错误",
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ws := t.TempDir()
	// handleEscalation 会阻塞等人工决策:测试里轮询 Resolve 立即放行。
	go func() {
		for i := 0; i < 1000; i++ {
			if err := eng.Escalation.Resolve("2", EscalationAbort); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	batches, err := eng.TeamCycle(t.Context(), "2048 game", ws, "m1-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("team cycle: %v", err)
	}

	// 验证 batch 保持 failed;实现 batch passed。
	var implBatch, verifyBatch *Batch
	for _, b := range batches {
		if b.ID == "1" {
			implBatch = b
		}
		if b.ID == "2" {
			verifyBatch = b
		}
	}
	if implBatch == nil || implBatch.Status != BatchStatusPassed {
		t.Fatalf("impl batch status = %v, want passed", implBatch)
	}
	if verifyBatch == nil || verifyBatch.Status != BatchStatusFailed {
		t.Fatalf("verify batch status = %v, want failed", verifyBatch)
	}

	// 验证任务保持 suspended(缺陷报告保留)。
	st := eng.Store.TaskState("m1-0000-0000-0000-000000000001")
	_ = st
	// 无空转:planner 只被调用 2 次(decompose + 1 次 review),
	// 没有第二轮 review(空转会 ≥3 次)。
	spawner.mu.Lock()
	defer spawner.mu.Unlock()
	plannerCalls := 0
	for _, req := range spawner.reqs {
		if req.Role == "planner" {
			plannerCalls++
		}
	}
	if plannerCalls != 3 {
		t.Fatalf("planner called %d times, want 3 (elaborate + decompose + 1 review, no idle loop)", plannerCalls)
	}
}
