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
	Label        string             `yaml:"label"`
	Category     string             `yaml:"category,omitempty"`
	Capabilities []string           `yaml:"capabilities,omitempty"`
	Routing      []RoutingEntry     `yaml:"routing,omitempty"`
	Leader       TeamLeaderConfig   `yaml:"leader"`
	Roles        []string           `yaml:"roles"`
	Config       *TeamRuntimeConfig `yaml:"-"` // loaded from config.yaml

	MemoryDir    string `yaml:"-"` // memory directory path
	TemplatesDir string `yaml:"-"` // templates directory path
	TeamDir      string `yaml:"-"` // team directory path (for loading team-local agents)
	// Resolved agent info populated by ResolveRoles().
	// Maps role ref (from team.yaml roles[]) → resolved values.
	RoleTitles       map[string]string `yaml:"-"`
	RoleDescs        map[string]string `yaml:"-"`
	RoleUseAgents    map[string]string `yaml:"-"` // role ref → agent name
	RoleCapabilities map[string]string `yaml:"-"`
	RoleOutputSpecs  map[string]string `yaml:"-"`
}

// TeamRuntimeConfig is loaded from the team directory's config.yaml.
type TeamRuntimeConfig struct {
	MaxAgents      int `yaml:"max_agents"`
	DefaultTimeout int `yaml:"default_timeout"`
	Model          struct {
		Leader          string `yaml:"leader"`
		WorkerDefault   string `yaml:"worker_default"`
		VerifierDefault string `yaml:"verifier_default"`
	} `yaml:"model"`
	Workdir string       `yaml:"workdir"`
	Deploy  DeployConfig `yaml:"deploy"`
}

// DeployConfig defines where agents run.  Team-wide default — individual
// roles can override with their own Host field.
type DeployConfig struct {
	Host    string `yaml:"host"`              // server address (empty = local)
	Port    int    `yaml:"port"`              // SSH port
	User    string `yaml:"user"`              // SSH user
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
	AgentRole(name string) string         // returns display title, e.g. "后端工程师"
	AgentDesc(name string) string         // returns description
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

// teamAgentsDir returns the team's agents/ directory, or "" when the team has
// no on-disk directory (nil team, inline team, or a flat .yaml team). The
// adapter uses it to resolve team-local agent definitions.
func teamAgentsDir(tc *TeamConfig) string {
	if tc == nil || tc.TeamDir == "" {
		return ""
	}
	return filepath.Join(tc.TeamDir, "agents")
}

// teamName returns the team's name — the directory basename — or "" when the
// team has no on-disk directory. Recorded into member session meta so the
// session picker can render team members as distinguishable entries.
func teamName(tc *TeamConfig) string {
	if tc == nil || tc.TeamDir == "" {
		return ""
	}
	return filepath.Base(tc.TeamDir)
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

// agentInfoFromDir scans a team's agents/ directory and returns an
// AgentInfoProvider backed by the agent .md files' YAML frontmatter.
// Agent names are derived from the filename (e.g. "software-engineer.md" → "software-engineer").
func agentInfoFromDir(teamDir string) AgentInfoProvider {
	agentsDir := filepath.Join(teamDir, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}
	type agentMeta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	titles := make(map[string]string)
	descs := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		data, err := os.ReadFile(filepath.Join(agentsDir, e.Name()))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.HasPrefix(content, "---") {
			if end := strings.Index(content[3:], "---"); end > 0 {
				fm := content[3 : end+3]
				var meta agentMeta
				if yaml.Unmarshal([]byte(fm), &meta) == nil {
					if meta.Description != "" {
						descs[name] = meta.Description
					}
					if meta.Name != "" {
						titles[name] = meta.Name
					} else {
						titles[name] = name
					}
				}
			}
		}
	}
	if len(descs) == 0 && len(titles) == 0 {
		return nil
	}
	return &dirAgentInfo{titles: titles, descs: descs}
}

type dirAgentInfo struct {
	titles map[string]string
	descs  map[string]string
}

func (d *dirAgentInfo) AgentRole(name string) string         { return d.titles[name] }
func (d *dirAgentInfo) AgentDesc(name string) string         { return d.descs[name] }
func (d *dirAgentInfo) AgentCapabilities(name string) string { return "" }
func (d *dirAgentInfo) AgentOutputSpec(name string) string   { return "" }

// ResolveTeamRoles populates team role display info from the team's agents/
// directory.  Call this after LoadTeamConfig / FindTeamInRoots so that
// BuildLeaderPrompt can include role descriptions in decomposition prompts.
func ResolveTeamRoles(tc *TeamConfig) {
	if tc == nil || tc.TeamDir == "" {
		return
	}
	if p := agentInfoFromDir(tc.TeamDir); p != nil {
		tc.ResolveRoles(p)
	}
}

// leaderOrchestrationText returns the leader definition file's 编排/工作流
// sections so the LLM can read and follow them when decomposing and assigning
// tasks. Rather than the engine parsing the leader's written routing rules,
// the leader's own orchestration info is handed to the LLM as task context.
// The file lives at <TeamDir>/agents/<Leader.Role>.md. Returns "" when there
// is no team directory or no leader definition file.
func (tc *TeamConfig) leaderOrchestrationText() string {
	if tc == nil || tc.TeamDir == "" || tc.Leader.Role == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(tc.TeamDir, "agents", tc.Leader.Role+".md"))
	if err != nil {
		return ""
	}
	body := string(data)
	// Strip YAML frontmatter (--- ... ---) so only the orchestration prose is
	// injected — the frontmatter carries name/description, not routing rules.
	if strings.HasPrefix(body, "---") {
		if end := strings.Index(body[3:], "---"); end >= 0 {
			body = strings.TrimSpace(body[3+end+3:])
		}
	}
	return body
}

// PersonaSummary returns a compact "who am I + who are the members" block for
// the INLINE leader narration. It extracts ONLY identity lines — the leader
// headline and one line per member (display name + one-line duty) — from the
// team's agent definitions. It deliberately does NOT load member definitions
// (no SOPs, no tools, no skill lists): task orchestration only needs names and
// duties, never the members' full definitions.
func (tc *TeamConfig) PersonaSummary() string {
	if tc == nil || tc.TeamDir == "" {
		return ""
	}
	bodyOf := func(agent string) string {
		if agent == "" {
			return ""
		}
		data, err := os.ReadFile(filepath.Join(tc.TeamDir, "agents", agent+".md"))
		if err != nil {
			return ""
		}
		body := string(data)
		if strings.HasPrefix(body, "---") {
			if end := strings.Index(body[3:], "---"); end > 0 {
				body = strings.TrimSpace(body[3+end+3:])
			}
		}
		return body
	}
	headline := func(agent string) string {
		body := bodyOf(agent)
		if body == "" {
			return ""
		}
		// Persona headline rules:
		// 1) a "## " line that carries a name marker (（ or ·) — e.g. “齐活林（Qi） · 交付总监”;
		// 2) else the "# " title with a "Role - Name" suffix stripped (e.g. "Engineer - Alex" → "Alex");
		// 3) else "" — structural sections like "## Core Identity" are never names.
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "## ") && (strings.Contains(line, "（") || strings.Contains(line, "·")) {
				return strings.TrimSpace(strings.TrimPrefix(line, "## "))
			}
		}
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# ") {
				h := strings.TrimSpace(strings.TrimPrefix(line, "# "))
				if idx := strings.Index(h, " - "); idx > 0 {
					h = strings.TrimSpace(h[idx+3:])
				}
				return h
			}
		}
		return ""
	}

	var sb strings.Builder
	if tc.Leader.Role != "" {
		if h := headline(tc.Leader.Role); h != "" {
			sb.WriteString(fmt.Sprintf("你是团队主理人：%s。\n", h))
		} else {
			sb.WriteString(fmt.Sprintf("你是团队主理人：%s。\n", tc.RoleDisplayName(tc.Leader.Role)))
		}
	}
	if len(tc.Roles) > 0 {
		sb.WriteString("团队成员（编排只需知道名字与职责）：\n")
		names := memberNamesFromLeader(bodyOf(tc.Leader.Role))
		for _, ref := range tc.Roles {
			if tc.Leader.Role != "" && (ref == tc.Leader.Role || tc.RoleAgentName(ref) == tc.Leader.Role) {
				continue // the narrator IS the leader; do not list it as a member
			}
			agent := tc.RoleAgentName(ref)
			name := headline(agent)
			if name == "" {
				name = tc.RoleDisplayName(ref)
			}
			if n, ok := names[agent]; ok && n != "" {
				name = n
			}
			desc := tc.RoleDisplayDesc(ref)
			if desc == ref || desc == name || desc == "" {
				desc = ""
			} else if runes := []rune(desc); len(runes) > 60 {
				desc = string(runes[:60]) + "…"
			}
			if desc != "" {
				sb.WriteString(fmt.Sprintf("- %s（%s）：%s\n", name, agent, desc))
			} else {
				sb.WriteString(fmt.Sprintf("- %s（%s）\n", name, agent))
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// memberNamesFromLeader parses a leader definition's member table
// (| 成员 | 姓名 | 文件 | 职责 |) into agent filename → display name, so the
// narrator addresses members by their human names (e.g. 宫豆码啁 Kou).
// Returns an empty map when the leader file has no such table; callers fall
// back to headline/role titles.
func memberNamesFromLeader(body string) map[string]string {
	out := map[string]string{}
	if body == "" {
		return out
	}
	lines := strings.Split(body, "\n")
	headerIdx := -1
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "|") && strings.Contains(line, "姓名") && strings.Contains(line, "文件") {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return out
	}
	for _, line := range lines[headerIdx+1:] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cols := strings.Split(strings.Trim(line, "|"), "|")
		if len(cols) < 3 {
			continue
		}
		name := strings.TrimSpace(cols[1])
		file := strings.TrimSpace(cols[2])
		if !strings.HasSuffix(file, ".md") || strings.Contains(file, "\u2016") || strings.Contains(file, "|") {
			continue
		}
		agent := strings.TrimSuffix(file, ".md")
		if agent != "" && name != "" {
			out[agent] = name
		}
	}
	return out
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
	// Bundled teams shipped with the binary (teams/ next to the exe).
	// The exe lives inside bin/ already, so teams are at <exe_dir>/teams/.
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Join(filepath.Dir(exe), "teams"))
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
		// Defer the base prompt's role-assignment instructions to the team
		// role list injected below (handles both old and current formats).
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

	// The leader's own orchestration info (工作流路由 / 标准SOP / 快速模式 /
	// 圆桌编排 …) is handed to the LLM so it understands and follows the
	// team's编排 semantics when decomposing and assigning tasks — the engine
	// does NOT parse these rules deterministically; the LLM reads them.
	if orchestration := tc.leaderOrchestrationText(); orchestration != "" {
		sb.WriteString("\n## 团队编排与工作流（Leader 定义，必须遵循）\n")
		sb.WriteString("以下是本团队 Leader 的编排定义。你的任务分解与角色分配必须理解并遵循其中的\n")
		sb.WriteString("工作流路由判断标准、各工作模式（快速模式/BugFix/标准SOP/部分工作流/圆桌编排）的\n")
		sb.WriteString("成员调度顺序与产出流转规则。\n\n")
		sb.WriteString(orchestration)
	}

	return sb.String()
}

// stripGenericRoleSection removes or rewrites the base decompose prompt's
// generic role-assignment instructions so the team-specific role list injected
// by BuildLeaderPrompt takes over. It handles two historical formats:
//   - the old numbered "5. Assign an appropriate ROLE ... 6. Group tasks"
//     section, which is stripped wholesale;
//   - the current "## 角色分配" `- role:` bullet, which told the leader to
//     invent a domain-specific name (contradicting Rule 5) and is rewritten to
//     defer to the injected team role list.
func stripGenericRoleSection(prompt string) string {
	// Old numbered rule-5 section: remove it wholesale.
	if idx := strings.Index(prompt, "5. Assign an appropriate ROLE"); idx >= 0 {
		endIdx := strings.Index(prompt[idx:], "\n6. Group tasks into **batches**")
		if endIdx < 0 {
			// Fallback: find "6. Group" without bold markers.
			endIdx = strings.Index(prompt[idx:], "\n6. Group tasks into batches")
		}
		if endIdx > 0 {
			prompt = prompt[:idx] + prompt[idx+endIdx:]
		} else {
			prompt = prompt[:idx]
		}
	}
	// Current "## 角色分配" role bullet: defer to the team role list.
	return strings.Replace(prompt,
		"- role：与领域精确匹配的具体角色名",
		"- role：从下方 Available Team Roles 列表选择，禁止自造角色名",
		1)
}
