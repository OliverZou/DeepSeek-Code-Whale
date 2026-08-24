package team_engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// TestLogRunSummary locks the RUN SUMMARY timing table as a first-class
// feature: at the end of a run the engine must emit a per-stage wall-clock
// breakdown (elaborate / decompose / each batch / TOTAL) so a single glance
// shows which stage consumed the most time.  The assertion strings pin the
// output shape so a future refactor that drops or renames a stage fails here.
func TestLogRunSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.log")
	tl := log.NewTeamLogAt(path)
	SetLogger(tl)
	defer CloseLogger()

	eng := newTestEngine(t)
	defer eng.Close()
	batches := []*Batch{
		{ID: "b1", Label: "核心逻辑", Status: BatchStatusPassed, Tasks: []*Task{{ID: "t1"}}, CycleCount: 1, TotalDuration: 40.0},
		{ID: "b2", Label: "UI 集成", Status: BatchStatusPassed, Tasks: []*Task{{ID: "t2"}}, CycleCount: 1, TotalDuration: 30.0},
	}

	runStart := time.Now().Add(-100 * time.Second)
	eng.logRunSummary(runStart, 10*time.Second, 20*time.Second, batches)

	if err := tl.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(raw)

	for _, want := range []string{
		"=== RUN SUMMARY ===",
		"elaborate: ",
		"decompose: ",
		"batch 核心逻辑 [passed]: 1 tasks, cycles=1",
		"batch UI 集成 [passed]: 1 tasks, cycles=1",
		"TOTAL: ",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("RUN SUMMARY missing %q:\n%s", want, content)
		}
	}
}
