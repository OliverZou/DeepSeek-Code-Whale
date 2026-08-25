package team_engine

import (
	"path/filepath"
	"strings"
)

// unexpectedDeliverables returns the delivered files (relative) that are NOT
// declared in the task Output — the worker touched files owned by another
// task (parallel overwrite hazard) or produced undeclared artifacts.
func unexpectedDeliverables(task *Task, workdir string, changed []string) []string {
	declared := map[string]bool{}
	for _, o := range strings.Split(strings.TrimSpace(task.Output), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		declared[filepath.Clean(o)] = true
	}
	var out []string
	for _, f := range changed {
		if !declared[filepath.Clean(f)] {
			out = append(out, f)
		}
	}
	return out
}
