package team_engine

import (
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// Escalator is the fallback re-decomposition mechanism.  Workers self-split
// proactively when a task is too large; Escalator handles the remaining cases:
// estimation errors, persistent quality failures, and re-split deadlocks.
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
		if current != nil && current.State == TaskStateSuspended {
			out = append(out, current)
		}
	}
	return out
}

// ReplaceTaskWithSubtasks marks the original task as done (replaced) and
// inserts the new smaller subtasks into the same batch.  Child tasks get
// parentIDs pointing back to the original so the tree view and resume logic
// know they were decomposed from it.
func (esc *Escalator) ReplaceTaskWithSubtasks(original *Task, smaller []PlanTask, masterTaskID string,
	createTask func(PlanTask, string, string, []string) (*Task, error),
	transitionState func(string, TaskState, string) error) []*Task {

	var children []*Task
	// Mark the original as done — it becomes a virtual management node.
	// Its children's completion represents the original's completion.
	_ = transitionState(original.ID, TaskStateDone, "re-decomposed into smaller tasks")
	for _, pt := range smaller {
		task, err := createTask(pt, original.BatchID, masterTaskID, []string{original.ID})
		if err != nil {
			continue
		}
		_ = transitionState(task.ID, TaskStateAssigned, "re-decomposed from "+original.ID[:8])
		children = append(children, task)
	}
	return children
}

// ProcessBatch runs the full re-decomposition flow on a batch.
// Returns the newly created child tasks that must be added to batch.Tasks
// so they are picked up by the next cycle's RunBatch.
func (esc *Escalator) ProcessBatch(batch *Batch, masterTaskID, workdir string, timeout time.Duration, model string,
	getTask func(string) (*Task, error),
	createTask func(PlanTask, string, string, []string) (*Task, error),
	transitionState func(string, TaskState, string) error) ([]*Task, int) {

	var newTasks []*Task
	stuck := esc.StuckTasks(batch, getTask)
	if len(stuck) == 0 {
		return nil, 0
	}

	count := 0
	for _, t := range stuck {
		// Skip tasks suspended for non-retry reasons (user interrupt, crash restart).
		if t.RetryCount == 0 {
			continue
		}
		if esc.loggers != nil {
			esc.loggers.Engine("escalator: re-decompose task %s (%s) after %d retries", t.ID[:8], t.Title, t.MaxRetries)
		}
		smaller, err := esc.planner.DecomposeTask(t, workdir, timeout, model)
		if err != nil {
			if esc.loggers != nil {
				esc.loggers.Engine("escalator: DecomposeTask FAILED for %s (%s): %v", t.ID[:8], t.Title, err)
			}
			continue
		}
		if len(smaller) <= 1 {
			if esc.loggers != nil {
				esc.loggers.Engine("escalator: DecomposeTask for %s returned only %d tasks — cannot split further", t.ID[:8], len(smaller))
			}
			continue
		}
		children := esc.ReplaceTaskWithSubtasks(t, smaller, masterTaskID, createTask, transitionState)
		newTasks = append(newTasks, children...)
		if esc.loggers != nil {
			esc.loggers.Engine("escalator: re-decomposed %s into %d smaller tasks", t.ID[:8], len(smaller))
		}
		count++
	}
	return newTasks, count
}
