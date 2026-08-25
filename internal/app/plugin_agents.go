package app

import (
	"github.com/usewhale/whale/internal/plugins"
	"github.com/usewhale/whale/internal/tasks"
)

func taskAgentDefinitions(in []plugins.AgentDefinition) []tasks.AgentDefinition {
	// System-provided built-in agents (system verifier) are always available,
	// independent of plugins.
	all := []tasks.AgentDefinition{}
	if defs, err := tasks.SystemAgentDefinitions(); err == nil {
		all = append(all, defs...)
	}
	if len(in) == 0 {
		return all
	}
	for _, agent := range in {
		tools := append([]string(nil), agent.Capabilities...)
		tools = append(tools, agent.AllowedTools...)
		all = append(all, tasks.AgentDefinition{
			Name:            agent.Name,
			Description:     agent.Description,
			Prompt:          agent.SystemPrompt,
			Model:           agent.Model,
			Effort:          agent.Effort,
			MaxToolIters:    agent.MaxToolIters,
			MaxToolCalls:    agent.MaxToolCalls,
			Tools:           tools,
			DisallowedTools: append([]string(nil), agent.DisallowedTools...),
		})
	}
	return all
}
