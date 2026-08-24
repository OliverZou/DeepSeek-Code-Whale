package team_engine

import (
	"context"
	"path/filepath"
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

// TestRunDecomposerResolvesLeaderAgentName verifies the P0 change: the Leader's
// decompose spawn now carries the leader's AgentName and TeamAgentsDir so the
// native adapter resolves the team's agents/<leader-role>.md definition (its
// tools, permission mode, and persona) instead of the old session-less lite
// path. Role stays "planner" (the semantic category) while AgentName carries
// the concrete .md identity — mirroring worker/verifier resolution.
func TestRunDecomposerResolvesLeaderAgentName(t *testing.T) {
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
	if capture.req.AgentName != "software-team-lead" {
		t.Errorf("AgentName = %q, want %q", capture.req.AgentName, "software-team-lead")
	}
	if want := filepath.Join(teamDir, "agents"); capture.req.TeamAgentsDir != want {
		t.Errorf("TeamAgentsDir = %q, want %q", capture.req.TeamAgentsDir, want)
	}
	if capture.req.Role != "planner" {
		t.Errorf("Role = %q, want %q", capture.req.Role, "planner")
	}
	if res.SessionID != "sess-leader-1" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-leader-1")
	}
}
