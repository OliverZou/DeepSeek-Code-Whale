package agent

import "testing"

// TestWithCompactSummaryContext verifies the compact-summary guidance option
// stores the caller's preservation requirements. Team workers rely on this to
// keep task contracts/acceptance criteria through history compaction — a
// generic summary would silently drop them (v11 压缩实验的教训).
func TestWithCompactSummaryContext(t *testing.T) {
	a := NewAgentWithRegistry(nil, nil, nil, WithCompactSummaryContext("  preserve contracts and acceptance criteria  "))
	if a.compactSummaryContext != "preserve contracts and acceptance criteria" {
		t.Fatalf("compactSummaryContext = %q, want trimmed guidance", a.compactSummaryContext)
	}

	b := NewAgentWithRegistry(nil, nil, nil)
	if b.compactSummaryContext != "" {
		t.Fatalf("default compactSummaryContext = %q, want empty", b.compactSummaryContext)
	}
}
