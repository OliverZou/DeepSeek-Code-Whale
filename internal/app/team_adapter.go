package app

import (
	"context"
	"fmt"
	"sort"
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
			// req.Tools carries team-engine tool names (ProfileToToolNames),
			// which the shell spawner understands but the native subagent
			// does not. Translate them to native capabilities so the child
			// agent actually receives workspace.write / shell.run / etc.
			tasksReq.Tools = teamToolsToCapabilities(req.Tools)
		}

		// Resolve agent definition from .md file when AgentName is set.
		if req.AgentName != "" && library != nil {
			// Team-local agent definitions live in the team's agents/ directory,
			// which is not in the library's default roots (~/.whale/agents +
			// project .whale/agents). Thread it through as an extra root so a
			// team role like "software-engineer" resolves to its team definition
			// instead of falling back to a bare inline definition.
			lib := library
			if req.TeamAgentsDir != "" {
				lib = library.WithExtraRoot(req.TeamAgentsDir, "team", -1)
			}
			if def, ok, err := lib.Resolve(req.AgentName); err == nil && ok {
				tasksReq.Agent = def
				team_engine.Log("adapter", "resolve agent=%q ok tools=%d promptLen=%d", req.AgentName, len(def.Tools), len(def.Prompt))
				// The persona is capability context, not the task. Inject it as
				// the agent's system prompt (via Agent.Prompt, which the runner
				// renders as its "Agent system prompt" block) — NOT prepended to
				// the user task. Prepending it to the task put the persona at the
				// same level as (and ahead of) the task instruction, so a QA
				// persona's "write tests" SOP overrode the verifier's "do NOT
				// author a new test suite", and every token was billed twice
				// (system + user).
				if def.Prompt != "" {
					// Strip "团队协作" section — WorkBuddy
					// SendMessage/shutdown protocols conflict with
					// Team Engine stdout-capture mode.
					tasksReq.Agent.Prompt = stripWorkbuddySections(def.Prompt)
				}
			} else {
				team_engine.Log("adapter", "resolve agent=%q MISSING err=%v", req.AgentName, err)
			}
		}

		// Fallback: provide inline agent definitions for built-in roles.
		if tasksReq.Agent.Name == "" {
			tasksReq.Agent = tasks.AgentDefinition{
				Name:           req.Role,
				Description:    "Team engine " + req.Role + " agent",
				PermissionMode: permissionForRole(req.Role),
			}
			// A verifier whose agent definition didn't resolve still needs the
			// tool-grounded verify profile (read + shell.run, no write) so it
			// can execute tests/linters instead of only static review.
			if req.Role == "verifier" && len(tasksReq.Tools) == 0 {
				tasksReq.Tools = teamToolsToCapabilities(team_engine.ProfileToToolNames(team_engine.ProfileVerify))
			}
		}

		// An agent resolved from a .md file may omit permission mode, which
		// normalizes to read-only and would strip workspace.write / shell.run
		// from workers, leaving them unable to produce artifacts. Default it
		// by role (read-only roles stay read-only; workers get auto).
		if tasksReq.Agent.PermissionMode == "" {
			tasksReq.Agent.PermissionMode = permissionForRole(req.Role)
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

		diag := fmt.Sprintf("status=%s report=%d reqTools=%v resolvedTools=%v usage(p=%d c=%d t=%d) err=%q truncated=%v structured=%v",
			resp.Status, len(resp.Report),
			resp.RequestedTools, resp.ResolvedTools,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens,
			resp.Error, resp.Truncated, resp.StructuredResult != nil)

		success := resp.Status == "completed" || resp.Status == "done"
		if success && strings.TrimSpace(resp.Report) == "" && resp.StructuredResult == nil {
			success = false
		}
		exitCode := 0
		if !success {
			exitCode = 1
		}

		// resp.Summary is a one-line preview of the full final message and is
		// explicitly NOT a substitute for resp.Report. Using Summary here
		// truncates a worker's completion note (and a verifier's VERDICT: line)
		// to its first line, which the verifier then cannot judge and fails.
		return team_engine.SubagentResponse{
			SessionID:       resp.SessionID,
			SpawnerType:     "adapter",
			Output:          resp.Report,
			Structured:      resp.StructuredResult,
			ExitCode:        exitCode,
			Success:         success,
			UsagePrompt:     resp.Usage.PromptTokens,
			UsageCompletion: resp.Usage.CompletionTokens,
			Diagnostic:      diag,
			SystemPrompt:    resp.SystemPrompt,
		}, nil
	}
}

// permissionForRole returns the default permission mode for a team role.
// Read-only roles (planner, reviewer, researcher, …) stay read-only; workers
// get auto so they can write artifacts and run shell commands. The verifier is
// special: it needs shell.run to execute tests/linters (tool-grounded
// verification), but its toolset is constrained to the verify profile
// (read + shell, no write) in the adapter fallback, so auto is safe.
func permissionForRole(role string) string {
	switch role {
	case "verifier":
		return tasks.AgentPermissionAuto
	case "planner", "reviewer", "researcher", "evaluator", "synthesizer":
		return tasks.AgentPermissionReadOnly
	default:
		return tasks.AgentPermissionAuto
	}
}

// teamToolsToCapabilities maps team-engine tool names (ProfileToToolNames) to
// native subagent capabilities. The shell spawner consumes tool names directly;
// the native subagent selects tools by capability (workspace.write, shell.run,
// …), so "apply_patch" — which the native toolset does not provide — correctly
// collapses into workspace.write alongside "edit"/"write".
func teamToolsToCapabilities(toolNames []string) []string {
	caps := map[string]bool{}
	add := func(c string) { caps[c] = true }
	for _, name := range toolNames {
		switch name {
		case "read_file", "list_dir", "grep", "search_files":
			add(tasks.CapabilityWorkspaceRead)
		case "edit", "write", "apply_patch", "multi_edit":
			add(tasks.CapabilityWorkspaceWrite)
		case "shell_run", "shell_wait", "shell_cancel":
			add(tasks.CapabilityShellRun)
		case "write_stdin":
			add(tasks.CapabilityTerminalWrite)
		case "web_search":
			add(tasks.CapabilityWebSearch)
		case "web_fetch", "fetch":
			add(tasks.CapabilityWebFetch)
		}
	}
	if len(caps) == 0 {
		return nil
	}
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// stripWorkbuddySections removes WorkBuddy-specific sections from an agent
// .md prompt that don't apply in Team Engine.  Team Engine handles retry,
// routing, and output format itself — the agent doesn't need these instructions.
//
// Sections before "团队协作" are removed by heading so the role's core
// competency sections (coding standards, design rules, testing standards)
// survive — the persona is never reduced to a bare stub. "团队协作" (and
// everything after it) is truncated, because it marks the start of the
// SendMessage/shutdown orchestration protocol — and in team-lead.md the whole
// SOP workflow follows it, so truncation drops that too.
//
// Stripped sections:
//   - ## Input ("You will receive Design Doc / PRD" pipeline handoff)
//   - ### 3. Run Tests and Smart Routing (QA's Send-To routing protocol)
//   - ## Test Round Control (STRICT — MAX N ROUNDS)
//   - ## Test Report Format (engine provides OUTPUT FORMAT)
//   - ## 团队协作 … (SendMessage / shutdown_request / SOP orchestration)
func stripWorkbuddySections(prompt string) string {
	for _, h := range []string{
		"## Input",
		"### 3. Run Tests and Smart Routing",
		"## Test Round Control",
		"## Test Report Format",
	} {
		prompt = stripSection(prompt, h)
	}
	// 团队协作 starts the orchestration protocol; truncate from there.
	if idx := strings.Index(prompt, "## 团队协作"); idx >= 0 {
		prompt = prompt[:idx]
	}
	return prompt
}

// stripSection removes the section beginning at heading (inclusive) up to the
// next level-2 ("## ") heading, or the end of the prompt if none follows. A
// level-2 boundary is used (rather than the next heading of any level) so a
// section's deeper sub-headings ("###"/"####") are removed with it instead of
// being mistaken for the section's end.
func stripSection(prompt, heading string) string {
	idx := strings.Index(prompt, heading)
	if idx < 0 {
		return prompt
	}
	rest := prompt[idx+len(heading):]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		return prompt[:idx] + rest[next:]
	}
	return prompt[:idx]
}

// NewTeamEngineSpawnFunc exposes the team-engine subagent adapter for callers
// outside the app package (e.g. the `whale team` CLI command) that construct
// their own tasks.Runner and AgentDefinitionLibrary. It returns the same
// SpawnFunc that app_runtime_init wires as the package default, so team engine
// instances built by other entry points get identical AgentName resolution.
func NewTeamEngineSpawnFunc(runner *tasks.Runner, library *tasks.AgentDefinitionLibrary) team_engine.SpawnFunc {
	return teamEngineSpawnAdapter(runner, library)
}
