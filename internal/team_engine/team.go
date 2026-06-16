package team_engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TeamConfig defines a named team that can be assigned to execute a goal.
// Teams are loaded from .whale/teams/{name}.yaml
type TeamConfig struct {
	Label string     `yaml:"label"`
	Leader TeamLeaderConfig `yaml:"leader"`
	Roles  map[string]TeamRoleConfig `yaml:"roles"`
}

// TeamLeaderConfig configures the team's Leader agent.
type TeamLeaderConfig struct {
	Role        string   `yaml:"role"`
	Description string   `yaml:"description,omitempty"`
	Model       string   `yaml:"model,omitempty"`
	Prompt      string   `yaml:"prompt,omitempty"`
	Rules       []string `yaml:"rules,omitempty"` // 团队级规则，注入到所有子任务
}

// TeamRoleConfig defines one role/member in a team.
// A role can reference an agent definition (use_agent) or define everything inline.
// When use_agent is set, inline fields override the corresponding agent fields.
type TeamRoleConfig struct {
	// Agent reference — load from .whale/agents/{name}.yaml
	UseAgent string `yaml:"use_agent,omitempty"`

	// Inline fields (all optional when use_agent is set)
	Description     string   `yaml:"description,omitempty"`
	Prompt          string   `yaml:"prompt,omitempty"`
	Tools           []string `yaml:"tools,omitempty"`
	DisallowedTools []string `yaml:"disallowedTools,omitempty"`
	Skills          []string `yaml:"skills,omitempty"`
	MCPServers      []string `yaml:"mcpServers,omitempty"`
	Model           string   `yaml:"model,omitempty"`
	Effort          string   `yaml:"effort,omitempty"`
	PermissionMode  string   `yaml:"permissionMode,omitempty"`
	Memory          string   `yaml:"memory,omitempty"`
	MaxToolIters    int      `yaml:"maxToolIters,omitempty"`
	MaxToolCalls    int      `yaml:"maxToolCalls,omitempty"`
	Timeout         int      `yaml:"timeout,omitempty"` // seconds
	MaxRetries      int      `yaml:"maxRetries,omitempty"`
}

// LoadTeamConfig reads and parses a single team YAML file.
func LoadTeamConfig(path string) (*TeamConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read team file %s: %w", path, err)
	}
	var tc TeamConfig
	if err := yaml.Unmarshal(data, &tc); err != nil {
		return nil, fmt.Errorf("parse team file %s: %w", path, err)
	}
	if tc.Label == "" {
		tc.Label = strings.TrimSuffix(filepath.Base(path), ".yaml")
	}
	if tc.Leader.Role == "" {
		return nil, fmt.Errorf("team %s: leader.role is required", path)
	}
	return &tc, nil
}

// LoadAllTeams loads all team YAML files from a directory.
func LoadAllTeams(teamsDir string) ([]*TeamConfig, error) {
	entries, err := os.ReadDir(teamsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no teams dir is fine
		}
		return nil, fmt.Errorf("read teams dir: %w", err)
	}
	var teams []*TeamConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		tc, err := LoadTeamConfig(filepath.Join(teamsDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("load team %s: %w", e.Name(), err)
		}
		teams = append(teams, tc)
	}
	return teams, nil
}

// FindTeam loads a single team by name from the teams directory.
func FindTeam(teamsDir, name string) (*TeamConfig, error) {
	path := filepath.Join(teamsDir, name+".yaml")
	return LoadTeamConfig(path)
}

// DefaultTeamRoots returns the team discovery roots for a workspace.
// Workspace .whale/teams is listed first so it takes priority over global.
func DefaultTeamRoots(workspaceRoot string) []string {
	var roots []string
	if root := strings.TrimSpace(workspaceRoot); root != "" {
		roots = append(roots, filepath.Join(root, ".whale", "teams"))
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		roots = append(roots, filepath.Join(home, ".whale", "teams"))
	}
	return roots
}

// FindTeamInRoots loads a single team by name, searching multiple roots in order.
// Returns the first match found (workspace overrides global).
func FindTeamInRoots(roots []string, name string) (*TeamConfig, error) {
	for _, dir := range roots {
		tc, err := FindTeam(dir, name)
		if err == nil {
			return tc, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("team %q not found in any roots", name)
}

// LoadAllTeamsFromRoots loads all team YAML files from multiple roots.
// Workspace teams take priority over global teams with the same name.
func LoadAllTeamsFromRoots(roots []string) ([]*TeamConfig, error) {
	seen := make(map[string]bool)
	var teams []*TeamConfig
	// Iterate in reverse so earlier roots (workspace) overwrite later ones (global).
	for i := len(roots) - 1; i >= 0; i-- {
		dir := roots[i]
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read teams dir %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".yaml")
			if seen[name] {
				continue
			}
			tc, err := LoadTeamConfig(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("load team %s: %w", e.Name(), err)
			}
			seen[name] = true
			teams = append(teams, tc)
		}
	}
	return teams, nil
}

// BuildLeaderPrompt augments the base decompose prompt with team-specific
// leader instructions, available roles, and rules.
// When team roles are defined, the generic role list (Rule 5) in the base
// prompt is REPLACED so the AI does not pick generic names like "developer".
func (tc *TeamConfig) BuildLeaderPrompt(basePrompt string) string {
	var sb strings.Builder

	// If team has roles, strip the generic role list (Rule 5) from the base
	// prompt and replace it with team-specific roles.
	if len(tc.Roles) > 0 {
		// Remove the generic "5. Assign an appropriate ROLE..." section.
		basePrompt = stripGenericRoleSection(basePrompt)
	}

	sb.WriteString(basePrompt)

	if tc.Leader.Prompt != "" {
		sb.WriteString("\n\n## Your Role\n")
		sb.WriteString(tc.Leader.Prompt)
	}

	if len(tc.Roles) > 0 {
		sb.WriteString("\n\n## Available Team Roles\n")
		sb.WriteString("Rule 5 (ROLE ASSIGNMENT): You MUST assign EVERY subtask using EXACTLY ONE of the role names below. Do NOT invent new role names and do NOT use generic names like \"developer\", \"tester\", or \"researcher\".\n\n")
		for name, cfg := range tc.Roles {
			desc := cfg.Description
			if desc == "" {
				desc = name
			}
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", name, desc))
		}
	}

	if len(tc.Leader.Rules) > 0 {
		sb.WriteString("\n## Team Rules\n")
		sb.WriteString("The following rules apply to ALL subtasks. Make sure each task description includes them:\n")
		for i, rule := range tc.Leader.Rules {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, rule))
		}
	}
	return sb.String()
}

// stripGenericRoleSection removes the generic "5. Assign an appropriate ROLE"
// section from the base decompose prompt so team-specific roles take precedence.
func stripGenericRoleSection(prompt string) string {
	// Find "5. Assign an appropriate ROLE" and remove everything up to the
	// next numbered rule (6. Group tasks into batches) or to the end of the
	// role list (ends with "- \"synthesizer\" ...").
	idx := strings.Index(prompt, "5. Assign an appropriate ROLE")
	if idx < 0 {
		return prompt
	}
	// Find the next rule "6." that starts after the role list.
	endIdx := strings.Index(prompt[idx:], "\n6. Group tasks into **batches**")
	if endIdx < 0 {
		// Fallback: find "6. Group" without bold markers.
		endIdx = strings.Index(prompt[idx:], "\n6. Group tasks into batches")
	}
	if endIdx > 0 {
		return prompt[:idx] + prompt[idx+endIdx:]
	}
	return prompt[:idx]
}
