package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/usewhale/whale/internal/core"
)

func (b *Toolset) codeGraphTools() []core.Tool {
	if b.codeGraphCaller == nil {
		return nil
	}
	return []core.Tool{
		b.codebaseSearchTool(),
		b.codebaseTraceTool(),
		b.codebaseImpactTool(),
	}
}

// defaultCodeGraphProject returns the project name for the current workspace.
// Uses the auto-detected name from setup if available; falls back to the
// workspace root's base directory name.
func (b *Toolset) defaultCodeGraphProject() string {
	if b.codeGraphProject != "" {
		return b.codeGraphProject
	}
	return filepath.Base(b.root)
}

func (b *Toolset) codebaseSearchTool() core.Tool {
	return toolFn{
		name:        "codebase_search",
		description: "Search the code knowledge graph for functions, classes, methods, and other symbols. Use this for semantic code discovery — finding where things are defined, understanding who calls what, and locating relevant code by purpose or natural-language description. Returns matching symbols with their qualified names and locations. Prefer this over grep when searching for code structure (functions, classes, modules). Use grep for text patterns, error messages, and string literals.",
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Natural-language or keyword query describing what to find. E.g., \"authentication handler\", \"user login function\", \"database connection pool\".",
				},
				"project": map[string]any{
					"type":        "string",
					"description": "Codebase-memory project name. Defaults to the current workspace name if omitted.",
				},
			},
			"required": []string{"query"},
		},
		readOnly:     true,
		capabilities: []string{"workspace.read"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				Query   string `json:"query"`
				Project string `json:"project"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid codebase_search input: %v", err)), nil
			}
			if strings.TrimSpace(in.Query) == "" {
				return marshalToolError(call, "invalid_args", "query is required"), nil
			}
			if in.Project == "" {
				in.Project = b.defaultCodeGraphProject()
			}

			text, isError, err := b.codeGraphCaller(ctx, "search_graph", map[string]any{
				"query":   in.Query,
				"project": in.Project,
			})
			if err != nil {
				return marshalToolError(call, "code_graph_error", fmt.Sprintf("code graph search failed: %v", err)), nil
			}
			if isError {
				return core.ToolResult{
					ToolCallID: call.ID,
					Name:       call.Name,
					ModelText:  text,
					Outcome:    core.OutcomeFailure,
					Code:       "code_graph_error",
				}, nil
			}
			return core.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				ModelText:  text,
				Outcome:    core.OutcomeSuccess,
				Code:       "ok",
			}, nil
		},
	}
}

func (b *Toolset) codebaseTraceTool() core.Tool {
	return toolFn{
		name:        "codebase_trace",
		description: "Trace call chains and data flow from a symbol. Returns callers (inbound), callees (outbound), or both directions. Use this after codebase_search to understand how a symbol fits into the codebase — who calls it, what it depends on, and what depends on it. Accepts a qualified name from codebase_search results.",
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"symbol_name": map[string]any{
					"type":        "string",
					"description": "Qualified name of the symbol to trace, as returned by codebase_search (e.g., \"whale.internal.agent.RunStream\").",
				},
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"inbound", "outbound", "both"},
					"description": "Trace direction: \"inbound\" for callers, \"outbound\" for callees, \"both\" for full context. Defaults to \"both\" if omitted.",
				},
				"project": map[string]any{
					"type":        "string",
					"description": "Codebase-memory project name. Defaults to the current workspace name if omitted.",
				},
			},
			"required": []string{"symbol_name"},
		},
		readOnly:     true,
		capabilities: []string{"workspace.read"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				SymbolName string `json:"symbol_name"`
				Direction  string `json:"direction"`
				Project    string `json:"project"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid codebase_trace input: %v", err)), nil
			}
			if strings.TrimSpace(in.SymbolName) == "" {
				return marshalToolError(call, "invalid_args", "symbol_name is required"), nil
			}
			if in.Direction == "" {
				in.Direction = "both"
			}
			if in.Project == "" {
				in.Project = b.defaultCodeGraphProject()
			}

			text, isError, err := b.codeGraphCaller(ctx, "trace_path", map[string]any{
				"function_name": in.SymbolName,
				"direction":     in.Direction,
				"project":       in.Project,
			})
			if err != nil {
				return marshalToolError(call, "code_graph_error", fmt.Sprintf("code graph trace failed: %v", err)), nil
			}
			if isError {
				return core.ToolResult{
					ToolCallID: call.ID,
					Name:       call.Name,
					ModelText:  text,
					Outcome:    core.OutcomeFailure,
					Code:       "code_graph_error",
				}, nil
			}
			return core.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				ModelText:  text,
				Outcome:    core.OutcomeSuccess,
				Code:       "ok",
			}, nil
		},
	}
}

func (b *Toolset) codebaseImpactTool() core.Tool {
	return toolFn{
		name:        "codebase_impact",
		description: "Analyze the impact of a symbol — returns callers, callees, and related tests. Use this AFTER editing a function to understand what your change affects: which callers might break, and which tests should be run to validate the change. Also useful before editing to assess the blast radius of a planned change. Use codebase_trace for lightweight call-chain exploration; use codebase_impact when you need the full picture including tests.",
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"symbol_name": map[string]any{
					"type":        "string",
					"description": "Qualified name of the symbol to analyze, as returned by codebase_search (e.g., \"whale.internal.agent.RunStream\").",
				},
				"project": map[string]any{
					"type":        "string",
					"description": "Codebase-memory project name. Defaults to the current workspace name if omitted.",
				},
			},
			"required": []string{"symbol_name"},
		},
		readOnly:     true,
		capabilities: []string{"workspace.read"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				SymbolName string `json:"symbol_name"`
				Project    string `json:"project"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid codebase_impact input: %v", err)), nil
			}
			if strings.TrimSpace(in.SymbolName) == "" {
				return marshalToolError(call, "invalid_args", "symbol_name is required"), nil
			}
			if in.Project == "" {
				in.Project = b.defaultCodeGraphProject()
			}

			text, isError, err := b.codeGraphCaller(ctx, "trace_path", map[string]any{
				"function_name": in.SymbolName,
				"direction":     "both",
				"include_tests": true,
				"depth":         2,
				"project":       in.Project,
			})
			if err != nil {
				return marshalToolError(call, "code_graph_error", fmt.Sprintf("code graph impact analysis failed: %v", err)), nil
			}
			if isError {
				return core.ToolResult{
					ToolCallID: call.ID,
					Name:       call.Name,
					ModelText:  text,
					Outcome:    core.OutcomeFailure,
					Code:       "code_graph_error",
				}, nil
			}
			return core.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				ModelText:  text,
				Outcome:    core.OutcomeSuccess,
				Code:       "ok",
			}, nil
		},
	}
}
