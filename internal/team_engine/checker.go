package team_engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checker performs fast, deterministic validation of a Worker's output
// without spawning an LLM agent.  It checks:
//
//  1. Output file existence (if declared)
//  2. Build / compilation
//  3. Linter / formatter
//  4. Referenced file existence
//
// The Checker runs in-process and returns in seconds.  Semantic
// verification (requirements coverage, logic correctness, security)
// is handled by the Verifier, which runs AFTER the Checker passes.
type Checker struct {
	whiteboard *Whiteboard
	timeout    time.Duration // per-command timeout (default 60s)
}

// NewChecker creates a deterministic Checker.
func NewChecker(wb *Whiteboard, timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Checker{
		whiteboard: wb,
		timeout:    timeout,
	}
}

// CheckResult is the structured output from deterministic checking.
type CheckResult struct {
	Passed       bool     `json:"passed"`
	Issues       []string `json:"issues"`
	Evidence     []string `json:"evidence"`      // actual command output excerpts
	OutputExists bool     `json:"output_exists"`
	BuildPassed  bool     `json:"build_passed"`
	LintPassed   bool     `json:"lint_passed"`
	RefsAllExist bool     `json:"refs_all_exist"`
}

// ---------------------------------------------------------------------------
// Check — deterministic, no LLM agent
// ---------------------------------------------------------------------------

// Check performs deterministic validation of the task's Worker output.
// Returns (passed, retry, feedback, err).  passed=true means all checks
// passed and the task is ready for Verifier (LLM) semantic review.
func (c *Checker) Check(task *Task) (passed bool, retry bool, feedback string, err error) {
	workerOutput, err := c.whiteboard.ReadOutput(task.ID)
	if err != nil {
		return false, false, "", fmt.Errorf("read worker output: %w", err)
	}

	workdir := task.Workdir
	if workdir == "" {
		workdir = "."
	}
	// Fallback: use task directory for file checks (agent files via stdout capture).
	taskDir := c.whiteboard.TaskDir(task.ID)

	result := &CheckResult{Passed: true}

	// 1. Check output file exists (if task declares one).
	if task.Output != "" {
		outputPath := extractOutputPath(task.Output, workdir)
		exists := fileExists(outputPath) || outputDirExists(outputPath)
		if !exists {
			outputPath = extractOutputPath(task.Output, taskDir)
			exists = fileExists(outputPath) || outputDirExists(outputPath)
		}
		if exists {
			result.OutputExists = true
			result.Evidence = append(result.Evidence, fmt.Sprintf("output file exists: %s", outputPath))
		} else {
			result.Passed = false
			result.Issues = append(result.Issues, fmt.Sprintf("output file/dir not found: %s", outputPath))
		}
	}

	// Use taskDir for build/lint/test if workdir lacks project files.
	checkDir := workdir
	if !hasProjectFiles(checkDir) && hasProjectFiles(taskDir) {
		checkDir = taskDir
	}

	// 2. Role-aware checks.
	if task.Role.IsCodeRole() {
		// Code tasks: build → lint → test.
		buildOK, buildEvidence := c.runBuild(checkDir)
		result.BuildPassed = buildOK
		result.Evidence = append(result.Evidence, buildEvidence...)
		if !buildOK {
			result.Passed = false
			result.Issues = append(result.Issues, "build/compilation failed")
		}

		lintOK, lintEvidence := c.runLint(checkDir)
		result.LintPassed = lintOK
		result.Evidence = append(result.Evidence, lintEvidence...)
		if !lintOK {
			result.Passed = false
			result.Issues = append(result.Issues, "linter/format check failed")
		}

		testOK, testEvidence := c.runTests(checkDir)
		result.Evidence = append(result.Evidence, testEvidence...)
		if !testOK {
			result.Passed = false
			result.Issues = append(result.Issues, "tests failed")
		}
	} else if task.Role.IsContentRole() {
		// Content tasks (researcher, writer, etc.): check output is substantive.
		if len(workerOutput) < 100 {
			result.Passed = false
			result.Issues = append(result.Issues, fmt.Sprintf("output too short (%d chars) — appears empty or insubstantial", len(workerOutput)))
		} else if hasOnlyIntentPhrases(workerOutput) {
			result.Passed = false
			result.Issues = append(result.Issues, "output contains only intent/planning phrases, no actual substance")
		} else {
			result.Evidence = append(result.Evidence, fmt.Sprintf("output is substantive (%d chars)", len(workerOutput)))
		}
	}
	// Other roles: no additional deterministic checks.

	// 3. Check referenced files exist in worker output.
	missingRefs := findMissingRefs(workerOutput, checkDir)
	if len(missingRefs) > 0 {
		result.RefsAllExist = false
		result.Passed = false
		result.Issues = append(result.Issues, fmt.Sprintf("referenced files missing: %v", missingRefs))
	} else {
		result.RefsAllExist = true
	}

	// 5. Build feedback string.
	feedback = c.buildFeedback(result)

	return result.Passed, false, feedback, nil
}

// ---------------------------------------------------------------------------
// RunWithContext — legacy compatibility stub.
// The Checker is deterministic and does not use a runner.
// ---------------------------------------------------------------------------

// RunCheck runs a shell command with a timeout and returns (exitCode, stdout, stderr).
func runCheck(workdir string, args []string, timeout time.Duration) (int, string, string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()

	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return -1, "", fmt.Sprintf("failed to start %v: %v", args, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	exitCode := 0
	select {
	case waitErr := <-done:
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
	case <-time.After(timeout):
		cmd.Process.Kill()
		return -2, outBuf.String(), fmt.Sprintf("timeout after %v", timeout)
	}

	return exitCode, outBuf.String(), errBuf.String()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// detectProjectType returns the language/framework based on files in workdir.
func detectProjectType(workdir string) string {
	type check struct {
		file string
		lang string
	}
	checks := []check{
		{"go.mod", "go"},
		{"Cargo.toml", "rust"},
		{"package.json", "node"},
		{"pyproject.toml", "python"},
		{"setup.py", "python"},
		{"requirements.txt", "python"},
		{"CMakeLists.txt", "cmake"},
		{"Makefile", "make"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"build.gradle.kts", "java"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(workdir, c.file)); err == nil {
			return c.lang
		}
	}
	return ""
}

// Deprecated: Use AgentRole.IsCodeRole() instead.
func isCodeRole(role AgentRole) bool {
	return role.IsCodeRole()
}

func (c *Checker) runTests(workdir string) (bool, []string) {
	lang := detectProjectType(workdir)
	switch lang {
	case "go":
		return c.runGoTest(workdir)
	default:
		return true, nil // test run not implemented for this language
	}
}

func (c *Checker) runBuild(workdir string) (bool, []string) {
	lang := detectProjectType(workdir)
	if lang == "" {
		return true, nil // can't determine — skip build check
	}

	switch lang {
	case "go":
		return c.runGoBuild(workdir)
	case "rust":
		return c.runCargoCheck(workdir)
	case "python":
		return c.runPythonCheck(workdir)
	case "node":
		return c.runNodeCheck(workdir)
	default:
		return true, nil
	}
}

func (c *Checker) runLint(workdir string) (bool, []string) {
	lang := detectProjectType(workdir)
	switch lang {
	case "go":
		return c.runGofmt(workdir)
	default:
		return true, nil // lint check not implemented for this language
	}
}

// ---------------------------------------------------------------------------
// Language-specific checks
// ---------------------------------------------------------------------------

func (c *Checker) runGoBuild(workdir string) (bool, []string) {
	exitCode, stdout, stderr := runCheck(workdir, []string{"go", "build", "./..."}, c.timeout)
	evidence := []string{fmt.Sprintf("go build: exit=%d", exitCode)}
	if stdout != "" {
		evidence = append(evidence, "stdout: "+truncateStr(stdout, 200))
	}
	if stderr != "" {
		evidence = append(evidence, "stderr: "+truncateStr(stderr, 200))
	}
	return exitCode == 0, evidence
}

func (c *Checker) runGofmt(workdir string) (bool, []string) {
	exitCode, stdout, stderr := runCheck(workdir, []string{"gofmt", "-d", "."}, c.timeout)
	evidence := []string{fmt.Sprintf("gofmt -d: exit=%d", exitCode)}
	if stdout != "" {
		evidence = append(evidence, "diff: "+truncateStr(stdout, 200))
	}
	if stderr != "" {
		evidence = append(evidence, "stderr: "+truncateStr(stderr, 200))
	}
	// gofmt -d exits 0 even when diffs exist; we check output emptiness.
	if strings.TrimSpace(stdout) != "" {
		return false, evidence
	}
	return true, evidence
}

func (c *Checker) runCargoCheck(workdir string) (bool, []string) {
	exitCode, stdout, stderr := runCheck(workdir, []string{"cargo", "check"}, c.timeout)
	evidence := []string{fmt.Sprintf("cargo check: exit=%d", exitCode)}
	if stdout != "" {
		evidence = append(evidence, "stdout: "+truncateStr(stdout, 200))
	}
	if stderr != "" {
		evidence = append(evidence, "stderr: "+truncateStr(stderr, 200))
	}
	return exitCode == 0, evidence
}

func (c *Checker) runPythonCheck(workdir string) (bool, []string) {
	exitCode, _, stderr := runCheck(workdir, []string{"python", "-m", "py_compile", "."}, c.timeout)
	evidence := []string{fmt.Sprintf("python -m py_compile: exit=%d", exitCode)}
	if stderr != "" {
		evidence = append(evidence, "stderr: "+truncateStr(stderr, 200))
	}
	return exitCode == 0, evidence
}

func (c *Checker) runNodeCheck(workdir string) (bool, []string) {
	exitCode, _, stderr := runCheck(workdir, []string{"npm", "run", "build", "--if-present"}, c.timeout)
	evidence := []string{fmt.Sprintf("npm run build: exit=%d", exitCode)}
	if stderr != "" {
		evidence = append(evidence, "stderr: "+truncateStr(stderr, 200))
	}
	return exitCode == 0, evidence
}

// ---------------------------------------------------------------------------
// Feedback formatting
// ---------------------------------------------------------------------------

func (c *Checker) buildFeedback(result *CheckResult) string {
	if result.Passed {
		var b strings.Builder
		b.WriteString("✅ Checker PASS — all deterministic checks passed.\n")
		b.WriteString("Evidence:\n")
		for _, e := range result.Evidence {
			b.WriteString(fmt.Sprintf("  - %s\n", e))
		}
		b.WriteString("\nReady for Verifier (LLM semantic review).")
		return b.String()
	}

	var b strings.Builder
	b.WriteString("❌ Checker FAIL\n\n")
	b.WriteString("Issues:\n")
	for _, issue := range result.Issues {
		b.WriteString(fmt.Sprintf("  - %s\n", issue))
	}
	if len(result.Evidence) > 0 {
		b.WriteString("\nEvidence:\n")
		for _, e := range result.Evidence {
			b.WriteString(fmt.Sprintf("  - %s\n", e))
		}
	}
	b.WriteString("\nPlease fix these issues and re-run.")
	return b.String()
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

// extractOutputPath extracts a clean file path from task.Output, which may
// contain a description after the path (e.g. "add.go — a Go source file").
func extractOutputPath(output, workdir string) string {
	// Split on common separators: em-dash, en-dash, colon with space.
	for _, sep := range []string{" — ", " – ", ": "} {
		if idx := strings.Index(output, sep); idx > 0 {
			candidate := strings.TrimSpace(output[:idx])
			if looksLikePath(candidate) {
				return absPath(candidate, workdir)
			}
		}
	}
	// No separator found — use the whole string as-is.
	return absPath(strings.TrimSpace(output), workdir)
}

// hasProjectFiles checks if a directory contains recognizable project files.
func hasProjectFiles(dir string) bool {
	for _, name := range []string{"go.mod", "package.json", "Cargo.toml", "CMakeLists.txt", "Makefile", "setup.py", "pyproject.toml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	// Check for any .go, .rs, .ts, .js, .py files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		switch ext {
		case ".go", ".rs", ".ts", ".js", ".py", ".java", ".c", ".cpp":
			return true
		}
	}
	return false
}

// looksLikePath returns true if s looks like a file/directory path
// (contains a dot extension or a path separator).
func looksLikePath(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, "/") || strings.Contains(s, "\\")
}

func absPath(p, workdir string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workdir, p)
}

func outputDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func truncateStr(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ---------------------------------------------------------------------------
// Language-specific test runner
// ---------------------------------------------------------------------------

func (c *Checker) runGoTest(workdir string) (bool, []string) {
	exitCode, stdout, stderr := runCheck(workdir, []string{"go", "test", "./..."}, c.timeout)
	evidence := []string{fmt.Sprintf("go test: exit=%d", exitCode)}
	if stdout != "" {
		evidence = append(evidence, "stdout: "+truncateStr(stdout, 300))
	}
	if stderr != "" {
		evidence = append(evidence, "stderr: "+truncateStr(stderr, 200))
	}
	return exitCode == 0, evidence
}

// ---------------------------------------------------------------------------
// Content checks
// ---------------------------------------------------------------------------

// hasOnlyIntentPhrases returns true when the output consists mostly of
// planning/intent phrases ("I will...", "准备...") without actual substance.
func hasOnlyIntentPhrases(output string) bool {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) < 100 {
		return true // too short to be substantive
	}
	intentPatterns := []string{"I will", "I plan", "plan to", "准备", "计划", "将", "I'll"}
	cleaned := trimmed
	for _, p := range intentPatterns {
		cleaned = strings.ReplaceAll(cleaned, p, "")
	}
	return len(strings.TrimSpace(cleaned)) < 50
}

// Deprecated: BuildCheckerPrompt was used by the old LLM-based Checker.
// Use the Verifier for LLM-based semantic verification instead.
func BuildCheckerPrompt(task *Task, workerOutput string) string {
	return fmt.Sprintf(`(deprecated — use Verifier)
ORIGINAL TASK: %s
WORKER OUTPUT: %s`, task.Description, workerOutput)
}

// Deprecated: BuildContentCheckerPrompt was used by the old LLM-based Checker.
// Use the Verifier for LLM-based semantic verification instead.
func BuildContentCheckerPrompt(task *Task, workerOutput string) string {
	return BuildCheckerPrompt(task, workerOutput)
}

// Deprecated: BuildAgentVerifierPrompt was used by the old LLM-based Checker.
// Use BuildVerifierSemanticPrompt in verifier.go instead.
func BuildAgentVerifierPrompt(task *Task, workerOutput, _, _ string) string {
	return fmt.Sprintf(`(deprecated — use Verifier)
ORIGINAL TASK: %s
WORKER OUTPUT: %s`, task.Description, workerOutput)
}
