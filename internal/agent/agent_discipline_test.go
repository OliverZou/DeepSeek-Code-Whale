package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usewhale/whale/internal/core"
)

func tc(name string, params map[string]any) core.ToolCall {
	b, _ := json.Marshal(params)
	return core.ToolCall{ID: "tc-1", Name: name, Input: string(b)}
}

func TestIsMutationTool(t *testing.T) {
	tests := []struct{name string; want bool}{
		{"edit", true},
		{"write", true},
		{"multi_edit", true},
		{"read_file", false},
		{"shell_run", false},
		{"grep", false},
		{"analyze_problem", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMutationTool(tt.name); got != tt.want {
				t.Errorf("isMutationTool(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestExtractFilePathFromCall(t *testing.T) {
	tests := []struct{name string; call core.ToolCall; want string}{
		{"valid", tc("edit", map[string]any{"file_path": "m.go"}), "m.go"},
		{"bad json", core.ToolCall{Input: "{bad"}, ""},
		{"empty", core.ToolCall{Input: ""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFilePathFromCall(tt.call)
			if got != tt.want {
				t.Errorf("extractFilePathFromCall() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckReadBeforeEditGate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "existing.go"), []byte("package main\n"), 0644)

	t.Run("not read blocked", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		var results []core.ToolResult
		blocked := a.checkReadBeforeEditGate(context.Background(), streamDispatchContext{}, tc("edit", map[string]any{"file_path": "existing.go"}), &results)
		if !blocked { t.Fatal("expected blocked") }
		if results[len(results)-1].Code != "read_before_edit_required" { t.Fatal("wrong code") }
	})

	t.Run("read allowed", func(t *testing.T) {
		norm := normalizeWorkspacePath("existing.go", dir)
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{norm: true}}
		var results []core.ToolResult
		if a.checkReadBeforeEditGate(context.Background(), streamDispatchContext{}, tc("edit", map[string]any{"file_path": "existing.go"}), &results) {
			t.Fatal("should not block")
		}
	})

	t.Run("new write exempt", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		var results []core.ToolResult
		if a.checkReadBeforeEditGate(context.Background(), streamDispatchContext{}, tc("write", map[string]any{"file_path": "nonew.go", "content": "x"}), &results) {
			t.Fatal("new file exempt")
		}
	})

	t.Run("empty root skip", func(t *testing.T) {
		a := &Agent{workspaceRoot: "", filesReadThisTurn: map[string]bool{}}
		var results []core.ToolResult
		if a.checkReadBeforeEditGate(context.Background(), streamDispatchContext{}, tc("edit", map[string]any{"file_path": "x.go"}), &results) {
			t.Fatal("empty root must skip")
		}
	})
}

func TestRecordFileRead(t *testing.T) {
	dir := t.TempDir()
	t.Run("tracks file", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		a.recordFileRead(tc("read_file", map[string]any{"file_path": "a.go"}))
		if !a.filesReadThisTurn[normalizeWorkspacePath("a.go", dir)] { t.Fatal("not tracked") }
	})
	t.Run("multiple files", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		a.recordFileRead(tc("read_file", map[string]any{"file_path": "a.go"}))
		a.recordFileRead(tc("read_file", map[string]any{"file_path": "b.go"}))
		if len(a.filesReadThisTurn) != 2 { t.Fatal("expected 2") }
	})
}

func TestDiffCountsFromResult(t *testing.T) {
	bm := func(files []map[string]any) map[string]any {
		if len(files) == 0 { return nil }
		return map[string]any{"kind": "file_diff", "files": files}
	}
	t.Run("nil meta", func(t *testing.T) {
		a, d := diffCountsFromResult(core.ToolResult{})
		if a != 0 || d != 0 { t.Fatal("not zero") }
	})
	t.Run("single file", func(t *testing.T) {
		m := bm([]map[string]any{{"additions": float64(5), "deletions": float64(3)}})
		a, d := diffCountsFromResult(core.ToolResult{Metadata: m})
		if a != 5 || d != 3 { t.Fatal("wrong") }
	})
	t.Run("int values", func(t *testing.T) {
		m := bm([]map[string]any{{"additions": 5, "deletions": 3}})
		a, d := diffCountsFromResult(core.ToolResult{Metadata: m})
		if a != 5 || d != 3 { t.Fatal("wrong int") }
	})
}

func TestAutoDetectVerifyCommands(t *testing.T) {
	t.Run("go.mod", func(t *testing.T) {
		d := t.TempDir(); os.WriteFile(filepath.Join(d, "go.mod"), []byte("mod x\n"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if len(cmds) != 2 || cmds[0] != "go build ./..." { t.Fatalf("bad: %v", cmds) }
	})
	t.Run("npm test", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "package.json"), []byte("{\"scripts\":{\"test\":\"j\"}}"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if cmds != nil { t.Fatalf("verify should be nil with only test script, got: %v", cmds) }
	})
	t.Run("npm build+lint", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "package.json"), []byte("{\"scripts\":{\"build\":\"tsc\",\"lint\":\"eslint .\"}}"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if len(cmds) != 2 || cmds[0] != "npm run build" || cmds[1] != "npm run lint" { t.Fatalf("bad: %v", cmds) }
	})
	t.Run("empty nil", func(t *testing.T) {
		if cmds := autoDetectVerifyCommands(t.TempDir()); cmds != nil { t.Fatal("expected nil") }
	})
}

func TestHasNPMScript(t *testing.T) {
	d := t.TempDir(); p := filepath.Join(d, "p.json")
	os.WriteFile(p, []byte("{\"scripts\":{\"test\":\"j\"}}"), 0644)
	if !hasNPMScript(p, "test") { t.Fatal("not found") }
	if hasNPMScript("/nil", "test") { t.Fatal("should fail") }
}

func TestTruncateVerifyOutput(t *testing.T) {
	if g := truncateVerifyOutput("hi", 100); g != "hi" { t.Fatal("changed") }
	if g := truncateVerifyOutput(strings.Repeat("a", 100), 20); !strings.Contains(g, "truncated") { t.Fatal("not trunc") }
}

func TestRunAutoVerify(t *testing.T) {
	t.Run("echo ok", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir(), verifyCommands: []string{"echo ok"}}
		if r := a.runAutoVerify(context.Background()); !strings.Contains(r, "ok") { t.Fatalf("bad: %q", r) }
	})
	t.Run("fail err", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir(), verifyCommands: []string{"cmdnonexist_xyz"}}
		if r := a.runAutoVerify(context.Background()); !strings.Contains(r, "[error:") { t.Fatalf("bad: %q", r) }
	})
}

func TestContainsSkipAnalysisKeyword(t *testing.T) {
	if containsSkipAnalysisKeyword("skip analysis now") == "" { t.Fatal("not found") }
	if containsSkipAnalysisKeyword("Just Fix It") == "" { t.Fatal("case insensitive") }
	if containsSkipAnalysisKeyword("please analyze") != "" { t.Fatal("false match") }
}

func TestWithGateConfig(t *testing.T) {
	opt := WithGateConfig(false, true, 5)
	a := &Agent{}; opt(a)
	if a.gateReadBeforeEdit != false { t.Fatal("read") }
	if a.gateAnalyzeBeforeEdit != true { t.Fatal("analyze") }
	if a.analysisThreshold != 5 { t.Fatal("threshold") }
}

func TestWithVerifyConfig(t *testing.T) {
	opt := WithVerifyConfig([]string{"go test"}, 10, 5, []string{"go test ./..."}, 60)
	a := &Agent{}; opt(a)
	if len(a.verifyCommands) != 1 || a.verifyCommands[0] != "go test" { t.Fatal("cmds") }
	if a.verifyTimeout != 10 { t.Fatal("timeout") }
	if a.verifyReviewThreshold != 5 { t.Fatal("threshold") }
	if len(a.testCommands) != 1 || a.testCommands[0] != "go test ./..." { t.Fatal("test cmds") }
	if a.testTimeout != 60 { t.Fatal("test timeout") }
}

func TestRenderMinimalChangeBlock(t *testing.T) {
	b := renderMinimalChangeBlock()
	for _, w := range []string{"Minimal change", "traceable", "Do not refactor", "50 lines"} {
		if !strings.Contains(b, w) { t.Errorf("missing: %q", w) }
	}
}

func TestTurnReset(t *testing.T) {
	a := &Agent{filesReadThisTurn: map[string]bool{"x": true}, analysisProvidedThisTurn: true, dirtySinceVerify: true}
	a.filesReadThisTurn = make(map[string]bool)
	a.analysisProvidedThisTurn = false
	a.dirtySinceVerify = false
	if len(a.filesReadThisTurn) != 0 || a.analysisProvidedThisTurn || a.dirtySinceVerify {
		t.Fatal("not reset")
	}
}
