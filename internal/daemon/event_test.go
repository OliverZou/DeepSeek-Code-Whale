package daemon

import (
	"testing"

	"github.com/usewhale/whale/internal/runtime/protocol"
	"github.com/usewhale/whale/internal/team_engine"
)

func TestNewAgentEvent(t *testing.T) {
	ev := protocol.Event{Kind: "text", Text: "hello", TurnID: "t1"}
	got := NewAgentEvent(ev)
	if got.Channel != ChannelAgent {
		t.Fatalf("channel = %q, want %q", got.Channel, ChannelAgent)
	}
	if got.Agent == nil {
		t.Fatal("Agent payload is nil")
	}
	if got.Agent.Text != "hello" || got.Agent.TurnID != "t1" {
		t.Fatalf("agent payload mismatch: %+v", got.Agent)
	}
	if got.Team != nil || got.DAG != nil {
		t.Fatal("unexpected team/dag payload set")
	}
	if got.Time.IsZero() {
		t.Fatal("Time is zero")
	}
}

func TestNewTeamEvent(t *testing.T) {
	ev := team_engine.TaskEvent{Type: team_engine.EventStateChanged, TaskID: "task-1", OldState: "pending", NewState: "assigned"}
	got := NewTeamEvent(ev)
	if got.Channel != ChannelTeam {
		t.Fatalf("channel = %q, want %q", got.Channel, ChannelTeam)
	}
	if got.Team == nil {
		t.Fatal("Team payload is nil")
	}
	if got.Team.TaskID != "task-1" || got.Team.Type != team_engine.EventStateChanged {
		t.Fatalf("team payload mismatch: %+v", got.Team)
	}
	if got.Agent != nil || got.DAG != nil {
		t.Fatal("unexpected agent/dag payload set")
	}
}

func TestNewDAGEvent(t *testing.T) {
	dag := DAGEvent{MasterTaskID: "master-1", Batches: []DAGBatchEvent{{BatchID: "b1", Status: "running"}}}
	got := NewDAGEvent(dag, "master-1")
	if got.Channel != ChannelDAG {
		t.Fatalf("channel = %q, want %q", got.Channel, ChannelDAG)
	}
	if got.MasterTaskID != "master-1" {
		t.Fatalf("master_task_id = %q, want %q", got.MasterTaskID, "master-1")
	}
	if got.DAG == nil {
		t.Fatal("DAG payload is nil")
	}
	if got.Team != nil || got.Agent != nil {
		t.Fatal("unexpected team/agent payload set")
	}
}
