package team_engine

import (
	"time"

	"github.com/usewhale/whale/team_engine/log"
)

// Escalator handles tasks that have exhausted their retry budget.
// Instead of auto-re-decomposing (which doesn't fix Worker quality issues),
// suspended tasks are left for user intervention.
type Escalator struct {
	loggers *log.Loggers
}

// NewEscalator creates an Escalator.
func NewEscalator() *Escalator {
	return &Escalator{}
}

// WithLoggers attaches a Loggers for recording escalation events.
func (esc *Escalator) WithLoggers(loggers *log.Loggers) *Escalator {
	esc.loggers = loggers
	return esc
}

// LogSuspendedTasks logs suspended tasks that need user attention.
// Auto-re-decomposition is disabled: task failure is usually a Worker quality
// issue, not a decomposition error.  Re-splitting doesn't fix the Worker.
// NOTE: the createTask/transitionState callbacks and the ([]*Task, int) return
// are unused (always nil, 0) — kept for call-site compatibility; the callers'
// re-decompose branches are consequently dead. Safe to simplify in a follow-up.
func (esc *Escalator) LogSuspendedTasks(batch *Batch, masterTaskID, workdir string, timeout time.Duration, model string,
	getTask func(string) (*Task, error),
	createTask func(PlanTask, string, string, []string) (*Task, error),
	transitionState func(string, TaskState, string) error) ([]*Task, int) {

	for _, t := range batch.Tasks {
		current, err := getTask(t.ID)
		if err != nil || current == nil {
			continue
		}
		if current.State == TaskStateSuspended && current.RetryCount > 0 {
			if esc.loggers != nil {
				esc.loggers.Engine("escalator: task %s (%s) suspended after %d retries — needs user intervention",
					t.ID[:8], t.Title, current.RetryCount)
			}
		}
	}
	return nil, 0
}
