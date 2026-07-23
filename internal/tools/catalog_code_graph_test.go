package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/usewhale/whale/internal/core"
)

func TestCodeGraphTools_Available(t *testing.T) {
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		return "{}", false, nil
	})
	tools := b.codeGraphTools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	if !names["codebase_search"] {
		t.Fatal("expected codebase_search tool")
	}
	if !names["codebase_trace"] {
		t.Fatal("expected codebase_trace tool")
	}
	if !names["codebase_impact"] {
		t.Fatal("expected codebase_impact tool")
	}
}

func TestCodeGraphTools_Unavailable(t *testing.T) {
	b := &Toolset{}
	tools := b.codeGraphTools()
	if tools != nil {
		t.Fatalf("expected nil tools when caller is nil, got %d tools", len(tools))
	}
}

func TestCodebaseSearch_CallsCodeGraph(t *testing.T) {
	called := false
	var receivedTool string
	var receivedArgs map[string]any
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		called = true
		receivedTool = toolName
		receivedArgs = args
		return `{"match":"found"}`, false, nil
	})

	tools := b.codeGraphTools()
	var searchTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_search" {
			searchTool = tool
			break
		}
	}
	if searchTool == nil {
		t.Fatal("codebase_search not found")
	}

	input, _ := json.Marshal(map[string]string{"query": "authenticate"})
	result, err := searchTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_search", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("expected CodeGraphCaller to be called")
	}
	if receivedTool != "search_graph" {
		t.Fatalf("expected search_graph, got %q", receivedTool)
	}
	if result.Code != "ok" {
		t.Fatalf("expected ok, got %q: %s", result.Code, result.ModelText)
	}
	if v, ok := receivedArgs["query"]; !ok || v != "authenticate" {
		t.Fatalf("expected query=authenticate, got %v", receivedArgs)
	}
}

func TestCodebaseSearch_EmptyQueryReturnsError(t *testing.T) {
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		t.Fatal("caller should not be invoked for empty query")
		return "", false, nil
	})

	tools := b.codeGraphTools()
	var searchTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_search" {
			searchTool = tool
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"query": ""})
	result, err := searchTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_search", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != core.OutcomeFailure {
		t.Fatal("expected failure for empty query")
	}
}

func TestCodebaseTrace_CallsCodeGraph(t *testing.T) {
	var receivedTool string
	var receivedArgs map[string]any
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		receivedTool = toolName
		receivedArgs = args
		return `{"callers": ["main"], "callees": ["helper"]}`, false, nil
	})

	tools := b.codeGraphTools()
	var traceTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_trace" {
			traceTool = tool
			break
		}
	}
	if traceTool == nil {
		t.Fatal("codebase_trace not found")
	}

	input, _ := json.Marshal(map[string]string{"symbol_name": "whale.Run", "direction": "both"})
	result, err := traceTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_trace", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receivedTool != "trace_path" {
		t.Fatalf("expected trace_path, got %q", receivedTool)
	}
	if result.Code != "ok" {
		t.Fatalf("expected ok, got %q", result.Code)
	}
	if v, ok := receivedArgs["function_name"]; !ok || v != "whale.Run" {
		t.Fatalf("expected function_name=whale.Run, got %v", receivedArgs)
	}
	if v, ok := receivedArgs["direction"]; !ok || v != "both" {
		t.Fatalf("expected direction=both, got %v", receivedArgs)
	}
}

func TestCodebaseTrace_EmptySymbolNameReturnsError(t *testing.T) {
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		t.Fatal("caller should not be invoked for empty symbol_name")
		return "", false, nil
	})

	tools := b.codeGraphTools()
	var traceTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_trace" {
			traceTool = tool
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"symbol_name": ""})
	result, err := traceTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_trace", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != core.OutcomeFailure {
		t.Fatal("expected failure for empty symbol_name")
	}
}

func TestCodebaseTrace_DefaultsDirectionToBoth(t *testing.T) {
	var receivedArgs map[string]any
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		receivedArgs = args
		return "{}", false, nil
	})

	tools := b.codeGraphTools()
	var traceTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_trace" {
			traceTool = tool
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"symbol_name": "whale.Run"})
	_, err := traceTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_trace", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v, ok := receivedArgs["direction"]; !ok || v != "both" {
		t.Fatalf("expected default direction=both, got %v", receivedArgs)
	}
}

func TestCodebaseSearch_HandlesMCPError(t *testing.T) {
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		return "code graph service unavailable", true, nil
	})

	tools := b.codeGraphTools()
	var searchTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_search" {
			searchTool = tool
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"query": "auth"})
	result, err := searchTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_search", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != core.OutcomeFailure {
		t.Fatal("expected failure when MCP reports error")
	}
	if !strings.Contains(result.ModelText, "code graph service unavailable") {
		t.Fatalf("expected error text in result, got %q", result.ModelText)
	}
}

func TestCodeGraphTools_ReadOnly(t *testing.T) {
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		return "{}", false, nil
	})

	tools := b.codeGraphTools()
	for _, tool := range tools {
		if !core.DescribeTool(tool).ReadOnly {
			t.Fatalf("%s should be read-only", tool.Name())
		}
	}
}

func TestCodebaseImpact_CallsCodeGraphWithTests(t *testing.T) {
	var receivedTool string
	var receivedArgs map[string]any
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		receivedTool = toolName
		receivedArgs = args
		return `{"callers": ["main"], "callees": ["helper"], "tests": ["auth_test.go"]}`, false, nil
	})

	tools := b.codeGraphTools()
	var impactTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_impact" {
			impactTool = tool
			break
		}
	}
	if impactTool == nil {
		t.Fatal("codebase_impact not found")
	}

	input, _ := json.Marshal(map[string]string{"symbol_name": "whale.Run"})
	result, err := impactTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_impact", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receivedTool != "trace_path" {
		t.Fatalf("expected trace_path, got %q", receivedTool)
	}
	if result.Code != "ok" {
		t.Fatalf("expected ok, got %q", result.Code)
	}
	if v, ok := receivedArgs["function_name"]; !ok || v != "whale.Run" {
		t.Fatalf("expected function_name=whale.Run, got %v", receivedArgs)
	}
	if v, ok := receivedArgs["include_tests"]; !ok || v != true {
		t.Fatalf("expected include_tests=true, got %v", receivedArgs)
	}
	if v, ok := receivedArgs["direction"]; !ok || v != "both" {
		t.Fatalf("expected direction=both, got %v", receivedArgs)
	}
}

func TestCodebaseImpact_EmptySymbolNameReturnsError(t *testing.T) {
	b := &Toolset{}
	b.SetMCPBridge(func(ctx context.Context, toolName string, args map[string]any) (string, bool, error) {
		t.Fatal("caller should not be invoked for empty symbol_name")
		return "", false, nil
	})

	tools := b.codeGraphTools()
	var impactTool core.Tool
	for _, tool := range tools {
		if tool.Name() == "codebase_impact" {
			impactTool = tool
			break
		}
	}

	input, _ := json.Marshal(map[string]string{"symbol_name": ""})
	result, err := impactTool.Run(context.Background(), core.ToolCall{ID: "c1", Name: "codebase_impact", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != core.OutcomeFailure {
		t.Fatal("expected failure for empty symbol_name")
	}
}

// Keyword matching is tested in the app package (TestCodeGraphDynamicSystemBlock_*).
