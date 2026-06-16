package main

import (
	"encoding/json"
	"fmt"
)

func main() {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":              map[string]any{"type": "string"},
						"description":        map[string]any{"type": "string"},
						"role":               map[string]any{"type": "string"},
						"batch_id":           map[string]any{"type": "string"},
						"batch_label":        map[string]any{"type": "string"},
						"depends_on_batch":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"depends_on_index":   map[string]any{"type": "integer"},
						"verifier_focus":     map[string]any{"type": "string"},
						"use_dw":             map[string]any{"type": "boolean"},
						"max_cycles":         map[string]any{"type": "integer"},
					},
					"required": []string{"title", "description", "role"},
				},
			},
		},
		"required": []string{"tasks"},
	}
	data, _ := json.MarshalIndent(schema, "", "  ")
	fmt.Println(string(data))
}
