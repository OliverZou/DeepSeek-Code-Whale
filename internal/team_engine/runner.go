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
	SpawnerType     string  `json:"-"` // "adapter" or "shell" — which spawner was used
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
type SubagentProgress func(status, summary, toolName string)

// SubagentRequest is the minimum set of parameters needed to spawn
// a Whale subagent for a Team Engine task.
type SubagentRequest struct {
	Task         string   // The prompt/description for the agent
	Role         string   // Role name (e.g. "developer", "verifier")
	AgentName    string   // Agent definition name from .md file (e.g. "backend-engineer")
	Model        string   // LLM model name; "" = Whale default
	Tools        []string // Allowed tool names
	Workdir      string   // Working directory
	Timeout      time.Duration
	MaxIters     int
	MaxCalls     int
	MaxTokens    int                  // Completion token budget (0 = runner default)
	OutputSchema map[string]any       // Force structured JSON output (nil = free text)
	OnProgress   SubagentProgress     // Real-time progress callback (nil = no streaming)
	OnPID        func(int)            // Called with PID when OS process starts (nil = no-op)
	OnStdin      func(io.WriteCloser) // Called with stdin pipe for real-time messaging (nil = no-op)
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
	Diagnostic      string // detailed debug info (tool resolution, status, errors)
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
func (ar *AgentRunner) RunWithContext(ctx context.Context, prompt, workdir, tools string, timeout time.Duration, onProgress SubagentProgress, onPID func(int), onStdin func(io.WriteCloser), model ...string) *RunResult {
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
		Task:       prompt,
		Role:       role,
		AgentName:  agentName,
		Tools:      toolNames,
		Workdir:    workdir,
		Timeout:    timeout,
		MaxIters:   80,
		MaxCalls:   200,
		OnProgress: onProgress,
		OnPID:      onPID,
		OnStdin:    onStdin,
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}
	req.MaxTokens = effectiveMaxTokens(0, req.Model)

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
		UsagePrompt:     resp.UsagePrompt,
		UsageCompletion: resp.UsageCompletion,
		PID:             resp.PID,
	}
}

// Run is a convenience wrapper for RunWithContext with a background context.
func (ar *AgentRunner) Run(prompt, workdir, tools string, timeout time.Duration, model ...string) *RunResult {
	return ar.RunWithContext(context.Background(), prompt, workdir, tools, timeout, nil, nil, nil, model...)
}

// RunVerifier spawns a verifier subagent.  When agentName is set, the
// agent definition (from .md file or builtin) provides tools, system prompt,
// and skills — no hand-crafted tool list needed.  When agentName is empty,
// falls back to ProfileVerify tools so the verifier can still run
// tests/linters.
func (ar *AgentRunner) RunVerifier(prompt, workdir string, timeout time.Duration, agentName string, model ...string) *RunResult {
	req := SubagentRequest{
		Task:      prompt,
		Role:      "verifier",
		AgentName: agentName,
		Workdir:   workdir,
		Timeout:   timeout,
		MaxIters:  15,
		MaxCalls:  50,
	}
	// When no agent definition is available, fall back to ProfileVerify tools
	// so the verifier can at least run tests and read files.
	if agentName == "" {
		req.Tools = ProfileToToolNames(ProfileVerify)
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}
	req.MaxTokens = effectiveMaxTokens(0, req.Model)

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
		UsagePrompt:     resp.UsagePrompt,
		UsageCompletion: resp.UsageCompletion,
		PID:             resp.PID,
	}
}

// RunDecomposer runs a Leader/Planner subagent to decompose a goal into subtasks.
//
// For reasoning models (deepseek-v4-pro, etc.) the decomposer gets a larger
// token budget (defaultMaxTokens) because the planning prompt is significantly
// longer than a typical task prompt and the model's chain-of-thought can
// consume 60-80% of the completion budget.
//
// When a liteSpawner is configured (in-process LLM call), RunDecomposer uses it
// instead of shelling out — this avoids subprocess cold-start overhead (~3-5s).
// In lite mode, tools are skipped (pure reasoning) and OutputSchema is omitted
// (DeepSeek doesn't support structured output).  The caller parses JSON from
// the text response.
func (ar *AgentRunner) RunDecomposer(prompt, workdir string, timeout time.Duration, model ...string) *RunResult {
	mdl := ""
	if len(model) > 0 && model[0] != "" {
		mdl = model[0]
	}

	// Prefer in-process lite spawner when available — decomposition is a pure
	// reasoning task that doesn't need tools or multi-turn iteration.
	spawner := ar.spawner
	usingLite := ar.liteSpawner != nil
	if usingLite {
		spawner = ar.liteSpawner
		// Default to reasoning model — decomposition benefits from CoT.
		if mdl == "" {
			mdl = "deepseek-v4-pro"
		}
	}

	req := SubagentRequest{
		Task:     prompt,
		Role:     "planner",
		Workdir:  workdir,
		Timeout:  timeout,
		MaxIters: 15,
		MaxCalls: 40,
	}
	if mdl != "" {
		req.Model = mdl
	}

	// Lite spawner: no tools (pure reasoning), large token budget for CoT.
	// Shell spawner: include read-only tools, token budget via effectiveMaxTokens.
	if usingLite {
		req.Tools = nil
		req.MaxTokens = defaultMaxTokens
	} else {
		req.Tools = ProfileToToolNames(ProfileReadOnly)
		req.MaxTokens = effectiveMaxTokens(0, mdl)

		// OutputSchema only works with shell spawner + Claude models.
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
								"verifier_role":    map[string]any{"type": "string"},
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
		UsagePrompt:     resp.UsagePrompt,
		UsageCompletion: resp.UsageCompletion,
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
		UsagePrompt:     resp.UsagePrompt,
		UsageCompletion: resp.UsageCompletion,
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
