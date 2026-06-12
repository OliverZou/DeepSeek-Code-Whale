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

// BuildLeaderPrompt augments the standard decompose prompt with team-specific
// leader instructions and rules.
func (tc *TeamConfig) BuildLeaderPrompt(basePrompt string) string {
	var sb strings.Builder
	sb.WriteString(basePrompt)
	if tc.Leader.Prompt != "" {
		sb.WriteString("\n\n## Your Role\n")
		sb.WriteString(tc.Leader.Prompt)
	}
	if len(tc.Leader.Rules) > 0 {
		sb.WriteString("\n\n## Team Rules\n")
		sb.WriteString("The following rules apply to ALL subtasks. Make sure each task description includes them:\n")
		for i, rule := range tc.Leader.Rules {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, rule))
		}
	}
	return sb.String()
}
