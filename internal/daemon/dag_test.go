package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/usewhale/whale/internal/team_engine"
)

// mockSpawner 是最小 SubagentSpawner 实现，用于离线构造 TeamEngine。
type mockSpawner struct{}

func (mockSpawner) SpawnSubagent(_ context.Context, _ team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
	return team_engine.SubagentResponse{Output: "mock output", Success: true, ExitCode: 0}, nil
}

// newTestEngine 在临时目录构造一个离线 TeamEngine。
func newTestEngine(t *testing.T) *team_engine.TeamEngine {
	t.Helper()
	eng, err := team_engine.New(":memory:", t.TempDir(), "", mockSpawner{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng
}

func TestDeriveBatchStatus(t *testing.T) {
	done := &team_engine.Task{State: team_engine.TaskStateDone}
	verified := &team_engine.Task{State: team_engine.TaskStateVerified}
	failed := &team_engine.Task{State: team_engine.TaskStateFailed}
	suspended := &team_engine.Task{State: team_engine.TaskStateSuspended}
	produced := &team_engine.Task{State: team_engine.TaskStateProduced}
	assigned := &team_engine.Task{State: team_engine.TaskStateAssigned}

	cases := []struct {
		name  string
		tasks []*team_engine.Task
		want  team_engine.BatchStatus
	}{
		{"empty", nil, team_engine.BatchStatusPending},
		{"all done", []*team_engine.Task{done}, team_engine.BatchStatusPassed},
		{"done and verified", []*team_engine.Task{done, verified}, team_engine.BatchStatusPassed},
		{"any failed", []*team_engine.Task{done, failed}, team_engine.BatchStatusFailed},
		{"any suspended", []*team_engine.Task{produced, suspended}, team_engine.BatchStatusFailed},
		{"active wins over done", []*team_engine.Task{done, produced}, team_engine.BatchStatusRunning},
		{"assigned active", []*team_engine.Task{assigned}, team_engine.BatchStatusRunning},
		{"failed wins over active", []*team_engine.Task{produced, failed}, team_engine.BatchStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveBatchStatus(tc.tasks); got != string(tc.want) {
				t.Fatalf("deriveBatchStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDAGSnapshot(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()
	s := New(ServerOptions{Engine: eng})

	master, err := eng.CreateMasterTask("goal", ".", "sess-1")
	if err != nil {
		t.Fatalf("create master: %v", err)
	}

	// 写 plan.json（DAG 权威数据源）到 <BaseDir>/<masterID>/plan.json。
	plan := `{"generated":"2026-01-01T00:00:00Z","tasks":[
		{"title":"T1","description":"","output":"","role":"developer","batch_id":"b1","depends_on":[]},
		{"title":"T2","description":"","output":"","role":"developer","batch_id":"b2","depends_on":["b1"]}
	]}`
	dir := filepath.Join(eng.Whiteboard.BaseDir(), master.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(plan), 0644); err != nil {
		t.Fatal(err)
	}

	// 插入两个运行时任务。
	if err := eng.Store.InsertTask(team_engine.NewTask("task-a", "T1", "", team_engine.RoleDeveloper, team_engine.ProfileDefault, 3, ".", nil, "b1", master.ID)); err != nil {
		t.Fatalf("insert task-a: %v", err)
	}
	if err := eng.Store.InsertTask(team_engine.NewTask("task-b", "T2", "", team_engine.RoleDeveloper, team_engine.ProfileDefault, 3, ".", nil, "b2", master.ID)); err != nil {
		t.Fatalf("insert task-b: %v", err)
	}

	// 控制状态（deriveState 从文件推导）：
	// task-a: done（output.md + verify.md）；task-b: produced（output.md，活动态）。
	if err := os.WriteFile(filepath.Join(eng.Whiteboard.BaseDir(), "task-a", "output.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eng.Whiteboard.BaseDir(), "task-a", "verify.md"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eng.Whiteboard.BaseDir(), "task-b", "output.md"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	snap, err := s.dagSnapshot(master.ID)
	if err != nil {
		t.Fatalf("dag snapshot: %v", err)
	}
	if snap.MasterTaskID != master.ID {
		t.Fatalf("master_task_id = %q, want %q", snap.MasterTaskID, master.ID)
	}
	if len(snap.Batches) != 2 {
		t.Fatalf("batch count = %d, want 2", len(snap.Batches))
	}

	byID := map[string]DAGBatchEvent{}
	for _, b := range snap.Batches {
		byID[b.BatchID] = b
	}

	b1, ok := byID["b1"]
	if !ok {
		t.Fatal("batch b1 missing")
	}
	if b1.Status != string(team_engine.BatchStatusPassed) {
		t.Fatalf("b1 status = %q, want %q", b1.Status, team_engine.BatchStatusPassed)
	}
	if b1.Label != "T1" {
		t.Fatalf("b1 label = %q, want T1", b1.Label)
	}
	if len(b1.Tasks) != 1 || b1.Tasks[0].State != string(team_engine.TaskStateDone) {
		t.Fatalf("b1 tasks mismatch: %+v", b1.Tasks)
	}

	b2, ok := byID["b2"]
	if !ok {
		t.Fatal("batch b2 missing")
	}
	if b2.Status != string(team_engine.BatchStatusRunning) {
		t.Fatalf("b2 status = %q, want %q", b2.Status, team_engine.BatchStatusRunning)
	}
	if len(b2.Tasks) != 1 || b2.Tasks[0].State != string(team_engine.TaskStateProduced) {
		t.Fatalf("b2 tasks mismatch: %+v", b2.Tasks)
	}
	if len(b2.DependsOn) != 1 || b2.DependsOn[0] != "b1" {
		t.Fatalf("b2 depends_on = %v, want [b1]", b2.DependsOn)
	}
}

func TestDAGSnapshotMissingPlanReturnsErr(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()
	s := New(ServerOptions{Engine: eng})
	master, err := eng.CreateMasterTask("goal", ".", "")
	if err != nil {
		t.Fatalf("create master: %v", err)
	}
	if _, err := s.dagSnapshot(master.ID); err == nil {
		t.Fatal("expected error for missing plan.json")
	}
}
