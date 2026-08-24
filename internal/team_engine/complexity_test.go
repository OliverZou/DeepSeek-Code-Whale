package team_engine

import (
	"strings"
	"testing"
)

func TestLexicalComplexity(t *testing.T) {
	tests := []struct {
		goal string
		want string
	}{
		{"写一个 2048 网页游戏", "simple"},
		{"重构整个支付系统", "complex"},
		{"把单体应用迁移到微服务", "complex"},
		{"写一个包含用户、订单、库存、物流、支付五个模块的完整电商平台", "medium"}, // 长但无关键词
	}
	for _, tt := range tests {
		if got := lexicalComplexity(tt.goal); got != tt.want {
			t.Errorf("lexicalComplexity(%q) = %q, want %q", tt.goal, got, tt.want)
		}
	}
}

func TestDecomposePromptComplexity(t *testing.T) {
	t.Run("simple injects batch constraint", func(t *testing.T) {
		p := DecomposePrompt("写一个 2048 游戏", "simple")
		if !strings.Contains(p, "1 个 batch") {
			t.Errorf("simple decompose prompt should constrain to 1 batch:\n%s", p)
		}
	})
	t.Run("complex injects parallelism hint", func(t *testing.T) {
		p := DecomposePrompt("重构平台", "complex")
		if !strings.Contains(p, "多个 batch 并行") {
			t.Errorf("complex decompose prompt should allow parallel batches:\n%s", p)
		}
	})
	t.Run("no complexity defaults gracefully", func(t *testing.T) {
		p := DecomposePrompt("test goal")
		if strings.Contains(p, "目标规模评估为 simple") {
			t.Errorf("no-complexity prompt should not claim simple:\n%s", p)
		}
		if !strings.Contains(p, "目标规模未评估") {
			t.Errorf("no-complexity prompt should state '未评估':\n%s", p)
		}
	})

	t.Run("depends_on semantics discourage serial chains", func(t *testing.T) {
		p := DecomposePrompt("写一个 2048 游戏", "medium")
		if !strings.Contains(p, "depends_on_batch 使用规则") {
			t.Errorf("decompose prompt should document depends_on_batch rules:\n%s", p)
		}
		if strings.Contains(p, "不同 batch 串行") {
			t.Errorf("decompose prompt must not claim batches run serially:\n%s", p)
		}
		if !strings.Contains(p, "必须 depends_on 产出任务") {
			t.Errorf("decompose prompt should tell tests to depend on implementation:\n%s", p)
		}
		if !strings.Contains(p, "合并为同一个任务") {
			t.Errorf("decompose prompt should merge impl + tests into one task:\n%s", p)
		}
	})
}

func TestElaborateFull_ReturnsComplexity(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	eng.Runner.spawner = &mockSpawner{output: `{
		"verdict": "COMPLETE",
		"dimensions": [
			{"name": "scope", "status": "OK", "detail": ""},
			{"name": "interface", "status": "OK", "detail": ""},
			{"name": "behaviour", "status": "OK", "detail": ""},
			{"name": "quality", "status": "OK", "detail": ""},
			{"name": "dependencies", "status": "OK", "detail": ""},
			{"name": "constraints", "status": "OK", "detail": ""}
		]
	}`}

	p := NewPlanner(eng.Runner)
	goal := "Write a GCD function in Go"
	elaborated, complexity, err := p.ElaborateFull(goal, t.TempDir(), 30)
	if err != nil {
		t.Fatalf("ElaborateFull() unexpected error: %v", err)
	}
	if elaborated != goal {
		t.Errorf("ElaborateFull() = %q, want original goal %q", elaborated, goal)
	}
	// Complexity is lexical (deterministic): the short keyword-free goal
	// yields "simple".
	if complexity != "simple" {
		t.Errorf("ElaborateFull() complexity = %q, want %q", complexity, "simple")
	}
}

func TestElaborateFull_FallsBackToLexical(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	// Completeness check fails (empty output) → lexical fallback. The goal
	// carries "重构"+"微服务" keywords → complex.
	eng.Runner.spawner = &mockSpawner{output: ""}

	p := NewPlanner(eng.Runner)
	goal := "重构整个支付系统为微服务架构"
	elaborated, complexity, err := p.ElaborateFull(goal, t.TempDir(), 30)
	if err != nil {
		t.Fatalf("ElaborateFull() unexpected error: %v", err)
	}
	if elaborated != goal {
		t.Errorf("ElaborateFull() = %q, want original goal %q", elaborated, goal)
	}
	if complexity != "complex" {
		t.Errorf("ElaborateFull() complexity = %q, want %q", complexity, "complex")
	}
}
