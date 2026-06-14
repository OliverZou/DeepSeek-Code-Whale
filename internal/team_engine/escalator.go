package team_engine

import (
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// Escalator handles automatic re-decomposition of tasks that exhausted retries.
// It orchestrates: detect stuck tasks → call Planner.DecomposeTask → re-dispatch.
type Escalator struct {
	planner *Planner
	loggers *log.Loggers
}

// NewEscalator creates an Escalator that uses the given Planner.
func NewEscalator(planner *Planner) *Escalator {
	return &Escalator{planner: planner}
}

// WithLoggers attaches a Loggers for recording escalation events.
func (esc *Escalator) WithLoggers(loggers *log.Loggers) *Escalator {
	esc.loggers = loggers
	return esc
}

// StuckTasks finds tasks in a batch that have exhausted their retry budget.
func (esc *Escalator) StuckTasks(batch *Batch, getTask func(string) (*Task, error)) []*Task {
	var out []*Task
	for _, t := range batch.Tasks {
		current, err := getTask(t.ID)
		if err != nil {
			continue
		}
		if current != nil && current.State == TaskStateSuspended && current.RetryCount >= current.MaxRetries {
			out = append(out, current)
		}
	}
	return out
}

// ReplaceTaskWithSubtasks marks the original task as done (replaced) and
// inserts the new smaller subtasks into the same batch.
func (esc *Escalator) ReplaceTaskWithSubtasks(original *Task, smaller []PlanTask, masterTaskID string,
	createTask func(PlanTask, string, string) (*Task, error),
	transitionState func(string, TaskState, string) error) {

	_ = transitionState(original.ID, TaskStateDone, "re-decomposed into smaller tasks")
	for _, pt := range smaller {
		task, err := createTask(pt, original.BatchID, masterTaskID)
		if err != nil {
			continue
		}
		_ = transitionState(task.ID, TaskStateAssigned, "re-decomposed from "+original.ID[:8])
	}
}

// ProcessBatch runs the full re-decomposition flow on a batch.
// Returns the number of tasks that were re-decomposed.
func (esc *Escalator) ProcessBatch(batch *Batch, masterTaskID, workdir string, timeout time.Duration, model string,
	getTask func(string) (*Task, error),
	createTask func(PlanTask, string, string) (*Task, error),
	transitionState func(string, TaskState, string) error) int {

	stuck := esc.StuckTasks(batch, getTask)
	if len(stuck) == 0 {
		return 0
	}

	count := 0
	for _, t := range stuck {
		if esc.loggers != nil {
			esc.loggers.Engine("escalator: re-decompose task %s (%s) after %d retries", t.ID[:8], t.Title, t.MaxRetries)
		}
		smaller, err := esc.planner.DecomposeTask(t, workdir, timeout, model)
		if err != nil || len(smaller) <= 1 {
			continue
		}
		esc.ReplaceTaskWithSubtasks(t, smaller, masterTaskID, createTask, transitionState)
		if esc.loggers != nil {
			esc.loggers.Engine("escalator: re-decomposed %s into %d smaller tasks", t.ID[:8], len(smaller))
		}
		count++
	}
	return count
}
