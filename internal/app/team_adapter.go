package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/usewhale/whale/internal/tasks"
	"github.com/usewhale/whale/internal/team_engine"
)

// teamEngineSpawnAdapter wraps a tasks.Runner into a team_engine.SpawnFunc,
// replacing the ShellSubagentSpawner (whale exec subprocess) with Whale's
// native subagent spawning mechanism. This avoids cold-starting a new Whale
// process for every team task, and enables proper context isolation, tool
// permissions, and audit logging.
func teamEngineSpawnAdapter(runner *tasks.Runner) team_engine.SpawnFunc {
	return func(ctx context.Context, req team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
		tasksReq := tasks.SpawnSubagentRequest{
			Task:         req.Task,
			Role:         req.Role,
			Model:        req.Model,
			MaxToolIters: req.MaxIters,
			MaxToolCalls: req.MaxCalls,
			MaxTokens:    req.MaxTokens,
		}
		if len(req.Tools) > 0 {
			tasksReq.Tools = req.Tools
		}
		// Provide inline agent definitions for team-engine roles
		// that aren't in the builtin registry.
		switch req.Role {
		case "worker":
			tasksReq.Agent = tasks.AgentDefinition{
				Name:           "worker",
				Description:    "General-purpose worker agent",
				PermissionMode: tasks.AgentPermissionAuto,
			}
		case "verifier":
			tasksReq.Agent = tasks.AgentDefinition{
				Name:           "verifier",
				Description:    "Verification agent",
				PermissionMode: tasks.AgentPermissionReadOnly,
			}
		}
		if req.Workdir != "" {
			tasksReq.Task = fmt.Sprintf("Working directory: %s\n\n%s", req.Workdir, req.Task)
		}

		resp, err := runner.SpawnSubagent(ctx, tasksReq)
		if err != nil {
			return team_engine.SubagentResponse{
				Output:   "",
				ExitCode: -1,
				Success:  false,
			}, fmt.Errorf("spawn subagent: %w", err)
		}

		// Diagnostic: capture full subagent response details so they can
		// be logged to engine.log even when stderr is not visible.
		diag := fmt.Sprintf("status=%s summary=%d reqTools=%v resolvedTools=%v usage(p=%d c=%d t=%d) err=%q truncated=%v structured=%v",
			resp.Status, len(resp.Summary),
			resp.RequestedTools, resp.ResolvedTools,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens,
			resp.Error, resp.Truncated, resp.StructuredResult != nil)

		success := resp.Status == "completed" || resp.Status == "done"
		// If the subagent completed but produced no output at all, treat it as
		// a failure rather than returning an empty Success=true response — the
		// caller (e.g. Leader.Decompose) expects meaningful output.
		// Also guard against whitespace-only summaries (which can happen when
		// reasoning models exhaust their token budget on chain-of-thought).
		if success && strings.TrimSpace(resp.Summary) == "" && resp.StructuredResult == nil {
			success = false
		}
		exitCode := 0
		if !success {
			exitCode = 1
		}

		return team_engine.SubagentResponse{
			SpawnerType:     "adapter",
			Output:          resp.Summary,
			ExitCode:        exitCode,
			Success:         success,
			UsagePrompt:     resp.Usage.PromptTokens,
			UsageCompletion: resp.Usage.CompletionTokens,
			Diagnostic:      diag,
		}, nil
	}
}
