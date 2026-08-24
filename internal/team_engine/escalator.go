package team_engine

import (
	"github.com/usewhale/whale/internal/team_engine/log"
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
func (esc *Escalator) LogSuspendedTasks(batch *Batch, getTask func(string) (*Task, error)) {
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
}
