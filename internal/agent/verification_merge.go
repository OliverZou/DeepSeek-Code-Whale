package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type verificationFinding struct {
	severity  string
	file      string
	line      string
	problem   string
	fix       string
	sources   []string
	rawVerify string
	rawTest   string
}

var findingRe = regexp.MustCompile(`FINDING \[(P[0-3])\]:\s*(\S+?)(?::(\d+))?\s*[—\-]+\s*(.+?)\s*[—\-]+\s*Fix:\s*(.+)`)

func parseReviewFindings(reviewText string) []verificationFinding {
	var findings []verificationFinding
	for _, line := range strings.Split(reviewText, "\n") {
		line = strings.TrimSpace(line)
		m := findingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		findings = append(findings, verificationFinding{
			severity: m[1],
			file:     m[2],
			line:     m[3],
			problem:  m[4],
			fix:      m[5],
			sources:  []string{"review"},
		})
	}
	return findings
}

var fileRefRe = regexp.MustCompile(`(\S+\.(?:go|py|js|ts|tsx|jsx|java|rs|c|cpp|h|rb))(?::(\d+))?`)

func extractFileRefs(text string) map[string]bool {
	refs := make(map[string]bool)
	errorIndicators := []string{"error", "fail", "FAIL", "Error", "panic", "fatal", "FATAL", "undefined", "cannot", "syntax", "unresolved", "not found", "missing", "import"}
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

func mergeVerificationResults(verifyText, testText, reviewText string) string {
	findings := parseReviewFindings(reviewText)

	verifyRefs := extractFileRefs(verifyText)
	testRefs := extractFileRefs(testText)

	for i := range findings {
		f := &findings[i]
		lowerFile := strings.ToLower(strings.ReplaceAll(f.file, `\`, "/"))
		if verifyRefs[lowerFile] {
			f.sources = append(f.sources, "verify")
			f.rawVerify = extractLinesForFile(verifyText, f.file)
		}
		if testRefs[lowerFile] {
			f.sources = append(f.sources, "test")
			f.rawTest = extractLinesForFile(testText, f.file)
		}
		delete(verifyRefs, lowerFile)
		delete(testRefs, lowerFile)
	}

	for file := range verifyRefs {
		findings = append(findings, verificationFinding{
			severity:  "P0",
			file:      file,
			problem:   "build/lint error (no review finding matched)",
			fix:       "see verify output below",
			sources:   []string{"verify"},
			rawVerify: extractLinesForFile(verifyText, file),
		})
	}
	for file := range testRefs {
		if verifyRefs[file] {
			continue
		}
		findings = append(findings, verificationFinding{
			severity: "P1",
			file:     file,
			problem:  "test failure (no review finding matched)",
			fix:      "see test output below",
			sources:  []string{"test"},
			rawTest:  extractLinesForFile(testText, file),
		})
	}

	if len(findings) == 0 {
		if strings.Contains(reviewText, "REVIEW: PASS") {
			return "All checks passed."
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
		return strings.Join(parts, "\n\n")
	}

	sort.Slice(findings, func(i, j int) bool {
		return severityOrder(findings[i].severity) < severityOrder(findings[j].severity)
	})

	var b strings.Builder
	for i, f := range findings {
		b.WriteString(fmt.Sprintf("#%d [%s] %s", i+1, f.severity, f.file))
		if f.line != "" {
			b.WriteString(fmt.Sprintf(":%s", f.line))
		}
		b.WriteString(fmt.Sprintf(" — %s — Fix: %s\n", f.problem, f.fix))
		b.WriteString(fmt.Sprintf("     sources: %s\n", strings.Join(f.sources, ", ")))
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
	return strings.TrimRight(b.String(), "\n")
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
