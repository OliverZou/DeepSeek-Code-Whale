package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/usewhale/whale/internal/core"
)

func TestASTEditTools_Available(t *testing.T) {
	b := &Toolset{}
	b.SetASTEditCaller(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		return "{}", false, nil
	})
	tools := b.astEditTools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	if !names["ast_edit"] {
		t.Fatal("expected ast_edit tool")
	}
	if !names["ast_patch"] {
		t.Fatal("expected ast_patch tool")
	}
	if !names["ast_symbols"] {
		t.Fatal("expected ast_symbols tool")
	}
}

func TestASTEditTools_Unavailable(t *testing.T) {
	b := &Toolset{}
	tools := b.astEditTools()
	if tools != nil {
		t.Fatalf("expected nil tools when caller is nil, got %d tools", len(tools))
	}
}

func TestASTEdit_CallsASTWrite(t *testing.T) {
	var receivedTool string
	b := &Toolset{}
	b.SetASTEditCaller(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		receivedTool = toolName
		return `{"ok":true}`, false, nil
	})

	tool := findASTTool(t, b, "ast_edit")
	input, _ := json.Marshal(map[string]string{
		"file_path":   "/proj/auth.go",
		"symbol_name": "Authenticate",
		"new_code":    "func Authenticate() error { return nil }",
	})
	result, err := tool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "ast_edit", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receivedTool != "ast_write" {
		t.Fatalf("expected ast_write, got %q", receivedTool)
	}
	if result.Code != "ok" {
		t.Fatalf("expected ok, got %q", result.Code)
	}
}

func TestASTEdit_MissingFieldsReturnsError(t *testing.T) {
	b := &Toolset{}
	b.SetASTEditCaller(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		t.Fatal("should not be called")
		return "", false, nil
	})

	tool := findASTTool(t, b, "ast_edit")
	input, _ := json.Marshal(map[string]string{"symbol_name": "X"})
	result, _ := tool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "ast_edit", Input: string(input)})
	if result.Outcome != core.OutcomeFailure {
		t.Fatal("expected failure for missing file_path")
	}
}

func TestASTPatch_CallsASTPatch(t *testing.T) {
	var receivedTool string
	b := &Toolset{}
	b.SetASTEditCaller(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		receivedTool = toolName
		return `{"ok":true}`, false, nil
	})

	tool := findASTTool(t, b, "ast_patch")
	input, _ := json.Marshal(map[string]string{
		"file_path":   "/proj/auth.go",
		"symbol_name": "Authenticate",
		"old_snippet": "return nil",
		"new_snippet": "return fmt.Errorf(\"not implemented\")",
	})
	result, err := tool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "ast_patch", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receivedTool != "ast_patch" {
		t.Fatalf("expected ast_patch, got %q", receivedTool)
	}
	if result.Code != "ok" {
		t.Fatalf("expected ok, got %q", result.Code)
	}
}

func TestASTSymbols_CallsASTSymbols(t *testing.T) {
	var receivedTool string
	b := &Toolset{}
	b.SetASTEditCaller(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		receivedTool = toolName
		return `[]`, false, nil
	})

	tool := findASTTool(t, b, "ast_symbols")
	input, _ := json.Marshal(map[string]string{"file_path": "/proj/auth.go"})
	result, err := tool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "ast_symbols", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receivedTool != "ast_symbols" {
		t.Fatalf("expected ast_symbols, got %q", receivedTool)
	}
	if result.Code != "ok" {
		t.Fatalf("expected ok, got %q", result.Code)
	}
}

func TestASTEditTools_ReadOnly(t *testing.T) {
	b := &Toolset{}
	b.SetASTEditCaller(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		return "{}", false, nil
	})

	// ast_edit and ast_patch are writable; ast_symbols is read-only
	tools := b.astEditTools()
	for _, tool := range tools {
		spec := core.DescribeTool(tool)
		switch tool.Name() {
		case "ast_symbols":
			if !spec.ReadOnly {
				t.Fatalf("%s should be read-only", tool.Name())
			}
		case "ast_edit", "ast_patch":
			if spec.ReadOnly {
				t.Fatalf("%s should NOT be read-only", tool.Name())
			}
		}
	}
}

func findASTTool(t *testing.T, b *Toolset, name string) core.Tool {
	t.Helper()
	for _, tool := range b.astEditTools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}
