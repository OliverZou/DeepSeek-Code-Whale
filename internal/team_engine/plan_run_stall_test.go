package team_engine

import (
	"strings"
	"testing"
	"time"
)

// TestSettleStallFiresWithoutProgress locks the liveness-based settle wait:
// a running-but-stalled plan run (lastProgress older than planRunStallTimeout —
// an engine-side hang that neither per-task timeouts nor runBatch turned into a
// terminal state) is reported unsettled instead of waiting forever, while a
// freshly-started run stays in the wait.
func TestSettleStallFiresWithoutProgress(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	master := "deadbeef-1234-4321-90ab-abcdef012345"
	defer planRuns.Delete(master)

	// Freshly started (lastProgress = store time): the wait must NOT report a
	// hang immediately — the stall condition is the only error path, so the
	// wait keeps looping; verify by not being done and not erroring after a
	// couple of beats.
	planRuns.Store(master, &PlanRunState{running: true, lastProgress: time.Now()})
	_, settled, err := eng.waitPlanRunSettledBeat(master)
	if settled || err != nil {
		t.Fatalf("fresh run: settled=%v err=%v, want a live wait", settled, err)
	}

	// Stalled beyond the threshold: settle reports the hang.
	planRuns.Store(master, &PlanRunState{running: true, lastProgress: time.Now().Add(-planRunStallTimeout - time.Minute)})
	_, settled, err = eng.waitPlanRunSettled(master)
	if settled {
		t.Fatal("stalled run reported settled")
	}
	if err == nil || !strings.Contains(err.Error(), "no batch progress") {
		t.Fatalf("stalled run: want no-batch-progress error, got %v", err)
	}
}

// waitPlanRunSettledBeat runs the real wait on a goroutine and observes a
// fixed number of the 2s polling beats — enough to cross one check without
// waiting well past the threshold: the liveness wait has no total deadline, so
// a live run is expected to still be waiting when the observation window ends.
func (e *TeamEngine) waitPlanRunSettledBeat(masterTaskID string) (PlanRunView, bool, error) {
	type out struct {
		v       PlanRunView
		settled bool
		err     error
	}
	done := make(chan out, 1)
	go func() {
		v, settled, err := e.waitPlanRunSettled(masterTaskID)
		done <- out{v, settled, err}
	}()
	select {
	case r := <-done:
		return r.v, r.settled, r.err
	case <-time.After(1500 * time.Millisecond):
		// Still waiting on a live run — expected.
		return PlanRunView{}, false, nil
	}
}
