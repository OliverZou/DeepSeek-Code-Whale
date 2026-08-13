package team_engine

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CompletenessVerdict helper methods
// ---------------------------------------------------------------------------

func TestCompletenessVerdict_IsComplete(t *testing.T) {
	tests := []struct {
		name    string
		verdict *CompletenessVerdict
		want    bool
	}{
		{
			name:    "nil verdict",
			verdict: nil,
			want:    false,
		},
		{
			name:    "all OK",
			verdict: &CompletenessVerdict{Verdict: "COMPLETE"},
			want:    true,
		},
		{
			name:    "incomplete",
			verdict: &CompletenessVerdict{Verdict: "INCOMPLETE"},
			want:    false,
		},
		{
			name:    "unknown verdict",
			verdict: &CompletenessVerdict{Verdict: "UNKNOWN"},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.verdict.IsComplete(); got != tt.want {
				t.Errorf("IsComplete() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompletenessVerdict_Gaps(t *testing.T) {
	tests := []struct {
		name      string
		verdict   *CompletenessVerdict
		wantGaps  int
		wantNames []string
	}{
		{
			name:     "nil verdict",
			verdict:  nil,
			wantGaps: 0,
		},
		{
			name: "all OK — no gaps",
			verdict: &CompletenessVerdict{
				Verdict: "COMPLETE",
				Dimensions: []DimensionGap{
					{Name: "scope", Status: "OK"},
					{Name: "interface", Status: "OK"},
				},
			},
			wantGaps: 0,
		},
		{
			name: "mixed — 2 gaps",
			verdict: &CompletenessVerdict{
				Verdict: "INCOMPLETE",
				Dimensions: []DimensionGap{
					{Name: "scope", Status: "GAP", Detail: "missing boundaries"},
					{Name: "interface", Status: "OK"},
					{Name: "behaviour", Status: "GAP", Detail: "algorithm unclear"},
				},
			},
			wantGaps:  2,
			wantNames: []string{"scope", "behaviour"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gaps := tt.verdict.Gaps()
			if len(gaps) != tt.wantGaps {
				t.Errorf("Gaps() returned %d gaps, want %d", len(gaps), tt.wantGaps)
			}
			for i, name := range tt.wantNames {
				if i >= len(gaps) || gaps[i].Name != name {
					t.Errorf("Gaps()[%d].Name = %q, want %q", i, gaps[i].Name, name)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// checkAndResearch — merged Step 1+2
// ---------------------------------------------------------------------------

func TestCheckAndResearch(t *testing.T) {
	tests := []struct {
		name         string
		spawnOut     string
		spawnErr     error
		wantComplete bool
		wantFacts    bool // true if DomainFacts should be populated
		wantErr      string
	}{
		{
			name: "complete goal — GCD",
			spawnOut: `{
				"verdict": "COMPLETE",
				"dimensions": [
					{"name": "scope", "status": "OK", "detail": ""},
					{"name": "interface", "status": "OK", "detail": ""},
					{"name": "behaviour", "status": "OK", "detail": ""},
					{"name": "quality", "status": "OK", "detail": ""},
					{"name": "dependencies", "status": "OK", "detail": ""},
					{"name": "constraints", "status": "OK", "detail": ""}
				]
			}`,
			wantComplete: true,
			wantFacts:    false,
		},
		{
			name: "incomplete goal — stock indicators with domain facts",
			spawnOut: `{
				"verdict": "INCOMPLETE",
				"dimensions": [
					{"name": "scope", "status": "GAP", "detail": "哪些指标？几个？"},
					{"name": "interface", "status": "GAP", "detail": "函数签名、包结构？"},
					{"name": "behaviour", "status": "GAP", "detail": "计算公式？对齐标准？"},
					{"name": "quality", "status": "GAP", "detail": "精度要求？如何验证？"},
					{"name": "dependencies", "status": "OK", "detail": ""},
					{"name": "constraints", "status": "OK", "detail": ""}
				],
				"domain_facts": {
					"domain_overview": "股票技术指标",
					"common_scope": ["MA","EMA","MACD"],
					"typical_interface": "func([]float64) []float64",
					"quality_benchmarks": ["通达信对齐"],
					"explicitly_out_of_scope": ["数据获取"]
				}
			}`,
			wantComplete: false,
			wantFacts:    true,
		},
		{
			name: "markdown code fence",
			spawnOut: "```json\n{\"verdict\": \"COMPLETE\", \"dimensions\": [" +
				"{\"name\":\"scope\",\"status\":\"OK\",\"detail\":\"\"}," +
				"{\"name\":\"interface\",\"status\":\"OK\",\"detail\":\"\"}," +
				"{\"name\":\"behaviour\",\"status\":\"OK\",\"detail\":\"\"}," +
				"{\"name\":\"quality\",\"status\":\"OK\",\"detail\":\"\"}," +
				"{\"name\":\"dependencies\",\"status\":\"OK\",\"detail\":\"\"}," +
				"{\"name\":\"constraints\",\"status\":\"OK\",\"detail\":\"\"}" +
				"]}\n```",
			wantComplete: true,
			wantFacts:    false,
		},
		{
			name:     "empty output",
			spawnOut: "",
			wantErr:  "output_empty",
		},
		{
			name:     "no JSON in output",
			spawnOut: "The goal looks fine to me.",
			wantErr:  "no JSON found",
		},
		{
			name:     "spawner error",
			spawnErr: assertAnError{},
			wantErr:  "output_empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := newTestEngine(t)
			defer eng.Close()

			eng.Runner.spawner = &mockSpawner{output: tt.spawnOut, err: tt.spawnErr}

			p := NewPlanner(eng.Runner)
			verdict, err := p.checkAndResearch("test goal", t.TempDir(), 30, "test-model")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("checkAndResearch() = %+v, want error containing %q", verdict, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("checkAndResearch() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkAndResearch() unexpected error: %v", err)
			}
			if verdict.IsComplete() != tt.wantComplete {
				t.Errorf("verdict.IsComplete() = %v, want %v", verdict.IsComplete(), tt.wantComplete)
			}
			if tt.wantFacts {
				if verdict.DomainFacts == nil {
					t.Error("DomainFacts should be populated for INCOMPLETE verdict")
				} else if len(verdict.DomainFacts.CommonScope) == 0 {
					t.Error("DomainFacts.CommonScope is empty")
				}
			} else {
				if verdict.DomainFacts != nil && verdict.DomainFacts.DomainOverview != "" {
					t.Error("DomainFacts should be empty for COMPLETE verdict")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Elaborate — multi-step pipeline (merged Step 1+2 → Step 3)
// ---------------------------------------------------------------------------

func TestElaborate_CompleteShortCircuits(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	completeVerdict := `{
		"verdict": "COMPLETE",
		"dimensions": [
			{"name": "scope", "status": "OK", "detail": ""},
			{"name": "interface", "status": "OK", "detail": ""},
			{"name": "behaviour", "status": "OK", "detail": ""},
			{"name": "quality", "status": "OK", "detail": ""},
			{"name": "dependencies", "status": "OK", "detail": ""},
			{"name": "constraints", "status": "OK", "detail": ""}
		]
	}`
	eng.Runner.spawner = &mockSpawner{output: completeVerdict}

	p := NewPlanner(eng.Runner)
	goal := "Write a GCD function in Go"
	elaborated, err := p.Elaborate(goal, t.TempDir(), 30)

	if err != nil {
		t.Fatalf("Elaborate() unexpected error: %v", err)
	}
	if elaborated != goal {
		t.Errorf("Elaborate() = %q, want original goal %q (COMPLETE should short-circuit)", elaborated, goal)
	}
}

func TestElaborate_IncompleteFullPipeline(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	// Merged Step 1+2 returns INCOMPLETE + domain_facts in one call.
	// Step 3 (spec) is the second planner call.
	eng.Runner.spawner = &mockSpawner{
		roleSeq: map[string][]string{
			"planner": {
				// Merged Step 1+2: completeness check + domain facts
				`{"verdict": "INCOMPLETE", "dimensions": [
					{"name": "scope", "status": "GAP", "detail": "哪些指标？"},
					{"name": "interface", "status": "GAP", "detail": "函数签名？"},
					{"name": "behaviour", "status": "OK", "detail": ""},
					{"name": "quality", "status": "OK", "detail": ""},
					{"name": "dependencies", "status": "OK", "detail": ""},
					{"name": "constraints", "status": "OK", "detail": ""}
				], "domain_facts": {
					"domain_overview": "股票技术指标库",
					"common_scope": ["MA","EMA","MACD"],
					"typical_interface": "func(indicator, []float64, ...int) ([]float64, error)",
					"quality_benchmarks": ["通达信对齐"],
					"explicitly_out_of_scope": ["数据获取","可视化"]
				}}`,

				// Step 3: final YAML spec
				"```yaml\n" +
					"goal_summary: 用 Go 实现股票技术指标计算库，包含 MA/EMA/MACD 等常用指标\n" +
					"scope:\n" +
					"  included:\n" +
					"    - MA\n" +
					"    - EMA\n" +
					"    - MACD\n" +
					"  excluded:\n" +
					"    - 数据获取\n" +
					"    - 可视化\n" +
					"```",
			},
		},
	}

	p := NewPlanner(eng.Runner)
	goal := "写一个股票常用指标库，Go语言"
	elaborated, err := p.Elaborate(goal, t.TempDir(), 30)

	if err != nil {
		t.Fatalf("Elaborate() unexpected error: %v", err)
	}
	if elaborated == goal {
		t.Error("Elaborate() returned original goal, expected elaborated spec")
	}
	if !strings.Contains(elaborated, "MA") {
		t.Errorf("Elaborate() output doesn't contain expected indicators: %s", elaborated)
	}
}

func TestElaborate_CheckFailureFallsBackToRaw(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	// Merged Step 1+2 fails (empty output), pipeline treats as incomplete
	// and tries Step 3 — which also fails.  Falls back to raw goal.
	eng.Runner.spawner = &mockSpawner{
		roleSeq: map[string][]string{
			"planner": {
				"", // Merged Step 1+2 — FAILS
				"", // Step 3 spec — also FAILS
			},
		},
	}

	p := NewPlanner(eng.Runner)
	goal := "some goal"
	elaborated, err := p.Elaborate(goal, t.TempDir(), 30)

	if err != nil {
		t.Fatalf("Elaborate() unexpected error: %v", err)
	}
	if elaborated != goal {
		t.Errorf("Elaborate() = %q, want original goal %q (full failure should fall back to raw)", elaborated, goal)
	}
}

// ---------------------------------------------------------------------------
// gapNames helper
// ---------------------------------------------------------------------------

func TestGapNames(t *testing.T) {
	gaps := []DimensionGap{
		{Name: "scope", Status: "GAP"},
		{Name: "interface", Status: "GAP"},
		{Name: "behaviour", Status: "OK"},
	}
	names := gapNames(gaps)
	if len(names) != 3 {
		t.Fatalf("gapNames() returned %d names, want 3", len(names))
	}
	if names[0] != "scope" || names[1] != "interface" || names[2] != "behaviour" {
		t.Errorf("gapNames() = %v, want [scope interface behaviour]", names)
	}
}
