package team_engine

import (
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// Leader bundles the Planner and Reviewer into the TeamCycle orchestration
// surface: decompose the goal (Cycle 0), then review each plan-level
// CycleReport. Both delegate to subagent spawns via the native adapter, so
// the Leader's session is observable and forkable like any member session.
type Leader struct {
	planner  *Planner
	reviewer *Reviewer
	runner   *AgentRunner
	loggers  *log.Loggers
	team     *TeamConfig
	onLog    func()
}

// NewLeader creates a backward-compatible Leader wrapper.
func NewLeader(runner *AgentRunner) *Leader {
	return &Leader{
		runner:   runner,
		planner:  NewPlanner(runner),
		reviewer: NewReviewer(runner),
	}
}

// WithLoggers attaches a Loggers to the underlying Planner and Reviewer.
func (l *Leader) WithLoggers(loggers *log.Loggers) *Leader {
	l.loggers = loggers
	l.planner.WithLoggers(loggers)
	l.reviewer.WithLoggers(loggers)
	return l
}

// WithTeam attaches a TeamConfig to the underlying Planner and Reviewer.
func (l *Leader) WithTeam(team *TeamConfig) *Leader {
	l.team = team
	l.planner.WithTeam(team)
	l.reviewer.WithTeam(team)
	return l
}

// WithOnLog sets a callback to the underlying Planner and Reviewer.
func (l *Leader) WithOnLog(fn func()) *Leader {
	l.onLog = fn
	l.planner.WithOnLog(fn)
	l.reviewer.WithOnLog(fn)
	return l
}

// WithComplexity forwards the goal-size hint to the underlying Planner.
func (l *Leader) WithComplexity(c string) *Leader {
	l.planner.WithComplexity(c)
	return l
}

// WithDecomposeContext reads the Planner's decompose result and passes it
// to the Reviewer so review prompts include the Leader's own decisions.
func (l *Leader) WithDecomposeContext() *Leader {
	if dc := l.planner.DecomposeContext(); dc != "" {
		l.reviewer.WithDecomposeContext(dc)
	}
	return l
}

// --- Delegated methods ---

// Elaborate delegates to Planner.
func (l *Leader) Elaborate(goal string, workdir string, timeout time.Duration, model ...string) (string, error) {
	return l.planner.Elaborate(goal, workdir, timeout, model...)
}

// ElaborateFull delegates to Planner and also returns the goal-size hint.
func (l *Leader) ElaborateFull(goal string, workdir string, timeout time.Duration, model ...string) (string, string, error) {
	return l.planner.ElaborateFull(goal, workdir, timeout, model...)
}

// Decompose delegates to Planner.
func (l *Leader) Decompose(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	return l.planner.Decompose(goal, workdir, timeout, model...)
}

// DecomposeFull delegates to Planner.
func (l *Leader) DecomposeFull(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	return l.planner.DecomposeFull(goal, workdir, timeout, model...)
}

// DecomposeSessionID delegates to Planner and returns the Leader subagent
// session ID from the last successful decompose.
func (l *Leader) DecomposeSessionID() string {
	return l.planner.DecomposeSessionID()
}

// ReviewCycle delegates to Reviewer.
func (l *Leader) ReviewCycle(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, error) {
	return l.reviewer.ReviewCycle(goal, report, workdir, timeout, model...)
}

// ReviewCycleFull delegates to Reviewer.
func (l *Leader) ReviewCycleFull(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, string, error) {
	return l.reviewer.ReviewCycleFull(goal, report, workdir, timeout, model...)
}

// ReviewPlanCycle delegates to Reviewer. It reviews a plan-level CycleReport —
// the TE's report after one full pass over the plan — and returns the Leader's
// accept/reject decision for the whole Cycle.
func (l *Leader) ReviewPlanCycle(goal string, report *PlanCycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, error) {
	return l.reviewer.ReviewPlanCycle(goal, report, workdir, timeout, model...)
}
