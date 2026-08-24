package team_engine

import (
	"testing"
	"time"
)

// TestIsOverloadedTask locks the「上帝任务」检测启发式：一个任务描述如果同时
// 承担「集成/联调」和「多端/兼容验证」两类职责，应被判定为超重、需要再拆。
// 只含其中一类的任务（如纯整合核心逻辑，或纯兼容验证）不命中。
func TestIsOverloadedTask(t *testing.T) {
	cases := []struct {
		name string
		desc string
		want bool
	}{
		{
			name: "集成+多端兼容合并（2048 集成测试层）",
			desc: "实现重新开始按钮逻辑，重置棋盘、分数和游戏状态。整合所有模块，确保在Chrome、Firefox、Safari、Edge最新版上运行正常，响应式布局在320px-1920px无破版。",
			want: true,
		},
		{
			name: "仅整合核心逻辑（无兼容验证）",
			desc: "管理游戏状态（进行中/胜利/失败），处理有效移动判定、新方块生成时机、胜利/失败状态切换。整合所有核心逻辑函数。",
			want: false,
		},
		{
			name: "纯功能叶子（核心逻辑）",
			desc: "创建4x4棋盘数组，实现generateRandomTile函数：在空位随机生成新方块，90%概率为2，10%概率为4。导出供测试使用。",
			want: false,
		},
		{
			name: "纯兼容验证（无集成词）",
			desc: "确保页面在Chrome、Firefox最新版上运行正常，响应式布局无破版。",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOverloadedTask(tc.desc); got != tc.want {
				t.Fatalf("isOverloadedTask(%q) = %v, want %v", tc.desc, got, tc.want)
			}
		})
	}
}

// TestSplitTaskIntoChildren 锁定拆分的机械动作（self-split 与 tool-cap 拆分
// 共同依赖的落点）：父任务标记 done、子任务带正确 ParentIDs/BatchID/MasterTaskID。
func TestSplitTaskIntoChildren(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	parent, err := eng.CreateTask("上帝任务", "整合所有模块并确保多端兼容", RoleDeveloper, "", nil, 0, t.TempDir(), "", "batch-1", "master-1")
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// 模拟 worker 已产出（真实流程里拆分发生在 worker 写入 output.md 之后）。
	if err := eng.Whiteboard.WriteOutput(parent.ID, "partial output before split"); err != nil {
		t.Fatalf("write parent output: %v", err)
	}

	childPlan := []PlanTask{
		{Title: "leaf A", Description: "实现模块导出", Role: "developer", Output: "a.js"},
		{Title: "leaf B", Description: "多端兼容验证", Role: "developer", Output: "b.js"},
	}
	if !eng.splitTaskIntoChildren(parent, childPlan, parent.Workdir, "test split") {
		t.Fatal("splitTaskIntoChildren returned false")
	}

	got, err := eng.Store.GetTask(parent.ID)
	if err != nil || got == nil {
		t.Fatalf("get parent: %v", err)
	}
	if got.State != TaskStateDone {
		t.Errorf("parent state = %s, want done", got.State)
	}

	children, err := eng.Store.ListTasksByParent(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	for _, c := range children {
		if len(c.ParentIDs) != 1 || c.ParentIDs[0] != parent.ID {
			t.Errorf("child %s parentIDs = %v, want [%s]", c.ID[:8], c.ParentIDs, parent.ID)
		}
		if c.BatchID != "batch-1" {
			t.Errorf("child %s batchID = %q, want batch-1", c.ID[:8], c.BatchID)
		}
		if c.MasterTaskID != "master-1" {
			t.Errorf("child %s masterTaskID = %q, want master-1", c.ID[:8], c.MasterTaskID)
		}
	}
}

// TestSplitOverloadedPlanTasks 锁定 A 的后置拆分：超重任务被重新分解为叶子，
// 普通任务原样保留。
func TestSplitOverloadedPlanTasks(t *testing.T) {
	spawner := &mockSpawner{
		roleOutputs: map[string]string{
			"planner": `[{"title":"leaf A","description":"实现模块导出","role":"developer"},{"title":"leaf B","description":"多端兼容验证","role":"developer"}]`,
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	tasks := []PlanTask{
		{Title: "上帝任务", Description: "整合所有模块，确保在Chrome、Firefox最新版运行正常", Role: "developer", BatchID: "b1", BatchLabel: "集成测试层"},
		{Title: "普通叶子", Description: "实现棋盘初始化", Role: "developer", BatchID: "b2", BatchLabel: "核心逻辑层"},
	}

	got := eng.splitOverloadedPlanTasks(tasks, t.TempDir(), 30*time.Second, "test-model")

	if len(got) != 3 {
		t.Fatalf("split result = %d tasks, want 3 (1 overloaded→2 leaves + 1 passthrough)", len(got))
	}
	// 拆分出的两个叶子应继承父任务的 BatchID/BatchLabel。
	leafCount := 0
	for _, pt := range got {
		if pt.Title == "leaf A" || pt.Title == "leaf B" {
			leafCount++
			if pt.BatchID != "b1" {
				t.Errorf("leaf %q batchID = %q, want b1", pt.Title, pt.BatchID)
			}
			if pt.BatchLabel != "集成测试层" {
				t.Errorf("leaf %q batchLabel = %q, want 集成测试层", pt.Title, pt.BatchLabel)
			}
		}
	}
	if leafCount != 2 {
		t.Errorf("leaf count = %d, want 2", leafCount)
	}
	// 普通叶子应原样保留。
	foundPassthrough := false
	for _, pt := range got {
		if pt.Title == "普通叶子" {
			foundPassthrough = true
		}
	}
	if !foundPassthrough {
		t.Error("passthrough task missing from result")
	}
}
