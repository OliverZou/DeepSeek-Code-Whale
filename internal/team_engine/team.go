package team_engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TeamConfig defines a named team that can be assigned to execute a goal.
// Teams are loaded from .whale/teams/{name}.yaml or {name}/team.yaml
type TeamConfig struct {
	Label    string                    `yaml:"label"`
	Category string                    `yaml:"category,omitempty"`
	Leader   TeamLeaderConfig          `yaml:"leader"`
	Roles    []string                  `yaml:"roles"`
	Config    *TeamRuntimeConfig       `yaml:"-"` // loaded from config.yaml
	Pipeline  *PipelineFile            `yaml:"-"` // loaded from pipeline.yaml
	MemoryDir    string `yaml:"-"` // memory directory path
	TemplatesDir string `yaml:"-"` // templates directory path
	// Resolved agent info populated by ResolveRoles().
	// Maps agent name → display role title (e.g. "backend-engineer" → "后端工程师").
	RoleTitles    map[string]string `yaml:"-"`
	RoleDescs     map[string]string `yaml:"-"`
	RoleUseAgents map[string]string `yaml:"-"` // agent name → agent name (identity, for compat)
}

// PipelineFile represents the advisory pipeline templates for a team.
type PipelineFile struct {
	Pipelines map[string]PipelineDef `yaml:"pipelines"`
	Default   string                 `yaml:"default"`
}

// PipelineDef is one pipeline template (e.g. "new-feature", "bugfix").
type PipelineDef struct {
	Description string         `yaml:"description"`
	Trigger     string         `yaml:"trigger"`
	Stages      []PipelineStage `yaml:"stages"`
}

// PipelineStage is one stage in a pipeline template.
type PipelineStage struct {
	ID         string   `yaml:"id"`
	Label      string   `yaml:"label"`
	Roles      []string `yaml:"roles"`
	DependsOn  []string `yaml:"depends_on"`
	Parallel   bool     `yaml:"parallel"`
	VerifyBy   []string `yaml:"verify_by"`
	Output     string   `yaml:"output"`
	Next       []string `yaml:"next"`
}

// TeamRuntimeConfig is loaded from the team directory's config.yaml.
type TeamRuntimeConfig struct {
	MaxAgents      int    `yaml:"max_agents"`
	DefaultTimeout int    `yaml:"default_timeout"`
	Model          struct {
		Leader         string `yaml:"leader"`
		WorkerDefault  string `yaml:"worker_default"`
		VerifierDefault string `yaml:"verifier_default"`
	} `yaml:"model"`
	Workdir string `yaml:"workdir"`
	Deploy  DeployConfig `yaml:"deploy"`
}

// DeployConfig defines where agents run.  Team-wide default — individual
// roles can override with their own Host field.
type DeployConfig struct {
	Host   string `yaml:"host"`   // server address (empty = local)
	Port   int    `yaml:"port"`   // SSH port
	User   string `yaml:"user"`   // SSH user
	KeyFile string `yaml:"keyfile,omitempty"` // SSH key path
}

// TeamLeaderConfig configures the team's Leader agent.
type TeamLeaderConfig struct {
	Role        string   `yaml:"role"`
	Description string   `yaml:"description,omitempty"`
	Model       string   `yaml:"model,omitempty"`
	Prompt      string   `yaml:"prompt,omitempty"`
	Rules       []string `yaml:"rules,omitempty"` // 团队级规则，注入到所有子任务
}

// TeamRoleConfig is kept for backward compatibility with inline role overrides.
// New team.yaml files should use the simplified roles: []string format.
type TeamRoleConfig struct {
	UseAgent string `yaml:"use_agent,omitempty"`

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
	Output          string   `yaml:"output,omitempty"`
	Host            string   `yaml:"host,omitempty"`
	MaxToolIters    int      `yaml:"maxToolIters,omitempty"`
	MaxToolCalls    int      `yaml:"maxToolCalls,omitempty"`
	Timeout         int      `yaml:"timeout,omitempty"`
	MaxRetries      int      `yaml:"maxRetries,omitempty"`
}

// AgentInfoProvider resolves agent names to their display info.
// Implemented by the app layer to avoid team_engine → tasks import cycle.
type AgentInfoProvider interface {
	AgentRole(name string) string // returns display title, e.g. "后端工程师"
	AgentDesc(name string) string // returns description
}

// ResolveRoles populates RoleTitles and RoleDescs from an AgentInfoProvider.
func (tc *TeamConfig) ResolveRoles(p AgentInfoProvider) {
	tc.RoleTitles = make(map[string]string, len(tc.Roles))
	tc.RoleDescs = make(map[string]string, len(tc.Roles))
	tc.RoleUseAgents = make(map[string]string, len(tc.Roles))
	for _, name := range tc.Roles {
		tc.RoleUseAgents[name] = name
		if p != nil {
			if r := p.AgentRole(name); r != "" {
				tc.RoleTitles[name] = r
			}
			if d := p.AgentDesc(name); d != "" {
				tc.RoleDescs[name] = d
			}
		}
	}
}

// RoleDisplayName returns the human-readable title for an agent name.
// Falls back to the agent name itself if no title is resolved.
func (tc *TeamConfig) RoleDisplayName(agentName string) string {
	if tc.RoleTitles != nil {
		if t, ok := tc.RoleTitles[agentName]; ok && t != "" {
			return t
		}
	}
	return agentName
}

// RoleDisplayDesc returns the description for an agent name.
func (tc *TeamConfig) RoleDisplayDesc(agentName string) string {
	if tc.RoleDescs != nil {
		if d, ok := tc.RoleDescs[agentName]; ok && d != "" {
			return d
		}
	}
	return agentName
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
// Supports both flat files (name.yaml) and directory structure (name/team.yaml).
// When loading from directory, also loads config.yaml if present.
func FindTeam(teamsDir, name string) (*TeamConfig, error) {
	// Priority 1: directory-based team (name/team.yaml)
	dirPath := filepath.Join(teamsDir, name, "team.yaml")
	if _, err := os.Stat(dirPath); err == nil {
		tc, err := LoadTeamConfig(dirPath)
		if err == nil {
			tc.Config = loadRuntimeConfig(filepath.Join(teamsDir, name, "config.yaml"))
			tc.Pipeline = loadPipelineFile(filepath.Join(teamsDir, name, "pipeline.yaml"))
			tc.MemoryDir = filepath.Join(teamsDir, name, "memory")
			tc.TemplatesDir = filepath.Join(teamsDir, name, "templates")
			return tc, nil
		}
	}
	// Priority 2: flat file (name.yaml)
	path := filepath.Join(teamsDir, name+".yaml")
	return LoadTeamConfig(path)
}

// loadRuntimeConfig reads config.yaml if it exists; returns nil otherwise.
func loadRuntimeConfig(path string) *TeamRuntimeConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg TeamRuntimeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// loadPipelineFile reads pipeline.yaml if it exists; returns nil otherwise.
func loadPipelineFile(path string) *PipelineFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pf PipelineFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil
	}
	return &pf
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
		var tc *TeamConfig
		var name string
		if e.IsDir() {
			// Directory-based team: name/team.yaml
			tc, _ = LoadTeamConfig(filepath.Join(dir, e.Name(), "team.yaml"))
			name = e.Name()
			if tc != nil {
				tc.Pipeline = loadPipelineFile(filepath.Join(dir, e.Name(), "pipeline.yaml"))
			}
		} else if strings.HasSuffix(e.Name(), ".yaml") {
			// Flat file: name.yaml
			tc, _ = LoadTeamConfig(filepath.Join(dir, e.Name()))
			name = strings.TrimSuffix(e.Name(), ".yaml")
		}
		if tc == nil || seen[name] {
			continue
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
		for _, name := range tc.Roles {
			title := tc.RoleDisplayName(name)
			desc := tc.RoleDisplayDesc(name)
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", title, desc))
		}
	}

	if len(tc.Leader.Rules) > 0 {
		sb.WriteString("\n## Team Rules\n")
		sb.WriteString("The following rules apply to ALL subtasks. Make sure each task description includes them:\n")
		for i, rule := range tc.Leader.Rules {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, rule))
		}
	}

	if tc.Pipeline != nil && len(tc.Pipeline.Pipelines) > 0 {
		sb.WriteString("\n## Pipeline Templates (ADVISORY — use as reference, not mandatory)\n")
		sb.WriteString("The following pipeline templates suggest how to decompose common goal types. Match the goal to a template by trigger keywords. If no template matches, decompose freely.\n\n")
		for name, p := range tc.Pipeline.Pipelines {
			sb.WriteString(fmt.Sprintf("### Pipeline: %s\n", name))
			sb.WriteString(fmt.Sprintf("- Description: %s\n", p.Description))
			if p.Trigger != "" {
				sb.WriteString(fmt.Sprintf("- Trigger keywords: %s\n", p.Trigger))
			}
			sb.WriteString("- Stages:\n")
			for _, s := range p.Stages {
				sb.WriteString(fmt.Sprintf("  - %s (%s): roles=%v", s.ID, s.Label, s.Roles))
				if len(s.DependsOn) > 0 {
					sb.WriteString(fmt.Sprintf(", depends_on=%v", s.DependsOn))
				}
				if s.Parallel {
					sb.WriteString(", parallel=true")
				}
				if len(s.VerifyBy) > 0 {
					sb.WriteString(fmt.Sprintf(", verify_by=%v", s.VerifyBy))
				}
				if s.Output != "" {
					sb.WriteString(fmt.Sprintf(", output=%s", s.Output))
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
		if tc.Pipeline.Default != "" {
			sb.WriteString(fmt.Sprintf("Default strategy when no template matches: %s\n", tc.Pipeline.Default))
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
