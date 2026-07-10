package team_engine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	teampglog "team-engine/log"
)

// defaultTeamLog is the internal logger.  Set via SetLogger.
// When nil, all Log calls are no-ops.
var defaultTeamLog *teampglog.TeamLog

// SetLogger replaces the engine logger.  Pass nil to disable.
func SetLogger(tl *teampglog.TeamLog) { defaultTeamLog = tl }

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

	cmd := exec.CommandContext(ctx, whaleBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"WHALE_NO_DASHBOARD=1",
		fmt.Sprintf("WHALE_MAX_TOKENS=%d", req.MaxTokens),
	)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return SubagentResponse{SessionID: shellSessionID(0), SpawnerType: "shell", Diagnostic: "stdin pipe error", ExitCode: -1, Success: false}, nil
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return SubagentResponse{SessionID: shellSessionID(0), SpawnerType: "shell", Diagnostic: fmt.Sprintf("start error: %v", err), ExitCode: -1, Success: false}, nil
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
			return SubagentResponse{SessionID: shellSessionID(pid), SpawnerType: "shell", Output: stdout.String(), Diagnostic: stderr.String(), ExitCode: exitCode, Success: false, PID: pid}, nil
		}
		return SubagentResponse{SessionID: shellSessionID(pid), SpawnerType: "shell", Output: stdout.String(), Diagnostic: stderr.String(), ExitCode: 0, Success: true, PID: pid}, nil

	case <-ctx.Done():
		cmd.Process.Kill()
		return SubagentResponse{SessionID: shellSessionID(pid), Output: stdout.String(), Diagnostic: stderr.String(), ExitCode: -2, Success: false, PID: pid}, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// PersistentSession — long-lived subprocess for worker/verifier retries
// ---------------------------------------------------------------------------

const (
	whaleEOP = "\n__WHALE_EOP__\n"
	whaleEOT = "\n__WHALE_EOT__\n"
)

// PersistentSession wraps a long-running whale exec --persist subprocess.
// The parent writes prompts delimited by __WHALE_EOP__ and reads responses
// delimited by __WHALE_EOT__.  The session is reused across retries instead
// of spawning a new process each time.
type PersistentSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	// stdoutBuf wraps cmd's stdout pipe for line-by-line scanning.
	stdoutBuf *bufio.Scanner

	// stderr accumulates stderr output.
	stderr *bytes.Buffer

	pid int
	mu  sync.Mutex // serialises writes to stdin
}

// SpawnPersistent starts a whale exec --persist subprocess, writes the
// initial prompt, and reads the first response.  The session remains
// alive after this call returns — use ContinueSession for retries and
// CloseSession when the task is done.
func (s *ShellSubagentSpawner) SpawnPersistent(ctx context.Context, req SubagentRequest) (*PersistentSession, *SubagentResponse) {
	whaleBin := s.whaleBin
	if whaleBin == "" {
		if exe, err := os.Executable(); err == nil {
			whaleBin = exe
		} else {
			whaleBin = "whale"
		}
	}

	args := []string{
		"exec",
		"--dangerously-skip-permissions",
		"--persist",
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

	cmd := exec.CommandContext(context.Background(), whaleBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"WHALE_NO_DASHBOARD=1",
		fmt.Sprintf("WHALE_MAX_TOKENS=%d", req.MaxTokens),
	)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		r := SubagentResponse{SessionID: shellSessionID(0), SpawnerType: "shell", ExitCode: -1, Success: false}
		return nil, &r
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		r := SubagentResponse{SessionID: shellSessionID(0), SpawnerType: "shell", ExitCode: -1, Success: false}
		return nil, &r
	}

	var sessionStderr bytes.Buffer
	cmd.Stderr = &sessionStderr

	if err := cmd.Start(); err != nil {
		r := SubagentResponse{SessionID: shellSessionID(0), SpawnerType: "shell", ExitCode: -1, Success: false}
		return nil, &r
	}

	session := &PersistentSession{
		cmd:       cmd,
		stdin:     stdinPipe,
		stdoutBuf: bufio.NewScanner(stdoutPipe),
		stderr:    &sessionStderr,
		pid:       cmd.Process.Pid,
	}
	// Large buffer — agent output can be large.
	session.stdoutBuf.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	if req.OnPID != nil {
		req.OnPID(session.pid)
	}

	// Write initial prompt + EOP.
	resp := session.sendAndReceive(prompt)
	return session, resp
}

// ContinueSession sends a follow-up prompt (typically checker/verifier
// feedback) to a persistent session and reads the response.
func (s *ShellSubagentSpawner) ContinueSession(session *PersistentSession, prompt string) *SubagentResponse {
	return session.sendAndReceive(prompt)
}

// CloseSession writes EOF to stdin, waits for graceful exit, then
// force-kills if the subprocess hasn't exited within 5 seconds.
func (s *ShellSubagentSpawner) CloseSession(session *PersistentSession) {
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.stdin != nil {
		session.stdin.Close()
		session.stdin = nil
	}
	session.mu.Unlock()

	// Wait with a timeout, then force-kill.
	done := make(chan struct{})
	go func() {
		session.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		session.cmd.Process.Kill()
	}
}

// sendAndReceive writes a prompt + EOP to stdin, then reads stdout until
// EOT.  Thread-safe via session.mu.
func (ps *PersistentSession) sendAndReceive(prompt string) *SubagentResponse {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	// Write prompt + EOP.
	_, err := io.WriteString(ps.stdin, prompt+whaleEOP)
	if err != nil {
		return &SubagentResponse{SessionID: shellSessionID(ps.pid), SpawnerType: "shell", ExitCode: -1, Success: false}
	}

	// Read until EOT.
	var lines []string
	for ps.stdoutBuf.Scan() {
		line := ps.stdoutBuf.Text()
		if strings.TrimSpace(line) == "__WHALE_EOT__" {
			break
		}
		lines = append(lines, line)
	}
	output := strings.Join(lines, "\n")

	// Re-generate session ID per call to distinguish retry attempts.
	return &SubagentResponse{
		SessionID:   shellSessionID(ps.pid),
		SpawnerType: "shell",
		Output:      output,
		ExitCode:    0,
		Success:     true, // files may have been produced without stdout
		PID:         ps.pid,
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
