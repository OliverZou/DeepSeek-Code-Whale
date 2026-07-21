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
	tests := []struct {
		name string
		want bool
	}{
		{"edit", true}, {"write", true}, {"multi_edit", true},
		{"read_file", false}, {"shell_run", false}, {"grep", false},
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
	tests := []struct {
		name string
		call core.ToolCall
		want string
	}{
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

	newSC := func() *streamDispatchContext {
		return &streamDispatchContext{Events: make(chan AgentEvent, 8)}
	}

	t.Run("not read blocked", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		var results []core.ToolResult
		blocked := a.checkReadBeforeEditGate(context.Background(), newSC(), tc("edit", map[string]any{"file_path": "existing.go"}), &results)
		if !blocked {
			t.Fatal("expected blocked")
		}
		if results[len(results)-1].Code != "read_before_edit_required" {
			t.Fatal("wrong code")
		}
	})
	t.Run("read allowed", func(t *testing.T) {
		norm := normalizeWorkspacePath("existing.go", dir)
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{norm: true}}
		var results []core.ToolResult
		if a.checkReadBeforeEditGate(context.Background(), newSC(), tc("edit", map[string]any{"file_path": "existing.go"}), &results) {
			t.Fatal("should not block")
		}
	})
	t.Run("new write exempt", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		var results []core.ToolResult
		if a.checkReadBeforeEditGate(context.Background(), newSC(), tc("write", map[string]any{"file_path": "nonew.go", "content": "x"}), &results) {
			t.Fatal("new file exempt")
		}
	})
	t.Run("empty root skip", func(t *testing.T) {
		a := &Agent{workspaceRoot: "", filesReadThisTurn: map[string]bool{}}
		var results []core.ToolResult
		if a.checkReadBeforeEditGate(context.Background(), newSC(), tc("edit", map[string]any{"file_path": "x.go"}), &results) {
			t.Fatal("empty root must skip")
		}
	})
}

func TestRecordFileRead(t *testing.T) {
	dir := t.TempDir()
	t.Run("tracks file", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		a.recordFileRead(tc("read_file", map[string]any{"file_path": "a.go"}))
		if !a.filesReadThisTurn[normalizeWorkspacePath("a.go", dir)] {
			t.Fatal("not tracked")
		}
	})
	t.Run("multiple files", func(t *testing.T) {
		a := &Agent{workspaceRoot: dir, filesReadThisTurn: map[string]bool{}}
		a.recordFileRead(tc("read_file", map[string]any{"file_path": "a.go"}))
		a.recordFileRead(tc("read_file", map[string]any{"file_path": "b.go"}))
		if len(a.filesReadThisTurn) != 2 {
			t.Fatal("expected 2")
		}
	})
}

func TestAutoDetectVerifyCommands(t *testing.T) {
	t.Run("go.mod", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "go.mod"), []byte("mod x\n"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if len(cmds) != 2 || cmds[0] != "go build ./..." {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("npm only test nil", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "package.json"), []byte("{\"scripts\":{\"test\":\"j\"}}"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if cmds != nil {
			t.Fatalf("expected nil with only test: %v", cmds)
		}
	})
	t.Run("npm build+lint", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "package.json"), []byte("{\"scripts\":{\"build\":\"tsc\",\"lint\":\"eslint .\"}}"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if len(cmds) != 2 || cmds[0] != "npm run build" || cmds[1] != "npm run lint" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("Makefile with lint", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("check:\n\trun\n\nlint:\n\trun\n"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if len(cmds) != 2 || cmds[0] != "make check" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("Makefile no lint", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("build:\n\trun\n"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if cmds != nil {
			t.Fatalf("expected nil when no check/lint targets: %v", cmds)
		}
	})
	t.Run("pyproject.toml", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "pyproject.toml"), []byte("[project]\n"), 0644)
		cmds := autoDetectVerifyCommands(d)
		if len(cmds) != 1 || cmds[0] != "ruff check ." {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("empty nil", func(t *testing.T) {
		if cmds := autoDetectVerifyCommands(t.TempDir()); cmds != nil {
			t.Fatal("expected nil")
		}
	})
}

func TestHasNPMScript(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "p.json")
	os.WriteFile(p, []byte("{\"scripts\":{\"test\":\"j\"}}"), 0644)
	if !hasNPMScript(p, "test") {
		t.Fatal("not found")
	}
	if hasNPMScript("/nil", "test") {
		t.Fatal("should fail")
	}
}

func TestHasMakeTarget(t *testing.T) {
	t.Run("target exists", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("lint:\ncheck:\n"), 0644)
		if !hasMakeTarget(d, "lint") {
			t.Fatal("not found")
		}
		if !hasMakeTarget(d, "check") {
			t.Fatal("not found")
		}
	})
	t.Run("space before colon", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("test :\n"), 0644)
		if !hasMakeTarget(d, "test") {
			t.Fatal("space colon not found")
		}
	})
	t.Run("missing target", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("build:\n"), 0644)
		if hasMakeTarget(d, "lint") {
			t.Fatal("should not be found")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if hasMakeTarget(t.TempDir(), "test") {
			t.Fatal("false for missing file")
		}
	})
}

func TestTruncateVerifyOutput(t *testing.T) {
	if g := truncateVerifyOutput("hi", 100); g != "hi" {
		t.Fatal("changed")
	}
	if g := truncateVerifyOutput(strings.Repeat("a", 100), 20); !strings.Contains(g, "truncated") {
		t.Fatal("not trunc")
	}
}

func TestRunAutoVerify(t *testing.T) {
	t.Run("echo ok", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir(), verifyCommands: []string{"echo ok"}}
		if r := a.runAutoVerify(context.Background()); !strings.Contains(r, "ok") {
			t.Fatalf("bad: %q", r)
		}
	})
	t.Run("fail err", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir(), verifyCommands: []string{"cmdnonexist_xyz"}}
		if r := a.runAutoVerify(context.Background()); !strings.Contains(r, "[error:") {
			t.Fatalf("bad: %q", r)
		}
	})
}

func TestWithGateConfig(t *testing.T) {
	opt := WithGateConfig(false)
	a := &Agent{}
	opt(a)
	if a.gateReadBeforeEdit != false {
		t.Fatal("read")
	}
}

func TestWithVerifyConfig(t *testing.T) {
	opt := WithVerifyConfig(VerifyConfig{
		Commands:     []string{"go test"},
		Timeout:      10,
		TestCommands: []string{"go test ./..."},
		TestTimeout:  60,
	})
	a := &Agent{}
	opt(a)
	if len(a.verifyCommands) != 1 || a.verifyCommands[0] != "go test" {
		t.Fatal("cmds")
	}
	if a.verifyTimeout != 10 {
		t.Fatal("timeout")
	}
	if len(a.testCommands) != 1 || a.testCommands[0] != "go test ./..." {
		t.Fatal("test cmds")
	}
	if a.testTimeout != 60 {
		t.Fatal("test timeout")
	}
}

func TestRenderMinimalChangeBlock(t *testing.T) {
	b := renderMinimalChangeBlock()
	for _, w := range []string{"Minimal change", "traceable", "Do not refactor", "50 lines"} {
		if !strings.Contains(b, w) {
			t.Errorf("missing: %q", w)
		}
	}
}

// --- P1/P2 turn-level reset ---

func TestTurnReset(t *testing.T) {
	a := &Agent{
		filesReadThisTurn:   map[string]bool{"x": true},
		dirtySinceVerify:    true,
		dirtySinceTurnTest:  true,
		sourceFilesThisTurn: map[string]bool{"a.go": true},
		testFilesThisTurn:   map[string]bool{"a_test.go": true},
	}
	a.resetTurnState()
	if len(a.filesReadThisTurn) != 0 || a.dirtySinceVerify || a.dirtySinceTurnTest ||
		len(a.sourceFilesThisTurn) != 0 || len(a.testFilesThisTurn) != 0 {
		t.Fatal("not reset")
	}
}

// --- P2: new features from 787a92e ---

func TestAutoDetectTestCommands(t *testing.T) {
	t.Run("go.mod", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "go.mod"), []byte("mod x\n"), 0644)
		cmds := autoDetectTestCommands(d)
		if len(cmds) != 1 || cmds[0] != "go test ./... -count=1" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("npm test script", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "package.json"), []byte("{\"scripts\":{\"test\":\"jest\"}}"), 0644)
		cmds := autoDetectTestCommands(d)
		if len(cmds) != 1 || cmds[0] != "npm test" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("npm no test nil", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "package.json"), []byte("{}"), 0644)
		if cmds := autoDetectTestCommands(d); cmds != nil {
			t.Fatal("expected nil")
		}
	})
	t.Run("pyproject.toml", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "pyproject.toml"), []byte("[project]\n"), 0644)
		cmds := autoDetectTestCommands(d)
		if len(cmds) != 1 || cmds[0] != "pytest -x -q" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("pytest.ini", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "pytest.ini"), []byte("[pytest]\n"), 0644)
		cmds := autoDetectTestCommands(d)
		if len(cmds) != 1 || cmds[0] != "pytest -x -q" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("Cargo.toml", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Cargo.toml"), []byte("[package]\n"), 0644)
		cmds := autoDetectTestCommands(d)
		if len(cmds) != 1 || cmds[0] != "cargo test" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("Makefile with test", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("test:\n\techo\n"), 0644)
		cmds := autoDetectTestCommands(d)
		if len(cmds) != 1 || cmds[0] != "make test" {
			t.Fatalf("bad: %v", cmds)
		}
	})
	t.Run("Makefile no test nil", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "Makefile"), []byte("build:\n"), 0644)
		if cmds := autoDetectTestCommands(d); cmds != nil {
			t.Fatal("expected nil")
		}
	})
	t.Run("empty nil", func(t *testing.T) {
		if cmds := autoDetectTestCommands(t.TempDir()); cmds != nil {
			t.Fatal("expected nil")
		}
	})
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main_test.go", true}, {"main_test.py", true},
		{"component.test.js", true}, {"component.spec.ts", true},
		{"component.test.tsx", true}, {"component.spec.jsx", true},
		{"test_utils.py", true}, {"test_.go", false}, {"test_.py", true},
		{"main.go", false}, {"main.py", false}, {"main.js", false},
		{"Makefile", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isTestFile(tt.path); got != tt.want {
				t.Errorf("isTestFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsExemptFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"README.md", true}, {"config.toml", true},
		{"config.yaml", true}, {"config.yml", true},
		{"package.json", true}, {"go.mod", true}, {"go.work", false},
		{"config.ini", true}, {"config.cfg", true}, {"config.conf", true},
		{"notes.txt", true}, {"poetry.lock", true},
		{"Makefile", true}, {"Dockerfile", true},
		{".gitignore", true}, {".env", true},
		{"main.go", false}, {"main.py", false},
		{"main_test.go", false}, {"component.tsx", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isExemptFile(tt.path); got != tt.want {
				t.Errorf("isExemptFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsExemptFileCaseInsensitive(t *testing.T) {
	if !isExemptFile("makefile") || !isExemptFile("MAKEFILE") {
		t.Fatal("makefile case insensitive")
	}
	if !isExemptFile("Dockerfile") || !isExemptFile("dockerfile") {
		t.Fatal("dockerfile case insensitive")
	}
	if !isExemptFile(".GITIGNORE") {
		t.Fatal("gitignore case insensitive")
	}
}

func TestTrackMutatedFiles(t *testing.T) {
	bm := func(files []map[string]any) map[string]any {
		return map[string]any{"kind": "file_diff", "files": files}
	}
	t.Run("source file tracked", func(t *testing.T) {
		src, tst := map[string]bool{}, map[string]bool{}
		trackMutatedFiles(core.ToolResult{Metadata: bm([]map[string]any{{"path": "main.go"}})}, src, tst)
		if !src["main.go"] {
			t.Fatal("main.go not in source")
		}
		if len(tst) != 0 {
			t.Fatal("should not be in test")
		}
	})
	t.Run("test file separate", func(t *testing.T) {
		src, tst := map[string]bool{}, map[string]bool{}
		trackMutatedFiles(core.ToolResult{Metadata: bm([]map[string]any{{"path": "main_test.go"}})}, src, tst)
		if len(src) != 0 {
			t.Fatal("test file in source")
		}
		if !tst["main_test.go"] {
			t.Fatal("main_test.go not in test")
		}
	})
	t.Run("exempt ignored", func(t *testing.T) {
		src, tst := map[string]bool{}, map[string]bool{}
		trackMutatedFiles(core.ToolResult{Metadata: bm([]map[string]any{{"path": "README.md"}})}, src, tst)
		if len(src) != 0 || len(tst) != 0 {
			t.Fatal("README.md should be exempt")
		}
	})
	t.Run("nil meta no-op", func(t *testing.T) {
		src, tst := map[string]bool{}, map[string]bool{}
		trackMutatedFiles(core.ToolResult{}, src, tst)
		if len(src) != 0 || len(tst) != 0 {
			t.Fatal("nil meta should add nothing")
		}
	})
	t.Run("wrong kind no-op", func(t *testing.T) {
		src, tst := map[string]bool{}, map[string]bool{}
		trackMutatedFiles(core.ToolResult{Metadata: map[string]any{"kind": "other"}}, src, tst)
		if len(src) != 0 || len(tst) != 0 {
			t.Fatal("wrong kind should add nothing")
		}
	})
	t.Run("mixed files", func(t *testing.T) {
		src, tst := map[string]bool{}, map[string]bool{}
		trackMutatedFiles(core.ToolResult{Metadata: bm([]map[string]any{
			{"path": "handler.go"}, {"path": "handler_test.go"}, {"path": "config.yml"},
		})}, src, tst)
		if !src["handler.go"] {
			t.Fatal("handler.go missing")
		}
		if !tst["handler_test.go"] {
			t.Fatal("handler_test.go missing")
		}
		if src["config.yml"] || tst["config.yml"] {
			t.Fatal("config.yml should be exempt")
		}
	})
}

func TestRunAutoTest(t *testing.T) {
	t.Run("no commands empty", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir()}
		if r := a.runAutoTest(context.Background()); r != "" {
			t.Fatalf("expected empty: %q", r)
		}
	})
	t.Run("echo succeeds", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir(), testCommands: []string{"echo testok"}}
		if r := a.runAutoTest(context.Background()); !strings.Contains(r, "testok") {
			t.Fatalf("bad: %q", r)
		}
	})
	t.Run("failure error", func(t *testing.T) {
		a := &Agent{workspaceRoot: t.TempDir(), testCommands: []string{"cmdnonexist_xyz_test"}}
		if r := a.runAutoTest(context.Background()); !strings.Contains(r, "[error:") {
			t.Fatalf("bad: %q", r)
		}
	})
}

func TestResolveTestCommands(t *testing.T) {
	t.Run("configured returned", func(t *testing.T) {
		a := &Agent{workspaceRoot: "/x", testCommands: []string{"go test ./..."}}
		c := a.resolveTestCommands()
		if len(c) != 1 || c[0] != "go test ./..." {
			t.Fatal("wrong")
		}
	})
	t.Run("empty root nil", func(t *testing.T) {
		a := &Agent{workspaceRoot: ""}
		if c := a.resolveTestCommands(); c != nil {
			t.Fatal("expected nil")
		}
	})
	t.Run("auto detect disabled by default", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "go.mod"), []byte("mod x\n"), 0644)
		a := &Agent{workspaceRoot: d}
		c := a.resolveTestCommands()
		if c != nil {
			t.Fatal("auto-detect should be disabled by default")
		}
	})
}

// TestDisciplineGateIntegration tests the full P1 gate flow:
// recordFileRead -> checkReadBeforeEditGate -> recordFileRead (mutation) -> check -> reset -> check.
func TestDisciplineGateIntegration(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644)
	a := &Agent{workspaceRoot: dir, gateReadBeforeEdit: true, filesReadThisTurn: make(map[string]bool)}
	sc := &streamDispatchContext{Events: make(chan AgentEvent, 32)}

	// Step 1: read_file populates the tracking map
	a.recordFileRead(tc("read_file", map[string]any{"file_path": "main.go"}))
	norm := normalizeWorkspacePath("main.go", dir)
	if !a.filesReadThisTurn[norm] {
		t.Fatal("recordFileRead should mark file as read")
	}

	// Step 2: edit on read file passes the gate
	var r1 []core.ToolResult
	if a.checkReadBeforeEditGate(context.Background(), sc, tc("edit", map[string]any{"file_path": "main.go"}), &r1) {
		t.Fatal("edit on previously read file must pass gate")
	}

	// Step 3: edit on unread file is blocked
	var r2 []core.ToolResult
	if !a.checkReadBeforeEditGate(context.Background(), sc, tc("edit", map[string]any{"file_path": "other.go"}), &r2) {
		t.Fatal("edit on unread file must be blocked")
	}
	if r2[0].Code != "read_before_edit_required" {
		t.Fatalf("wrong code: %q", r2[0].Code)
	}

	// Step 4: write to new file is exempt
	var r3 []core.ToolResult
	if a.checkReadBeforeEditGate(context.Background(), sc, tc("write", map[string]any{"file_path": "new.go", "content": "x"}), &r3) {
		t.Fatal("write to new file must be exempt")
	}

	// Step 5: successful mutation also marks file as read
	a.recordFileRead(tc("edit", map[string]any{"file_path": "main.go"}))
	var r4 []core.ToolResult
	if a.checkReadBeforeEditGate(context.Background(), sc, tc("multi_edit", map[string]any{"file_path": "main.go"}), &r4) {
		t.Fatal("multi_edit on previously edited file must pass")
	}

	// Step 6: turn reset clears tracking
	a.resetTurnState()
	if len(a.filesReadThisTurn) != 0 {
		t.Fatal("resetTurnState must clear filesReadThisTurn")
	}
	var r5 []core.ToolResult
	if !a.checkReadBeforeEditGate(context.Background(), sc, tc("edit", map[string]any{"file_path": "main.go"}), &r5) {
		t.Fatal("after reset, edit must be blocked again")
	}
	if r5[0].Code != "read_before_edit_required" {
		t.Fatalf("wrong code after reset: %q", r5[0].Code)
	}
}
