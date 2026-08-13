package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MergedVerification is the structured result of running verify + test + review.
// RawText is the human-readable formatted output for injection into history.
// Findings are parsed for programmatic decisions (P0/P1 check, flaky detection).
type MergedVerification struct {
	Passed   bool                  // true when no P0/P1 findings from own mutations
	Findings []VerificationFinding // all findings, sorted by severity
	RawText  string                // formatted text for history injection
}

// VerificationFinding represents one issue found by verify/test/review.
type VerificationFinding struct {
	Severity  string   // P0, P1, P2, P3
	File      string   // affected file path
	Line      string   // line number (may be "")
	Problem   string   // one-line problem description
	Fix       string   // suggested fix
	Sources   []string // which sources found this: verify, test, review
	Origin    string   // "own" for direct mutations, "subagent" for child agent mutations
	rawVerify string   // raw verify output lines for this finding
	rawTest   string   // raw test output lines for this finding
}

var findingRe = regexp.MustCompile(`FINDING \[(P[0-3])\]:\s*(\S+?)(?::(\d+))?\s*[—\-]+\s*(.+?)\s*[—\-]+\s*Fix:\s*(.+)`)

func parseReviewFindings(reviewText string) []VerificationFinding {
	var findings []VerificationFinding
	for _, line := range strings.Split(reviewText, "\n") {
		line = strings.TrimSpace(line)
		m := findingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		findings = append(findings, VerificationFinding{
			Severity: m[1],
			File:     m[2],
			Line:     m[3],
			Problem:  m[4],
			Fix:      m[5],
			Sources:  []string{"review"},
		})
	}
	return findings
}

var fileRefRe = regexp.MustCompile(`(\S+\.(?:go|py|js|ts|tsx|jsx|java|rs|c|cpp|h|rb))(?::(\d+))?`)

func extractFileRefs(text string) map[string]bool {
	refs := make(map[string]bool)
	errorIndicators := []string{"error", "fail", "FAIL", "Error", "panic", "fatal", "FATAL", "undefined", "cannot", "syntax", "unresolved", "not found", "missing", "import", "expected", "mismatch", "assertion"}
	for _, line := range strings.Split(text, "\n") {
		hasError := false
		lowerLine := strings.ToLower(line)
		for _, ind := range errorIndicators {
			if strings.Contains(lowerLine, strings.ToLower(ind)) {
				hasError = true
				break
			}
		}
		if !hasError {
			continue
		}
		for _, m := range fileRefRe.FindAllStringSubmatch(line, -1) {
			if len(m) < 2 || m[1] == "" {
				continue
			}
			normalized := strings.ReplaceAll(m[1], `\`, "/")
			refs[strings.ToLower(normalized)] = true
		}
	}
	return refs
}

// mergeVerificationResults parses verify/test/review outputs and merges them
// into a structured MergedVerification. originFileMap maps file paths to their
// mutation origin ("own" or "subagent"); files not in the map default to "own".
func mergeVerificationResults(verifyText, testText, reviewText string, originFileMap map[string]bool) MergedVerification {
	findings := parseReviewFindings(reviewText)

	verifyRefs := extractFileRefs(verifyText)
	testRefs := extractFileRefs(testText)

	for i := range findings {
		f := &findings[i]
		lowerFile := strings.ToLower(strings.ReplaceAll(f.File, `\`, "/"))
		if verifyRefs[lowerFile] {
			f.Sources = append(f.Sources, "verify")
			f.rawVerify = extractLinesForFile(verifyText, f.File)
		}
		if testRefs[lowerFile] {
			f.Sources = append(f.Sources, "test")
			f.rawTest = extractLinesForFile(testText, f.File)
		}
		// Set origin: if file is in subagent map, mark as subagent
		if originFileMap[lowerFile] {
			f.Origin = "subagent"
		} else {
			f.Origin = "own"
		}
		delete(verifyRefs, lowerFile)
		delete(testRefs, lowerFile)
	}

	for file := range verifyRefs {
		findings = append(findings, VerificationFinding{
			Severity:  "P0",
			File:      file,
			Problem:   "build/lint error (no review finding matched)",
			Fix:       "see verify output below",
			Sources:   []string{"verify"},
			Origin:    "own",
			rawVerify: extractLinesForFile(verifyText, file),
		})
	}
	for file := range testRefs {
		if verifyRefs[file] {
			continue
		}
		findings = append(findings, VerificationFinding{
			Severity: "P1",
			File:     file,
			Problem:  "test failure (no review finding matched)",
			Fix:      "see test output below",
			Sources:  []string{"test"},
			Origin:   "own",
			rawTest:  extractLinesForFile(testText, file),
		})
	}

	if len(findings) == 0 {
		if strings.Contains(reviewText, "REVIEW: PASS") {
			return MergedVerification{Passed: true, RawText: "All checks passed."}
		}
		// Fallback: if verify or test produced output but no findings were
		// extracted, return the raw output so the model can see failures.
		var parts []string
		if verifyText != "" {
			parts = append(parts, "--- verify output ---\n"+verifyText)
		}
		if testText != "" {
			parts = append(parts, "--- test output ---\n"+testText)
		}
		if reviewText != "" {
			parts = append(parts, "--- review output ---\n"+reviewText)
		}
		raw := strings.Join(parts, "\n\n")
		// If verification reported a failure but no file reference could be
		// parsed (e.g. Go test's `--- FAIL: TestX` line carries no filename),
		// do not silently pass — surface it as an unparsed P0 so the fix loop
		// re-engages instead of declaring a false "all green".
		if looksFailed(verifyText) || looksFailed(testText) {
			return MergedVerification{
				Passed: false,
				Findings: []VerificationFinding{{
					Severity: "P0",
					Problem:  "verification/test reported a failure that could not be parsed into file references",
					Fix:      "inspect the raw output below and fix the underlying issue",
					Sources:  []string{"verify"},
					Origin:   "own",
				}},
				RawText: raw,
			}
		}
		return MergedVerification{Passed: true, RawText: raw}
	}

	sort.Slice(findings, func(i, j int) bool {
		return severityOrder(findings[i].Severity) < severityOrder(findings[j].Severity)
	})

	var b strings.Builder
	for i, f := range findings {
		b.WriteString(fmt.Sprintf("#%d [%s] %s", i+1, f.Severity, f.File))
		if f.Line != "" {
			b.WriteString(fmt.Sprintf(":%s", f.Line))
		}
		b.WriteString(fmt.Sprintf(" — %s — Fix: %s\n", f.Problem, f.Fix))
		b.WriteString(fmt.Sprintf("     sources: %s\n", strings.Join(f.Sources, ", ")))
		if f.rawVerify != "" {
			for _, l := range strings.Split(f.rawVerify, "\n") {
				if strings.TrimSpace(l) != "" {
					b.WriteString(fmt.Sprintf("     [verify] %s\n", l))
				}
			}
		}
		if f.rawTest != "" {
			for _, l := range strings.Split(f.rawTest, "\n") {
				if strings.TrimSpace(l) != "" {
					b.WriteString(fmt.Sprintf("     [test]   %s\n", l))
				}
			}
		}
	}

	passed := !hasP0P1FromOwn(findings)
	return MergedVerification{Passed: passed, Findings: findings, RawText: strings.TrimRight(b.String(), "\n")}
}

// hasP0P1FromOwn reports whether findings include any P0 or P1 issues
// that originate from the main agent's own mutations (not subagent).
func hasP0P1FromOwn(findings []VerificationFinding) bool {
	for _, f := range findings {
		if f.Origin == "subagent" {
			continue
		}
		if f.Severity == "P0" || f.Severity == "P1" {
			return true
		}
	}
	return false
}

// hasP0Finding reports whether findings include any P0 issue, regardless of
// origin. P0 issues are deterministic correctness failures and must never be
// skipped as flaky.
func hasP0Finding(findings []VerificationFinding) bool {
	for _, f := range findings {
		if f.Severity == "P0" {
			return true
		}
	}
	return false
}

// findingFingerprint returns a stable key for flaky-test detection.
// Uses file + first 80 chars of problem text.
func findingFingerprint(f VerificationFinding) string {
	p := f.Problem
	if len(p) > 80 {
		p = p[:80]
	}
	return f.File + "\x00" + p
}

// fingerprintFindings returns a set of fingerprints for a slice of findings.
func fingerprintFindings(findings []VerificationFinding) map[string]bool {
	m := make(map[string]bool, len(findings))
	for _, f := range findings {
		m[findingFingerprint(f)] = true
	}
	return m
}

// anyRepeatedFindings reports whether any finding in current also appears in prev.
func anyRepeatedFindings(current []VerificationFinding, prev map[string]bool) bool {
	for _, f := range current {
		if prev[findingFingerprint(f)] {
			return true
		}
	}
	return false
}

// TODO: Consider splitting on delimiter blocks instead of substring
// matching to avoid false positives when a file path is referenced
// in output about a different file.
func extractLinesForFile(text, file string) string {
	lowerFile := strings.ToLower(file)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(line), lowerFile) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func severityOrder(s string) int {
	switch s {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	default:
		return 4
	}
}

// looksFailed reports whether verification/test output contains a failure
// signal even when no file reference could be extracted. This catches the
// common Go test shape `--- FAIL: TestX` (no filename) followed by an
// assertion line, so an unparsed failure is never reported as passing.
func looksFailed(text string) bool {
	lower := strings.ToLower(text)
	for _, sig := range []string{"--- fail", "panic:", "error:", "undefined:", "expected ", "cannot "} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}
