package team_engine

import (
	"strings"
	"testing"
)

// TestCheckOutputConflicts guards the mechanical gate: a deliverable file may be
// owned by exactly one task. This is the shape of the v26 "stale index.html"
// root cause moved into the new write-to-workspace model — two parallel workers
// writing the same path would overwrite each other, so the plan itself is
// rejected up front instead of being left to runtime or live decision-making.
func TestCheckOutputConflicts(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []*Task
		wantErr string // empty = no conflict expected
	}{
		{
			"disjoint outputs",
			[]*Task{
				{ID: "t1", Title: "骨架", Role: RoleDeveloper, Output: "index.html"},
				{ID: "t2", Title: "样式", Role: RoleDeveloper, Output: "style.css"},
			},
			"",
		},
		{
			"same file two tasks",
			[]*Task{
				{ID: "t1", Title: "页面骨架", Output: "index.html"},
				{ID: "t2", Title: "DOM 接线", Output: "index.html"},
			},
			"index.html",
		},
		{
			"multi-file output intersects other task",
			[]*Task{
				{ID: "t1", Title: "游戏", Output: "game.js, game.test.js"},
				{ID: "t2", Title: "测试", Output: "game.test.js"},
			},
			"game.test.js",
		},
		{
			"case-insensitive variant",
			[]*Task{
				{ID: "t1", Title: "骨架", Output: "index.html"},
				{ID: "t2", Title: "补全", Output: "Index.html"},
			},
			"index.html",
		},
		{
			"path variants normalize",
			[]*Task{
				{ID: "t1", Title: "模块", Output: "src/api.js"},
				{ID: "t2", Title: "模块改", Output: "src/./api.js"},
			},
			"src/api.js",
		},
		{
			"duplicate within one task is fine",
			[]*Task{
				{ID: "t1", Title: "游戏", Output: "game.js, game.js"},
			},
			"",
		},
		{
			"empty outputs",
			[]*Task{
				{ID: "t1", Title: "验证", Output: ""},
				{ID: "t2", Title: "验证二", Output: ""},
			},
			"",
		},
	}
	for _, c := range cases {
		err := checkOutputConflicts(c.tasks)
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

// TestCheckPlanTaskOutputConflicts covers the source gate: the check runs inside
// decomposePlan before plan.json is written, so a conflicting plan never gets
// persisted or executed.
func TestCheckPlanTaskOutputConflicts(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []PlanTask
		wantErr string
	}{
		{
			"disjoint",
			[]PlanTask{
				{Title: "骨架", Output: "index.html"},
				{Title: "样式", Output: "style.css"},
				{Title: "游戏", Output: "game.js, game.test.js"},
			},
			"",
		},
		{
			"same file split across tasks",
			[]PlanTask{
				{Title: "页面骨架", Output: "index.html"},
				{Title: "DOM 接线", Output: "index.html"},
			},
			"index.html",
		},
		{
			"multi-file output intersects",
			[]PlanTask{
				{Title: "游戏", Output: "game.js, game.test.js"},
				{Title: "测试", Output: "game.test.js"},
			},
			"game.test.js",
		},
	}
	for _, c := range cases {
		err := checkPlanTaskOutputConflicts(c.tasks)
		if c.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

// TestCheckBatchOutputConflicts covers the recover path (StartPlanRun).
func TestCheckBatchOutputConflicts(t *testing.T) {
	batches := []*Batch{
		{ID: "b1", Tasks: []*Task{{ID: "t1", Title: "甲", Output: "index.html"}}},
		{ID: "b2", Tasks: []*Task{{ID: "t2", Title: "乙", Output: "index.html"}}},
	}
	if err := checkBatchOutputConflicts(batches); err == nil {
		t.Fatal("want conflict across batches, got nil")
	}
}
