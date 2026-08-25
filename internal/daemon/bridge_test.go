package daemon

import (
	"encoding/json"
	"testing"

	"github.com/usewhale/whale/internal/team_engine"
)

func TestIsDAGRelevant(t *testing.T) {
	cases := []struct {
		name string
		typ  team_engine.TaskEventType
		want bool
	}{
		{"state changed", team_engine.EventStateChanged, true},
		{"task done", team_engine.EventTaskDone, true},
		{"worker output", team_engine.EventWorkerOutput, false},
		{"verifier result", team_engine.EventVerifierResult, false},
		{"leader log", team_engine.EventLeaderLog, false},
		{"agent log", team_engine.EventAgentLog, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDAGRelevant(team_engine.TaskEvent{Type: tc.typ}); got != tc.want {
				t.Fatalf("isDAGRelevant = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMasterTaskIDFor(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()
	s := New(ServerOptions{Engine: eng})

	task := team_engine.NewTask("task-1", "T1", "", team_engine.RoleDeveloper, team_engine.ProfileDefault, 3, ".", nil, "b1", "master-9")
	if err := eng.Store.InsertTask(task); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	if got := s.masterTaskIDFor("task-1"); got != "master-9" {
		t.Fatalf("masterTaskIDFor = %q, want master-9", got)
	}
	if got := s.masterTaskIDFor("nope"); got != "" {
		t.Fatalf("masterTaskIDFor(unknown) = %q, want empty", got)
	}
	if got := s.masterTaskIDFor(""); got != "" {
		t.Fatalf("masterTaskIDFor(empty) = %q, want empty", got)
	}
}

func TestBroadcastEventForwardsToHub(t *testing.T) {
	s := New(ServerOptions{})
	ch, cancel := s.hub.Subscribe()
	defer cancel()

	s.broadcastEvent(NewTeamEvent(team_engine.TaskEvent{
		Type:     team_engine.EventStateChanged,
		TaskID:   "task-1",
		OldState: "pending",
		NewState: "assigned",
	}))

	ev := <-ch
	if ev.Name != string(ChannelTeam) {
		t.Fatalf("SSE name = %q, want %q", ev.Name, ChannelTeam)
	}
	var parsed Event
	if err := json.Unmarshal(ev.Data, &parsed); err != nil {
		t.Fatalf("unmarshal event data: %v", err)
	}
	if parsed.Channel != ChannelTeam || parsed.Team == nil || parsed.Team.TaskID != "task-1" {
		t.Fatalf("parsed event mismatch: %+v", parsed)
	}
	if string(ev.Data) == "" {
		t.Fatal("data is empty")
	}
}
