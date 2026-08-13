package agent

import "testing"

func TestExtractFileRefsGoTestFailure(t *testing.T) {
	// Go test assertion lines (`foo_test.go:12: expected 1, got 2`) carry a
	// filename but none of the classic error keywords; they must still be
	// extracted so failures are not silently dropped.
	text := "--- FAIL: TestFoo\n    foo_test.go:12: expected 1, got 2\nFAIL\nFAIL\tgithub.com/foo/bar\t0.5s"
	refs := extractFileRefs(text)
	if !refs["foo_test.go"] {
		t.Fatalf("expected foo_test.go to be extracted, got %v", refs)
	}
}

func TestMergeVerificationResults(t *testing.T) {
	tests := []struct {
		name         string
		verify       string
		test         string
		review       string
		wantPass     bool
		wantFindings int
	}{
		{
			name:         "go test failure with file ref",
			test:         "--- FAIL: TestFoo\n    foo_test.go:12: expected 1, got 2\nFAIL",
			wantPass:     false,
			wantFindings: 1,
		},
		{
			name:         "failure without file ref surfaces unparsed P0",
			test:         "--- FAIL: TestFoo\nFAIL",
			wantPass:     false,
			wantFindings: 1,
		},
		{
			name:         "clean test output passes",
			test:         "ok\tgithub.com/foo/bar\t0.500s",
			wantPass:     true,
			wantFindings: 0,
		},
		{
			name:         "review pass only",
			review:       "REVIEW: PASS",
			wantPass:     true,
			wantFindings: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mv := mergeVerificationResults(tt.verify, tt.test, tt.review, nil)
			if mv.Passed != tt.wantPass {
				t.Fatalf("Passed = %v, want %v (findings=%d, raw=%q)", mv.Passed, tt.wantPass, len(mv.Findings), mv.RawText)
			}
			if len(mv.Findings) != tt.wantFindings {
				t.Fatalf("len(Findings) = %d, want %d (raw=%q)", len(mv.Findings), tt.wantFindings, mv.RawText)
			}
		})
	}
}

func TestHasP0Finding(t *testing.T) {
	if hasP0Finding([]VerificationFinding{{Severity: "P1"}, {Severity: "P2"}}) {
		t.Fatal("expected false for non-P0 findings")
	}
	if !hasP0Finding([]VerificationFinding{{Severity: "P1"}, {Severity: "P0"}}) {
		t.Fatal("expected true when a P0 is present")
	}
}
