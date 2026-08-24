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
//
// When the SubagentRequest carries an AgentName (set from team role's
// use_agent field), the adapter resolves the corresponding .md agent
// definition and injects its prompt, tools, skills, model, etc. into
// the SpawnSubagentRequest. This ensures team experts actually use their
// specialized skills and tool configurations.
func teamEngineSpawnAdapter(runner *tasks.Runner, library *tasks.AgentDefinitionLibrary) team_engine.SpawnFunc {
	return func(ctx context.Context, req team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
		tasksReq := tasks.SpawnSubagentRequest{
			Task:         req.Task,
			Role:         req.Role,
			Model:        req.Model,
			MaxToolIters: req.MaxIters,
			MaxToolCalls: req.MaxCalls,
			OutputSchema: req.OutputSchema,
		}
		if len(req.Tools) > 0 {
			tasksReq.Tools = req.Tools
		}

		// Resolve agent definition from .md file when AgentName is set.
		if req.AgentName != "" && library != nil {
			if def, ok, err := library.Resolve(req.AgentName); err == nil && ok {
				tasksReq.Agent = def
				team_engine.Log("adapter", "resolve agent=%q ok tools=%d promptLen=%d", req.AgentName, len(def.Tools), len(def.Prompt))
				// Prepend the agent's system prompt to the task so the
				// subagent inherits the expert's behavioral instructions.
				if def.Prompt != "" {
					// Strip "团队协作" section — WorkBuddy
					// SendMessage/shutdown protocols conflict with
					// Team Engine stdout-capture mode.
					tasksReq.Task = stripWorkbuddySections(def.Prompt) + "\n\n---\n\n" + tasksReq.Task
				}
			} else {
				team_engine.Log("adapter", "resolve agent=%q MISSING err=%v", req.AgentName, err)
			}
		}

		// Fallback: provide inline agent definitions for built-in roles.
		if tasksReq.Agent.Name == "" {
			switch req.Role {
			case "planner", "verifier", "reviewer", "researcher", "evaluator", "synthesizer":
				tasksReq.Agent = tasks.AgentDefinition{
					Name:           req.Role,
					Description:    "Team engine " + req.Role + " agent",
					PermissionMode: tasks.AgentPermissionReadOnly,
				}
			default:
				tasksReq.Agent = tasks.AgentDefinition{
					Name:           req.Role,
					Description:    "Team engine agent",
					PermissionMode: tasks.AgentPermissionAuto,
				}
			}
		}

		if req.Workdir != "" {
			tasksReq.Task = fmt.Sprintf("Working directory: %s\n\n%s", req.Workdir, tasksReq.Task)
		}

		resp, err := runner.SpawnSubagent(ctx, tasksReq)
		if err != nil {
			return team_engine.SubagentResponse{
				SessionID: resp.SessionID,
				Output:    "",
				ExitCode:  -1,
				Success:   false,
			}, fmt.Errorf("spawn subagent: %w", err)
		}

		diag := fmt.Sprintf("status=%s summary=%d reqTools=%v resolvedTools=%v usage(p=%d c=%d t=%d) err=%q truncated=%v structured=%v",
			resp.Status, len(resp.Summary),
			resp.RequestedTools, resp.ResolvedTools,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens,
			resp.Error, resp.Truncated, resp.StructuredResult != nil)

		success := resp.Status == "completed" || resp.Status == "done"
		if success && strings.TrimSpace(resp.Summary) == "" && resp.StructuredResult == nil {
			success = false
		}
		exitCode := 0
		if !success {
			exitCode = 1
		}

		return team_engine.SubagentResponse{
			SessionID:       resp.SessionID,
			SpawnerType:     "adapter",
			Output:          resp.Summary,
			Structured:      resp.StructuredResult,
			ExitCode:        exitCode,
			Success:         success,
			UsagePrompt:     resp.Usage.PromptTokens,
			UsageCompletion: resp.Usage.CompletionTokens,
			Diagnostic:      diag,
		}, nil
	}
}

// stripWorkbuddySections removes WorkBuddy-specific sections from an agent
// .md prompt that don't apply in Team Engine.  Team Engine handles retry,
// routing, and output format itself — the agent doesn't need these instructions.
//
// Stripped sections:
//   - Smart Routing / Send To: Engineer/QA/NoOne
//   - Test Round Control (STRICT — MAX N ROUNDS)
//   - Test Report Format (engine provides OUTPUT FORMAT)
//   - 团队协作 / SendMessage / shutdown_request
func stripWorkbuddySections(prompt string) string {
	markers := []string{
		"\n### 3. Run Tests and Smart Routing",
		"\n#### Smart Routing Decision",
		"\n## Test Round Control",
		"\n## Test Report Format",
		"\n## Input\n\nYou will receive:",
		"\n## 团队协作",
	}
	for _, m := range markers {
		if idx := strings.Index(prompt, m); idx >= 0 {
			prompt = prompt[:idx]
		}
	}
	return prompt
}

// NewTeamEngineSpawnFunc exposes the team-engine subagent adapter for callers
// outside the app package (e.g. the `whale team` CLI command) that construct
// their own tasks.Runner and AgentDefinitionLibrary. It returns the same
// SpawnFunc that app_runtime_init wires as the package default, so team engine
// instances built by other entry points get identical AgentName resolution.
func NewTeamEngineSpawnFunc(runner *tasks.Runner, library *tasks.AgentDefinitionLibrary) team_engine.SpawnFunc {
	return teamEngineSpawnAdapter(runner, library)
}
