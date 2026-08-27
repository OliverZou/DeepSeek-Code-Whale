package team_engine

import (
	"path/filepath"
	"strings"
)

// unexpectedDeliverablesExcluding is unexpectedDeliverables with an extra
// exclusion set (normalized relative paths): files declared by sibling tasks
// in the same master run are their concurrent writes misattributed to this
// task by the per-task baseline snapshot — not ownership anomalies.
func unexpectedDeliverablesExcluding(task *Task, workdir string, changed []string, exclude map[string]bool) []string {
	declared := map[string]bool{}
	for _, o := range strings.Split(strings.TrimSpace(task.Output), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		declared[strings.ToLower(filepath.ToSlash(filepath.Clean(o)))] = true
	}
	var out []string
	for _, f := range changed {
		key := strings.ToLower(filepath.ToSlash(filepath.Clean(f)))
		if !declared[key] && !exclude[key] {
			out = append(out, f)
		}
	}
	return out
}
