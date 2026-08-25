package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/tasks"
	"github.com/usewhale/whale/internal/team_engine"
)

// TeamRuntime is the app-layer SessionOps implementation, built on tasks.Runner
// + store.JSONLStore + sessionsDir. It also serves as the recording SpawnFunc:
// every team member spawn records the fully resolved SpawnSubagentRequest (with
// its resolved AgentDefinition) keyed by session ID, so prompt can reconstruct
// the agent and append a turn to the same session (re-entrancy).
type TeamRuntime struct {
	runner      *tasks.Runner
	library     *tasks.AgentDefinitionLibrary
	sessionsDir string
	msgStore    *store.JSONLStore

	mu       sync.Mutex
	requests map[string]tasks.SpawnSubagentRequest // sessionID → resolved spawn req
}

// NewTeamRuntime builds a TeamRuntime. It is the single place that couples the
// team engine to the native subagent runner and session store.
func NewTeamRuntime(runner *tasks.Runner, library *tasks.AgentDefinitionLibrary, sessionsDir string, msgStore *store.JSONLStore) *TeamRuntime {
	return &TeamRuntime{
		runner:      runner,
		library:     library,
		sessionsDir: sessionsDir,
		msgStore:    msgStore,
		requests:    make(map[string]tasks.SpawnSubagentRequest),
	}
}

// SpawnFunc returns a team_engine.SpawnFunc that wraps the native adapter and
// records each resolved request keyed by the resulting session ID.
func (rt *TeamRuntime) SpawnFunc() team_engine.SpawnFunc {
	return teamEngineSpawnAdapter(rt.runner, rt.library, rt.record)
}

// record stores the resolved request for a session. Called by SpawnFunc after a
// successful native spawn.
func (rt *TeamRuntime) record(sessionID string, req tasks.SpawnSubagentRequest) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	rt.mu.Lock()
	rt.requests[sessionID] = req
	rt.mu.Unlock()
}

func (rt *TeamRuntime) lookup(sessionID string) (tasks.SpawnSubagentRequest, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	req, ok := rt.requests[sessionID]
	return req, ok
}

var _ team_engine.SessionOps = (*TeamRuntime)(nil)

// Prompt appends a new turn to an existing member session and returns the
// reply. It works whether the session is running or already finished — the
// core of re-entrancy.
func (rt *TeamRuntime) Prompt(ctx context.Context, sessionID, task string) (string, error) {
	req, ok := rt.lookup(sessionID)
	if !ok {
		return "", fmt.Errorf("no recorded agent for session %s; cannot continue a non-native (shell) session", sessionID)
	}
	req.Task = task
	// Continued turns are appended turns, not the first structured turn — the
	// spawn-time OutputSchema (e.g. the leader's plan schema) must not force the
	// appended prompt into the same structured shape. Clear it for the rebuild.
	req.OutputSchema = nil
	resp, err := rt.runner.ContinueSubagent(ctx, req, sessionID)
	if err != nil {
		return "", err
	}
	return resp.Report, nil
}

// Continue appends a turn to an existing member session and returns the full
// structured result. It is the same ContinueSubagent primitive as Prompt, but
// maps the native response back to a team_engine.SubagentResponse so the
// engine's retry loop can record success, exit code, and token usage.
func (rt *TeamRuntime) Continue(ctx context.Context, sessionID, task string) (team_engine.SubagentResponse, error) {
	req, ok := rt.lookup(sessionID)
	if !ok {
		return team_engine.SubagentResponse{}, fmt.Errorf("no recorded agent for session %s; cannot continue a non-native (shell) session", sessionID)
	}
	req.Task = task
	// Continued turns are appended turns, not the first structured turn — the
	// spawn-time OutputSchema (e.g. the leader's plan schema) must not force the
	// appended prompt into the same structured shape. Clear it for the rebuild.
	req.OutputSchema = nil
	resp, err := rt.runner.ContinueSubagent(ctx, req, sessionID)
	if err != nil {
		return team_engine.SubagentResponse{}, err
	}

	success := resp.Status == "completed" || resp.Status == "done"
	if success && strings.TrimSpace(resp.Report) == "" && resp.StructuredResult == nil {
		success = false
	}
	exitCode := 0
	if !success {
		exitCode = 1
	}
	return team_engine.SubagentResponse{
		SessionID:       resp.SessionID,
		SpawnerType:     "adapter",
		Output:          resp.Report,
		Structured:      resp.StructuredResult,
		ExitCode:        exitCode,
		Success:         success,
		UsagePrompt:     resp.Usage.PromptTokens,
		UsageCompletion: resp.Usage.CompletionTokens,
		SystemPrompt:    resp.SystemPrompt,
	}, nil
}

// Fork clones a member session (verbatim transcript + meta) and returns the new
// session ID. The clone inherits the same resolved agent request so it too can
// be continued.
func (rt *TeamRuntime) Fork(ctx context.Context, sessionID string) (string, error) {
	if rt.msgStore == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	msgs, err := rt.msgStore.List(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("no conversation to fork for session %s", sessionID)
	}

	nextID := newSessionID(time.Now())
	copied := make([]core.Message, len(msgs))
	for i, msg := range msgs {
		msg.SessionID = nextID
		copied[i] = msg
	}
	if err := rt.msgStore.RewriteSession(ctx, nextID, copied); err != nil {
		return "", err
	}
	if err := session.SaveSessionMeta(rt.sessionsDir, nextID, session.SessionMeta{
		Kind:            "fork",
		ParentSessionID: sessionID,
	}); err != nil {
		return "", err
	}

	// The fork is a verbatim clone of the same agent — inherit its resolved
	// request so it can be prompted/continued like the original.
	if req, ok := rt.lookup(sessionID); ok {
		rt.record(nextID, req)
	}
	return nextID, nil
}

// Summarize returns the member session's last report/summary text.
func (rt *TeamRuntime) Summarize(ctx context.Context, sessionID string) (string, error) {
	meta, err := rt.runner.SubagentStatus(sessionID)
	if err != nil {
		return "", err
	}
	if report := strings.TrimSpace(meta.Report); report != "" {
		return report, nil
	}
	return strings.TrimSpace(meta.Summary), nil
}

// Abort gracefully cancels a running member session turn (no-op if none).
func (rt *TeamRuntime) Abort(_ context.Context, sessionID string) error {
	_, _, err := rt.runner.CancelBackgroundSubagent(sessionID)
	return err
}

// Kill forcefully terminates a running member session turn (no-op if none).
func (rt *TeamRuntime) Kill(_ context.Context, sessionID string) error {
	_, _, err := rt.runner.CancelBackgroundSubagent(sessionID)
	return err
}
