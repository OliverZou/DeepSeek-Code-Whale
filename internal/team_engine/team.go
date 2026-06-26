package team_engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// RoutingEntry maps intent keywords to roles and execution mode.
type RoutingEntry struct {
	Intent string   `yaml:"intent"` // keywords separated by |
	Roles  []string `yaml:"roles"`
	Mode   string   `yaml:"mode"` // "agent" or "team"
}

// TeamConfig defines a named team that can be assigned to execute a goal.
// Teams are loaded from .whale/teams/{name}.yaml or {name}/team.yaml
type TeamConfig struct {
	Label        string                    `yaml:"label"`
	Category     string                    `yaml:"category,omitempty"`
	Capabilities []string                  `yaml:"capabilities,omitempty"`
	Routing      []RoutingEntry            `yaml:"routing,omitempty"`
	Leader       TeamLeaderConfig          `yaml:"leader"`
	Roles        []string                  `yaml:"roles"`
	Config    *TeamRuntimeConfig       `yaml:"-"` // loaded from config.yaml

	MemoryDir    string `yaml:"-"` // memory directory path
	TemplatesDir string `yaml:"-"` // templates directory path
	TeamDir      string `yaml:"-"` // team directory path (for loading team-local agents)
	// Resolved agent info populated by ResolveRoles().
	// Maps role ref (from team.yaml roles[]) → resolved values.
	RoleTitles    map[string]string `yaml:"-"`
	RoleDescs     map[string]string `yaml:"-"`
	RoleUseAgents map[string]string `yaml:"-"` // role ref → agent name
	RoleCapabilities map[string]string `yaml:"-"`
	RoleOutputSpecs  map[string]string `yaml:"-"`
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
	AgentCapabilities(name string) string // returns "核心能力" section from agent MD
	AgentOutputSpec(name string) string   // returns "输出规范" section from agent MD
}

// ResolveRoles populates RoleTitles and RoleDescs from an AgentInfoProvider.
// When an ExpertRegistry is provided, roles like "软件工程/后端工程师" are
// resolved through the expert layer: display name comes from the expert
// definition, and the agent name is resolved from expert.agent.
func (tc *TeamConfig) ResolveRoles(p AgentInfoProvider, experts ...*ExpertRegistry) {
	var reg *ExpertRegistry
	if len(experts) > 0 && experts[0] != nil {
		reg = experts[0]
	}

	tc.RoleTitles = make(map[string]string, len(tc.Roles))
	tc.RoleDescs = make(map[string]string, len(tc.Roles))
	tc.RoleUseAgents = make(map[string]string, len(tc.Roles))
	tc.RoleCapabilities = make(map[string]string, len(tc.Roles))
	tc.RoleOutputSpecs = make(map[string]string, len(tc.Roles))

	for _, ref := range tc.Roles {
		agentName := ref
		displayName := ref

		if reg != nil {
			exp := reg.Resolve(ref)
			if exp != nil {
				displayName = exp.Name
				if exp.Agent != "" {
					agentName = exp.Agent
				}
			}
		}

		tc.RoleUseAgents[ref] = agentName
		tc.RoleTitles[ref] = displayName

		if p != nil {
			if r := p.AgentRole(agentName); r != "" && displayName == ref {
				tc.RoleTitles[ref] = r
			}
			if d := p.AgentDesc(agentName); d != "" {
				tc.RoleDescs[ref] = d
			}
			if c := p.AgentCapabilities(agentName); c != "" {
				tc.RoleCapabilities[ref] = c
			}
			if o := p.AgentOutputSpec(agentName); o != "" {
				tc.RoleOutputSpecs[ref] = o
			}
		}
	}
}

// RoleDisplayName returns the human-readable title for a role ref.
// Falls back to the role ref itself if no title is resolved.
func (tc *TeamConfig) RoleDisplayName(ref string) string {
	if tc.RoleTitles != nil {
		if t, ok := tc.RoleTitles[ref]; ok && t != "" {
			return t
		}
	}
	return ref
}

// RoleDisplayDesc returns the description for a role ref.
func (tc *TeamConfig) RoleDisplayDesc(ref string) string {
	if tc.RoleDescs != nil {
		if d, ok := tc.RoleDescs[ref]; ok && d != "" {
			return d
		}
	}
	return ref
}

// RoleAgentName returns the underlying agent name for a role ref.
// When experts are used, this resolves "软件工程/后端工程师" → "backend-engineer".
func (tc *TeamConfig) RoleAgentName(ref string) string {
	if tc.RoleUseAgents != nil {
		if a, ok := tc.RoleUseAgents[ref]; ok && a != "" {
			return a
		}
	}
	return ref
}

// LoadTeamAgentPrompt reads a team-local agent MD file from the team's agents/ directory.
// Returns ("", false) if no team-local agent exists.
func (tc *TeamConfig) LoadTeamAgentPrompt(agentName string) (string, bool) {
	if tc.TeamDir == "" {
		return "", false
	}
	path := filepath.Join(tc.TeamDir, "agents", agentName+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// LoadTeamConfig reads and parses a single team YAML file.
func LoadTeamConfig(path string) (*TeamConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err // keep IsNotExist so callers can skip
		}
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
	// If the teams directory itself doesn't exist, skip early.
	if _, err := os.Stat(teamsDir); os.IsNotExist(err) {
		return nil, err
	}
	// Priority 1: directory-based team (name/team.yaml)
	dirPath := filepath.Join(teamsDir, name, "team.yaml")
	if _, err := os.Stat(dirPath); err == nil {
		tc, err := LoadTeamConfig(dirPath)
		if err == nil {
			tc.Config = loadRuntimeConfig(filepath.Join(teamsDir, name, "config.yaml"))
			tc.TeamDir = filepath.Join(teamsDir, name)
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


// DefaultTeamRoots returns the team discovery roots for a workspace.
// Priority: workspace > global home > bundled (next to the executable).
func DefaultTeamRoots(workspaceRoot string) []string {
	var roots []string
	if root := strings.TrimSpace(workspaceRoot); root != "" {
		roots = append(roots, filepath.Join(root, ".whale", "teams"))
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		roots = append(roots, filepath.Join(home, ".whale", "teams"))
	}
	// Bundled teams shipped with the binary (bin/teams/ next to the exe).
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Join(filepath.Dir(exe), "bin", "teams"))
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
			tc, _ = LoadTeamConfig(filepath.Join(dir, e.Name(), "team.yaml"))
			name = e.Name()
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
		for _, ref := range tc.Roles {
			title := tc.RoleDisplayName(ref)
			desc := tc.RoleDisplayDesc(ref)
			agentName := tc.RoleAgentName(ref)
			if desc == ref || desc == agentName {
				desc = ""
			}
			if desc != "" {
				sb.WriteString(fmt.Sprintf("- **%s** (agent: %s): %s\n", title, agentName, desc))
			} else {
				sb.WriteString(fmt.Sprintf("- **%s** (agent: %s)\n", title, agentName))
			}
		}
	}

	if len(tc.Roles) > 0 && tc.RoleCapabilities != nil {
		sb.WriteString("\n## 团队成员能力清单\n\n")
		sb.WriteString("| 专家 | 角色 | 核心能力 | 输出规范 |\n")
		sb.WriteString("|------|------|---------|---------|\n")
		for _, ref := range tc.Roles {
			title := tc.RoleDisplayName(ref)
			agentName := tc.RoleAgentName(ref)
			capabilities := tc.RoleCapabilities[ref]
			outputSpec := tc.RoleOutputSpecs[ref]
			if capabilities == "" {
				capabilities = "—"
			}
			if outputSpec == "" {
				outputSpec = "—"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", agentName, title, capabilities, outputSpec))
		}
	}

	sb.WriteString("\n## 协作铁律\n\n")
	sb.WriteString("0. ⚠️ 简单任务只用最相关角色，不必全员出动。1个角色能完成就只用1个。\n")
	sb.WriteString("1. 你是编排者，不是执行者——禁止自己写代码、写文档、做专业分析\n")
	sb.WriteString("2. 分配给某角色的任务必须由该角色输出后采信，你只做编排与汇编\n")
	sb.WriteString("3. 未完成前序任务不可跳到后续任务\n")
	sb.WriteString("4. 验证不通过的任务必须回退重做，不可跳过\n")
	sb.WriteString("5. 禁止自己代写任何团队成员的专业产出\n")

	if len(tc.Routing) > 0 {
		sb.WriteString("\n## 意图路由表\n\n")
		sb.WriteString("| 意图关键词 | 路由角色 | 模式 |\n")
		sb.WriteString("|-----------|---------|------|\n")
		for _, r := range tc.Routing {
			sb.WriteString(fmt.Sprintf("| %s | %v | %s |\n", r.Intent, r.Roles, r.Mode))
		}
		sb.WriteString("\n根据用户问题匹配意图关键词，选择对应的角色和模式。\n")
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
