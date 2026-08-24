package team_engine

import "testing"

// TestIterationBudget locks the complexity→budget mapping so a future edit that
// loosens the tight "simple" budget (or breaks the unassessed→default fallback)
// fails here.  Simple tasks must not inherit the generous 80/200 worker budget —
// that is exactly the licence-to-over-explore that drives up latency and tokens.
func TestIterationBudget(t *testing.T) {
	cases := []struct {
		complexity string
		isVerifier bool
		wantIters  int
		wantCalls  int
		wantTokens int
	}{
		{"simple", false, 20, 50, 16000},
		{"simple", true, 8, 20, 8000},
		{"medium", false, 50, 120, 24000},
		{"medium", true, 12, 35, 16000},
		{"complex", false, 80, 200, defaultMaxTokens},
		{"complex", true, 15, 50, defaultMaxTokens},
		{"", false, 80, 200, defaultMaxTokens},      // unassessed → default
		{"unknown", true, 15, 50, defaultMaxTokens}, // unknown → default
	}
	for _, c := range cases {
		iters, calls, tokens := iterationBudget(c.complexity, c.isVerifier)
		if iters != c.wantIters || calls != c.wantCalls || tokens != c.wantTokens {
			t.Errorf("iterationBudget(%q, verifier=%v) = (%d, %d, %d), want (%d, %d, %d)",
				c.complexity, c.isVerifier, iters, calls, tokens, c.wantIters, c.wantCalls, c.wantTokens)
		}
	}
}
