package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/usewhale/whale/internal/core"
)

func (b *Toolset) astEditTools() []core.Tool {
	if b.astEditCaller == nil {
		return nil
	}
	return []core.Tool{
		b.astEditTool(),
		b.astPatchTool(),
		b.astSymbolsTool(),
	}
}

// astEditTool replaces an entire symbol (function/method/class) by name.
// This is the AST-level equivalent of edit+write — safer because it locates by
// symbol name instead of exact string matching.
func (b *Toolset) astEditTool() core.Tool {
	return toolFn{
		name: "ast_edit",
		description: `Replace an entire symbol (function, method, class, etc.) by name. Safer than edit/write because it locates the target via AST rather than exact string matching — no fragile old_string copy-paste. Verifies the file still parses after replacement.

Use this when you know exactly what function or method to rewrite. Use ast_patch when you only need to change a few lines inside a symbol.`,
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Absolute path to the source file.",
				},
				"symbol_name": map[string]any{
					"type":        "string",
					"description": "Exact name of the symbol to replace (e.g., \"Authenticate\", \"LoginHandler\").",
				},
				"new_code": map[string]any{
					"type":        "string",
					"description": "Complete new definition including signature and body.",
				},
			},
			"required": []string{"file_path", "symbol_name", "new_code"},
		},
		capabilities: []string{"workspace.write"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				FilePath   string `json:"file_path"`
				SymbolName string `json:"symbol_name"`
				NewCode    string `json:"new_code"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid ast_edit input: %v", err)), nil
			}
			if strings.TrimSpace(in.FilePath) == "" || strings.TrimSpace(in.SymbolName) == "" {
				return marshalToolError(call, "invalid_args", "file_path and symbol_name are required"), nil
			}

			return b.callASTTool(ctx, call, "ast_write", map[string]any{
				"file_path":   in.FilePath,
				"symbol_name": in.SymbolName,
				"new_code":    in.NewCode,
			})
		},
	}
}

// astPatchTool patches code INSIDE a named symbol body. Only searches within
// the symbol — no global string matching. Safer than edit for targeted changes
// inside large functions.
func (b *Toolset) astPatchTool() core.Tool {
	return toolFn{
		name: "ast_patch",
		description: `Patch code inside a named symbol (function/method body). Finds old_snippet and replaces with new_snippet, but ONLY searches within the symbol body — no global string matching. Safer than edit for targeted changes inside large functions.

Use ast_edit to replace an entire symbol. Use ast_patch when you only need to change a few lines inside a symbol.`,
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Absolute path to the source file.",
				},
				"symbol_name": map[string]any{
					"type":        "string",
					"description": "Exact name of the enclosing function/method.",
				},
				"old_snippet": map[string]any{
					"type":        "string",
					"description": "Code snippet to find INSIDE the symbol body. Leave empty to append at end of function.",
				},
				"new_snippet": map[string]any{
					"type":        "string",
					"description": "Replacement code. If old_snippet is empty, this is appended at end of function body.",
				},
			},
			"required": []string{"file_path", "symbol_name", "new_snippet"},
		},
		capabilities: []string{"workspace.write"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				FilePath   string `json:"file_path"`
				SymbolName string `json:"symbol_name"`
				OldSnippet string `json:"old_snippet"`
				NewSnippet string `json:"new_snippet"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid ast_patch input: %v", err)), nil
			}
			if strings.TrimSpace(in.FilePath) == "" || strings.TrimSpace(in.SymbolName) == "" {
				return marshalToolError(call, "invalid_args", "file_path and symbol_name are required"), nil
			}

			return b.callASTTool(ctx, call, "ast_patch", map[string]any{
				"file_path":   in.FilePath,
				"symbol_name": in.SymbolName,
				"old_snippet": in.OldSnippet,
				"new_snippet": in.NewSnippet,
			})
		},
	}
}

// astSymbolsTool lists all top-level symbols in a source file.
// Replaces the common pattern of "read file → mentally parse structure".
func (b *Toolset) astSymbolsTool() core.Tool {
	return toolFn{
		name: "ast_symbols",
		description: `List all top-level symbols (functions, classes, methods) in a source file. Returns names and kinds. Use this before editing to map the file structure, instead of reading the whole file and mentally parsing it.

Use ast_edit or ast_patch to make changes after identifying the target symbol with ast_symbols.`,
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Absolute path to the source file.",
				},
			},
			"required": []string{"file_path"},
		},
		readOnly:     true,
		capabilities: []string{"workspace.read"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid ast_symbols input: %v", err)), nil
			}
			if strings.TrimSpace(in.FilePath) == "" {
				return marshalToolError(call, "invalid_args", "file_path is required"), nil
			}

			return b.callASTTool(ctx, call, "ast_symbols", map[string]any{
				"file_path": in.FilePath,
			})
		},
	}
}

func (b *Toolset) callASTTool(ctx context.Context, call core.ToolCall, mcpTool string, args map[string]any) (core.ToolResult, error) {
	text, isError, err := b.astEditCaller(ctx, mcpTool, args)
	if err != nil {
		return marshalToolError(call, "ast_edit_error", fmt.Sprintf("AST tool %s failed: %v", mcpTool, err)), nil
	}
	if isError {
		return core.ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			ModelText:  text,
			Outcome:    core.OutcomeFailure,
			Code:       "ast_edit_error",
		}, nil
	}
	return core.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		ModelText:  text,
		Outcome:    core.OutcomeSuccess,
		Code:       "ok",
	}, nil
}
