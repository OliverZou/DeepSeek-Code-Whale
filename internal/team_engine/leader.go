package team_engine

import (
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// Leader provides backward-compatible access to planning, review, and escalation.
// Deprecated: New code should use Planner, Reviewer, and Escalator directly.
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

// --- Delegated methods ---

// Elaborate delegates to Planner.
func (l *Leader) Elaborate(goal string, workdir string, timeout time.Duration, model ...string) (string, error) {
	return l.planner.Elaborate(goal, workdir, timeout, model...)
}

// Decompose delegates to Planner.
func (l *Leader) Decompose(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	return l.planner.Decompose(goal, workdir, timeout, model...)
}

// DecomposeFull delegates to Planner.
func (l *Leader) DecomposeFull(goal string, workdir string, timeout time.Duration, model ...string) ([]PlanTask, string, error) {
	return l.planner.DecomposeFull(goal, workdir, timeout, model...)
}

// DecomposeTask delegates to Planner.
func (l *Leader) DecomposeTask(task *Task, workdir string, timeout time.Duration, model ...string) ([]PlanTask, error) {
	return l.planner.DecomposeTask(task, workdir, timeout, model...)
}

// ReviewCycle delegates to Reviewer.
func (l *Leader) ReviewCycle(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, error) {
	return l.reviewer.ReviewCycle(goal, report, workdir, timeout, model...)
}

// ReviewCycleFull delegates to Reviewer.
func (l *Leader) ReviewCycleFull(goal string, report *CycleReport, workdir string, timeout time.Duration, model ...string) (*CycleReview, string, error) {
	return l.reviewer.ReviewCycleFull(goal, report, workdir, timeout, model...)
}

// ReviewProgress delegates to Reviewer.
func (l *Leader) ReviewProgress(goal, taskTitle, taskRole, taskState string, retryCount int, lastOutput, workdir string, timeout time.Duration, model ...string) (string, error) {
	return l.reviewer.ReviewProgress(goal, taskTitle, taskRole, taskState, retryCount, lastOutput, workdir, timeout, model...)
}
