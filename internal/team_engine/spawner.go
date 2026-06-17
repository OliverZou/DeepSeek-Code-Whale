package team_engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	teampglog "github.com/usewhale/whale/internal/team_engine/log"
)

// DefaultTeamLog is the package-level team-log.  Set via SetDefaultTeamLog
// during process startup.  When nil (default or no build tag), all calls
// are no-ops.
var DefaultTeamLog *teampglog.TeamLog

func SetDefaultTeamLog(tl *teampglog.TeamLog) { DefaultTeamLog = tl }

// Log writes a diagnostic entry.  Safe when DefaultTeamLog is nil.
func Log(cat, format string, args ...interface{}) {
	if DefaultTeamLog != nil {
		DefaultTeamLog.Log(cat, format, args...)
	}
}

// defaultSpawnFunc is a package-level fallback spawner, set by the toolset
// when the app wires in the native subagent adapter.  When non-nil, team
// engine instances prefer it over ShellSubagentSpawner.
var (
	defaultSpawnFunc      SpawnFunc
	defaultSpawnFuncMu    sync.RWMutex
)

// DefaultSpawnFunc returns the package-level default spawn function, or nil
// if the app has not yet wired in a native subagent adapter.
func DefaultSpawnFunc() SpawnFunc {
	defaultSpawnFuncMu.RLock()
	defer defaultSpawnFuncMu.RUnlock()
	return defaultSpawnFunc
}

// SetDefaultSpawnFunc sets the package-level default spawn function.
// Called by the toolset when the app wires in the native subagent adapter.
// Safe for concurrent use.
func SetDefaultSpawnFunc(fn SpawnFunc) {
	defaultSpawnFuncMu.Lock()
	defer defaultSpawnFuncMu.Unlock()
	defaultSpawnFunc = fn
}

// =============================================================================
// Context Isolation Principle
//
// Every call to SpawnSubagent creates a BRAND NEW subagent session with:
//   - Its own isolated LLM context (only the `Task` prompt is passed in)
//   - Its own tool registry (restricted by ToolProfile / role)
//   - Its own agent loop (no shared conversation history)
//   - Its own workspace (optional git worktree isolation)
//
// This enforces the MiniMax Agent Team design principle that each role
// (Leader, Worker, Verifier) operates in an isolated context:
//
//   Leader    → gets the goal           → outputs structured plan (JSON)
//   Worker    → gets ONE subtask        → outputs work product
//   Verifier  → gets task + output only → outputs PASS/FAIL verdict
//
// The Worker NEVER sees the Verifier's critique. The Verifier NEVER sees
// the Worker's internal reasoning — only the final output. This adversarial
// isolation is what prevents context pollution and drives quality.
//
// Two spawner implementations are provided:
//   - FuncSpawner: wraps a function callback; use with tasks.Runner's
//     SpawnSubagentWithProgress for full Whale runtime integration.
//   - ShellSubagentSpawner: process-level fallback via exec.Command.
//     Prefer FuncSpawner in production.
// =============================================================================

// ---------------------------------------------------------------------------
// FuncSpawner — wraps a function callback as SubagentSpawner
// ---------------------------------------------------------------------------

// SpawnFunc is the callback signature used by FuncSpawner.
// Implementations should call tasks.Runner.SpawnSubagentWithProgress or an
// equivalent Whale-native subagent mechanism for full context isolation.
type SpawnFunc func(ctx context.Context, req SubagentRequest) (SubagentResponse, error)

// FuncSpawner wraps a SpawnFunc as a SubagentSpawner.
// Use this to integrate the Team Engine with Whale's native subagent runtime
// instead of shelling out to the CLI.
type FuncSpawner struct {
	fn SpawnFunc
}

// NewFuncSpawner creates a FuncSpawner from the given callback.
func NewFuncSpawner(fn SpawnFunc) *FuncSpawner {
	return &FuncSpawner{fn: fn}
}

// SpawnSubagent implements SubagentSpawner by delegating to the wrapped function.
func (s *FuncSpawner) SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
	return s.fn(ctx, req)
}

// ---------------------------------------------------------------------------
// ShellSubagentSpawner — standalone / CLI mode (fallback)
// ---------------------------------------------------------------------------

// ShellSubagentSpawner implements SubagentSpawner by calling `whale exec`
// as a subprocess.  Each exec call is a fully isolated process with its
// own LLM context — no context leaks between calls.
//
// Limitations:
//   - No streaming (blocks until process exits)
//   - No structured output (must regex-parse stdout)
//   - No shared provider connection (cold start each time)
//   - No unified permission/audit (child runs with --dangerously-skip-permissions)
type ShellSubagentSpawner struct {
	whaleBin string
}

func NewShellSubagentSpawner() *ShellSubagentSpawner {
	return &ShellSubagentSpawner{}
}

func (s *ShellSubagentSpawner) SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
	whaleBin := s.whaleBin
	if whaleBin == "" {
		whaleBin = "whale"
	}

	args := []string{
		"exec",
		"--dangerously-skip-permissions",
		"--timeout-sec", fmt.Sprintf("%d", int(req.Timeout.Seconds())),
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	prompt := req.Task
	if req.Role != "" {
		prompt = fmt.Sprintf("[Role: %s]\n\n%s", req.Role, prompt)
	}
	args = append(args, prompt)

	cwd := req.Workdir
	if cwd == "" {
		cwd = "."
	}

	cmd := exec.CommandContext(ctx, whaleBin, args...)
	cmd.Dir = cwd

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	select {
	case err := <-done:
		if err != nil {
			exitCode := -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			return SubagentResponse{
				SpawnerType: "shell",
				Output:   stdout.String(),
				ExitCode: exitCode,
				Success:  false,
			}, nil
		}
		return SubagentResponse{
			SpawnerType: "shell",
			Output:   stdout.String(),
			ExitCode: 0,
			Success:  true,
		}, nil

	case <-ctx.Done():
		cmd.Process.Kill()
		return SubagentResponse{
			Output:   stdout.String(),
			ExitCode: -2,
			Success:  false,
		}, ctx.Err()
	}
}

var _ SubagentSpawner = (*ShellSubagentSpawner)(nil)

// ---------------------------------------------------------------------------
// WhaleNativeSpawner — deep integration mode
// ---------------------------------------------------------------------------

// SpawnSubagentFunc is the function signature for spawning a subagent.
// This allows injection of tasks.Runner.SpawnSubagentWithProgress or a
// test mock without hard-coding the dependency.
type SpawnSubagentFunc func(ctx context.Context, task string, opts SpawnOptions) (SpawnResult, error)

// SpawnOptions carries all knobs for a subagent spawn call.
type SpawnOptions struct {
	Role           string            // Agent role label
	Tools          []string          // Allowed tool names
	Workdir        string            // Working directory
	MaxIters       int               // Max tool iterations
	MaxCalls       int               // Max tool calls
	OutputSchema   map[string]any    // Optional structured output schema
	PermissionMode string            // "auto" | "ask" | "trusted"
	OnProgress     SubagentProgress  // Real-time progress callback
}

// SpawnResult is the structured result from a subagent spawn.
type SpawnResult struct {
	Output          string // Full agent output text
	Structured      map[string]any // Structured output (if OutputSchema was set)
	Success         bool
	ToolCalls       []string // Tools that were actually called
	TokenUsage      any      // LLM token usage info
}

// WhaleNativeSpawner implements SubagentSpawner via Whale's native
// tasks.Runner.SpawnSubagentWithProgress.  Each spawn is a fully isolated
// child session with its own LLM context, tool registry, and agent loop.
//
// Key properties:
//   - ✅ Context isolation: each spawn = fresh subagent session
//   - ✅ Streaming: progress callback receives real-time tool updates
//   - ✅ Structured output: parsed JSON returned directly (no regex)
//   - ✅ Shared provider: reuses Whale's LLM connection (warm cache)
//   - ✅ Unified permissions: child inherits parent's policy
//   - ✅ Audit: all tool calls logged to parent session
type WhaleNativeSpawner struct {
	spawn SpawnSubagentFunc
}

// NewWhaleNativeSpawner creates a WhaleNativeSpawner backed by the given
// spawn function.  Pass tasks.Runner.SpawnSubagentWithProgress adapted
// through the adapter:
//
//	runner := /* get from app init */
//	spawner := team_engine.NewWhaleNativeSpawner(
//	    team_engine.WhaleSpawnAdapter(runner),
//	)
func NewWhaleNativeSpawner(spawn SpawnSubagentFunc) *WhaleNativeSpawner {
	return &WhaleNativeSpawner{spawn: spawn}
}

func (s *WhaleNativeSpawner) SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
	opts := SpawnOptions{
		Role:           req.Role,
		Tools:          req.Tools,
		Workdir:        req.Workdir,
		MaxIters:       req.MaxIters,
		MaxCalls:       req.MaxCalls,
		OnProgress:     req.OnProgress,
		PermissionMode: "trusted", // workspace dir is trusted — never prompt
	}

	result, err := s.spawn(ctx, req.Task, opts)
	if err != nil {
		return SubagentResponse{
			Output:   result.Output,
			ExitCode: -1,
			Success:  false,
		}, nil
	}

	return SubagentResponse{
		Output:   result.Output,
		ExitCode: 0,
		Success:  result.Success,
	}, nil
}

var _ SubagentSpawner = (*WhaleNativeSpawner)(nil)

// ---------------------------------------------------------------------------
// WhaleSpawnAdapter — adapts tasks.Runner to SpawnSubagentFunc
// ---------------------------------------------------------------------------

// RunnerSpawner is the minimal interface we need from tasks.Runner.
// This avoids a hard import of the internal/tasks package.
type RunnerSpawner interface {
	// SpawnSubagentWithProgress spawns a child agent session with full
	// context isolation.  The `progress` callback receives real-time
	// tool execution events.
	SpawnSubagentWithProgress(ctx context.Context, task string, tools []string, maxIters, maxCalls int, outputSchema map[string]any, progress func(status, summary, toolName string)) (string, map[string]any, error)
}

// NewWhaleSpawnAdapter creates a SpawnSubagentFunc from a RunnerSpawner.
//
// Usage:
//
//	import "github.com/usewhale/whale/internal/tasks"
//
//	runner := /* get from app init */
//	spawner := team_engine.NewWhaleNativeSpawner(
//	    team_engine.NewWhaleSpawnAdapter(runner),
//	)
func NewWhaleSpawnAdapter(runner RunnerSpawner) SpawnSubagentFunc {
	return func(ctx context.Context, task string, opts SpawnOptions) (SpawnResult, error) {
		// Adapt SubagentProgress → function signature for the runner.
		var progress func(string, string, string)
		if opts.OnProgress != nil {
			progress = func(status, summary, toolName string) {
				opts.OnProgress(status, summary, toolName)
			}
		}

		output, structured, err := runner.SpawnSubagentWithProgress(
			ctx,
			task,
			opts.Tools,
			opts.MaxIters,
			opts.MaxCalls,
			opts.OutputSchema,
			progress,
		)
		if err != nil {
			return SpawnResult{
				Output:     output,
				Structured: structured,
				Success:    false,
			}, err
		}
		return SpawnResult{
			Output:     output,
			Structured: structured,
			Success:    true,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// Env helpers
// ---------------------------------------------------------------------------

func ToolConfigFromEnv() (dbPath, whiteboardDir, configPath, workdir string) {
	dbPath = envOrDefault("WHALE_TEAM_DB", ".whale/team_engine.db")
	whiteboardDir = envOrDefault("WHALE_TEAM_WHITEBOARD", ".whale/team_tasks")
	configPath = envOrDefault("WHALE_TEAM_CONFIG", "")
	workdir = envOrDefault("WHALE_TEAM_WORKDIR", ".")
	return
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
