package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/usewhale/whale/internal/core"
)

func NewTools(r *Runner) []core.Tool {
	return []core.Tool{
		parallelReasonTool{runner: r},
		spawnSubagentTool{runner: r},
		agentSearchTool{runner: r},
		subagentStatusTool{runner: r},
		cancelSubagentTool{runner: r},
	}
}

type parallelReasonTool struct {
	runner *Runner
}

func (t parallelReasonTool) Name() string { return "parallel_reason" }
func (t parallelReasonTool) Description() string {
	return "Run 1-8 independent cheap model-only subqueries in parallel. Use for comparison, classification, critique, or brainstorming when no tools, files, shell, web access, or agent loop are needed. Returns ordered results."
}
func (t parallelReasonTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"prompts": map[string]any{
				"type":        "array",
				"minItems":    1,
				"maxItems":    MaxParallelPrompts,
				"description": "Independent subqueries to answer in parallel.",
				"items":       map[string]any{"type": "string"},
			},
			"model":      map[string]any{"type": "string", "description": "Optional model override. Defaults to the configured cheap model."},
			"max_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 4096},
		},
		"required": []string{"prompts"},
	}
}
func (t parallelReasonTool) ReadOnly() bool         { return true }
func (t parallelReasonTool) SupportsParallel() bool { return true }
func (t parallelReasonTool) Run(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
	if t.runner == nil {
		return marshalError(call, "not_configured", "task runner is not configured")
	}
	req, err := decodeInput[ParallelReasonRequest](call)
	if err != nil {
		return marshalError(call, "invalid_input", err.Error())
	}
	res, err := t.runner.ParallelReason(ctx, req)
	if err != nil {
		return marshalError(call, "parallel_reason_failed", err.Error())
	}
	return marshalSuccess(call, map[string]any{
		"model":   res.Model,
		"results": res.Results,
		"usage":   res.Usage,
	})
}

type spawnSubagentTool struct {
	runner *Runner
}

func (t spawnSubagentTool) Name() string { return "spawn_subagent" }
func (t spawnSubagentTool) Description() string {
	return "Run one bounded child agent for independent exploration, research, or review. Prefer direct tools for small follow-ups; use a child agent mainly for parallel fan-out or when a task needs roughly 10+ read/search steps whose trail does not need to stay in the parent context. Each fresh child has its own provider request/prefix and may pay a prefix-cache miss plus a full child loop. Select a built-in role or named agent definition; advanced agent definitions are configured outside this tool schema. Omit tools to use the selected agent defaults, pass [] for model-only synthesis, or pass workspace.read or exact tool names for a custom allowlist. Use subagent_status or cancel_subagent for background lifecycle follow-up."
}
func (t spawnSubagentTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"task":  map[string]any{"type": "string", "description": "Self-contained task for the child agent. The available tools are determined by the requested role, named agent definition, or explicit tools allowlist."},
			"role":  map[string]any{"type": "string", "description": "Built-in role (explore, research, review) or an agent definition name from .whale/agents."},
			"model": map[string]any{"type": "string", "description": "Optional model override. Defaults to the configured cheap model."},
			// Execution-budget knobs (tool-call / turn ceilings) are intentionally
			// not exposed here: a parent model guessing a budget tends to starve a
			// thorough child (see the role defaults in builtinAgentDefinition). The
			// bound comes from the role definition / runner defaults. Workflow
			// scripts that genuinely need a custom budget set it programmatically
			// via the scheduler, which bypasses this tool.
			"tools": map[string]any{
				"type":        "array",
				"description": "Optional least-privilege tool selectors. Omit to use the selected agent defaults. Pass [] for model-only. Selectors may be known aliases such as workspace.read or exact tool names exposed to the parent agent.",
				"items":       map[string]any{"type": "string"},
			},
		},
		"required": []string{"task"},
	}
}

func (t spawnSubagentTool) ReadOnly() bool { return true }
func (t spawnSubagentTool) ReadOnlyCheck(args map[string]any) bool {
	return spawnSubagentLaunchReadOnly(args, t.runner)
}
func (t spawnSubagentTool) Run(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
	return t.RunWithProgress(ctx, call, nil)
}

func spawnSubagentLaunchReadOnly(args map[string]any, runner *Runner) bool {
	if args == nil {
		return false
	}
	if _, ok := args["capabilities"]; ok {
		return false
	}
	if _, ok := args["agent"]; ok {
		return false
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return false
	}
	var req SpawnSubagentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return false
	}
	if strings.TrimSpace(req.Task) == "" {
		return false
	}
	var library *AgentDefinitionLibrary
	if runner != nil {
		library = runner.agentDefinitions
	}
	cfg, err := ResolveAgentRuntimeConfigWithLibrary(req, RunnerDefaults{}, library)
	if err != nil {
		return false
	}
	if spawnSubagentSelectorsMutating(cfg.ToolSelectors) {
		return false
	}
	if cfg.PermissionProfile != AgentPermissionReadOnly {
		return false
	}
	if cfg.Isolation == AgentIsolationWorktree {
		return false
	}
	if len(cfg.Hooks) > 0 {
		return false
	}
	return true
}

func spawnSubagentSelectorsMutating(values []string) bool {
	for _, value := range values {
		switch strings.TrimSpace(value) {
		case CapabilityWorkspaceWrite, CapabilityShellRun:
			return true
		}
	}
	return false
}

func (t spawnSubagentTool) RunWithProgress(ctx context.Context, call core.ToolCall, progress func(core.ToolProgress)) (core.ToolResult, error) {
	if t.runner == nil {
		return marshalError(call, "not_configured", "task runner is not configured")
	}
	if spawnSubagentInputHasDeprecatedCapabilities(call.Input) {
		return marshalError(call, "invalid_input", "spawn_subagent no longer accepts capabilities; use tools instead")
	}
	if spawnSubagentInputHasInlineAgent(call.Input) {
		return marshalError(call, "invalid_input", "spawn_subagent no longer accepts inline agent definitions; use a named role or .whale/agents definition instead")
	}
	req, err := decodeInput[SpawnSubagentRequest](call)
	if err != nil {
		return marshalError(call, "invalid_input", err.Error())
	}
	// The model-facing spawn tool does not honor execution-budget knobs; bounds
	// come from the role definition / runner defaults, never from a model guess.
	// Clear anything the model passed (the schema omits these, but be explicit so
	// a smuggled value can't starve the child). The workflow scheduler builds the
	// request directly and is unaffected by this.
	req.MaxToolCalls = 0
	req.MaxToolIters = 0
	req.ParentToolCallID = call.ID
	res, err := t.runner.SpawnSubagentWithProgress(ctx, req, func(p core.ToolProgress) {
		if progress == nil {
			return
		}
		p.ToolCallID = call.ID
		p.ToolName = call.Name
		progress(p)
	})
	if err != nil {
		permissionProfile := AgentPermissionReadOnly
		if cfg, cfgErr := ResolveAgentRuntimeConfig(req, RunnerDefaults{}); cfgErr == nil && cfg.PermissionProfile != "" {
			permissionProfile = cfg.PermissionProfile
		}
		var subErr *SpawnSubagentError
		if errors.As(err, &subErr) {
			code := core.FirstNonEmpty(subErr.Code, "spawn_subagent_failed")
			return marshalErrorWithData(call, code, subErr.Error(), map[string]any{
				"session_id":         subErr.SessionID,
				"child_session_id":   subErr.SessionID,
				"permission_profile": permissionProfile,
				"status":             code,
			})
		}
		return marshalError(call, "spawn_subagent_failed", err.Error())
	}
	data := map[string]any{
		"session_id":         res.SessionID,
		"child_session_id":   res.SessionID,
		"role":               res.Role,
		"model":              res.Model,
		"permission_profile": res.PermissionProfile,
		"status":             res.Status,
		"report":             res.Report,
		"summary":            res.Summary,
		"truncated":          res.Truncated,
		"tool_calls":         res.ToolCalls,
		"requested_tools":    res.RequestedTools,
		"resolved_tools":     res.ResolvedTools,
		"tool_mode":          res.ToolMode,
		"duration_ms":        res.DurationMS,
		"completed_at":       res.CompletedAt,
	}
	if res.SubagentBudget.SpawnCount > 0 {
		budget := map[string]any{
			"spawn_count":  res.SubagentBudget.SpawnCount,
			"total_tokens": res.SubagentBudget.TotalTokens,
		}
		if strings.TrimSpace(res.SubagentBudget.Hint) != "" {
			budget["hint"] = res.SubagentBudget.Hint
		}
		data["subagent_budget"] = budget
	}
	content, err := core.MarshalToolEnvelope(core.ToolEnvelope{
		OK:       true,
		Success:  true,
		Code:     "ok",
		Data:     data,
		Metadata: map[string]any{"duration_ms": res.DurationMS},
	})
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{ToolCallID: call.ID, Name: call.Name, ModelText: content, Outcome: core.OutcomeSuccess, Code: "ok"}, nil
}

func spawnSubagentInputHasDeprecatedCapabilities(input string) bool {
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return false
	}
	_, ok := raw["capabilities"]
	return ok
}

func spawnSubagentInputHasInlineAgent(input string) bool {
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return false
	}
	_, ok := raw["agent"]
	return ok
}

type subagentStatusTool struct {
	runner *Runner
}

type subagentStatusRequest struct {
	SessionID string `json:"session_id"`
}

func (t subagentStatusTool) Name() string { return "subagent_status" }
func (t subagentStatusTool) Description() string {
	return "Read a child subagent lifecycle record by session_id. Use after launching a background child agent to observe running/completed/failed/cancelled state and recover its full result report."
}
func (t subagentStatusTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"session_id": map[string]any{"type": "string"},
		},
		"required": []string{"session_id"},
	}
}
func (t subagentStatusTool) ReadOnly() bool { return true }
func (t subagentStatusTool) Run(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
	if t.runner == nil {
		return marshalError(call, "not_configured", "task runner is not configured")
	}
	req, err := decodeInput[subagentStatusRequest](call)
	if err != nil {
		return marshalError(call, "invalid_input", err.Error())
	}
	meta, err := t.runner.SubagentStatus(req.SessionID)
	if err != nil {
		return marshalError(call, "subagent_status_failed", err.Error())
	}
	return marshalSuccess(call, map[string]any{
		"session_id":           req.SessionID,
		"kind":                 meta.Kind,
		"parent_session_id":    meta.ParentSessionID,
		"role":                 meta.Role,
		"model":                meta.Model,
		"task":                 meta.Task,
		"status":               meta.Status,
		"report":               meta.Report,
		"summary":              meta.Summary,
		"error":                meta.Error,
		"workspace":            meta.Workspace,
		"worktree_path":        meta.WorktreePath,
		"started_at":           meta.StartedAt,
		"completed_at":         meta.CompletedAt,
		"original_workspace":   meta.OriginalWorkspace,
		"original_branch":      meta.OriginalBranch,
		"original_head_commit": meta.OriginalHeadCommit,
	})
}

type cancelSubagentTool struct {
	runner *Runner
}

func (t cancelSubagentTool) Name() string { return "cancel_subagent" }
func (t cancelSubagentTool) Description() string {
	return "Cancel a running background child subagent by session_id. Returns the latest lifecycle metadata; completed or unknown subagents are not cancelled."
}
func (t cancelSubagentTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"session_id": map[string]any{"type": "string"},
		},
		"required": []string{"session_id"},
	}
}
func (t cancelSubagentTool) ReadOnly() bool { return false }
func (t cancelSubagentTool) Capabilities() []string {
	return []string{"mutates_state"}
}
func (t cancelSubagentTool) Run(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
	if t.runner == nil {
		return marshalError(call, "not_configured", "task runner is not configured")
	}
	req, err := decodeInput[subagentStatusRequest](call)
	if err != nil {
		return marshalError(call, "invalid_input", err.Error())
	}
	meta, cancelled, err := t.runner.CancelBackgroundSubagent(req.SessionID)
	if err != nil {
		return marshalError(call, "cancel_subagent_failed", err.Error())
	}
	return marshalSuccess(call, map[string]any{
		"session_id":   req.SessionID,
		"cancelled":    cancelled,
		"status":       meta.Status,
		"report":       meta.Report,
		"summary":      meta.Summary,
		"error":        meta.Error,
		"completed_at": meta.CompletedAt,
	})
}

func encodeInput(v any) string {
	b, _ := core.MarshalToolJSON(v)
	return string(b)
}

func errorContent(code, message string) string {
	return fmt.Sprintf(`{"ok":false,"code":%q,"message":%q}`, code, message)
}

// --- agent_search ---

// builtinAgentDefs returns Name/Description/WhenToUse for the three built-in
// roles. The canonical (full) definitions live in builtinAgentDefinition.
func builtinAgentDefs() []AgentDefinition {
	names := []string{"explore", "research", "review"}
	out := make([]AgentDefinition, 0, len(names))
	for _, name := range names {
		if def, ok := builtinAgentDefinition(name); ok {
			out = append(out, AgentDefinition{
				Name:        def.Name,
				Description: def.Description,
				WhenToUse:   def.WhenToUse,
			})
		}
	}
	return out
}

type agentSearchTool struct {
	runner *Runner
}

func (t agentSearchTool) Name() string { return "agent_search" }

func (t agentSearchTool) Description() string {
	return "Search custom subagent definitions by keyword. Use when the task domain goes beyond generic exploration/research/review — for example, security auditing, database operations, trading, or deployment. Matching agents are spawned by name via spawn_subagent. Built-in roles (explore, research, review) are always available."
}

func (t agentSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query: keyword phrase to match against agent name, description, and whenToUse. Case-insensitive substring matching. Returns up to 5 best matches. Use select:Name1,Name2 for exact name lookup. Combine multiple terms (e.g. 'security database') to search across domains in one call.",
			},
		},
		"required": []string{"query"},
	}
}

func (t agentSearchTool) ReadOnly() bool     { return true }
func (t agentSearchTool) Capabilities() []string { return nil }

func (t agentSearchTool) Run(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
	if t.runner == nil {
		return marshalError(call, "not_configured", "task runner is not configured")
	}

	type searchArgs struct {
		Query string `json:"query"`
	}
	args, err := decodeInput[searchArgs](call)
	if err != nil {
		return marshalError(call, "invalid_input", err.Error())
	}

	query := strings.TrimSpace(args.Query)
	if query == "" {
		return marshalError(call, "invalid_input", "query is required")
	}

	// select:Name1,Name2 — exact name lookup
	if strings.HasPrefix(query, "select:") {
		return t.selectByName(ctx, call, strings.TrimPrefix(query, "select:"))
	}

	builtins := builtinAgentDefs()

	// Load custom agent definitions from library
	customDefs, err := t.runner.agentDefinitions.List(ctx)
	if err != nil {
		// Non-fatal: return builtins + error note
		return t.renderResults(call, query, builtins, nil, fmt.Errorf("scan custom agents: %w", err))
	}

	return t.renderResults(call, query, builtins, customDefs, nil)
}

func (t agentSearchTool) selectByName(ctx context.Context, call core.ToolCall, names string) (core.ToolResult, error) {
	wanted := make(map[string]bool)
	for _, n := range strings.Split(names, ",") {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	if len(wanted) == 0 {
		return marshalError(call, "invalid_input", "no names provided in select: query")
	}

	library := t.runner.agentDefinitions
	if library == nil {
		return marshalError(call, "not_found", "no agent definitions available")
	}

	allDefs, err := library.List(ctx)
	if err != nil {
		return marshalError(call, "lookup_failed", fmt.Sprintf("scan agents: %v", err))
	}

	var found []AgentDefinition
	for _, def := range allDefs {
		if wanted[def.Name] {
			found = append(found, def)
		}
	}
	// Also check built-in roles
	for _, def := range builtinAgentDefs() {
		if wanted[def.Name] {
			found = append(found, def)
		}
	}

	if len(found) == 0 {
		text := "No agents found with the requested names. Use agent_search with a keyword to discover available agents."
		return core.ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			ModelText:  text,
		}, nil
	}

	// Pass names (without "select:" prefix) as query so scoring works correctly.
	return t.renderResults(call, names, nil, found, nil)
}

type agentMatch struct {
	AgentDefinition
	score int
}

func (t agentSearchTool) renderResults(call core.ToolCall, query string, builtins, customs []AgentDefinition, scanErr error) (core.ToolResult, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	tokens := strings.Fields(query)

	var matches []agentMatch
	seen := map[string]bool{}

	scoreFn := func(def AgentDefinition) int {
		name := strings.ToLower(def.Name)
		desc := strings.ToLower(def.Description)
		when := strings.ToLower(def.WhenToUse)
		score := 0
		for _, tok := range tokens {
			if name == tok {
				score += 20
			} else if strings.HasPrefix(name, tok) {
				score += 12
			} else if strings.Contains(name, tok) {
				score += 8
			}
			if strings.Contains(desc, tok) {
				score += 4
			}
			if strings.Contains(when, tok) {
				score += 5
			}
		}
		return score
	}

	for _, def := range builtins {
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		if s := scoreFn(def); s > 0 {
			matches = append(matches, agentMatch{AgentDefinition: def, score: s})
		}
	}
	for _, def := range customs {
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		if s := scoreFn(def); s > 0 {
			matches = append(matches, agentMatch{AgentDefinition: def, score: s})
		}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].Name < matches[j].Name
	})

	const maxResults = 5
	totalCustom := len(customs)
	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	// All builtins are always available — note this only when no custom agents matched
	if len(matches) == 0 && totalCustom == 0 && scanErr == nil {
		return core.ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			ModelText:  fmt.Sprintf("No custom agents matched %q. Built-in roles (explore, research, review) are always available — pass them directly to spawn_subagent. Try a different keyword or use select:Name to look up a specific custom agent.", query),
		}, nil
	}
	if len(matches) == 0 && scanErr == nil {
		return core.ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			ModelText:  fmt.Sprintf("No agents matched %q. Built-in roles (explore, research, review) are always available. Try a different keyword.", query),
		}, nil
	}

	var b strings.Builder
	if scanErr != nil {
		fmt.Fprintf(&b, "Note: could not scan custom agents (%v).\n\n", scanErr)
	}
	fmt.Fprintf(&b, "Matched %d agent(s) for %q:\n\n", len(matches), query)
	for _, m := range matches {
		fmt.Fprintf(&b, "- **%s**: %s\n", m.Name, m.Description)
		if m.WhenToUse != "" {
			fmt.Fprintf(&b, "  When to use: %s\n", m.WhenToUse)
		}
	}
	if totalCustom > maxResults {
		b.WriteString(fmt.Sprintf("\nShowing top %d of %d custom agents. Refine your query for more specific results. Built-in roles (explore, research, review) are always available.\n", maxResults, totalCustom))
	} else {
		b.WriteString("\nBuilt-in roles (explore, research, review) are always available.\n")
	}
	b.WriteString("\nIf an agent's \"When to use\" matches your task, spawn it — do not replicate its work with built-in tools.")

	return core.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		ModelText:  b.String(),
	}, nil
}
