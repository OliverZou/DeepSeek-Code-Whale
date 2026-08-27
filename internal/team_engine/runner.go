package team_engine

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Token budget helpers
// ---------------------------------------------------------------------------

// defaultMaxTokens is the minimum completion budget when no explicit
// limit is set.  DeepSeek models need this floor — otherwise they
// default to a very small output window, causing truncated plans.
const defaultMaxTokens = 32000

func effectiveMaxTokens(requested int, model string) int {
	if requested > 0 {
		return requested
	}
	return defaultMaxTokens
}

// iterationBudget resolves a worker/verifier's iteration budget from the goal
// complexity hint (simple/medium/complex, produced by the elaborate flash call
// — no extra LLM invocation).  Simple tasks get a tight budget so the subagent
// finishes fast and cheap instead of treating a large iteration allowance as
// licence to over-explore; complex or unassessed tasks keep the existing
// generous defaults.
func iterationBudget(complexity string, isVerifier bool) (maxIters, maxCalls, maxTokens int) {
	switch complexity {
	case "simple":
		if isVerifier {
			return 12, 30, 12000
		}
		return 20, 50, 16000
	case "medium":
		if isVerifier {
			// 12/35 太紧：v41 fix 的 semantic verifier 连续两轮被 tool cap
			// 中断（“读多文件+复验”>35 次调用），中断输出被误判 FAIL →
			// worker 3 次重试烧 2.1M token。放宽到 20/60。
			return 20, 60, 20000
		}
		// v45: medium 实现任务 50 轮允许“写→跑→改”长循环（game-dom.js
		// 1.36M tokens/35 调用）。30 轮/80 调用足够覆盖正常调试，压住失控循环。
		return 30, 80, 22000
	default: // complex or unassessed — keep existing defaults
		if isVerifier {
			return 25, 70, defaultMaxTokens
		}
		return 80, 200, defaultMaxTokens
	}
}

// isDeepSeekModel returns true for DeepSeek-family models.
func isDeepSeekModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "deepseek")
}

// isReasoningModel returns true for models that use chain-of-thought reasoning.
func isReasoningModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "v4-pro") || strings.Contains(lower, "opus")
}

// supportsStructuredOutput returns true for models that support the
// structured_output tool.  DeepSeek models don't; Claude models do.
func supportsStructuredOutput(model string) bool {
	lower := strings.ToLower(model)
	// Claude models support structured output; DeepSeek doesn't.
	return strings.Contains(lower, "claude") || strings.Contains(lower, "sonnet") || strings.Contains(lower, "opus") || strings.Contains(lower, "haiku") || strings.Contains(lower, "fable")
}

// ---------------------------------------------------------------------------
// AgentRunner — stateless subagent execution
// ---------------------------------------------------------------------------

// RunResult captures the outcome of a single agent run.
type RunResult struct {
	SessionID       string  `json:"session_id,omitempty"` // subagent session ID for traceability
	ExitCode        int     `json:"exit_code"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	DurationSeconds float64 `json:"duration_seconds"`
	Success         bool    `json:"success"`
	Structured      any     `json:"-"` // structured output (when OutputSchema was set)
	UsagePrompt     int     `json:"-"` // prompt tokens (0 if unavailable)
	UsageCompletion int     `json:"-"` // completion tokens (0 if unavailable)
	// Cache split (DeepSeek prefix caching): hit tokens are billed at ~1/31 of
	// miss tokens, so raw prompt sums massively overstate real cost.
	UsagePromptCacheHit  int `json:"-"` // prompt tokens served from prefix cache
	UsagePromptCacheMiss int `json:"-"` // prompt tokens billed at full input price
	SpawnerType     string  `json:"-"` // "adapter" or "shell" — which spawner was used
	SystemPrompt    string  `json:"-"` // assembled extra system-prompt content (adapter only; "" for shell)
	PID             int     `json:"-"` // OS process ID (0 if in-process)
}

func round(v float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(v*pow) / pow
}

// SubagentSpawner is the interface for spawning Whale subagents.
// The concrete implementation (provided by the Whale CLI toolset in
// internal/app/app.go) provides the concrete implementation that wires
// into Whale's agent runtime.
type SubagentSpawner interface {
	// SpawnSubagent runs a Whale subagent with the given prompt and tools.
	// Returns the full output text and any error.
	SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error)
}

// SubagentProgress is a callback that receives real-time progress from
// the running subagent.  It replaces the original team-engine-go's
// stdout pipe streaming with Whale-native event-driven progress.
type SubagentProgress func(status, summary, toolName string, toolDurationMS int64)

// SubagentRequest is the minimum set of parameters needed to spawn
// a Whale subagent for a Team Engine task.
type SubagentRequest struct {
	Task          string   // The prompt/description for the agent
	Role          string   // Role name (e.g. "developer", "verifier")
	AgentName     string   // Agent definition name from .md file (e.g. "backend-engineer")
	Team          string   // team name, recorded into session meta for picker rendering
	TeamAgentsDir string   // team's agents/ dir (e.g. ~/.whale/teams/<name>/agents); adapter resolves team-local agent definitions from here
	Model         string   // LLM model name; "" = Whale default
	Tools         []string // Allowed tool names
	// OrchestrationTools are team tool names (OrchestrationToolNames) merged
	// into the resolved agent definition's tool selectors by the app adapter —
	// they never replace the definition's own declared tools. Used for the
	// Leader's spawn so its continued turns can drive the team.
	OrchestrationTools []string
	Workdir            string // Working directory
	// WriteAllowlist is the task's declared output files (normalized later in
	// the agent layer). When non-empty, the spawned worker may only write these
	// paths (+ WriteExemptDirs). This is a system-level internal invariant that
	// prevents a worker from overwriting another task's deliverable.
	WriteAllowlist []string
	// WriteExemptDirs are dirs always writable regardless of the allowlist
	// (e.g. system temp dir for verification scripts).
	WriteExemptDirs []string
	Timeout         time.Duration
	MaxIters        int
	MaxCalls        int
	MaxTokens       int                  // Completion token budget (0 = runner default)
	ReportCap       int                  // Report cap override: <0 = never truncate, 0 = runner default
	OutputSchema    map[string]any       // Force structured JSON output (nil = free text)
	OnProgress      SubagentProgress     // Real-time progress callback (nil = no streaming)
	OnPID           func(int)            // Called with PID when OS process starts (nil = no-op)
	OnStdin         func(io.WriteCloser) // Called with stdin pipe for real-time messaging (nil = no-op)
}

// SubagentResponse contains the result of a subagent execution.
type SubagentResponse struct {
	SessionID       string // Whale subagent session ID (empty for shell spawner)
	SpawnerType     string // "adapter" or "shell" — which spawner was used
	Output          string // Full agent output
	Structured      any    // Structured output (when OutputSchema was set)
	ExitCode        int
	Success         bool
	UsagePrompt     int    // prompt tokens consumed
	UsageCompletion int    // completion tokens consumed
	// Cache split (DeepSeek prefix caching) — see RunResult.
	UsagePromptCacheHit  int
	UsagePromptCacheMiss int
	Diagnostic      string // detailed debug info (tool resolution, status, errors)
	SystemPrompt    string // assembled extra system-prompt content (adapter only; "" for shell)
	PID             int    // OS process ID (0 if in-process adapter)
}

// AgentRunner is a stateless wrapper around a SubagentSpawner.
type AgentRunner struct {
	spawner     SubagentSpawner
	liteSpawner SubagentSpawner // optional fast path (no subprocess) for light calls
	team        *TeamConfig
}

// NewRunner creates an AgentRunner backed by the given spawner.
func NewRunner(spawner SubagentSpawner) *AgentRunner {
	return &AgentRunner{spawner: spawner}
}

// WithTeam attaches a TeamConfig so the runner can resolve role→agent mappings.
func (ar *AgentRunner) WithTeam(team *TeamConfig) *AgentRunner {
	ar.team = team
	return ar
}

// SetLiteSpawner sets an optional lightweight spawner used by fast-path
// methods (e.g. RunElaborationStep).  When nil (default), callers fall back
// to the main spawner.  Use this to bypass subprocess overhead for pure
// prompt→response calls that don't need tools or multi-turn iteration.
func (ar *AgentRunner) SetLiteSpawner(s SubagentSpawner) {
	ar.liteSpawner = s
}

// RunWithContext spawns a worker subagent with cancellation support.  The
// `onProgress` callback receives real-time tool execution events.  When
// `ctx` is cancelled (e.g. by user clicking "stop" in the dashboard or
// by engine shutdown), the subagent context is cancelled and the spawn
// returns early.  The callback is invoked from a background goroutine and
// must be concurrency-safe.  If the caller doesn't need progress tracking,
// pass nil.
//
// IMPORTANT: RunWithContext creates its own derived context with the given
// timeout.  The `ctx` parameter is used ONLY for cancellation — if the
// parent context is cancelled (e.g. via Close()), the spawn is aborted.
func (ar *AgentRunner) RunWithContext(ctx context.Context, prompt, workdir, tools string, timeout time.Duration, maxIters, maxCalls, maxTokens int, writeAllowlist, writeExemptDirs []string, onProgress SubagentProgress, onPID func(int), onStdin func(io.WriteCloser), model ...string) *RunResult {
	start := time.Now()

	toolNames := parseToolList(tools)

	// Extract role from prompt prefix "[Role: xxx]" and resolve agent name.
	role := "worker"
	agentName := ""
	if strings.HasPrefix(prompt, "[Role: ") {
		if idx := strings.Index(prompt, "]"); idx > 7 {
			role = prompt[7:idx]
		}
	}
	if ar.team != nil {
		for _, name := range ar.team.Roles {
			if name == role {
				agentName = name
				break
			}
		}
	}

	req := SubagentRequest{
		Task:            prompt,
		Role:            role,
		AgentName:       agentName,
		Team:            teamName(ar.team),
		TeamAgentsDir:   teamAgentsDir(ar.team),
		Tools:           toolNames,
		Workdir:         workdir,
		WriteAllowlist:  writeAllowlist,
		WriteExemptDirs: writeExemptDirs,
		Timeout:         timeout,
		MaxIters:        maxIters,
		MaxCalls:        maxCalls,
		OnProgress:      onProgress,
		OnPID:           onPID,
		OnStdin:         onStdin,
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}
	req.MaxTokens = effectiveMaxTokens(maxTokens, req.Model)

	// Use a derived context so cancellation of the parent context
	// propagates to the spawn, but the timeout is independent.
	spawnCtx, spawnCancel := context.WithTimeout(ctx, timeout)
	defer spawnCancel()

	resp, err := ar.spawner.SpawnSubagent(spawnCtx, req)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		return &RunResult{
			SessionID:       resp.SessionID,
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
			PID:             resp.PID,
		}
	}

	return &RunResult{
		SessionID:       resp.SessionID,
		ExitCode:        resp.ExitCode,
		Stdout:          resp.Output,
		SpawnerType:     resp.SpawnerType,
		Stderr:          resp.Diagnostic,
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
		Structured:      resp.Structured,
		UsagePrompt:         resp.UsagePrompt,
		UsageCompletion:     resp.UsageCompletion,
		UsagePromptCacheHit: resp.UsagePromptCacheHit,
		UsagePromptCacheMiss: resp.UsagePromptCacheMiss,
		SystemPrompt:    resp.SystemPrompt,
		PID:             resp.PID,
	}
}

// Run is a convenience wrapper for RunWithContext with a background context.
func (ar *AgentRunner) Run(prompt, workdir, tools string, timeout time.Duration, model ...string) *RunResult {
	return ar.RunWithContext(context.Background(), prompt, workdir, tools, timeout, 80, 200, 0, nil, nil, nil, nil, nil, model...)
}

// RunVerifier spawns a verifier subagent.  The agent definition (the system
// verifier .md, embedded in the binary) provides the system prompt and skills,
// but the toolset is always ProfileVerify (read + shell.run + web, no write)
// — verification is tool-grounded by the task, so it never inherits a role's
// write/test tools.
func (ar *AgentRunner) RunVerifier(prompt, workdir string, timeout time.Duration, maxIters, maxCalls, maxTokens int, agentName string, model ...string) *RunResult {
	req := SubagentRequest{
		Task:          prompt,
		Role:          "verifier",
		AgentName:     agentName,
		Team:          teamName(ar.team),
		TeamAgentsDir: teamAgentsDir(ar.team),
		Workdir:       workdir,
		Timeout:       timeout,
		MaxIters:      maxIters,
		MaxCalls:      maxCalls,
	}
	// Verification is tool-grounded: the verifier needs read + shell.run (to
	// execute the worker's own tests/linters) but never write — it is an
	// inspector, not an editor, and must not inherit a QA persona's
	// write/test tools. The verify toolset is fixed by the task, not by the role.
	req.Tools = ProfileToToolNames(ProfileVerify)
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}
	req.MaxTokens = effectiveMaxTokens(maxTokens, req.Model)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	resp, err := ar.spawner.SpawnSubagent(ctx, req)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		return &RunResult{
			SessionID:       resp.SessionID,
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("verifier subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
			PID:             resp.PID,
		}
	}

	return &RunResult{
		SessionID:       resp.SessionID,
		ExitCode:        resp.ExitCode,
		Stdout:          resp.Output,
		SpawnerType:     resp.SpawnerType,
		Stderr:          resp.Diagnostic,
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
		Structured:      resp.Structured,
		UsagePrompt:         resp.UsagePrompt,
		UsageCompletion:     resp.UsageCompletion,
		UsagePromptCacheHit: resp.UsagePromptCacheHit,
		UsagePromptCacheMiss: resp.UsagePromptCacheMiss,
		SystemPrompt:    resp.SystemPrompt,
		PID:             resp.PID,
	}
}

// RunDecomposer runs the Leader subagent to decompose a goal into subtasks.
//
// buildLeaderSubagentRequest assembles the Leader's spawn request shared by
// decomposition and plan-file bootstrap: same agent-definition resolution,
// orchestration tools recorded on the session (turn 2 rebuilds the agent from
// this record), never-truncate report cap.
func (ar *AgentRunner) buildLeaderSubagentRequest(prompt, workdir string, timeout time.Duration, maxIters, maxCalls int, model ...string) SubagentRequest {
	mdl := ""
	if len(model) > 0 && model[0] != "" {
		mdl = model[0]
	}

	// Resolve the Leader's agent name from the team config. Role stays "planner"
	// (the semantic category, mirroring the verifier's fixed "verifier" role);
	// the .md definition is resolved via AgentName like worker/verifier.
	agentName := ""
	if ar.team != nil {
		agentName = ar.team.Leader.Role
	}

	req := SubagentRequest{
		Task:          prompt,
		Role:          "planner",
		AgentName:     agentName,
		Team:          teamName(ar.team),
		TeamAgentsDir: teamAgentsDir(ar.team),
		Workdir:       workdir,
		Timeout:       timeout,
		MaxIters:      maxIters,
		MaxCalls:      maxCalls,
		// The decompose plan JSON exceeds the default 8192-char cap on real
		// goals; truncating it corrupts the plan and silently degrades the
		// whole run to a single fallback task. Never truncate the planner.
		ReportCap: -1,
		// The Leader's continued (turn-2) turns drive the team from its own
		// session, so the spawn that records this session must also record the
		// orchestration tools — Turn 2 rebuilds the agent from this record.
		OrchestrationTools: OrchestrationToolNames,
	}
	if mdl != "" {
		req.Model = mdl
	}
	req.MaxTokens = effectiveMaxTokens(0, mdl)

	// Structured plan output for Claude models. The native adapter forwards
	// OutputSchema to the subagent run; DeepSeek models don't support it, so
	// for them the caller parses JSON from the free-text response.
	if supportsStructuredOutput(mdl) {
		req.OutputSchema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":            map[string]any{"type": "string"},
							"description":      map[string]any{"type": "string"},
							"role":             map[string]any{"type": "string"},
							"batch_id":         map[string]any{"type": "string"},
							"batch_label":      map[string]any{"type": "string"},
							"depends_on_batch": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"depends_on_index": map[string]any{"type": "integer"},
							"verifier_focus":   map[string]any{"type": "string"},
							"verify_mode":      map[string]any{"type": "string"},
							"use_dw":           map[string]any{"type": "boolean"},
							"max_cycles":       map[string]any{"type": "integer"},
						},
						"required": []string{"title", "description", "role"},
					},
				},
			},
			"required": []string{"tasks"},
		}
	}
	return req
}

// RunLeaderBootstrap spawns the Leader session WITHOUT letting it produce a
// plan: the plan arrives from outside (--plan-file).  The session is still
// created (with orchestration tools recorded) so the drive (turn 2) and review
// (turn 3) turns can Continue it, but the bootstrap turn itself is a single
// minimal reply — no planning, no plan regeneration — keeping the injected
// plan byte-for-byte fixed for head-to-head A/B runs.
// 预算记录必须按 review 轮的用量（15/40，与 RunDecomposer 一致）而非 1/1：
// Continue 复用 session 时按记录限额——v44 leader 想 team_feedback+team_run
// 重派 QA 时被 tool call cap 打断（bootstrap 时若记 1/1，review 轮每轮只有
// 1 次工具调用，永远无法完成重派闭环）。
func (ar *AgentRunner) RunLeaderBootstrap(prompt, workdir string, timeout time.Duration, model ...string) *RunResult {
	req := ar.buildLeaderSubagentRequest(prompt, workdir, timeout, 15, 40, model...)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	resp, err := ar.spawner.SpawnSubagent(ctx, req)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		return &RunResult{
			SessionID:       resp.SessionID,
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("leader bootstrap subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
			PID:             resp.PID,
		}
	}

	return &RunResult{
		SessionID:       resp.SessionID,
		ExitCode:        resp.ExitCode,
		Stdout:          resp.Output,
		SpawnerType:     resp.SpawnerType,
		Stderr:          resp.Diagnostic,
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
		Structured:      resp.Structured,
		UsagePrompt:         resp.UsagePrompt,
		UsageCompletion:     resp.UsageCompletion,
		UsagePromptCacheHit: resp.UsagePromptCacheHit,
		UsagePromptCacheMiss: resp.UsagePromptCacheMiss,
		SystemPrompt:    resp.SystemPrompt,
	}
}

// The Leader is spawned through the native adapter (ar.spawner) with its own
// agent definition — resolved from the team's agents/<leader-role>.md — exactly
// like a worker or verifier. This gives the Leader a persistent, forkable JSONL
// session and its .md persona/tools/permission. The lite (in-process,
// session-less) path is reserved for RunElaborationStep, never the Leader.
//
// For reasoning models (deepseek-v4-pro, etc.) the decomposer gets a larger
// token budget (defaultMaxTokens) because the planning prompt is significantly
// longer than a typical task prompt and the model's chain-of-thought can
// consume 60-80% of the completion budget.
// RunDecomposer is a LEAN planner spawn: pure text → plan JSON. It carries no
// team .md persona, no orchestration tools and a single generation pass —
// decomposition is a one-shot text-in/text-out analysis, and persona/tools on
// this call only add latency and cost. Deeper leader sessions (drive/review on
// a recorded plan) use RunLeaderBootstrap instead, which keeps the persona and
// orchestration tools for turn-2 rebuild.
func (ar *AgentRunner) RunDecomposer(prompt, workdir string, timeout time.Duration, model ...string) *RunResult {
	mdl := ""
	if len(model) > 0 && model[0] != "" {
		mdl = model[0]
	}
	req := SubagentRequest{
		Task:      prompt,
		Role:      "planner",
		Team:      teamName(ar.team),
		Workdir:   workdir,
		Timeout:   timeout,
		MaxIters:  1,
		MaxCalls:  1,
		ReportCap: -1, // never truncate the plan JSON
	}
	if mdl != "" {
		req.Model = mdl
	}
	req.MaxTokens = effectiveMaxTokens(0, mdl)

	// Decompose is a pure text→plan-JSON analysis: the lite spawner (no tools,
	// no subprocess) is the right path — the full subagent resolves a planner
	// agent definition whose workspace tools tempt the model into an
	// exploratory list_dir, which burns the one-shot tool budget and gets the
	// turn force-summarized before the JSON is emitted (v13: first decompose
	// draw auto-interrupted → retry → plan drifted from 5 tasks to 6 serial
	// batches). Elaboration already runs through the lite path; decompose
	// should match. When no lite spawner is wired, fall back to the main
	// spawner (SessionID is not needed here — BootstrapSession covers the
	// leader session for drive/review turns).
	spawner := ar.spawner
	if ar.liteSpawner != nil {
		spawner = ar.liteSpawner
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	resp, err := spawner.SpawnSubagent(ctx, req)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		return &RunResult{
			SessionID:       resp.SessionID,
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("decomposer subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
			PID:             resp.PID,
		}
	}

	return &RunResult{
		SessionID:       resp.SessionID,
		ExitCode:        resp.ExitCode,
		Stdout:          resp.Output,
		SpawnerType:     resp.SpawnerType,
		Stderr:          resp.Diagnostic,
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
		Structured:      resp.Structured,
		UsagePrompt:         resp.UsagePrompt,
		UsageCompletion:     resp.UsageCompletion,
		UsagePromptCacheHit: resp.UsagePromptCacheHit,
		UsagePromptCacheMiss: resp.UsagePromptCacheMiss,
		SystemPrompt:    resp.SystemPrompt,
	}
}

// ---------------------------------------------------------------------------
// RunElaborationStep runs a lightweight, one-shot LLM call for elaboration.
// Unlike RunDecomposer, this has NO tools, NO OutputSchema, NO iteration budget —
// it's a pure prompt→response call.  Elaboration steps (completeness check,
// domain research, spec production) are text-in/text-out analysis that never
// needs file I/O or multi-turn reasoning.
func (ar *AgentRunner) RunElaborationStep(prompt, workdir string, timeout time.Duration, maxTokens int, model ...string) *RunResult {
	req := SubagentRequest{
		Task:     prompt,
		Role:     "planner",
		Team:     teamName(ar.team),
		Tools:    nil, // no tools — pure text analysis
		Workdir:  workdir,
		Timeout:  timeout,
		MaxIters: 1, // one-shot
		MaxCalls: 1,
		// No OutputSchema — elaboration returns free-text YAML or JSON,
		// parsed by the caller.
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}
	if maxTokens > 0 {
		req.MaxTokens = maxTokens
	} else {
		req.MaxTokens = effectiveMaxTokens(0, req.Model)
	}

	spawner := ar.spawner
	if ar.liteSpawner != nil {
		spawner = ar.liteSpawner
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	resp, err := spawner.SpawnSubagent(ctx, req)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		return &RunResult{
			SessionID:       resp.SessionID,
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("elaboration subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
			PID:             resp.PID,
		}
	}

	return &RunResult{
		SessionID:       resp.SessionID,
		ExitCode:        resp.ExitCode,
		Stdout:          resp.Output,
		SpawnerType:     resp.SpawnerType,
		Stderr:          resp.Diagnostic,
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
		Structured:      resp.Structured,
		UsagePrompt:         resp.UsagePrompt,
		UsageCompletion:     resp.UsageCompletion,
		UsagePromptCacheHit: resp.UsagePromptCacheHit,
		UsagePromptCacheMiss: resp.UsagePromptCacheMiss,
		SystemPrompt:    resp.SystemPrompt,
	}
}

// ---------------------------------------------------------------------------
// Tool list helpers
// ---------------------------------------------------------------------------

func parseToolList(tools string) []string {
	if tools == "" {
		return nil
	}
	parts := strings.Split(tools, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
