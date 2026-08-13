package agent

import "testing"

func TestDefaultRecoveryPolicyReplanForUnknown(t *testing.T) {
	p := DefaultRecoveryPolicy()

	// Unclassifiable failures should nudge the model to replan rather than
	// silently pass through the raw error.
	if rule := p.Rules[FailureClassUnknown]; rule.Action != RecoveryActionRequestReplan {
		t.Fatalf("Unknown action = %q, want %q", rule.Action, RecoveryActionRequestReplan)
	}
	// exec_failed (e.g. a failing test/build) must keep its raw output so the
	// model can see and fix the underlying error instead of being told to replan.
	if rule := p.Rules[FailureClassExecFailed]; rule.Action != RecoveryActionPassThrough {
		t.Fatalf("ExecFailed action = %q, want %q", rule.Action, RecoveryActionPassThrough)
	}
}
