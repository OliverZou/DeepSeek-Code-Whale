package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/usewhale/whale/internal/core"
)

func TestAnalyzeProblemSuccess(t *testing.T) {
	ts, err := NewToolset(t.TempDir())
	if err != nil {
		t.Fatalf("new toolset: %v", err)
	}

	input, _ := json.Marshal(map[string]string{
		"observed":   "The login button does nothing when clicked",
		"expected":   "Clicking login should redirect to dashboard",
		"root_cause": "Missing onClick handler in LoginButton component",
	})
	call := core.ToolCall{ID: "tc-1", Name: "analyze_problem", Input: string(input)}

	res, err := ts.analyzeProblem(context.Background(), call)
	if err != nil {
		t.Fatalf("analyzeProblem returned error: %v", err)
	}
	if res.IsError() {
		t.Fatalf("analyzeProblem failed: %+v", res)
	}
	if res.Code != "ok" {
		t.Errorf("expected code=ok, got %q", res.Code)
	}
	if res.ModelText != "Analysis recorded. You may now use edit/write/multi_edit tools." {
		t.Errorf("unexpected ModelText: %q", res.ModelText)
	}
	if res.ToolCallID != "tc-1" {
		t.Errorf("expected ToolCallID=tc-1, got %q", res.ToolCallID)
	}
}

func TestAnalyzeProblemEmptyField(t *testing.T) {
	ts, err := NewToolset(t.TempDir())
	if err != nil {
		t.Fatalf("new toolset: %v", err)
	}

	tests := []struct {
		name   string
		fields map[string]string
	}{
		{"empty observed", map[string]string{"observed": "", "expected": "works", "root_cause": "bug"}},
		{"empty expected", map[string]string{"observed": "fails", "expected": "", "root_cause": "bug"}},
		{"empty root_cause", map[string]string{"observed": "fails", "expected": "works", "root_cause": ""}},
		{"all empty", map[string]string{"observed": "", "expected": "", "root_cause": ""}},
		{"whitespace only", map[string]string{"observed": "  ", "expected": "works", "root_cause": "bug"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, _ := json.Marshal(tt.fields)
			call := core.ToolCall{ID: "tc-1", Name: "analyze_problem", Input: string(input)}
			res, err := ts.analyzeProblem(context.Background(), call)
			if err != nil {
				t.Fatalf("analyzeProblem returned error: %v", err)
			}
			if !res.IsError() {
				t.Fatalf("expected failure for empty field, got success: %+v", res)
			}
			if res.Code != "empty_field" {
				t.Errorf("expected code=empty_field, got %q", res.Code)
			}
		})
	}
}

func TestAnalyzeProblemInvalidInput(t *testing.T) {
	ts, err := NewToolset(t.TempDir())
	if err != nil {
		t.Fatalf("new toolset: %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{"not JSON", "not json at all"},
		{"malformed JSON", "{\"observed\": broken}"},
		{"empty input", ""},
		{"array not object", "[\"observed\"]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := core.ToolCall{ID: "tc-1", Name: "analyze_problem", Input: tt.input}
			res, err := ts.analyzeProblem(context.Background(), call)
			if err != nil {
				t.Fatalf("analyzeProblem returned error: %v", err)
			}
			if !res.IsError() {
				t.Fatalf("expected failure for invalid input, got success: %+v", res)
			}
			if res.Code != "invalid_input" {
				t.Errorf("expected code=invalid_input, got %q", res.Code)
			}
		})
	}
}

func TestAnalyzeProblemExtraFieldsIgnored(t *testing.T) {
	ts, err := NewToolset(t.TempDir())
	if err != nil {
		t.Fatalf("new toolset: %v", err)
	}

	input, _ := json.Marshal(map[string]string{
		"observed":   "bug",
		"expected":   "fix",
		"root_cause": "logic",
		"severity":   "critical",
	})
	call := core.ToolCall{ID: "tc-1", Name: "analyze_problem", Input: string(input)}
	res, err := ts.analyzeProblem(context.Background(), call)
	if err != nil {
		t.Fatalf("analyzeProblem returned error: %v", err)
	}
	if res.IsError() {
		t.Fatalf("extra fields should not cause error: %+v", res)
	}
}

func TestAnalyzeProblemToolRegistered(t *testing.T) {
	ts, err := NewToolset(t.TempDir())
	if err != nil {
		t.Fatalf("new toolset: %v", err)
	}

	tools := ts.Tools()
	found := false
	for _, tool := range tools {
		if tool.Name() == "analyze_problem" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("analyze_problem tool not found in Tools()")
	}
}
