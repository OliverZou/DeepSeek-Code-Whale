package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/usewhale/whale/internal/core"
)

// sanitizeDefineName strips path separators and traversal from a user-supplied
// team/agent name so writes stay inside the target directory.
func sanitizeDefineName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	return strings.Trim(s, "-. ")
}

// --- agent_define ---

// agentDefineTool writes an agent definition to ~/.whale/agents/{name}.md so a
// team role can reference it. Whale-only (filtered in sessionToolRegistry).
func (b *Toolset) agentDefineTool() toolFn {
	return toolFn{
		name:        "agent_define",
		description: "Create or overwrite an agent definition at ~/.whale/agents/{name}.md so a team role can use it (via team_define role.use_agent). Use when assembling a team and a needed role has no existing agent. Required: name (file id), role, prompt (the agent's system prompt: persona + capabilities + output spec). Optional: description, when_to_use, model.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Agent id / filename without extension"},
				"role":        map[string]any{"type": "string", "description": "developer/tester/reviewer/researcher/writer/formatter/evaluator/synthesizer"},
				"description": map[string]any{"type": "string", "description": "One-line description"},
				"prompt":      map[string]any{"type": "string", "description": "The agent's full system prompt: persona, capabilities, output spec"},
				"when_to_use": map[string]any{"type": "string"},
				"model":       map[string]any{"type": "string"},
			},
			"required": []string{"name", "role", "prompt"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var a struct {
				Name        string `json:"name"`
				Role        string `json:"role"`
				Description string `json:"description"`
				Prompt      string `json:"prompt"`
				WhenToUse   string `json:"when_to_use"`
				Model       string `json:"model"`
			}
			if err := json.Unmarshal([]byte(call.Input), &a); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			a.Name = sanitizeDefineName(a.Name)
			if a.Name == "" || strings.TrimSpace(a.Prompt) == "" {
				return toolError("name and prompt are required"), nil
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return toolError("home dir: %v", err), nil
			}
			dir := filepath.Join(home, ".whale", "agents")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return toolError("mkdir: %v", err), nil
			}
			fm := map[string]any{"name": a.Name, "role": a.Role, "description": a.Description}
			if a.WhenToUse != "" {
				fm["whenToUse"] = a.WhenToUse
			}
			if a.Model != "" {
				fm["model"] = a.Model
			}
			fmBytes, err := yaml.Marshal(fm)
			if err != nil {
				return toolError("marshal frontmatter: %v", err), nil
			}
			content := "---\n" + string(fmBytes) + "---\n\n" + strings.TrimSpace(a.Prompt) + "\n"
			path := filepath.Join(dir, a.Name+".md")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return toolError("write: %v", err), nil
			}
			return toolResult(fmt.Sprintf("Created agent definition %s (role=%s). Reference it from team_define via use_agent=%q.", path, a.Role, a.Name)), nil
		},
	}
}

// --- team_define ---

// teamDefineTool writes a team definition to ~/.whale/teams/{name}/team.yaml,
// which team_plan(team=name) can then execute. Whale-only.
func (b *Toolset) teamDefineTool() toolFn {
	return toolFn{
		name:        "team_define",
		description: "Create or overwrite a team definition at ~/.whale/teams/{name}/team.yaml, then execute it via team_plan(goal, team=name). Use when team_roster shows no matching team. Provide a leader and roles; each role references an existing agent (use_agent, e.g. one made with agent_define) or defines an inline prompt.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":               map[string]any{"type": "string", "description": "Team id / folder name (pass this to team_plan's team param)"},
				"label":              map[string]any{"type": "string", "description": "Display name"},
				"leader_role":        map[string]any{"type": "string", "description": "Leader role title, e.g. 项目经理"},
				"leader_description": map[string]any{"type": "string"},
				"leader_prompt":      map[string]any{"type": "string"},
				"roles": map[string]any{
					"type":        "array",
					"description": "Team member roles",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":        map[string]any{"type": "string", "description": "Role title"},
							"description": map[string]any{"type": "string"},
							"use_agent":   map[string]any{"type": "string", "description": "Existing agent id (from agent_define or ~/.whale/agents)"},
							"prompt":      map[string]any{"type": "string", "description": "Inline prompt when there is no use_agent"},
							"model":       map[string]any{"type": "string"},
						},
						"required": []string{"name", "description"},
					},
				},
			},
			"required": []string{"name", "leader_role", "roles"},
		},
		fn: func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			var a struct {
				Name              string `json:"name"`
				Label             string `json:"label"`
				LeaderRole        string `json:"leader_role"`
				LeaderDescription string `json:"leader_description"`
				LeaderPrompt      string `json:"leader_prompt"`
				Roles             []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					UseAgent    string `json:"use_agent"`
					Prompt      string `json:"prompt"`
					Model       string `json:"model"`
				} `json:"roles"`
			}
			if err := json.Unmarshal([]byte(call.Input), &a); err != nil {
				return toolError("invalid args: %v", err), nil
			}
			a.Name = sanitizeDefineName(a.Name)
			if a.Name == "" || strings.TrimSpace(a.LeaderRole) == "" || len(a.Roles) == 0 {
				return toolError("name, leader_role and at least one role are required"), nil
			}
			label := a.Label
			if label == "" {
				label = a.Name
			}
			leader := map[string]any{"role": a.LeaderRole}
			if a.LeaderDescription != "" {
				leader["description"] = a.LeaderDescription
			}
			if a.LeaderPrompt != "" {
				leader["prompt"] = a.LeaderPrompt
			}
			roles := map[string]any{}
			var roleNames []string
			for _, r := range a.Roles {
				if strings.TrimSpace(r.Name) == "" {
					continue
				}
				rm := map[string]any{"description": r.Description}
				if r.UseAgent != "" {
					rm["use_agent"] = r.UseAgent
				}
				if r.Prompt != "" {
					rm["prompt"] = r.Prompt
				}
				if r.Model != "" {
					rm["model"] = r.Model
				}
				roles[r.Name] = rm
				roleNames = append(roleNames, r.Name)
			}
			if len(roles) == 0 {
				return toolError("no valid roles"), nil
			}
			data, err := yaml.Marshal(map[string]any{"label": label, "leader": leader, "roles": roles})
			if err != nil {
				return toolError("marshal: %v", err), nil
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return toolError("home dir: %v", err), nil
			}
			dir := filepath.Join(home, ".whale", "teams", a.Name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return toolError("mkdir: %v", err), nil
			}
			path := filepath.Join(dir, "team.yaml")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return toolError("write: %v", err), nil
			}
			return toolResult(fmt.Sprintf("Created team %q at %s with roles: %s. Now delegate with team_plan(goal, team=%q).", a.Name, path, strings.Join(roleNames, ", "), a.Name)), nil
		},
	}
}
