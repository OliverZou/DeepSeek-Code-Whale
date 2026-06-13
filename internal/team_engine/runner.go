package team_engine

import (
	"context"
	"fmt"
	"time"
)

// RunResult captures the outcome of a single agent run.
type RunResult struct {
	ExitCode        int     `json:"exit_code"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	DurationSeconds float64 `json:"duration_seconds"`
	Success         bool    `json:"success"`
}

// SubagentSpawner is the interface that the Whale integration layer must
// satisfy.  It replaces the original runner's external CLI calls with
// Whale's native subagent spawning mechanism.
//
// The integration layer (internal/tools/team_engine.go or
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
	Task       string            // The prompt/description for the agent
	Role       string            // Role name (e.g. "developer", "verifier")
	Model      string            // LLM model name; "" = Whale default
	Tools      []string          // Allowed tool names
	Workdir    string            // Working directory
	Timeout    time.Duration
	MaxIters   int
	MaxCalls   int
	MaxTokens  int               // Completion token budget (0 = runner default)
	OnProgress SubagentProgress  // Real-time progress callback (nil = no streaming)
}

// SubagentResponse contains the result of a subagent execution.
type SubagentResponse struct {
	Output   string // Full agent output
	ExitCode int
	Success  bool
}

// AgentRunner executes prompts through a Whale subagent.
//
// Unlike the original team-engine-go which called external agent CLIs
// (claude --print, opencode run, codex exec), this version calls Whale's
// internal subagent spawning mechanism, giving full control over tool
// permissions and execution context.
type AgentRunner struct {
	spawner SubagentSpawner
}

// NewRunner creates an AgentRunner backed by the given SubagentSpawner.
func NewRunner(spawner SubagentSpawner) *AgentRunner {
	return &AgentRunner{spawner: spawner}
}

// Run executes a prompt through a Whale subagent.
//
// Parameters:
//   - prompt:    The task description/prompt for the agent
//   - workdir:   Working directory (absolute or relative)
//   - tools:     Comma-separated list of allowed tools (e.g. "Read,Write,Bash")
//   - timeout:   Maximum execution duration
//
// Returns a RunResult with the agent's output.
func (ar *AgentRunner) Run(prompt, workdir, tools string, timeout time.Duration, onProgress SubagentProgress) *RunResult {
	return ar.RunWithContext(context.Background(), prompt, workdir, tools, timeout, onProgress)
}

// RunWithContext is like Run but accepts an external context for cancellation.
// The effective timeout is min(timeout, ctx deadline). If ctx is cancelled
// (e.g. via Close()), the spawn is aborted.
func (ar *AgentRunner) RunWithContext(ctx context.Context, prompt, workdir, tools string, timeout time.Duration, onProgress SubagentProgress, model ...string) *RunResult {
	start := time.Now()

	toolNames := parseToolList(tools)
	req := SubagentRequest{
		Task:       prompt,
		Role:       "worker",
		Tools:      toolNames,
		Workdir:    workdir,
		Timeout:    timeout,
		MaxIters:   30,
		MaxCalls:   100,
		OnProgress: onProgress,
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := ar.spawner.SpawnSubagent(runCtx, req)

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
		Stderr:          "",
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
	}
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
		Stderr:          "",
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
	}
}

// RunDecomposer is a convenience wrapper for running a leader/decomposer
// subagent that produces a structured JSON plan.
func (ar *AgentRunner) RunDecomposer(prompt, workdir string, timeout time.Duration, model ...string) *RunResult {
	toolNames := ProfileToToolNames(ProfileReadOnly)
	req := SubagentRequest{
		Task:      prompt,
		Role:      "planner",
		Tools:     toolNames,
		Workdir:   workdir,
		Timeout:   timeout,
		MaxIters:  15,
		MaxCalls:  40,
		MaxTokens: 4096, // reasoning models need headroom (default 800 too small)
	}
	if len(model) > 0 && model[0] != "" {
		req.Model = model[0]
	}

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
		Stderr:          "",
		DurationSeconds: round(elapsed, 2),
		Success:         resp.Success,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseToolList parses a comma-separated tool string, mapping logical names
// to actual Whale tool names via ProfileToToolNames.
func parseToolList(tools string) []string {
	if tools == "" {
		return ProfileToToolNames(ProfileDefault)
	}

	// Check if it's a known profile name.
	switch tools {
	case "default":
		return ProfileToToolNames(ProfileDefault)
	case "read_only":
		return ProfileToToolNames(ProfileReadOnly)
	case "research":
		return ProfileToToolNames(ProfileResearch)
	case "content":
		return ProfileToToolNames(ProfileContent)
	case "test":
		return ProfileToToolNames(ProfileTest)
	case "verify":
		return ProfileToToolNames(ProfileVerify)
	}

	// Otherwise, treat as comma-separated list of Whale tool names.
	var result []string
	for _, t := range splitAndTrim(tools, ",") {
		if t != "" {
			result = append(result, t)
		}
	}
	return result
}

func splitAndTrim(s, sep string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, part := range split(s, sep) {
		trimmed := stringsTrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// split is a simple strings.Split that doesn't require importing strings
// (available via built-in in Go 1.21+, but we keep it simple).
func split(s, sep string) []string {
	if s == "" {
		return nil
	}
	var result []string
	// Simple n-1 split
	for {
		i := indexOf(s, sep)
		if i < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:i])
		s = s[i+len(sep):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func stringsTrimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func round(val float64, precision int) float64 {
	format := fmt.Sprintf("%%.%df", precision)
	result := fmt.Sprintf(format, val)
	var rounded float64
	fmt.Sscanf(result, "%f", &rounded)
	return rounded
}
