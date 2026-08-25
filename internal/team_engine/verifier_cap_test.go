package team_engine

import (
	"strings"
	"testing"
)

// verifierCapTemplate carries a tool name (to pass the lazy-verdict check) but
// is a force-stop template — NOT a real verdict about the deliverable.
const verifierCapTemplate = "[FAIL] \u26a0\ufe0f This turn was auto-interrupted: tool iteration cap reached\nTOOLS USED: read_file index.html\n\n## Completed\n- (see summary below)\n"

const verifierPassTemplate = "TOOLS USED: read_file, grep\nVERDICT: PASS\nEVIDENCE: criterion A satisfied\n## FINDINGS\n---json\n[]\n---\n"

// TestVerifyRetriesOnceAfterCapInterrupt locks the cap-interrupt remediation:
// the engine re-runs the verifier once with a minimal completion prompt; a real
// verdict from that pass wins over the interrupted turn (v41/v43 loss pattern).
func TestVerifyRetriesOnceAfterCapInterrupt(t *testing.T) {
	spawner := &mockSpawner{
		sessionID: "sess-verifier",
		roleSeq: map[string][]string{
			"verifier": {verifierCapTemplate, verifierPassTemplate},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	task, _ := eng.CreateTask("T", "desc", RoleDeveloper, "", nil, 0, t.TempDir(), "", "", "")
	task.AcceptanceCriteria = []string{"criterion A"}
	eng.Whiteboard.WriteOutput(task.ID, "worker output")

	v := NewVerifier(eng.Whiteboard, eng.Runner, eng.Router, 0)
	passed, _, _, err := v.Verify(task)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !passed {
		t.Errorf("verifier should PASS after the minimal re-run, got FAIL")
	}
	spawner.mu.Lock()
	reqs := len(spawner.reqs)
	spawner.mu.Unlock()
	if reqs != 2 {
		t.Errorf("verifier spawns = %d, want 2 (interrupted + minimal re-run)", reqs)
	}
}

// TestVerifySuspendAfterDoubleCapInterrupt locks the last line of defense:
// two consecutive cap interruptions still return FAIL (run_task then suspends).
func TestVerifySuspendAfterDoubleCapInterrupt(t *testing.T) {
	spawner := &mockSpawner{
		sessionID: "sess-verifier",
		roleSeq: map[string][]string{
			"verifier": {verifierCapTemplate, verifierCapTemplate},
		},
	}
	eng, err := New(":memory:", t.TempDir(), "", spawner)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	task, _ := eng.CreateTask("T", "desc", RoleDeveloper, "", nil, 0, t.TempDir(), "", "", "")
	eng.Whiteboard.WriteOutput(task.ID, "worker output")

	v := NewVerifier(eng.Whiteboard, eng.Runner, eng.Router, 0)
	passed, _, _, err := v.Verify(task)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if passed {
		t.Errorf("verifier should FAIL after double cap interruption")
	}
}

func TestIsVerifierCapInterrupt(t *testing.T) {
	if !isVerifierCapInterrupt(verifierCapTemplate) {
		t.Errorf("cap template not recognized")
	}
	if isVerifierCapInterrupt("VERDICT: PASS\nEVIDENCE: read_file ok") {
		t.Errorf("real verdict misclassified as cap interrupt")
	}
	if !strings.Contains(verifierPassTemplate, "VERDICT: PASS") {
		t.Errorf("pass template must carry a real verdict")
	}
}
