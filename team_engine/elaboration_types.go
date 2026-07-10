package team_engine

// ---------------------------------------------------------------------------
// Goal Elaboration types — see goal-elaboration-methodology.md
// ---------------------------------------------------------------------------

// CompletenessVerdict is the parsed result of merged Step 1+2
// (completeness check + domain research).  The LLM checks the raw goal
// against 6 dimensions and returns a structured verdict: COMPLETE
// (all OK, skip elaboration) or INCOMPLETE (gaps found + domain facts).
type CompletenessVerdict struct {
	Verdict     string          `json:"verdict"`               // "COMPLETE" or "INCOMPLETE"
	Dimensions  []DimensionGap  `json:"dimensions"`            // per-dimension status
	DomainFacts *DomainResearch `json:"domain_facts,omitempty"` // filled when INCOMPLETE
}

// DimensionGap describes the status of a single completeness dimension.
type DimensionGap struct {
	Name   string `json:"name"`   // scope / interface / behaviour / quality / dependencies / constraints
	Status string `json:"status"` // "OK" or "GAP"
	Detail string `json:"detail"` // gap description (empty when OK)
}

// DomainResearch is the parsed result of merged Step 1+2 (domain knowledge gap-filling).
// When the completeness check finds gaps, the same LLM call fills in
// domain facts — no separate research step.  Produces structured domain
// facts — no design decisions yet.
type DomainResearch struct {
	DomainOverview       string   `json:"domain_overview"`
	CommonScope          []string `json:"common_scope"`
	TypicalInterface     string   `json:"typical_interface"`
	QualityBenchmarks    []string `json:"quality_benchmarks"`
	ExplicitlyOutOfScope []string `json:"explicitly_out_of_scope"`
}

// IsComplete returns true if the verdict is COMPLETE (all 6 dimensions OK).
func (v *CompletenessVerdict) IsComplete() bool {
	return v != nil && v.Verdict == "COMPLETE"
}

// Gaps returns only the dimensions whose status is "GAP".
func (v *CompletenessVerdict) Gaps() []DimensionGap {
	if v == nil {
		return nil
	}
	var gaps []DimensionGap
	for _, d := range v.Dimensions {
		if d.Status == "GAP" {
			gaps = append(gaps, d)
		}
	}
	return gaps
}
