//go:build teamlog

package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TeamLog writes structured lifecycle logs.  When the workspace root is
// provided, logs go to .whale/team_tasks/logs/team_engine.log.  An optional
// second file (e.g. dashboard-global log) can be added via AddLog.
type TeamLog struct {
	mu   sync.Mutex
	f    *os.File
	aux  *os.File // optional second log file
}

// NewTeamLog opens (or creates) the team_engine.log in the workspace.
func NewTeamLog(workspaceRoot string) *TeamLog {
	logDir := filepath.Join(workspaceRoot, ".whale", "team_tasks", "logs")
	os.MkdirAll(logDir, 0755)
	f, err := os.OpenFile(filepath.Join(logDir, "team_engine.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return &TeamLog{}
	}
	return &TeamLog{f: f}
}

// NewTeamLogAt creates a TeamLog at an explicit file path.
func NewTeamLogAt(path string) *TeamLog {
	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		// Fallback: use TEMP directory if the requested path isn't writable.
		fb, err2 := os.OpenFile(filepath.Join(os.TempDir(), filepath.Base(path)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err2 != nil {
			return &TeamLog{}
		}
		return &TeamLog{f: fb}
	}
	return &TeamLog{f: f}
}

// AddLog appends a second log file.  Writes go to both files.
func (t *TeamLog) AddLog(path string) {
	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		t.aux = f
	}
}

// Log writes an arbitrary diagnostic entry (for ad-hoc use).
func (t *TeamLog) Log(cat, format string, args ...interface{}) {
	t.write(cat, format, args...)
}

func (t *TeamLog) Close() error {
	if t.aux != nil {
		_ = t.aux.Close()
	}
	if t.f != nil {
		return t.f.Close()
	}
	return nil
}

func (t *TeamLog) write(cat, format string, args ...interface{}) {
	if t.f == nil && t.aux == nil {
		return
	}
	if !t.mu.TryLock() {
		return // avoid deadlock 鈥?drop log if mutex is contested
	}
	defer t.mu.Unlock()
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [%s] %s\n", ts, cat, msg)
	if t.f != nil {
		t.f.WriteString(line)
	}
	if t.aux != nil {
		t.aux.WriteString(line)
	}
}

// --- Dashboard ---

func (t *TeamLog) DashboardRegister(path, wsID string, err error) {
	if err != nil {
		t.write("dashboard", "register path=%s FAILED: %v", path, err)
	} else {
		t.write("dashboard", "register path=%s 鈫?%s", path, wsID)
	}
}
func (t *TeamLog) DashboardWSConnect(wsID string, ok bool, err error) {
	if err != nil {
		t.write("dashboard", "ws connect %s FAILED: %v", wsID, err)
	} else {
		t.write("dashboard", "ws connect %s", wsID)
	}
}
func (t *TeamLog) DashboardWSDisconnect(wsID string) {
	t.write("dashboard", "ws disconnect %s", wsID)
}
func (t *TeamLog) DashboardQueueResume(wsID, taskID, method string) {
	t.write("dashboard", "queue resume %s task=%s via=%s", wsID, taskID, method)
}
func (t *TeamLog) DashboardStateTransition(taskID, from, to, reason string) {
	t.write("dashboard", "state %s: %s 鈫?%s (%s)", taskID, from, to, reason)
}
func (t *TeamLog) DashboardResumeMaster(wsID, taskID string, err error) {
	if err != nil {
		t.write("dashboard", "resume master %s task=%s FAILED: %v", wsID, taskID, err)
	} else {
		t.write("dashboard", "resume master %s task=%s OK", wsID, taskID)
	}
}

// --- Spawner ---

func (t *TeamLog) SpawnerType(role, kind, model string, maxTokens int) {
	t.write("spawner", "role=%s type=%s model=%s maxTokens=%d", role, kind, model, maxTokens)
}

// --- Leader ---

func (t *TeamLog) LeaderDecompose(goal string, model string, attempt, maxTokens, promptTok, compTok int, dur float64, outputLen int, truncated, success bool) {
	t.write("leader", "decompose goal=%q model=%s attempt=%d maxTok=%d prompt=%d comp=%d dur=%.1fs out=%d chars truncated=%v success=%v",
		truncateGoal(goal), model, attempt, maxTokens, promptTok, compTok, dur, outputLen, truncated, success)
}
func (t *TeamLog) LeaderPlan(planTasks int, err error) {
	if err != nil {
		t.write("leader", "plan FAILED: %v", err)
	} else {
		t.write("leader", "plan ok: %d tasks", planTasks)
	}
}
func (t *TeamLog) LeaderRetry(attempt int, reason string) {
	t.write("leader", "retry attempt=%d: %s", attempt, reason)
}

// --- Worker ---

func (t *TeamLog) WorkerStart(taskID, role, model string, attempt, maxRetries int) {
	t.write("worker", "%s start role=%s model=%s attempt=%d/%d", taskID[:8], role, model, attempt, maxRetries)
}
func (t *TeamLog) WorkerDone(taskID string, dur float64, exitCode int, outputLen int, success bool) {
	t.write("worker", "%s done dur=%.1fs exit=%d out=%d chars success=%v", taskID[:8], dur, exitCode, outputLen, success)
}
func (t *TeamLog) WorkerRetry(taskID string, attempt int, feedback string) {
	t.write("worker", "%s retry attempt=%d feedback=%q", taskID[:8], attempt, truncateStr(feedback, 80))
}

// --- Verifier ---

func (t *TeamLog) VerifierStart(taskID string) {
	t.write("verifier", "%s start", taskID[:8])
}
func (t *TeamLog) VerifierDone(taskID string, passed bool, dur float64) {
	t.write("verifier", "%s done verdict=%s dur=%.1fs", taskID[:8], verdict(passed), dur)
}

// --- Batch ---

func (t *TeamLog) BatchStart(batchID, label string, taskCount, cycle, maxCycles int) {
	t.write("batch", "%s start label=%q tasks=%d cycle=%d/%d", batchID, label, taskCount, cycle, maxCycles)
}
func (t *TeamLog) BatchDone(batchID string, status string, dur float64) {
	t.write("batch", "%s done status=%s dur=%.1fs", batchID, status, dur)
}
func (t *TeamLog) BatchCycleReport(batchID string, cycle int, decision string) {
	t.write("batch", "%s cycle=%d decision=%s", batchID, cycle, decision)
}

// --- Engine ---

func (t *TeamLog) EngineResume(masterTaskID, goal string, batchCount int, err error) {
	if err != nil {
		t.write("engine", "resume %s FAILED: %v", masterTaskID[:8], err)
	} else {
		t.write("engine", "resume %s ok: goal=%q batches=%d", masterTaskID[:8], truncateGoal(goal), batchCount)
	}
}
func (t *TeamLog) EngineAutoResume(masterTaskID string, err error) {
	if err != nil {
		t.write("engine", "auto-resume %s FAILED: %v", masterTaskID[:8], err)
	} else {
		t.write("engine", "auto-resume %s started", masterTaskID[:8])
	}
}
func (t *TeamLog) EngineResumeTask(taskID, newState string) {
	t.write("engine", "resume-task %s 鈫?%s", taskID[:8], newState)
}

// --- CLI ---

func (t *TeamLog) CLIHeartbeat(wsID string, registered bool) {
	t.write("cli", "heartbeat wsID=%s registered=%v", wsID, registered)
}
func (t *TeamLog) CLIWSConnect(wsID string, err error) {
	if err != nil {
		t.write("cli", "ws connect %s FAILED: %v", wsID, err)
	} else {
		t.write("cli", "ws connect %s ok", wsID)
	}
}
func (t *TeamLog) CLIWSDisconnect(wsID string) {
	t.write("cli", "ws disconnect %s", wsID)
}
func (t *TeamLog) CLIReceiveResume(masterTaskID string) {
	t.write("cli", "receive resume %s", masterTaskID[:8])
}

// --- Helpers ---

func verdict(passed bool) string {
	if passed { return "PASS" }
	return "FAIL"
}
func truncateGoal(g string) string {
	if len(g) > 60 { return g[:60] + "鈥? }
	return g
}
func truncateStr(s string, n int) string {
	if len(s) > n { return s[:n] + "鈥? }
	return s
}
