// Package log provides structured logging for the team engine's agents
// (Leader, Worker, Verifier) and the engine itself.  Logs are written to
// the whiteboard directory under logs/ and separated by agent role and task.
//
// Directory layout:
//
//	team_tasks/logs/
//	├── leader/
//	│   ├── decompose_001.md
//	│   └── review_batch-1_001.md
//	├── tasks/<task_id>/
//	│   ├── worker_001.md
//	│   ├── verifier_001.md
//	│   └── engine.md
//	└── engine.log
package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Loggers is the central logging hub for a team engine session.
// It is safe for concurrent use.
type Loggers struct {
	baseDir string
	mu      sync.Mutex

	leaderSeq   int
	engineLog   *os.File
}

// New creates a Loggers rooted at whiteboardDir/logs.
func New(whiteboardDir string) (*Loggers, error) {
	baseDir := filepath.Join(whiteboardDir, "logs")
	if err := os.MkdirAll(filepath.Join(baseDir, "leader"), 0755); err != nil {
		return nil, fmt.Errorf("create leader log dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "tasks"), 0755); err != nil {
		return nil, fmt.Errorf("create tasks log dir: %w", err)
	}

	f, err := os.OpenFile(filepath.Join(baseDir, "engine.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open engine.log: %w", err)
	}

	return &Loggers{
		baseDir:  baseDir,
		engineLog: f,
	}, nil
}

// Close closes the engine log file.
func (l *Loggers) Close() error {
	if l.engineLog != nil {
		return l.engineLog.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Engine log
// ---------------------------------------------------------------------------

// Engine writes a line to the global engine.log with a timestamp.
func (l *Loggers) Engine(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.engineLog == nil {
		return
	}
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.engineLog, "[%s] %s\n", ts, msg)
		l.engineLog.Sync()
}

// ---------------------------------------------------------------------------
// Agent logs (Leader / Worker / Verifier)
// ---------------------------------------------------------------------------

// leaderSeq returns the next sequence number for a leader log file.
func (l *Loggers) nextLeaderSeq() int {
	l.leaderSeq++
	return l.leaderSeq
}

// AgentLog writes an agent interaction (prompt + response) to a markdown
// file.  The file is placed under the agent's role directory.
//
// For Leader logs:
//
//	LogLeader("decompose", prompt, response, err, dur)
//	→ logs/leader/decompose_001.md
//
// For Worker/Verifier logs:
//
//	LogAgent("worker", taskID, round, prompt, response, exitCode, dur, err)
//	→ logs/tasks/<taskID>/worker_001.md
func (l *Loggers) LogAgent(role, taskID string, round int, prompt, response string, exitCode int, dur time.Duration, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var dir, filename string
	if role == "leader" {
		dir = filepath.Join(l.baseDir, "leader")
		seq := l.nextLeaderSeq()
		filename = fmt.Sprintf("%s_%03d.md", taskID, seq)
	} else {
		dir = filepath.Join(l.baseDir, "tasks", taskID)
		os.MkdirAll(dir, 0755)
		filename = fmt.Sprintf("%s_%03d.md", role, round)
	}

	path := filepath.Join(dir, filename)
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# %s Log — %s round %d\n\n", strings.Title(role), taskID, round))
	sb.WriteString(fmt.Sprintf("**Time**: %s\n", time.Now().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("**Duration**: %.1fs\n", dur.Seconds()))
	if exitCode != 0 || err != nil {
		sb.WriteString(fmt.Sprintf("**Exit**: %d\n", exitCode))
	}
	if err != nil {
		sb.WriteString(fmt.Sprintf("**Error**: %v\n", err))
	}
	sb.WriteString("\n## Prompt\n\n```\n")
	sb.WriteString(truncateForLog(prompt, 32000))
	sb.WriteString("\n```\n\n## Response\n\n")
	if len(response) > 0 {
		sb.WriteString(response)
	} else {
		sb.WriteString("_(empty)_\n")
	}
	sb.WriteString("\n")

	os.WriteFile(path, []byte(sb.String()), 0644)
}

// LogLeader is a convenience wrapper for leader agent logs.
func (l *Loggers) LogLeader(kind, prompt, response string, dur time.Duration, err error) {
	l.LogAgent("leader", kind, 0, prompt, response, 0, dur, err)
}

// LogTaskFeedback writes leader/verifier feedback into the task's dialogue directory
// so the dashboard can display it chronologically between worker/verifier rounds.
// kind is "leader" or "verifier", round is the attempt number.
func (l *Loggers) LogTaskFeedback(taskID, kind string, round int, content string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	dir := filepath.Join(l.baseDir, "tasks", taskID)
	os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, fmt.Sprintf("%s_feedback_%03d.md", kind, round))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s Feedback — round %d\n\n", strings.Title(kind), round))
	sb.WriteString(fmt.Sprintf("**Time**: %s\n\n", time.Now().Format(time.RFC3339)))
	sb.WriteString(content)
	sb.WriteString("\n")

	os.WriteFile(path, []byte(sb.String()), 0644)
}

// ---------------------------------------------------------------------------
// Per-task engine event log
// ---------------------------------------------------------------------------

// LogTaskEvent appends a timestamped event line to the task's engine.md file.
func (l *Loggers) LogTaskEvent(taskID, event string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	dir := filepath.Join(l.baseDir, "tasks", taskID)
	os.MkdirAll(dir, 0755)

	path := filepath.Join(dir, "engine.md")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(event, args...)
	fmt.Fprintf(f, "| %s | %s |\n", ts, msg)
}

// LogTaskEventHeader writes the table header if the file is empty.
func (l *Loggers) LogTaskEventHeader(taskID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	dir := filepath.Join(l.baseDir, "tasks", taskID)
	os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, "engine.md")

	if info, _ := os.Stat(path); info != nil && info.Size() > 0 {
		return
	}
	os.WriteFile(path, []byte("# Engine Events\n\n| Time | Event |\n|------|-------|\n"), 0644)
}

func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n\n... (truncated)"
}
