package team_engine

import (
	"testing"
)

// ---------------------------------------------------------------------------
// extractJSON  — 从文本中提取 JSON 数组 [...]
// ---------------------------------------------------------------------------

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
	}{
		{
			name:   "raw JSON array",
			input:  `[{"title": "Task 1", "role": "developer"}]`,
			want:   `[{"title": "Task 1", "role": "developer"}]`,
		},
		{
			name:   "markdown json code fence",
			input:  "```json\n[{\"title\": \"Task 1\", \"role\": \"developer\"}]\n```",
			want:   `[{"title": "Task 1", "role": "developer"}]`,
		},
		{
			name:   "plain code fence",
			input:  "```\n[{\"title\": \"Task 1\", \"role\": \"developer\"}]\n```",
			want:   `[{"title": "Task 1", "role": "developer"}]`,
		},
		{
			name:   "JSON array embedded in natural language",
			input:  "Here is my plan:\n\n```json\n[{\"title\": \"Build API\", \"role\": \"developer\"}]\n```\n\nLet me know if this works.",
			want:   `[{"title": "Build API", "role": "developer"}]`,
		},
		{
			name:   "natural language only — no JSON",
			input:  "The plan has been executed. All tasks completed successfully.",
			want:   "",
		},
		{
			name:   "natural language with JSON object — not array",
			input:  `{"decision": "accept", "reason": "looks good"}`,
			want:   "",
		},
		{
			name:   "JSON array inside natural language text no fence",
			input:  `I have a plan: [{"title":"A","role":"dev"}] and that's it.`,
			want:   `[{"title":"A","role":"dev"}]`,
		},
		{
			name:   "empty input",
			input:  "",
			want:   "",
		},
		{
			name:   "whitespace only",
			input:  "   \n\n  ",
			want:   "",
		},
		{
			name:   "multiple JSON arrays — returns first balanced array (not greedy)",
			input:  `[{"title":"First"}] and then [{"title":"Second"}]`,
			want:   `[{"title":"First"}]`,
		},
		{
			name:   "malformed JSON array — no closing bracket",
			input:  `[{"title": "Broken"`,
			want:   "", // no closing ] means no valid JSON array detected
		},
		{
			name: "Go code with array types in description — not confused as JSON",
			input: `Here is the plan:
[
  {"title": "Task 1", "description": "Define Board [15][15]int and Stone type"},
  {"title": "Task 2", "description": "Use dirs = [4][2]int for directions"}
]`,
			want: `[
  {"title": "Task 1", "description": "Define Board [15][15]int and Stone type"},
  {"title": "Task 2", "description": "Use dirs = [4][2]int for directions"}
]`,
		},
		{
			name: "truncated JSON — ParsePlanTasks uses repairJSON path",
			input: `[{"title": "Incomplete task", "description": "This was cut off`,
			want: "", // extractJSON requires closing bracket; repairJSON handles it
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.input)
			if got != tt.want {
				t.Errorf("extractJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractJSONObject  — 从文本中提取 JSON 对象 {...}
// ---------------------------------------------------------------------------

func TestExtractJSONObject(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
	}{
		{
			name:   "raw JSON object",
			input:  `{"decision": "accept", "reason": "looks good"}`,
			want:   `{"decision": "accept", "reason": "looks good"}`,
		},
		{
			name:   "markdown json code fence",
			input:  "```json\n{\"decision\": \"reject\", \"reason\": \"needs work\"}\n```",
			want:   `{"decision": "reject", "reason": "needs work"}`,
		},
		{
			name:   "plain code fence",
			input:  "```\n{\"decision\": \"escalate\", \"reason\": \"ambiguous\"}\n```",
			want:   `{"decision": "escalate", "reason": "ambiguous"}`,
		},
		{
			name:   "JSON object embedded in natural language",
			input:  "My decision:\n\n```json\n{\"decision\": \"accept\", \"reason\": \"all good\"}\n```\n\nProceeding.",
			want:   `{"decision": "accept", "reason": "all good"}`,
		},
		{
			name:   "natural language only — no JSON",
			input:  "The batch passed. Everything looks good.",
			want:   "",
		},
		{
			name:   "JSON array with embedded object — extracts the object",
			input:  `[{"title": "Task 1"}]`,
			want:   `{"title": "Task 1"}`,
		},
		{
			name:   "JSON object inside text without fence",
			input:  `Result: {"decision":"accept","reason":"ok"} end`,
			want:   `{"decision":"accept","reason":"ok"}`,
		},
		{
			name:   "empty input",
			input:  "",
			want:   "",
		},
		{
			name:   "whitespace only",
			input:  "   \n  ",
			want:   "",
		},
		{
			name:   "malformed JSON object — no closing brace",
			input:  `{"decision": "accept"`,
			want:   "", // no closing } means no valid JSON object detected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSONObject(tt.input)
			if got != tt.want {
				t.Errorf("extractJSONObject() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ParsePlanTasks  — 解析 AI 输出的 PlanTask JSON 数组
// ---------------------------------------------------------------------------

func TestParsePlanTasks(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int           // expected number of tasks; -1 means expect error
		wantErr string        // substring of expected error
	}{
		{
			name:    "single task",
			input:   `[{"title": "Build API", "description": "Write the API", "role": "developer"}]`,
			wantLen: 1,
		},
		{
			name: "multiple tasks with batch labels",
			input: `[
				{"title": "Research", "description": "Do research", "role": "researcher", "batch_id": "phase-1", "batch_label": "Research Phase"},
				{"title": "Implement", "description": "Build it", "role": "developer", "batch_id": "phase-2", "batch_label": "Implementation Phase", "depends_on_batch": ["phase-1"]}
			]`,
			wantLen: 2,
		},
		{
			name:    "markdown json code fence",
			input:   "```json\n[{\"title\": \"Task 1\", \"description\": \"desc\", \"role\": \"developer\"}]\n```",
			wantLen: 1,
		},
		{
			name:    "plain code fence",
			input:   "```\n[{\"title\": \"Task 1\", \"description\": \"desc\", \"role\": \"developer\"}]\n```",
			wantLen: 1,
		},
		{
			name:    "depends_on_index normalization",
			input:   `[{"title": "Task A", "description": "First", "role": "dev", "depends_on_index": -1}]`,
			wantLen: 1,
		},
		{
			name:    "with profile and verifier_focus",
			input:   `[{"title": "Test", "description": "Test it", "role": "tester", "profile": "readonly", "verifier_focus": "correctness"}]`,
			wantLen: 1,
		},
		{
			name:    "with concurrency and max_cycles",
			input:   `[{"title": "Build", "description": "Build it", "role": "developer", "concurrency": 3, "max_cycles": 5}]`,
			wantLen: 1,
		},
		{
			name:    "natural language only — error",
			input:   "The plan has been executed. All tasks passed and the goal is complete.",
			wantErr: "no JSON found",
		},
		{
			name:    "empty JSON array — error",
			input:   `[]`,
			wantErr: "empty plan",
		},
		{
			name:    "task missing role — error",
			input:   `[{"title": "Task 1", "description": "desc"}]`,
			wantErr: "missing role",
		},
		{
			name:    "empty input — error",
			input:   "",
			wantErr: "no JSON found",
		},
		{
			name:    "JSON object not array — error",
			input:   `{"title": "Task 1", "role": "developer"}`,
			wantErr: "no JSON found",
		},
		{
			name:    "multiple dependencies via depends_on_indices",
			input:   `[{"title": "Final", "description": "Final step", "role": "dev", "depends_on_indices": [0, 1]}]`,
			wantLen: 1,
		},
		{
			name:    "truncated JSON — repairJSON closes unclosed strings and brackets",
			input:   `[{"title": "Incomplete", "role": "dev", "description": "Cut off mid-string`,
			wantLen: 1,
		},
		{
			name:    "truncated JSON with unclosed object",
			input:   `[{"title": "First", "role": "dev"}, {"title": "Second", "role": "tester"`,
			wantLen: 2,
		},
		{
			name:    "Go code in descriptions — not confused by [15][15]int",
			input:   `[{"title": "Board", "description": "Use Board [15][15]int", "role": "dev"}, {"title": "Dirs", "description": "Use [4][2]int{{0,1}}", "role": "dev"}]`,
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasks, err := ParsePlanTasks(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePlanTasks() = %v, want error containing %q", tasks, tt.wantErr)
				}
				if !contains(t, err.Error(), tt.wantErr) {
					t.Errorf("ParsePlanTasks() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePlanTasks() unexpected error: %v", err)
			}
			if len(tasks) != tt.wantLen {
				t.Errorf("ParsePlanTasks() returned %d tasks, want %d", len(tasks), tt.wantLen)
			}
			// Verify each task has required fields.
			for i, task := range tasks {
				if task.Title == "" {
					t.Errorf("task[%d] has empty title", i)
				}
				if task.Role == "" {
					t.Errorf("task[%d] has empty role", i)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ParseCycleReview  — 解析 Leader 的审查决策 JSON
// ---------------------------------------------------------------------------

func TestParseCycleReview(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantDec   CycleDecision
		wantErr   string
		wantHasReason bool
		wantHasFeedback bool
	}{
		{
			name:    "accept",
			input:   `{"decision": "accept", "reason": "All tests pass, code is clean"}`,
			wantDec: CycleAccept,
			wantHasReason: true,
		},
		{
			name:    "reject with feedback",
			input:   `{"decision": "reject", "reason": "Missing error handling", "feedback": "Add try-catch around network calls"}`,
			wantDec: CycleReject,
			wantHasReason: true,
			wantHasFeedback: true,
		},
		{
			name:    "escalate",
			input:   `{"decision": "escalate", "reason": "Security concern needs human review"}`,
			wantDec: CycleEscalate,
			wantHasReason: true,
		},
		{
			name:    "markdown code fence",
			input:   "```json\n{\"decision\": \"accept\", \"reason\": \"Looks good\"}\n```",
			wantDec: CycleAccept,
			wantHasReason: true,
		},
		{
			name:    "plain code fence",
			input:   "```\n{\"decision\": \"reject\", \"reason\": \"Needs work\"}\n```",
			wantDec: CycleReject,
			wantHasReason: true,
		},
		{
			name:    "JSON only the object portion",
			input:   "I have decided: {\"decision\": \"accept\", \"reason\": \"ok\"}",
			wantDec: CycleAccept,
			wantHasReason: true,
		},
		{
			name:    "unknown decision — error",
			input:   `{"decision": "maybe", "reason": "not sure"}`,
			wantErr: "unknown decision",
		},
		{
			name:    "natural language — no JSON",
			input:   "The batch passed. Everything looks good. Proceeding.",
			wantErr: "no JSON object found",
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: "no JSON object found",
		},
		{
			name:    "JSON array with embedded object — extracts and parses",
			input:   `[{"decision": "accept"}]`,
			wantDec: CycleAccept,
			// extractJSONObject finds {"decision":"accept"} inside the array,
			// so it parses successfully with an empty reason.
		},
		{
			name:    "missing decision field — invalid JSON schema",
			input:   `{"reason": "no decision given"}`,
			wantErr: "unknown decision",
		},
		{
			name:    "feedback without reject — still valid",
			input:   `{"decision": "accept", "reason": "ok", "feedback": "optional note"}`,
			wantDec: CycleAccept,
			wantHasReason: true,
			wantHasFeedback: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review, err := ParseCycleReview(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseCycleReview() = %+v, want error containing %q", review, tt.wantErr)
				}
				if !contains(t, err.Error(), tt.wantErr) {
					t.Errorf("ParseCycleReview() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCycleReview() unexpected error: %v", err)
			}
			if review.Decision != tt.wantDec {
				t.Errorf("ParseCycleReview().Decision = %v, want %v", review.Decision, tt.wantDec)
			}
			if tt.wantHasReason && review.Reason == "" {
				t.Errorf("ParseCycleReview().Reason is empty, expected non-empty")
			}
			if tt.wantHasFeedback && review.Feedback == "" {
				t.Errorf("ParseCycleReview().Feedback is empty, expected non-empty")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Decompose  — 集成测试：验证 mock spawner 的各种返回值
// ---------------------------------------------------------------------------

func TestDecompose(t *testing.T) {
	tests := []struct {
		name      string
		spawnOut  string          // what the mock spawner returns
		spawnErr  error
		wantErr   string
		wantTasks int             // -1 = any non-zero
	}{
		{
			name:     "valid JSON array",
			spawnOut: `[{"title": "Build API", "description": "Write the API", "role": "developer"}]`,
			wantTasks: 1,
		},
		{
			name:     "markdown code fence with JSON array",
			spawnOut: "```json\n[{\"title\": \"Task\", \"description\": \"desc\", \"role\": \"dev\"}]\n```",
			wantTasks: 1,
		},
		{
			name:     "natural language fallback — preserves output",
			spawnOut: "The plan has been executed. All 1/1 tasks completed. The goal is fulfilled.",
			wantTasks: 1,
		},
		{
			name:     "empty output",
			spawnOut: "",
			wantErr:  "empty output",
		},
		{
			name:     "spawner error",
			spawnOut: "",
			spawnErr: assertAnError{},
			wantErr:  "decomposer subagent error",
		},
		{
			name:     "JSON object not array — fallback",
			spawnOut: `{"decision": "accept", "reason": "done"}`,
			wantTasks: 1,
		},
		{
			name:     "empty JSON array — fallback (extractJSON finds [] but ParsePlanTasks errors)",
			spawnOut: `[]`,
			wantTasks: 1, // fallback creates single default task
		},
		{
			name:     "task with missing role — fallback",
			spawnOut: `[{"title": "Task", "description": "desc"}]`,
			wantTasks: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := newTestEngine(t)
			defer eng.Close()

			// Override the mock spawner's output.
			eng.Runner.spawner = &mockSpawner{output: tt.spawnOut, err: tt.spawnErr}

			leader := NewLeader(eng.Runner)
			tasks, err := leader.Decompose("test goal", t.TempDir(), 0)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Decompose() = %v, want error containing %q", tasks, tt.wantErr)
				}
				if !contains(t, err.Error(), tt.wantErr) {
					t.Errorf("Decompose() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decompose() unexpected error: %v", err)
			}
			if tt.wantTasks >= 0 && len(tasks) != tt.wantTasks {
				t.Errorf("Decompose() returned %d tasks, want %d", len(tasks), tt.wantTasks)
			}
			if len(tasks) == 0 {
				t.Fatal("Decompose() returned 0 tasks, expected at least 1")
			}
			// For the natural-language fallback case, verify the AI's output
			// is preserved in the task description, not replaced by the goal.
			if tt.name == "natural language fallback — preserves output" {
				if tasks[0].Description != tt.spawnOut {
					t.Errorf("fallback task Description = %q, want %q (original output preserved)",
						tasks[0].Description, tt.spawnOut)
				}
			}
		})
	}
}

// assertAnError implements error so mockSpawner can simulate spawn failures.
type assertAnError struct{}

func (assertAnError) Error() string { return "simulated spawn error" }

// contains is a test helper that reports whether s contains substr.
func contains(t *testing.T, s, substr string) bool {
	t.Helper()
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
