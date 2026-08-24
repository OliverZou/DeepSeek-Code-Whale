package team_engine

import (
	"context"
	"testing"
)

// sessionTrackingSpawner returns a non-empty SessionID for the worker role so
// the engine persists it (mimicking the native adapter), and sequences the
// verifier FAIL→PASS to force exactly one retry.
type sessionTrackingSpawner struct {
	workerCalls   int
	verifierCalls int
}

func (s *sessionTrackingSpawner) SpawnSubagent(_ context.Context, req SubagentRequest) (SubagentResponse, error) {
	switch req.Role {
	case "verifier":
		s.verifierCalls++
		if s.verifierCalls == 1 {
			return SubagentResponse{Output: failVerdict, Success: true, ExitCode: 0}, nil
		}
		return SubagentResponse{Output: passVerdict, Success: true, ExitCode: 0}, nil
	default:
		s.workerCalls++
		return SubagentResponse{SessionID: "worker-session-1", Output: "worker output", Success: true, ExitCode: 0}, nil
	}
}

// countingSessionOps is a minimal SessionOps whose Continue records the call so
// the test can assert that a retry reused the recorded session instead of
// spawning a fresh worker.
type countingSessionOps struct {
	continueCalls int
	lastSessionID string
	lastTask      string
}

func (o *countingSessionOps) Prompt(context.Context, string, string) (string, error) { return "", nil }
func (o *countingSessionOps) Continue(_ context.Context, sessionID, task string) (SubagentResponse, error) {
	o.continueCalls++
	o.lastSessionID = sessionID
	o.lastTask = task
	return SubagentResponse{SessionID: sessionID, Output: "continued worker output", Success: true, ExitCode: 0}, nil
}
func (o *countingSessionOps) Fork(context.Context, string) (string, error)      { return "", nil }
func (o *countingSessionOps) Summarize(context.Context, string) (string, error) { return "", nil }
func (o *countingSessionOps) Abort(context.Context, string) error               { return nil }
func (o *countingSessionOps) Kill(context.Context, string) error                { return nil }

// TestRunTaskNativeRetryReusesSession verifies the P2 hard requirement that a
// native (non-shell) worker retry reuses the recorded member session — appending
// a turn with the Verifier feedback — rather than spawning a fresh session.
func TestRunTaskNativeRetryReusesSession(t *testing.T) {
	spawner := &sessionTrackingSpawner{}
	eng := newRunTaskEngine(t, spawner)
	defer eng.Close()

	ops := &countingSessionOps{}
	eng.SetSessionOps(ops)

	task, err := eng.CreateTask("Build feature", "Write the feature", RoleDeveloper, "", nil, 2, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	ok, err := eng.RunTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if !ok {
		t.Fatalf("expected task to reach done, got ok=false")
	}

	if spawner.workerCalls != 1 {
		t.Fatalf("expected 1 worker spawn, got %d (retry should continue the session)", spawner.workerCalls)
	}
	if ops.continueCalls != 1 {
		t.Fatalf("expected 1 session continue on retry, got %d", ops.continueCalls)
	}
	if ops.lastSessionID != "worker-session-1" {
		t.Fatalf("continue session = %q, want worker-session-1", ops.lastSessionID)
	}
}
