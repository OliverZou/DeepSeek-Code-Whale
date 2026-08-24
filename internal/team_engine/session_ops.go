package team_engine

import (
	"context"
	"sync"
)

// defaultSessionOps is a package-level fallback, set by the app layer when it
// wires in the native subagent adapter. Mirror of defaultSpawnFunc.
var (
	defaultSessionOps   SessionOps
	defaultSessionOpsMu sync.RWMutex
)

// DefaultSessionOps returns the package-level default SessionOps, or nil if the
// app has not yet wired one in.
func DefaultSessionOps() SessionOps {
	defaultSessionOpsMu.RLock()
	defer defaultSessionOpsMu.RUnlock()
	return defaultSessionOps
}

// SetDefaultSessionOps sets the package-level default SessionOps. Called by the
// app layer alongside SetDefaultSpawnFunc. Safe for concurrent use.
func SetDefaultSessionOps(ops SessionOps) {
	defaultSessionOpsMu.Lock()
	defer defaultSessionOpsMu.Unlock()
	defaultSessionOps = ops
}

// SessionHandle is a lightweight reference to a member subagent session. It
// carries the session ID plus the member's identity so callers (CLI, session
// pickers, other agents) can address a specific member — Leader, worker, or
// verifier — and render a distinguishable entry. It is NOT a new session type:
// it just promotes the already-existing sessionID string to a first-class value.
type SessionHandle struct {
	SessionID string
	AgentName string
	Role      string
}

// SessionOps is the seam through which the engine operates on member subagent
// sessions. team_engine cannot import tasks (the adapter lives in app, which
// imports both), so this interface is defined here and implemented by the app
// layer on top of tasks.Runner + store.JSONLStore + the sessions directory.
//
// Every method addresses a member session by its sessionID. A nil SessionOps
// means the engine is running in shell/standalone fallback mode, where member
// sessions are not forkable and these operations return explicit errors.
type SessionOps interface {
	// Prompt appends a new turn to an existing session and returns the reply.
	Prompt(ctx context.Context, sessionID, task string) (string, error)

	// Continue appends a turn to an existing session and returns the full
	// structured result. Unlike Prompt (which returns only the reply text for
	// the channel surface), Continue returns success/exit-code/token usage so
	// the engine's retry loop can record them and drive produce→verify→done.
	// It reuses the session recorded at spawn time, preserving failure context.
	Continue(ctx context.Context, sessionID, task string) (SubagentResponse, error)

	// Fork clones a session (verbatim transcript + meta) and returns the new
	// session ID. The clone continues from the same transcript under a
	// different subsequent instruction.
	Fork(ctx context.Context, sessionID string) (string, error)

	// Summarize returns the session's last report/summary text.
	Summarize(ctx context.Context, sessionID string) (string, error)

	// Abort gracefully cancels a running session turn.
	Abort(ctx context.Context, sessionID string) error

	// Kill forcefully terminates a running session turn.
	Kill(ctx context.Context, sessionID string) error
}
