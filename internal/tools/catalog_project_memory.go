package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/usewhale/whale/internal/core"
)

func (b *Toolset) projectMemoryTools() []core.Tool {
	return []core.Tool{
		b.saveProjectMemoryTool(),
	}
}

func (b *Toolset) saveProjectMemoryTool() core.Tool {
	return toolFn{
		name:        "save_project_memory",
		description: "Save a fact, convention, or pattern about this project to persistent memory. Saved facts are loaded automatically in future sessions. Use this to remember: project-specific coding conventions, architecture decisions, key file locations, or anything you learn that would help next time. Format content as markdown.",
		parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"description": "Short kebab-case identifier for this fact (e.g., \"auth-pattern\", \"build-commands\", \"api-conventions\"). Used as the filename.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The fact or pattern to remember, in markdown format. Include specific file paths, code snippets, and rationale when helpful.",
				},
			},
			"required": []string{"key", "content"},
		},
		capabilities: []string{"workspace.write"},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var in struct {
				Key     string `json:"key"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
				return marshalToolError(call, "invalid_args", fmt.Sprintf("invalid save_project_memory input: %v", err)), nil
			}
			key := strings.TrimSpace(in.Key)
			content := strings.TrimSpace(in.Content)
			if key == "" || content == "" {
				return marshalToolError(call, "invalid_args", "key and content are required"), nil
			}
			// Validate key: only safe filename characters.
			for _, c := range key {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
					return marshalToolError(call, "invalid_args", fmt.Sprintf("key %q contains invalid character '%c' — use only a-z, 0-9, -, _", key, c)), nil
				}
			}

			dir := filepath.Join(b.root, ".whale", "memory")
			if err := os.MkdirAll(dir, 0700); err != nil {
				return marshalToolError(call, "write_failed", fmt.Sprintf("create memory dir: %v", err)), nil
			}
			path := filepath.Join(dir, key+".md")
			if err := os.WriteFile(path, []byte(content+"\n"), 0600); err != nil {
				return marshalToolError(call, "write_failed", fmt.Sprintf("write memory file: %v", err)), nil
			}
			return core.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				ModelText:  fmt.Sprintf(`{"ok":true,"saved":"%s","path":"%s"}`, key, path),
				Outcome:    core.OutcomeSuccess,
				Code:       "ok",
			}, nil
		},
	}
}
