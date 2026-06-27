package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"strings"

	"gopkg.in/yaml.v3"
)

type yamlStringList []string

func (l *yamlStringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.SequenceNode {
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		*l = items
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	*l = out
	return nil
}

const AgentDefinitionFileExt = ".md"

var agentDefinitionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

type AgentDefinitionLibrary struct {
	Roots       []AgentDefinitionRoot
	Definitions []AgentDefinition
}

type AgentDefinitionRoot struct {
	Path   string
	Source string
	Rank   int
}

func NewAgentDefinitionLibrary(workspaceRoot string) *AgentDefinitionLibrary {
	var roots []AgentDefinitionRoot
	if root := strings.TrimSpace(workspaceRoot); root != "" {
		roots = append(roots,
			AgentDefinitionRoot{Path: filepath.Join(root, ".whale", "agents"), Source: "project", Rank: 0},
		)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		roots = append(roots,
			AgentDefinitionRoot{Path: filepath.Join(home, ".whale", "agents"), Source: "user", Rank: 1},
		)
	}
	return NewAgentDefinitionLibraryWithRoots(roots)
}

func NewAgentDefinitionLibraryWithDefinitions(workspaceRoot string, definitions []AgentDefinition) *AgentDefinitionLibrary {
	library := NewAgentDefinitionLibrary(workspaceRoot)
	if library == nil {
		library = &AgentDefinitionLibrary{}
	}
	library.Definitions = cloneAgentDefinitions(definitions)
	return library
}

func NewAgentDefinitionLibraryWithRoots(roots []AgentDefinitionRoot) *AgentDefinitionLibrary {
	out := make([]AgentDefinitionRoot, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		path := strings.TrimSpace(root.Path)
		if path == "" {
			continue
		}
		clean := filepath.Clean(path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		source := strings.TrimSpace(root.Source)
		if source == "" {
			source = "agent"
		}
		out = append(out, AgentDefinitionRoot{Path: clean, Source: source, Rank: root.Rank})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		return out[i].Path < out[j].Path
	})
	return &AgentDefinitionLibrary{Roots: out}
}

func ValidAgentDefinitionName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && len(name) <= 64 && agentDefinitionNamePattern.MatchString(name)
}

func (l *AgentDefinitionLibrary) Resolve(name string) (AgentDefinition, bool, error) {
	name = strings.TrimSpace(name)
	if !ValidAgentDefinitionName(name) {
		return AgentDefinition{}, false, nil
	}
	if l == nil {
		return AgentDefinition{}, false, nil
	}
	var best AgentDefinition
	bestRank := 0
	found := false
	for _, root := range l.Roots {
		def, ok, err := resolveAgentDefinitionFromRoot(context.Background(), root, name)
		if err != nil {
			return AgentDefinition{}, false, err
		}
		if !ok {
			continue
		}
		if found && bestRank <= root.Rank {
			continue
		}
		best = def
		bestRank = root.Rank
		found = true
	}
	for _, def := range l.Definitions {
		if def.Name != name {
			continue
		}
		if found && bestRank <= 2 {
			continue
		}
		best = def
		bestRank = 2
		found = true
	}
	return best, found, nil
}

func (l *AgentDefinitionLibrary) List(ctx context.Context) ([]AgentDefinition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	byName := map[string]AgentDefinition{}
	nameRank := map[string]int{}
	for _, root := range l.Roots {
		defs, err := scanAgentDefinitionRoot(ctx, root)
		if err != nil {
			return nil, err
		}
		for _, def := range defs {
			rank, exists := nameRank[def.Name]
			if exists && rank <= root.Rank {
				continue
			}
			byName[def.Name] = def
			nameRank[def.Name] = root.Rank
		}
	}
	for _, def := range l.Definitions {
		if strings.TrimSpace(def.Name) == "" {
			continue
		}
		rank, exists := nameRank[def.Name]
		if exists && rank <= 2 {
			continue
		}
		byName[def.Name] = def
		nameRank[def.Name] = 2
	}
	out := make([]AgentDefinition, 0, len(byName))
	for _, def := range byName {
		out = append(out, def)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func scanAgentDefinitionRoot(ctx context.Context, root AgentDefinitionRoot) ([]AgentDefinition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(root.Path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var defs []AgentDefinition
	err := filepath.WalkDir(root.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != root.Path && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".json" {
			return nil
		}
		def, ok, err := parseAgentDefinitionFile(path, root.Source)
		if err != nil {
			return nil
		}
		if ok {
			defs = append(defs, def)
		}
		return nil
	})
	return defs, err
}

func resolveAgentDefinitionFromRoot(ctx context.Context, root AgentDefinitionRoot, name string) (AgentDefinition, bool, error) {
	if err := ctx.Err(); err != nil {
		return AgentDefinition{}, false, err
	}
	if _, err := os.Stat(root.Path); err != nil {
		if os.IsNotExist(err) {
			return AgentDefinition{}, false, nil
		}
		return AgentDefinition{}, false, err
	}
	var matched AgentDefinition
	found := false
	var matchedErr error
	err := filepath.WalkDir(root.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != root.Path && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".json" {
			return nil
		}
		def, ok, err := parseAgentDefinitionFile(path, root.Source)
		if err != nil {
			if strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) == name {
				matchedErr = fmt.Errorf("parse agent definition %s: %w", path, err)
				return matchedErr
			}
			return nil
		}
		if ok && def.Name == name {
			matched = def
			found = true
		}
		return nil
	})
	if matchedErr != nil {
		return AgentDefinition{}, false, matchedErr
	}
	if err != nil {
		return AgentDefinition{}, false, err
	}
	return matched, found, nil
}

func parseAgentDefinitionFile(path, source string) (AgentDefinition, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return AgentDefinition{}, false, err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		var def AgentDefinition
		if err := json.Unmarshal(content, &def); err != nil {
			return AgentDefinition{}, false, err
		}
		if strings.TrimSpace(def.Name) == "" {
			def.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		return validateLoadedAgentDefinition(def)
	case ".md":
		return parseMarkdownAgentDefinition(string(content), filepath.Base(path), source)
	default:
		return AgentDefinition{}, false, nil
	}
}

// ParseMarkdownAgentDefinition is the exported version of parseMarkdownAgentDefinition.
func ParseMarkdownAgentDefinition(content, filename, source string) (AgentDefinition, bool, error) {
	return parseMarkdownAgentDefinition(content, filename, source)
}

func parseMarkdownAgentDefinition(content, filename, _ string) (AgentDefinition, bool, error) {
	frontmatter, body, ok, err := splitAgentFrontmatter(content)
	if err != nil || !ok {
		return AgentDefinition{}, false, err
	}

	var raw struct {
		Name            string         `yaml:"name"`
		Role            string         `yaml:"role"`
		Description     string         `yaml:"description"`
		WhenToUse       string         `yaml:"whenToUse"`
		Tools           yamlStringList `yaml:"tools"`
		DisallowedTools yamlStringList `yaml:"disallowedTools"`
		Skills          yamlStringList `yaml:"skills"`
		MCPServers      yamlStringList `yaml:"mcpServers"`
		Model           string         `yaml:"model"`
		Effort          string         `yaml:"effort"`
		PermissionMode  string         `yaml:"permissionMode"`
		MaxTurns        int            `yaml:"maxTurns"`
		InitialPrompt   string         `yaml:"initialPrompt"`
		Memory          string         `yaml:"memory"`
		Background      bool           `yaml:"background"`
		Isolation       string         `yaml:"isolation"`
		DisplayName     map[string]string `yaml:"displayName"`
		Profession      map[string]string `yaml:"profession"`
		Generation      struct {
			AssistantPrefix  string `yaml:"assistantPrefix"`
			PrefixCompletion bool   `yaml:"prefixCompletion"`
		} `yaml:"generation"`
		Hooks any `yaml:"hooks"`
	}

	if err := yaml.Unmarshal([]byte(frontmatter), &raw); err != nil {
		return AgentDefinition{}, false, fmt.Errorf("parse frontmatter: %w", err)
	}

	name := raw.Name
	if name == "" {
		name = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	desc := raw.Description
	if desc == "" {
		return AgentDefinition{}, false, fmt.Errorf("description is required")
	}

	role := raw.Role
	if role == "" {
		if raw.Profession != nil {
			if zh, ok := raw.Profession["zh"]; ok && zh != "" {
				role = zh
			} else if en, ok := raw.Profession["en"]; ok && en != "" {
				role = en
			}
		}
	}
	if raw.DisplayName != nil {
		if zh, ok := raw.DisplayName["zh"]; ok && zh != "" && role == "" {
			role = zh
		}
	}

	def := AgentDefinition{
		Name:            name,
		Role:            role,
		Description:     desc,
		WhenToUse:       raw.WhenToUse,
		Prompt:          strings.TrimSpace(body),
		Tools:           raw.Tools,
		DisallowedTools: raw.DisallowedTools,
		Skills:          raw.Skills,
		MCPServers:      raw.MCPServers,
		Model:           raw.Model,
		Effort:          raw.Effort,
		PermissionMode:  raw.PermissionMode,
		MaxTurns:        raw.MaxTurns,
		InitialPrompt:   raw.InitialPrompt,
		Memory:          raw.Memory,
		Hooks:           raw.Hooks,
		Background:      raw.Background,
		Isolation:       raw.Isolation,
		Generation: AgentGenerationConfig{
			AssistantPrefix:  raw.Generation.AssistantPrefix,
			PrefixCompletion: raw.Generation.PrefixCompletion,
		},
	}
	return validateLoadedAgentDefinition(def)
}

func validateLoadedAgentDefinition(def AgentDefinition) (AgentDefinition, bool, error) {
	def.Name = strings.TrimSpace(def.Name)
	if !ValidAgentDefinitionName(def.Name) {
		return AgentDefinition{}, false, fmt.Errorf("invalid agent name %q", def.Name)
	}
	if strings.TrimSpace(def.Description) == "" && strings.TrimSpace(def.WhenToUse) == "" {
		return AgentDefinition{}, false, fmt.Errorf("description is required")
	}
	if strings.TrimSpace(def.WhenToUse) == "" {
		def.WhenToUse = strings.TrimSpace(def.Description)
	}
	if strings.TrimSpace(def.Description) == "" {
		def.Description = strings.TrimSpace(def.WhenToUse)
	}
	return def, true, nil
}

func splitAgentFrontmatter(content string) (frontmatter, body string, ok bool, err error) {
	content = strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", "", false, nil
	}
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	rest := strings.TrimPrefix(normalized, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false, fmt.Errorf("unclosed frontmatter")
	}
	frontmatter = rest[:end]
	body = strings.TrimPrefix(rest[end+len("\n---"):], "\n")
	return frontmatter, body, true, nil
}

func cloneAgentDefinitions(definitions []AgentDefinition) []AgentDefinition {
	if len(definitions) == 0 {
		return nil
	}
	out := make([]AgentDefinition, 0, len(definitions))
	for _, def := range definitions {
		def.Name = strings.TrimSpace(def.Name)
		if !ValidAgentDefinitionName(def.Name) {
			continue
		}
		def.Tools = cloneStrings(def.Tools)
		def.DisallowedTools = cloneStrings(def.DisallowedTools)
		def.Skills = cloneStrings(def.Skills)
		def.MCPServers = cloneStrings(def.MCPServers)
		out = append(out, def)
	}
	return out
}
