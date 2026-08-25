package team_engine

import "testing"

// TestIsTestTask is the regression guard for the v26 misclassification:
// output "game.js, game.test.js" (implementation + bundled unit tests) was
// treated as a test task, pushing it into RunBatch's serial second stage and
// costing ~205s of wall time on a task with no dependencies. Only "pure test"
// tasks (qa/test role, or every output entry matching a test naming pattern)
// belong in the test stage.
func TestIsTestTask(t *testing.T) {
	cases := []struct {
		name   string
		role   AgentRole
		output string
		want   bool
	}{
		{"impl plus bundled tests", RoleDeveloper, "game.js, game.test.js", false},
		{"mixed impl non-test entry", RoleDeveloper, "game.test.js, index.html", false},
		{"pure test output", RoleDeveloper, "game.test.js", true},
		{"spec output", RoleDeveloper, "util.spec.js", true},
		{"test dir output", RoleDeveloper, "tests/foo.js", true},
		{"test_ prefixed", RoleDeveloper, "test_helper.js", true},
		{"all entries test files", RoleDeveloper, "a.test.js, b.spec.js", true},
		{"go _test", RoleDeveloper, "src/api_test.go", true},
		{"plain impl", RoleDeveloper, "game.js", false},
		{"empty output", RoleDeveloper, "", false},
		{"tester role, test output", RoleTester, "test_game.js", true},
		{"tester role, report output", RoleTester, "report.md", false},
		{"qa-engineer role, report output", AgentRole("software-qa-engineer"), "验收报告.md", false},
		{"qa-engineer role, test output", AgentRole("software-qa-engineer"), "game.test.js", true},
		{"qa-engineer role, empty output", AgentRole("software-qa-engineer"), "", true},
		{"developer role w/ impl", RoleDeveloper, "style.css", false},
	}
	for _, c := range cases {
		if got := isTestTask(&Task{Role: c.role, Output: c.output}); got != c.want {
			t.Errorf("%s: isTestTask(role=%q output=%q) = %v, want %v", c.name, c.role, c.output, got, c.want)
		}
	}
}

// TestIsReportTask locks the report-classification used by RunBatch's first
// stage: QA/test roles whose outputs are findings reports (not test files)
// must be recognized as report tasks so they run BEFORE impl/fix tasks — the
// v31_full regression where the acceptance batch's fix task ran first and
// re-audited the whole repo (59 rounds / 1.45M tokens) because the audit
// checklists did not exist yet.
func TestIsReportTask(t *testing.T) {
	cases := []struct {
		name   string
		role   AgentRole
		output string
		want   bool
	}{
		{"qa audit findings", AgentRole("software-qa-engineer"), "AUDIT_FINDINGS_static.md", true},
		{"qa acceptance report", AgentRole("software-qa-engineer"), "验收报告.md", true},
		{"qa runtime findings", AgentRole("software-qa-engineer"), "AUDIT_FINDINGS_runtime.md, AUDIT_FINDINGS_static.md", true},
		{"qa pure test", AgentRole("software-qa-engineer"), "game.test.js", false},
		{"qa empty output", AgentRole("software-qa-engineer"), "", false},
		{"tester report", RoleTester, "e2e-check.md", true},
		{"developer report", RoleDeveloper, "FIX_REPORT.md", false},
		{"developer test", RoleDeveloper, "test_game.js", false},
	}
	for _, c := range cases {
		if got := isReportTask(&Task{Role: c.role, Output: c.output}); got != c.want {
			t.Errorf("%s: isReportTask(role=%q output=%q) = %v, want %v", c.name, c.role, c.output, got, c.want)
		}
	}
}
