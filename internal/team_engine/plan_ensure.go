package team_engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnsureMasterPlan decomposes a master that has no plan.json yet (the inline
// leader flow: the human's main session acts as Leader and calls team_run_plan
// directly, without a pre-decomposed plan). It runs the same decompose path as
// RunLeaderDriven — elaborate skip + Leader decompose + overload split +
// output-ownership gate + plan.json + task records — and returns the plan text
// so the leader session can display and adopt it before execution starts.
func (e *TeamEngine) EnsureMasterPlan(ctx context.Context, masterID string) (string, error) {
	mt, err := e.GetMasterTask(masterID)
	if err != nil || mt == nil {
		return "", fmt.Errorf("master %s not found", masterID)
	}
	// Already decomposed: nothing to do — the text is derived from plan.json.
	if _, err := os.Stat(filepath.Join(e.Whiteboard.MasterDir(masterID), "plan.json")); err == nil {
		return "plan already exists (see plan.md / plan.json in the master dir)", nil
	}

	leader := NewLeader(e.Runner).WithLoggers(e.Loggers).WithTeam(e.team).WithOnLog(func() {
		e.fireEvent(TaskEvent{Type: EventLeaderLog})
	})
	decomposerTimeout := time.Duration(e.Router.ResolveDecomposerTimeout()) * time.Second
	leaderModel := e.Router.ResolveModel("planner")

	planTasks, complexity, _, decompDur, err := e.decomposePlan(mt.Goal, mt.WorkspacePath, masterID, leader, decomposerTimeout, leaderModel, "")
	if err != nil {
		return "", fmt.Errorf("decompose: %w", err)
	}
	if _, err := e.createBatchesFromPlan(planTasks, mt.Goal, mt.WorkspacePath, masterID, complexity); err != nil {
		return "", fmt.Errorf("create batches: %w", err)
	}
	if e.Loggers != nil {
		e.Loggers.Engine("master %s inline-decomposed in %.1fs (%d tasks)", masterID[:8], decompDur.Seconds(), len(planTasks))
	}
	return formatPlanForLeader(planTasks), nil
}

// formatPlanForLeader renders the plan as a compact readable digest the Leader
// session can echo to the user ("here is the plan, executing...").
func formatPlanForLeader(tasks []PlanTask) string {
	var b strings.Builder
	b.WriteString("Decomposed plan (adopt as-is or call team_feedback to adjust):\n")
	for i, t := range tasks {
		fmt.Fprintf(&b, "%d. [batch %s] %s (%s) -> %s\n", i+1, t.BatchID, t.Title, t.Role, t.Output)
	}
	return b.String()
}
