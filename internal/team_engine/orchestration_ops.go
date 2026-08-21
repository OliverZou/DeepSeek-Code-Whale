package team_engine

// Orchestration ops — thin engine-side operations the Leader consults while
// driving a cycle. The coarse TeamCycle model keeps the Leader's control loop
// in the review decision; these are the queries it can call on the plan graph.

// AffectedTaskIDs returns the closure of tasks affected by a change to the
// given task IDs: the batches containing them, plus every downstream batch
// (transitive depends_on closure), excluding already-passed batches. This is
// the blast radius the Leader computes before re-dispatching — e.g. "if I
// rework task T3, which tasks must be re-run?".
//
// The caller (Leader) is responsible for mapping a textual requirement change
// to the concrete task IDs it touches; this method only does graph reachability.
// Unknown task IDs are ignored; when none of the given IDs resolve to a
// non-passed batch, the result is empty.
func (e *TeamEngine) AffectedTaskIDs(batches []*Batch, changedIDs ...string) []string {
	if len(changedIDs) == 0 {
		return nil
	}
	batchByID := make(map[string]*Batch, len(batches))
	taskByID := make(map[string]*Task)
	for _, b := range batches {
		batchByID[b.ID] = b
		for _, t := range b.Tasks {
			taskByID[t.ID] = t
		}
	}

	affected := make(map[string]bool)
	var queue []string
	enqueue := func(bid string) {
		if !affected[bid] {
			affected[bid] = true
			queue = append(queue, bid)
		}
	}
	for _, id := range changedIDs {
		t, ok := taskByID[id]
		if !ok {
			continue
		}
		b := batchByID[t.BatchID]
		if b == nil || b.Status == BatchStatusPassed {
			continue
		}
		enqueue(b.ID)
	}

	// Downstream closure: any non-passed batch depending on an affected batch.
	for len(queue) > 0 {
		queue = queue[1:]
		for _, b := range batches {
			if b.Status == BatchStatusPassed || affected[b.ID] {
				continue
			}
			for _, dep := range b.DependsOn {
				if affected[dep] {
					enqueue(b.ID)
					break
				}
			}
		}
	}

	var out []string
	for _, b := range batches {
		if !affected[b.ID] {
			continue
		}
		for _, t := range b.Tasks {
			out = append(out, t.ID)
		}
	}
	return out
}
