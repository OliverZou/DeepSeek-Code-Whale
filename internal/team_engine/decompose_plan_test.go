package team_engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDecomposePlan_WritesSplitPlanJSON 锁定 v21 串行根因的回归：
// decompose 后 splitOverloadedPlanTasks 可能把「上帝任务」再拆成叶子，
// 叶子（含新 title）成为 createBatchesFromPlan 的输入、写入磁盘 Task 记录；
// plan.json 必须是同一份「最终计划」（split 后），否则 team_run_plan 的
// recoverMasterBatches 按 title 匹配 Task 记录失败 → StartPlanRun 拒绝启动 →
// leader 回退逐个 team_run（串行、无并行调度）。
//
// 修复前：writePlanJSON 在 split 之前执行，plan.json 停留 split 前原文，
// recoverMasterBatches 返回 "not found on disk"（本测试第 3 段断言失败）。
func TestDecomposePlan_WritesSplitPlanJSON(t *testing.T) {
	spawner := &mockSpawner{
		roleSeq: map[string][]string{
			"planner": {
				completeVerdictJSON, // elaborate check → COMPLETE，跳过 spec
				`{"tasks":[` +
					`{"title":"实现 game.js 核心逻辑","description":"实现2048移动合并胜负逻辑","role":"developer","batch_id":"1","batch_label":"实现层","output":"game.js"},` +
					`{"title":"实现 style.css 棋盘样式","description":"整合所有模块，响应式布局在320px-1920px无破版","role":"developer","batch_id":"1","batch_label":"实现层","output":"style.css"}` +
					`]}`,
				// split 的叶子：title 与原文不同（v21 里「实现 style.css 响应式棋盘与方块样式」
				// →「编写 style.css 实现 2048 视觉样式与响应式布局」）。
				`[{"title":"编写 style.css 实现 2048 视觉样式与响应式布局","description":"写CSS实现2048视觉样式与响应式布局","role":"developer","batch_id":"1","batch_label":"实现层","output":"style.css"},{"title":"编写 style.css 动效与交互细节","description":"写CSS实现tile动画与hover效果","role":"developer","batch_id":"1","batch_label":"实现层","output":"style-anim.css"}]`,
			},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	workdir := t.TempDir()
	mt, err := eng.CreateMasterTask("2048 游戏", workdir, "")
	if err != nil {
		t.Fatalf("create master task: %v", err)
	}

	leader := NewLeader(eng.Runner)
	planTasks, _, _, _, err := eng.decomposePlan("2048 游戏", workdir, mt.ID, leader, 30*time.Second, "test-model", "")
	if err != nil {
		t.Fatalf("decompose plan: %v", err)
	}
	if len(planTasks) != 3 {
		t.Fatalf("plan tasks = %d, want 3 (1 passthrough + 2 leaves from split)", len(planTasks))
	}
	foundLeaf := false
	for _, pt := range planTasks {
		if pt.Title == "编写 style.css 实现 2048 视觉样式与响应式布局" {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		t.Fatalf("split leaf missing from plan: %+v", planTasks)
	}

	// plan.json 必须等于 split 后的计划（与 Task 记录同源）。
	raw, err := os.ReadFile(filepath.Join(eng.Whiteboard.MasterDir(mt.ID), "plan.json"))
	if err != nil {
		t.Fatalf("read plan.json: %v", err)
	}
	var plan struct {
		Tasks []struct {
			Title string `json:"title"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatalf("parse plan.json: %v", err)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("plan.json tasks = %d, want 3", len(plan.Tasks))
	}
	if plan.Tasks[0].Title != "实现 game.js 核心逻辑" || plan.Tasks[1].Title != "编写 style.css 实现 2048 视觉样式与响应式布局" || plan.Tasks[2].Title != "编写 style.css 动效与交互细节" {
		t.Fatalf("plan.json tasks = %+v, want [game.js, split leaves] (split must be reflected)", plan.Tasks)
	}

	// 恢复路径（team_run_plan → StartPlanRun 的前半）：按 title 匹配必须成功。
	batches, err := eng.createBatchesFromPlan(planTasks, "2048 游戏", workdir, mt.ID, "")
	if err != nil {
		t.Fatalf("create batches: %v", err)
	}
	recovered, err := eng.recoverMasterBatches(mt.ID, "2048 游戏", workdir)
	if err != nil {
		t.Fatalf("recover batches (v21 failure mode): %v", err)
	}
	if len(recovered) != len(batches) {
		t.Fatalf("recovered %d batches, want %d", len(recovered), len(batches))
	}
	ids := map[string]bool{}
	for _, b := range batches {
		for _, tk := range b.Tasks {
			ids[tk.ID] = true
		}
	}
	for _, b := range recovered {
		for _, tk := range b.Tasks {
			if !ids[tk.ID] {
				t.Fatalf("recovered task %s not in original batches", tk.ID[:8])
			}
		}
	}
}
