package tasks

import (
	"embed"
	"fmt"
	"strings"
	"sync"
)

var (
	systemAgentDefsOnce sync.Once
	systemAgentDefs     []AgentDefinition
	systemAgentDefsErr  error
)

// System agent definitions embedded in the binary. These are the engine's own
// built-in agents — they exist independent of any project/team/plugin and are
// provided to every team run. The system verifier lives here so a team can
// never define or shadow the verifier role: "verifier" always resolves to
// this definition, and no team .md is consulted for it.
//
// System agents are passed into the AgentDefinitionLibrary's Definitions
// (rank 2 = lowest-priority fallback) by app; the verifier additionally has a
// dedicated always-win resolution in the team adapter, so neither a project
// .whale/agents nor a team agents/ file can override it.

//go:embed builtin_agents/*.md
var builtinAgentsFS embed.FS

// SystemAgentDefinitions parses and returns all embedded system agent
// definitions. Parsing is done once at startup via sync.Once; a parse error
// is a programming error (broken shipped file) and is returned to the caller
// so misconfiguration is loud rather than silent.
func SystemAgentDefinitions() ([]AgentDefinition, error) {
	systemAgentDefsOnce.Do(func() {
		entries, err := builtinAgentsFS.ReadDir("builtin_agents")
		if err != nil {
			systemAgentDefsErr = fmt.Errorf("read builtin agents: %w", err)
			return
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
				continue
			}
			// embed.FS paths always use "/" — filepath.Join would yield "\" on
			// Windows and the ReadFile would fail to find the file.
			content, err := builtinAgentsFS.ReadFile("builtin_agents/" + entry.Name())
			if err != nil {
				systemAgentDefsErr = fmt.Errorf("read builtin agent %s: %w", entry.Name(), err)
				return
			}
			def, ok, err := parseMarkdownAgentDefinition(string(content), entry.Name(), "system")
			if err != nil || !ok {
				systemAgentDefsErr = fmt.Errorf("parse builtin agent %s: %v", entry.Name(), err)
				return
			}
			systemAgentDefs = append(systemAgentDefs, def)
		}
	})
	if systemAgentDefsErr != nil {
		return nil, systemAgentDefsErr
	}
	return cloneAgentDefinitions(systemAgentDefs), nil
}

// SystemAgentDefinition returns the system agent definition with the given
// name, or ok=false when no such system agent exists.
func SystemAgentDefinition(name string) (AgentDefinition, bool, error) {
	defs, err := SystemAgentDefinitions()
	if err != nil {
		return AgentDefinition{}, false, err
	}
	for _, def := range defs {
		if def.Name == strings.TrimSpace(name) {
			return def, true, nil
		}
	}
	return AgentDefinition{}, false, nil
}
