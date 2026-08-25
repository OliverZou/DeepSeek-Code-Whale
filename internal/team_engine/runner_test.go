package team_engine

import (
	"context"
	"testing"
	"time"
)

// capturingSpawner records the SubagentRequest it receives and returns a fixed
// response, so RunDecomposer can be exercised without a real subagent runtime.
type capturingSpawner struct {
	req  SubagentRequest
	resp SubagentResponse
}

func (c *capturingSpawner) SpawnSubagent(_ context.Context, req SubagentRequest) (SubagentResponse, error) {
	c.req = req
	return c.resp, nil
}

// TestRunDecomposerIsLeanPlanner verifies the lean decompose contract: the
// one-shot task split spawns a plain "planner" session with NO team .md
// persona, NO team agent directory, and a single generation pass (1/1) —
// persona and orchestration tools belong to the turn-2 drive/review session
// (RunLeaderBootstrap), not to pure decomposition.
func TestRunDecomposerIsLeanPlanner(t *testing.T) {
	teamDir := t.TempDir()
	team := &TeamConfig{
		TeamDir: teamDir,
		Leader:  TeamLeaderConfig{Role: "software-team-lead"},
	}

	capture := &capturingSpawner{
		resp: SubagentResponse{SessionID: "sess-leader-1", Output: "{}", Success: true, ExitCode: 0},
	}
	runner := NewRunner(capture).WithTeam(team)

	res := runner.RunDecomposer("decompose this", teamDir, 5*time.Second)

	if !res.Success {
		t.Fatalf("RunDecomposer failed: %s", res.Stderr)
	}
	if capture.req.AgentName != "" {
		t.Errorf("AgentName = %q, want %q (lean decomposer carries no persona)", capture.req.AgentName, "")
	}
	if capture.req.TeamAgentsDir != "" {
		t.Errorf("TeamAgentsDir = %q, want %q (lean decomposer resolves no agent)", capture.req.TeamAgentsDir, "")
	}
	if capture.req.MaxIters != 1 || capture.req.MaxCalls != 1 {
		t.Errorf("MaxIters/MaxCalls = %d/%d, want 1/1 (single generation pass)", capture.req.MaxIters, capture.req.MaxCalls)
	}
	if capture.req.Role != "planner" {
		t.Errorf("Role = %q, want %q", capture.req.Role, "planner")
	}
	if res.SessionID != "sess-leader-1" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-leader-1")
	}
}
