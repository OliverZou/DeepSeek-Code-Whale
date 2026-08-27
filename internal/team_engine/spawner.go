package team_engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	teampglog "github.com/usewhale/whale/internal/team_engine/log"
	"github.com/usewhale/whale/internal/telemetry"
)

// defaultTeamLog is the internal logger.  Set via SetLogger.
// When nil, all Log calls are no-ops.
var defaultTeamLog *teampglog.TeamLog

// SetLogger replaces the engine logger.  Pass nil to disable.
func SetLogger(tl *teampglog.TeamLog) {
	if defaultTeamLog != nil {
		_ = defaultTeamLog.Close()
	}
	defaultTeamLog = tl
}

// CloseLogger closes and clears the engine logger.  Call it when the owning
// app/process is shutting down so the open team_engine.log file handle is
// released — Windows locks open files, so tests that build an App inside a
// temp dir must close it or t.TempDir cleanup fails.
func CloseLogger() {
	if defaultTeamLog != nil {
		_ = defaultTeamLog.Close()
		defaultTeamLog = nil
	}
}

// Log writes a diagnostic entry.  Safe when no logger is set.
func Log(cat, format string, args ...interface{}) {
	if defaultTeamLog != nil {
		defaultTeamLog.Log(cat, format, args...)
	}
}

// AddLogWriter adds an auxiliary log file (e.g. workspace log).
func AddLogWriter(path string) {
	if defaultTeamLog != nil {
		defaultTeamLog.AddLog(path)
	}
}

// --- Typed log wrappers (previously exposed via DefaultTeamLog) ---

func LogSpawnerType(role, kind, model string, maxTokens int) {
	if defaultTeamLog != nil {
		defaultTeamLog.SpawnerType(role, kind, model, maxTokens)
	}
}
func LeaderDecompose(goal, model string, attempt, maxTokens, promptTok, compTok int, dur float64, outputLen int, truncated, success bool) {
	if defaultTeamLog != nil {
		defaultTeamLog.LeaderDecompose(goal, model, attempt, maxTokens, promptTok, compTok, dur, outputLen, truncated, success)
	}
}
func LeaderRetry(attempt int, reason string) {
	if defaultTeamLog != nil {
		defaultTeamLog.LeaderRetry(attempt, reason)
	}
}
func WorkerStart(taskID, role, model string, attempt, maxRetries int) {
	if defaultTeamLog != nil {
		defaultTeamLog.WorkerStart(taskID, role, model, attempt, maxRetries)
	}
}
func WorkerDone(taskID string, dur float64, exitCode int, outputLen int, success bool) {
	if defaultTeamLog != nil {
		defaultTeamLog.WorkerDone(taskID, dur, exitCode, outputLen, success)
	}
}
func WorkerRetry(taskID string, attempt int, feedback string) {
	if defaultTeamLog != nil {
		defaultTeamLog.WorkerRetry(taskID, attempt, feedback)
	}
}
func VerifierStart(taskID string) {
	if defaultTeamLog != nil {
		defaultTeamLog.VerifierStart(taskID)
	}
}
func VerifierDone(taskID string, passed bool, dur float64) {
	if defaultTeamLog != nil {
		defaultTeamLog.VerifierDone(taskID, passed, dur)
	}
}
func BatchStart(batchID, label string, taskCount, cycle, maxCycles int) {
	if defaultTeamLog != nil {
		defaultTeamLog.BatchStart(batchID, label, taskCount, cycle, maxCycles)
	}
}
func BatchDone(batchID string, status string, dur float64) {
	if defaultTeamLog != nil {
		defaultTeamLog.BatchDone(batchID, status, dur)
	}
}
func BatchCycleReport(batchID string, cycle int, decision string) {
	if defaultTeamLog != nil {
		defaultTeamLog.BatchCycleReport(batchID, cycle, decision)
	}
}
func EngineResume(masterTaskID, goal string, batchCount int, err error) {
	if defaultTeamLog != nil {
		defaultTeamLog.EngineResume(masterTaskID, goal, batchCount, err)
	}
}
func EngineAutoResume(masterTaskID string, err error) {
	if defaultTeamLog != nil {
		defaultTeamLog.EngineAutoResume(masterTaskID, err)
	}
}
func EngineResumeTask(taskID, newState string) {
	if defaultTeamLog != nil {
		defaultTeamLog.EngineResumeTask(taskID, newState)
	}
}
func CLIHeartbeat(wsID string, registered bool) {
	if defaultTeamLog != nil {
		defaultTeamLog.CLIHeartbeat(wsID, registered)
	}
}
func CLIWSConnect(wsID string, err error) {
	if defaultTeamLog != nil {
		defaultTeamLog.CLIWSConnect(wsID, err)
	}
}
func CLIWSDisconnect(wsID string) {
	if defaultTeamLog != nil {
		defaultTeamLog.CLIWSDisconnect(wsID)
	}
}
func CLIReceiveResume(masterTaskID string) {
	if defaultTeamLog != nil {
		defaultTeamLog.CLIReceiveResume(masterTaskID)
	}
}
func DashboardRegister(path, wsID string, err error) {
	if defaultTeamLog != nil {
		defaultTeamLog.DashboardRegister(path, wsID, err)
	}
}
func DashboardWSConnect(wsID string, ok bool, err error) {
	if defaultTeamLog != nil {
		defaultTeamLog.DashboardWSConnect(wsID, ok, err)
	}
}
func DashboardWSDisconnect(wsID string) {
	if defaultTeamLog != nil {
		defaultTeamLog.DashboardWSDisconnect(wsID)
	}
}
func DashboardQueueResume(wsID, taskID, method string) {
	if defaultTeamLog != nil {
		defaultTeamLog.DashboardQueueResume(wsID, taskID, method)
	}
}
func DashboardStateTransition(taskID, from, to, reason string) {
	if defaultTeamLog != nil {
		defaultTeamLog.DashboardStateTransition(taskID, from, to, reason)
	}
}
func DashboardResumeMaster(wsID, taskID string, err error) {
	if defaultTeamLog != nil {
		defaultTeamLog.DashboardResumeMaster(wsID, taskID, err)
	}
}

// defaultSpawnFunc is a package-level fallback spawner, set by the toolset
// when the app wires in the native subagent adapter.  When non-nil, team
// engine instances prefer it over ShellSubagentSpawner.
var (
	defaultSpawnFunc   SpawnFunc
	defaultSpawnFuncMu sync.RWMutex
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

// ---------------------------------------------------------------------------
// Single-run engine ownership
//
// 一次 team 任务全程只能有一个 TeamEngine。CLI/App 入口创建自己的引擎后
// 注册为「活动执行引擎」;team_run_plan 等工具不再每次重建实例,而是复用
// 注册的引擎——否则工具路径的引擎会丢失发起方配置(lite spawner、team、
// 计数器),v13/v14 实测:batch 执行引擎没有 lite → tool-cap 重分解走回
// 带工具的 subagent 路径;执行实例与发起实例各记各的 token,run_report
// 只有发起侧的一小部分。
// ---------------------------------------------------------------------------

var (
	defaultRunEngine   *TeamEngine
	defaultRunEngineMu sync.RWMutex
)

// DefaultRunEngine returns the package-level active run engine, or nil when
// none is registered (standalone tool paths fall back to per-call engines).
func DefaultRunEngine() *TeamEngine {
	defaultRunEngineMu.RLock()
	defer defaultRunEngineMu.RUnlock()
	return defaultRunEngine
}

// SetDefaultRunEngine registers the run engine owned by the current
// CLI/App session. The caller keeps ownership of its lifecycle (Close);
// tool paths must NOT close a registered engine.
func SetDefaultRunEngine(e *TeamEngine) {
	defaultRunEngineMu.Lock()
	defer defaultRunEngineMu.Unlock()
	defaultRunEngine = e
}

// ClearDefaultRunEngine unregisters the run engine (e.g. on session close).
func ClearDefaultRunEngine() {
	defaultRunEngineMu.Lock()
	defer defaultRunEngineMu.Unlock()
	defaultRunEngine = nil
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

// shellSessionID generates a synthetic session identifier for shell-spawned
// subprocesses.  Format: "shell-<pid>-<nanotime>".
func shellSessionID(pid int) string {
	return fmt.Sprintf("shell-%d-%d", pid, time.Now().UnixNano())
}

// newShellSessionID generates a synthetic session identifier before the
// subprocess is started (so it can be passed via WHALE_SESSION_ID).  It uses
// the parent pid for stability and nanotime for uniqueness.
func newShellSessionID() string {
	return fmt.Sprintf("shell-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// readShellUsage reads the accumulated prompt/completion tokens for a
// shell-spawned subprocess session.  The child `whale exec` process inherits
// WHALE_SESSION_ID and writes its per-turn usage to
// <usageDir>/<sid>.jsonl; read that file back so the parent reports real
// token usage instead of zeros.  Returns zeros when no usage is found.
func readShellUsage(sid string) (prompt, completion, hit, miss int) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return 0, 0, 0, 0
	}
	f, err := os.Open(filepath.Join(telemetry.DefaultUsageLogDir(), sid+".jsonl"))
	if err != nil {
		return 0, 0, 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec telemetry.UsageRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		prompt += rec.PromptTokens
		completion += rec.CompletionTokens
		hit += rec.PromptCacheHit
		miss += rec.PromptCacheMiss
	}
	return prompt, completion, hit, miss
}

func (s *ShellSubagentSpawner) SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
	whaleBin := s.whaleBin
	if whaleBin == "" {
		// Use the current executable so subprocesses get the same binary
		// (including any local fixes), not whatever is in PATH.
		if exe, err := os.Executable(); err == nil {
			whaleBin = exe
		} else {
			whaleBin = "whale"
		}
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

	cwd := req.Workdir
	if cwd == "" {
		cwd = "."
	}

	sid := newShellSessionID()
	cmd := exec.CommandContext(ctx, whaleBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("WHALE_MAX_TOKENS=%d", req.MaxTokens),
		fmt.Sprintf("WHALE_SESSION_ID=%s", sid),
	)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return SubagentResponse{SessionID: sid, SpawnerType: "shell", Diagnostic: "stdin pipe error", ExitCode: -1, Success: false}, nil
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return SubagentResponse{SessionID: sid, SpawnerType: "shell", Diagnostic: fmt.Sprintf("start error: %v", err), ExitCode: -1, Success: false}, nil
	}
	pid := cmd.Process.Pid
	if req.OnPID != nil {
		req.OnPID(pid)
	}

	// Write prompt to stdin in a background goroutine so the main
	// goroutine is never blocked on the pipe buffer.  A large
	// decompose prompt (>4 KB) will fill the OS pipe buffer if the
	// subprocess hasn't started reading yet (e.g. it is still
	// initializing the LLM provider), creating a circular wait:
	// parent blocks on Write → context timeout unreachable → both
	// sides stuck.  Writing in a goroutine breaks the circle:
	// the select below can always respond to ctx cancellation and
	// kill the subprocess, which unblocks the write via broken pipe.
	//
	// The OnStdin callback is still called so the engine can track
	// the pipe for external kill signals.  Stdin is always closed
	// after writing — ShellSubagentSpawner does not support
	// mid-execution interactive input.
	go func() {
		stdinPipe.Write([]byte(prompt))
		if req.OnStdin != nil {
			req.OnStdin(stdinPipe)
		}
		stdinPipe.Close()
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		exitCode := 0
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		usagePrompt, usageCompletion, usageHit, usageMiss := readShellUsage(sid)
		return SubagentResponse{SessionID: sid, SpawnerType: "shell", Output: stdout.String(), Diagnostic: stderr.String(), ExitCode: exitCode, Success: waitErr == nil, PID: pid, UsagePrompt: usagePrompt, UsageCompletion: usageCompletion, UsagePromptCacheHit: usageHit, UsagePromptCacheMiss: usageMiss}, nil

	case <-ctx.Done():
		cmd.Process.Kill()
		return SubagentResponse{SessionID: sid, Output: stdout.String(), Diagnostic: stderr.String(), ExitCode: -2, Success: false, PID: pid}, ctx.Err()
	}
}

var _ SubagentSpawner = (*ShellSubagentSpawner)(nil)

// ---------------------------------------------------------------------------
// Subprocess helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Env helpers
// ---------------------------------------------------------------------------

func ToolConfigFromEnv() (dbPath, whiteboardDir, configPath, workdir string) {
	dbPath = envOrDefault("WHALE_TEAM_DB", ".whale/team_engine.db") // deprecated: file-based state, db no longer used
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
