package team_engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Token budget helpers
// ---------------------------------------------------------------------------

func effectiveMaxTokens(requested int, model string) int {
	if requested > 0 {
		return requested
	}
	return 0 // let the model/provider decide
	return 0
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
	ExitCode        int            `json:"exit_code"`
	Stdout          string         `json:"stdout"`
	Stderr          string         `json:"stderr"`
	DurationSeconds float64        `json:"duration_seconds"`
	Success         bool           `json:"success"`
	Structured      any            `json:"-"` // structured output (when OutputSchema was set)
	UsagePrompt     int            `json:"-"` // prompt tokens (0 if unavailable)
	UsageCompletion int            `json:"-"` // completion tokens (0 if unavailable)
	SpawnerType     string         `json:"-"` // "adapter" or "shell" — which spawner was used
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
	Task         string            // The prompt/description for the agent
	Role         string            // Role name (e.g. "developer", "verifier")
	Model        string            // LLM model name; "" = Whale default
	Tools        []string          // Allowed tool names
	Workdir      string            // Working directory
	Timeout      time.Duration
	MaxIters     int
	MaxCalls     int
	MaxTokens    int               // Completion token budget (0 = runner default)
	OutputSchema map[string]any    // Force structured JSON output (nil = free text)
	OnProgress   SubagentProgress  // Real-time progress callback (nil = no streaming)
}

// SubagentResponse contains the result of a subagent execution.
type SubagentResponse struct {
	SpawnerType     string         // "adapter" or "shell" — which spawner was used
	Output          string         // Full agent output
	Structured      any            // Structured output (when OutputSchema was set)
	ExitCode        int
	Success         bool
	UsagePrompt     int            // prompt tokens consumed
	UsageCompletion int            // completion tokens consumed
	Diagnostic      string         // detailed debug info (tool resolution, status, errors)
}

// AgentRunner is a stateless wrapper around a SubagentSpawner.
type AgentRunner struct {
	spawner SubagentSpawner
}

// NewRunner creates an AgentRunner backed by the given spawner.
func NewRunner(spawner SubagentSpawner) *AgentRunner {
	return &AgentRunner{spawner: spawner}
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
func (ar *AgentRunner) RunWithContext(ctx context.Context, prompt, workdir, tools string, timeout time.Duration, onProgress SubagentProgress, model ...string) *RunResult {
	start := time.Now()

	toolNames := parseToolList(tools)
	req := SubagentRequest{
		Task:       prompt,
		Role:       "worker",
		Tools:      toolNames,
		Workdir:    workdir,
		Timeout:    timeout,
		MaxIters:   80,
		MaxCalls:   200,
		OnProgress: onProgress,
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
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
		}
	}

	return &RunResult{
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

// Run is a convenience wrapper for RunWithContext with a background context.
func (ar *AgentRunner) Run(prompt, workdir, tools string, timeout time.Duration, model ...string) *RunResult {
	return ar.RunWithContext(context.Background(), prompt, workdir, tools, timeout, nil, model...)
}

// RunVerifier is a convenience wrapper for running a verifier subagent
// with stricter limits (shorter timeout, read-only tools).
func (ar *AgentRunner) RunVerifier(prompt, workdir string, timeout time.Duration, model ...string) *RunResult {
	toolNames := ProfileToToolNames(ProfileReadOnly)
	req := SubagentRequest{
		Task:     prompt,
		Role:     "verifier",
		Tools:    toolNames,
		Workdir:  workdir,
		Timeout:  timeout,
		MaxIters: 10,
		MaxCalls: 30,
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
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("verifier subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
		}
	}

	return &RunResult{
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

// RunDecomposer runs a Leader/Planner subagent to decompose a goal into subtasks.
//
// For reasoning models (deepseek-v4-pro, etc.) the decomposer gets a larger
// token budget (ReasoningDecomposerMaxTokens) because the planning prompt
// is significantly longer than a typical task prompt and the model's
// chain-of-thought can consume 60-80% of the completion budget.
func (ar *AgentRunner) RunDecomposer(prompt, workdir string, timeout time.Duration, model ...string) *RunResult {
	toolNames := ProfileToToolNames(ProfileReadOnly)
	req := SubagentRequest{
		Task:     prompt,
		Role:     "planner",
		Tools:    toolNames,
		Workdir:  workdir,
		Timeout:  timeout,
		MaxIters: 15,
		MaxCalls: 40,
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":              map[string]any{"type": "string"},
							"description":        map[string]any{"type": "string"},
							"role":               map[string]any{"type": "string"},
							"batch_id":           map[string]any{"type": "string"},
							"batch_label":        map[string]any{"type": "string"},
							"depends_on_batch":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"depends_on_index":   map[string]any{"type": "integer"},
							"verifier_focus":     map[string]any{"type": "string"},
							"use_dw":             map[string]any{"type": "boolean"},
							"max_cycles":         map[string]any{"type": "integer"},
						},
						"required": []string{"title", "description", "role"},
					},
				},
			},
			"required": []string{"tasks"},
		},
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}
	if !supportsStructuredOutput(req.Model) {
		// Fall back to text JSON parsing for models that don't support
		// the structured_output tool (e.g. DeepSeek).
		req.OutputSchema = nil
	}
	// chain-of-thought doesn't starve the JSON plan output.
	req.MaxTokens = effectiveMaxTokens(0, req.Model)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	resp, err := ar.spawner.SpawnSubagent(ctx, req)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		return &RunResult{
			ExitCode:        -1,
			Stdout:          "",
			Stderr:          fmt.Sprintf("decomposer subagent error: %v", err),
			DurationSeconds: round(elapsed, 2),
			Success:         false,
		}
	}

	return &RunResult{
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
