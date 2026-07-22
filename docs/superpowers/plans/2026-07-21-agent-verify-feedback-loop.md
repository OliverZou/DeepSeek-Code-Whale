# Agent Verify-Feedback Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 turn-level verification 升级为 turn 内的验证-修复闭环，使模型在同一个 turn 内看到验证结果并自动修复问题。

**Architecture:** 验证-修复轮次作为主 loop 的一等迭代（`verifyFixIteration` flag），复用现有 `streamAndHandle` 和 dispatch 管线。子 agent mutation 标记来源跳过自动修复。内置 flaky test 检测和 context headroom check。

**Tech Stack:** Go 1.21+, 现有 `internal/agent` 包

## Global Constraints

- 不改变现有 P1-P4 discipline gates 的触发逻辑
- 不改变 review agent 的 prompt 或模型选择
- 不改变 tool dispatch 的并行模型
- `sync.Once` 用于 review agent HTTP client 初始化（匹配现有模式）
- `map[string]bool` 用于 turn-level state（匹配现有 `filesReadThisTurn` 模式）
- 测试使用 table-driven 风格

---

## File Structure

| 文件 | 职责 | 改动类型 |
|------|------|---------|
| `internal/agent/agent.go` | `VerifyLoopConfig` struct、新事件类型、Agent 状态字段、`runTurnLevelVerification` 重构 | Modify |
| `internal/agent/turn_loop.go` | verify-fix loop 主逻辑、flaky 检测、context headroom check、self-check nudge、task guard | Modify |
| `internal/agent/verification_merge.go` | `MergedVerification` 结构化返回、`hasP0P1Findings()`、`findingsFromOwnMutations()` | Modify |
| `internal/agent/stream.go` | subagent mutation 来源标记（`mutationsFromSubagent`）、`appendDispatchedToolResult` 增强 | Modify |
| `internal/agent/stream_dispatch.go` | `verifyFixIteration` 期间跳过 storm/redundant 计数 | Modify |
| `internal/agent/force_summary.go` | 结构化 forced summary 模板 | Modify |
| `internal/agent/agent_discipline_test.go` | 验证闭环集成测试 | Modify |

---

### Task 1: Add VerifyLoopConfig and new event types to agent.go

**Files:**
- Modify: `internal/agent/agent.go`

**Interfaces:**
- Produces: `VerifyLoopConfig` struct, `DefaultVerifyLoopConfig()`, `WithVerifyLoopConfig()` option, 6 new `AgentEventType` constants, `AgentEvent.TurnVerification` → `AgentEvent.VerifyFix` field

- [ ] **Step 1: Add VerifyLoopConfig struct**

在 `Agent` struct 上方（约 line 330），紧接现有 discipline config 字段之后添加：

```go
// VerifyLoopConfig controls the verify-feedback loop (Feature A of
// the agent-verify-feedback-loop design). Verification runs after the
// model finishes its work but before the turn is declared Done, giving
// the model a chance to fix issues in the same turn.
type VerifyLoopConfig struct {
	// Enabled toggles the verify-feedback loop. Default false — opt in.
	Enabled bool
	// MaxRounds caps verify-fix iterations. Default 1, max 3.
	MaxRounds int
	// SelfCheck enables the Pre-Done self-check nudge (Feature B).
	SelfCheck bool
	// PerRoundTimeout is the max time for one verification round (test + review agent).
	// Default 120s.
	PerRoundTimeout time.Duration
	// TotalTimeout is the max time for the entire verify loop across all rounds.
	// Default 300s.
	TotalTimeout time.Duration
	// DegradeOnFailure skips remaining rounds on review agent failure.
	DegradeOnFailure bool
	// IgnoreFlakyFindings skips repeated findings across consecutive rounds.
	IgnoreFlakyFindings bool
}

// DefaultVerifyLoopConfig returns the default configuration: verify-feedback
// loop is disabled (opt-in), max 1 round, self-check enabled.
func DefaultVerifyLoopConfig() VerifyLoopConfig {
	return VerifyLoopConfig{
		Enabled:             false,
		MaxRounds:           1,
		SelfCheck:           true,
		PerRoundTimeout:     120 * time.Second,
		TotalTimeout:        300 * time.Second,
		DegradeOnFailure:    true,
		IgnoreFlakyFindings: true,
	}
}
```

- [ ] **Step 2: Add Agent state fields for verify-fix loop**

在 `Agent` struct 的 discipline state 区域（约 line 329），`lastAssistantText` 之后添加：

```go
// Verify-feedback loop state (Feature A). Reset per turn.
verifyLoopConfig       VerifyLoopConfig // set once at construction, read-only
mutationsFromSubagent  map[string]bool  // file paths mutated by subagents (skip auto-fix)
verifyFixRound         int              // current verify-fix round (0 = not in loop)
verifyFixIteration     bool             // true when current main-loop iteration is a fix attempt
prevRoundFindings      map[string]bool  // fingerprint of previous round's findings (flaky detection)
```

- [ ] **Step 3: Add new event types**

在 `AgentEventType` 常量块（约 line 65-77），`AgentEventTypeTurnVerification` 之后添加：

```go
// Verify-feedback loop events (Feature A).
AgentEventTypeVerifyFixStarted     AgentEventType = "verify_fix_started"
AgentEventTypeVerifyFixRoundStart  AgentEventType = "verify_fix_round_start"
AgentEventTypeVerifyFixRoundResult AgentEventType = "verify_fix_round_result"
AgentEventTypeVerifyFixPassed      AgentEventType = "verify_fix_passed"
AgentEventTypeVerifyFixFailed      AgentEventType = "verify_fix_failed"
AgentEventTypeVerifyFixSkipped     AgentEventType = "verify_fix_skipped"
```

- [ ] **Step 4: Add VerifyFix field to AgentEvent**

在 `AgentEvent` struct 中（约 line 160），`TurnVerification *string` 之后添加：

```go
VerifyFix *VerifyFixInfo
```

在 `AgentEvent` struct 上方添加 `VerifyFixInfo` 类型：

```go
type VerifyFixInfo struct {
	Round    int      // current round number (1-based)
	MaxRound int      // configured max rounds
	Passed   bool     // true when verification passes
	Findings []string // finding summaries (one per P0/P1 finding)
	Skipped  bool     // true when skipped (flaky, context, force summary)
	Reason   string   // skip reason or "" if not skipped
}
```

- [ ] **Step 5: Add WithVerifyLoopConfig option**

在 `agent.go` options 区域（约 line 650），`WithClassifierConfig` 之后添加：

```go
// WithVerifyLoopConfig sets the verify-feedback loop configuration.
func WithVerifyLoopConfig(cfg VerifyLoopConfig) AgentOption {
	return func(a *Agent) {
		if cfg.MaxRounds < 1 {
			cfg.MaxRounds = 1
		}
		if cfg.MaxRounds > 3 {
			cfg.MaxRounds = 3
		}
		a.verifyLoopConfig = cfg
	}
}
```

- [ ] **Step 6: Run tests to verify compilation**

```bash
go build ./internal/agent/...
```
Expected: clean build, no errors.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/agent.go
git commit -m "feat(agent): add VerifyLoopConfig, event types, and state fields for verify-feedback loop"
```

---

### Task 2: Refactor verification_merge.go to return structured data

**Files:**
- Modify: `internal/agent/verification_merge.go`

**Interfaces:**
- Produces: `MergedVerification` struct, `hasP0P1FromOwn()` method, `fingerprintFindings()` function
- Consumes: existing `parseReviewFindings()`, `extractFileRefs()` (unchanged)

- [ ] **Step 1: Add MergedVerification struct and helpers**

在 `verification_merge.go` 顶部，import 块之后添加：

```go
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
	Severity string   // P0, P1, P2, P3
	File     string   // affected file path
	Line     string   // line number (may be "")
	Problem  string   // one-line problem description
	Fix      string   // suggested fix
	Sources  []string // which sources found this: verify, test, review
	Origin   string   // "own" for direct mutations, "subagent" for child agent mutations
}
```

- [ ] **Step 2: Refactor mergeVerificationResults to return MergedVerification**

将现有 `func mergeVerificationResults(verifyText, testText, reviewText string) string` 改为返回 `MergedVerification`：

```go
// mergeVerificationResults parses verify/test/review outputs and merges them
// into a structured MergedVerification. originFileMap maps file paths to their
// mutation origin ("own" or "subagent"); files not in the map default to "own".
func mergeVerificationResults(verifyText, testText, reviewText string, originFileMap map[string]bool) MergedVerification {
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
		// Set origin: if file is in subagent map, mark as subagent
		if originFileMap[lowerFile] {
			f.origin = "subagent"
		} else {
			f.origin = "own"
		}
		delete(verifyRefs, lowerFile)
		delete(testRefs, lowerFile)
	}

	// ... rest of unmatched refs handling (same logic as before, origin = "own") ...

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
		var parts []string
		if verifyText != "" { parts = append(parts, "--- verify output ---\n"+verifyText) }
		if testText != "" { parts = append(parts, "--- test output ---\n"+testText) }
		if reviewText != "" { parts = append(parts, "--- review output ---\n"+reviewText) }
		return MergedVerification{Passed: true, RawText: strings.Join(parts, "\n\n")}
	}

	sort.Slice(findings, func(i, j int) bool {
		return severityOrder(findings[i].Severity) < severityOrder(findings[j].Severity)
	})

	var b strings.Builder
	// ... existing formatting logic (unchanged) ...

	passed := !hasP0P1FromOwn(findings)
	return MergedVerification{Passed: passed, Findings: findings, RawText: strings.TrimRight(b.String(), "\n")}
}
```

- [ ] **Step 3: Add hasP0P1FromOwn and fingerprintFindings helpers**

```go
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
```

- [ ] **Step 4: Update caller in runTurnLevelVerification (agent.go line 373)**

将：
```go
merged := mergeVerificationResults("", testText, reviewText)
if merged == "" {
    return
}
msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification ---\n"+merged, false)
```

改为：
```go
mv := mergeVerificationResults("", testText, reviewText, a.mutationsFromSubagent)
if mv.RawText == "" {
    return
}
msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification ---\n"+mv.RawText, false)
```

- [ ] **Step 5: Update existing test callers**

搜索 `mergeVerificationResults` 的所有调用处，更新参数：

```bash
grep -rn "mergeVerificationResults" internal/agent/
```

将 `mergeVerificationResults(a, b, c)` 改为 `mergeVerificationResults(a, b, c, nil)`。

- [ ] **Step 6: Add hasVerificationFindings helper to turn_loop.go usage**

`runTurnLevelVerification` 现在需要返回 `MergedVerification` 而非纯文本。修改其签名和调用处。

- [ ] **Step 7: Build and run tests**

```bash
go build ./internal/agent/...
go test ./internal/agent/... -run "Verification|Merge" -v -count=1
```
Expected: clean build, existing tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/verification_merge.go internal/agent/agent.go internal/agent/agent_discipline_test.go
git commit -m "refactor(agent): return MergedVerification struct from mergeVerificationResults"
```

---

### Task 3: Add subagent mutation tracking in stream.go

**Files:**
- Modify: `internal/agent/stream.go`

**Interfaces:**
- Produces: `mutationsFromSubagent` map population during `appendDispatchedToolResult`
- Consumes: existing `isMutationTool()`, `extractFilePathFromCall()` from `stream_dispatch.go`

- [ ] **Step 1: Add trackSubagentMutation helper**

在 `stream.go` 底部添加：

```go
// trackSubagentMutation records that a file was mutated by a subagent.
// These files are skipped by the verify-feedback auto-fix loop because
// the main model lacks context to fix subagent-introduced issues.
func (a *Agent) trackSubagentMutation(call core.ToolCall) {
	if !isMutationTool(call.Name) {
		return
	}
	filePath := extractFilePathFromCall(call)
	if filePath == "" {
		return
	}
	absPath := normalizeWorkspacePath(filePath, a.workspaceRoot)
	if a.mutationsFromSubagent == nil {
		a.mutationsFromSubagent = make(map[string]bool)
	}
	a.mutationsFromSubagent[strings.ToLower(absPath)] = true
}
```

- [ ] **Step 2: Wire trackSubagentMutation into flushPendingParallelSubagents**

在 `stream.go` 的 `flushPendingParallelSubagents` 函数中，所有 outcomes 处理完毕后，遍历子 agent 返回的 mutation 信息：

```go
func (a *Agent) flushPendingParallelSubagents(ctx context.Context, sessionID, assistantMessageID, model string, pending []preparedToolDispatch, events chan<- AgentEvent, results *[]core.ToolResult, tools *core.ToolRegistry) error {
    // ... existing code ...

    for _, outcome := range outcomes {
        if !outcome.OK {
            continue
        }
        // Track subagent-mutated files for verify-feedback attribution.
        if outcome.Prepared.Call.Name == parallelSubagentToolName || outcome.Prepared.Call.Name == "spawn_subagent" {
            if subResults, ok := outcome.Result.Metadata["sub_results"].([]any); ok {
                for _, sr := range subResults {
                    if srm, ok := sr.(map[string]any); ok {
                        if name, ok := srm["name"].(string); ok && isMutationTool(name) {
                            if input, ok := srm["input"].(string); ok {
                                a.trackSubagentMutation(core.ToolCall{Name: name, Input: input})
                            }
                        }
                    }
                }
            }
        }
        if !a.appendDispatchedToolResult(ctx, sessionID, outcome.Prepared, outcome.Result, outcome.PrimarySucceeded, events, results, true) {
            return ctx.Err()
        }
    }
    return nil
}
```

如果 subagent 工具尚未在 metadata 中暴露 sub_results，则采用备选方案：在 `appendDispatchedToolResult` 中添加 `fromSubagent` 参数：

```go
// 在 preparedToolDispatch 中新增字段：
type preparedToolDispatch struct {
    // ... existing fields ...
    FromSubagent bool // true when this dispatch is from a child agent
}

// 在 flushPendingParallelSubagents 中设置：
prepared.FromSubagent = true
for _, p := range pending { p.FromSubagent = true }

// 在 appendDispatchedToolResult 中检查：
if isMutationTool(call.Name) && primarySucceeded && prepared.FromSubagent {
    a.trackSubagentMutation(call)
}
```

第二种方案更简洁可靠，推荐优先采用。

- [ ] **Step 3: Reset mutationsFromSubagent in resetTurnState**

在 `agent.go` 的 `resetTurnState()` 函数中（约 line 426），添加：

```go
if a.mutationsFromSubagent == nil {
    a.mutationsFromSubagent = make(map[string]bool)
} else {
    clear(a.mutationsFromSubagent)
}
```

- [ ] **Step 4: Build and verify**

```bash
go build ./internal/agent/...
```
Expected: clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/stream.go internal/agent/agent.go
git commit -m "feat(agent): track subagent mutation origin for verify-feedback loop"
```

---

### Task 4: Implement verify-fix loop as first-class main loop iteration in turn_loop.go

**Files:**
- Modify: `internal/agent/turn_loop.go`

**Interfaces:**
- Consumes: `VerifyLoopConfig` from `Agent`, `MergedVerification` from `verification_merge.go`, `runTurnLevelVerification` (refactored to return `MergedVerification`)
- Produces: verify-fix loop logic, new event emissions

- [ ] **Step 1: Refactor runTurnLevelVerification to return MergedVerification**

在 `agent.go` 中修改 `runTurnLevelVerification` 签名（约 line 335）：

```go
// runTurnLevelVerification runs test and review agent once per turn.
// Returns the merged verification result. Caller decides whether to persist
// or inject the results based on the verify-feedback loop state.
func (a *Agent) runTurnLevelVerification(ctx context.Context, sessionID string, emit func(AgentEvent) bool) MergedVerification {
```

函数体末尾改为 `return mergeVerificationResults(...)` 而非直接 persist + emit。

将 persist + emit 逻辑移到调用处（`turn_loop.go`）。

- [ ] **Step 2: Modify turn_loop.go — add verify-fix state variables**

在 `turn_loop.go` 的 goroutine 函数中，现有 state 变量区域（约 line 123-131），在 `progress := &progressTracker{}` 之后添加：

```go
// Verify-feedback loop state (Feature A).
verifyFixRound := 0
verifyFixIteration := false
var prevRoundFindings map[string]bool
```

- [ ] **Step 3: Inject verification results from subagent mutations (skip auto-fix)**

在 `turn_loop.go` 的主循环中，tool use 分支（约 line 267），当检测到 dirtySinceTurnTest 且 verifyFixIteration 为 false 时，不立即进入验证闭环。现有的 dispatch 管线会在 mutation 后设置 dirtySinceTurnTest。subagent mutation 也设置了它（通过 `appendDispatchedToolResult`）。

关键改动：当模型输出文本而不含 tool calls（line 390-462），且 `dirtySinceTurnTest == true` 且 `verifyFixIteration == false` 时，启动验证闭环而非直接 Done。

- [ ] **Step 4: Replace lines 447-462 (Done path) with verify-fix logic**

将现有的 lines 447-462：
```go
// Plan-as-reply finalization ...
if a.mode == session.ModePlan && strings.TrimSpace(assistant.Text) != "" {
    emit(AgentEvent{Type: AgentEventTypePlanCompleted, Content: assistant.Text})
}
if a.dirtySinceTurnTest {
    a.runTurnLevelVerification(ctx, sessionID, emit)
    a.dirtySinceTurnTest = false
}
emit(AgentEvent{Type: AgentEventTypeDone, Message: &assistant})
return
```

替换为：

```go
// === Turn finalization with verify-feedback loop (Feature A+B) ===

// Feature B: Pre-Done self-check nudge
if a.verifyLoopConfig.SelfCheck && strings.TrimSpace(assistant.Text) != "" {
    nudge := a.buildSelfCheckNudge()
    assistant.Text = strings.TrimSpace(assistant.Text) + "\n\n" + nudge
}

// Feature A: Verify-feedback loop entry
if a.verifyLoopConfig.Enabled && a.dirtySinceTurnTest && !verifyFixIteration {
    // Check context headroom before entering verify loop.
    headroom := a.estimateContextHeadroom(history, rt)
    maxRounds := a.verifyLoopConfig.MaxRounds
    if headroom < maxRounds*2000 {
        if headroom < 2000 {
            // Not enough for even 1 round; skip verification.
            emit(AgentEvent{
                Type: AgentEventTypeVerifyFixSkipped,
                VerifyFix: &VerifyFixInfo{Skipped: true, Reason: "insufficient context headroom"},
            })
            finalizeAndDone()
				return
        }
        maxRounds = headroom / 2000
    }

    // Run verification.
    emit(AgentEvent{Type: AgentEventTypeVerifyFixStarted})
    mv := a.runTurnLevelVerification(ctx, sessionID, emit)

    if mv.Passed || !hasP0P1FromOwn(mv.Findings) {
        // All clear. If there are subagent-only findings, include them in Done.
        emit(AgentEvent{Type: AgentEventTypeVerifyFixPassed})
        if mv.RawText != "" && mv.RawText != "All checks passed." {
            msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification ---\n"+mv.RawText, false)
            a.store.Create(ctx, msg)
        }
        finalizeAndDone()
				return
    }

    // Check for flaky findings.
    if a.verifyLoopConfig.IgnoreFlakyFindings && anyRepeatedFindings(mv.Findings, prevRoundFindings) {
        emit(AgentEvent{
            Type: AgentEventTypeVerifyFixSkipped,
            VerifyFix: &VerifyFixInfo{Skipped: true, Reason: "repeated findings — possible flaky test or pre-existing issue"},
        })
        // Still persist results so the user sees them.
        msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification (skipped auto-fix) ---\n"+mv.RawText, false)
        a.store.Create(ctx, msg)
        finalizeAndDone()
				return
    }
    prevRoundFindings = fingerprintFindings(mv.Findings)

    // Inject verification results and re-enter the main loop.
    msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification ---\n"+mv.RawText, false)
    created, err := a.store.Create(ctx, msg)
    if err != nil {
        emit(AgentEvent{Type: AgentEventTypeError, Err: err})
        return
    }
    rt.Log.Append(created)
    history = append(history, created)
    rt.Log.Append(assistant)
    history = append(history, assistant)

    verifyFixIteration = true
    verifyFixRound = 1
    emit(AgentEvent{
        Type: AgentEventTypeVerifyFixRoundStart,
        VerifyFix: &VerifyFixInfo{Round: 1, MaxRound: maxRounds},
    })

    if !emit(AgentEvent{Type: AgentEventTypeResponseReset}) {
        return
    }
    continue
}

// Handle verify-fix iteration: model responded to verification results.
if verifyFixIteration {
    if assistant.FinishReason == core.FinishReasonToolUse && toolMsg != nil {
        // Model is trying to fix issues — dispatch normally.
        verifyFixRound++
        if verifyFixRound > a.verifyLoopConfig.MaxRounds {
            // Round cap reached.
            emit(AgentEvent{
                Type: AgentEventTypeVerifyFixFailed,
                VerifyFix: &VerifyFixInfo{Round: verifyFixRound - 1, MaxRound: a.verifyLoopConfig.MaxRounds},
            })
            finalizeAndDone()
				return
        }
        toolIters++
        toolCalls += attemptedToolCalls
        rt.Log.Append(assistant)
        rt.Log.Append(*toolMsg)
        history = append(history, assistant, *toolMsg)

        // Re-run verification after fix.
        mv := a.runTurnLevelVerification(ctx, sessionID, emit)
        if mv.Passed || !hasP0P1FromOwn(mv.Findings) {
            emit(AgentEvent{Type: AgentEventTypeVerifyFixPassed})
            if mv.RawText != "" && mv.RawText != "All checks passed." {
                msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification ---\n"+mv.RawText, false)
                a.store.Create(ctx, msg)
            }
            finalizeAndDone()
				return
        }

        // Flaky check.
        if a.verifyLoopConfig.IgnoreFlakyFindings && anyRepeatedFindings(mv.Findings, prevRoundFindings) {
            emit(AgentEvent{
                Type: AgentEventTypeVerifyFixSkipped,
                VerifyFix: &VerifyFixInfo{Skipped: true, Reason: "repeated findings"},
            })
            finalizeAndDone()
				return
        }
        prevRoundFindings = fingerprintFindings(mv.Findings)

        // Inject results for another round.
        msg := core.TextMessage(sessionID, core.RoleTool, "--- Turn verification (round "+strconv.Itoa(verifyFixRound)+") ---\n"+mv.RawText, false)
        created, err := a.store.Create(ctx, msg)
        if err != nil {
            emit(AgentEvent{Type: AgentEventTypeError, Err: err})
            return
        }
        rt.Log.Append(created)
        history = append(history, created)

        emit(AgentEvent{
            Type: AgentEventTypeVerifyFixRoundStart,
            VerifyFix: &VerifyFixInfo{Round: verifyFixRound, MaxRound: a.verifyLoopConfig.MaxRounds},
        })
        if !emit(AgentEvent{Type: AgentEventTypeResponseReset}) {
            return
        }
        continue
    }
    // Model output text instead of tool calls — accepts the remaining issues.
    emit(AgentEvent{
        Type: AgentEventTypeVerifyFixFailed,
        VerifyFix: &VerifyFixInfo{Round: verifyFixRound, MaxRound: a.verifyLoopConfig.MaxRounds},
    })
    finalizeAndDone()
				return
}

// Normal completion path — call finalizeAndDone defined above.
finalizeAndDone()
return
```

- [ ] **Step 4a: Verify strconv import**

在 `turn_loop.go` 的 import 中确认 `"strconv"` 已导入。

- [ ] **Step 5: Add buildSelfCheckNudge helper to agent.go**

```go
func (a *Agent) buildSelfCheckNudge() string {
	if a.mode == session.ModePlan {
		return "Before finalizing the plan, verify: (1) all user requirements addressed? (2) each step is concrete and actionable? (3) dependencies between steps noted?"
	}
	return "Before finalizing, verify: (1) all parts of the user's request addressed? (2) tests pass? (3) output complete and correct?"
}
```

- [ ] **Step 6: Build and fix compilation errors**

```bash
go build ./internal/agent/...
```
Fix any variable shadowing or type mismatch errors. Expected: clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/turn_loop.go internal/agent/agent.go
git commit -m "feat(agent): implement verify-fix loop as first-class main loop iteration"
```

---

### Task 5: Skip storm/redundant counting during verifyFixIteration in stream_dispatch.go

**Files:**
- Modify: `internal/agent/stream_dispatch.go`

- [ ] **Step 1: Add verifyFixIteration check to dispatchToolCalls**

在 `stream_dispatch.go` 的 `dispatchToolCalls` 函数中，后处理 auto-verify 的代码块不会影响 loop guard。但 tool call 计数在 `turn_loop.go` 中处理（line 269: `toolCalls += attemptedToolCalls`）。verify-fix 迭代期间，我们已经在 `turn_loop.go` 中跳过了该行的递增（因为 verify-fix 分支直接处理 `toolIters++` 和 `toolCalls += attemptedToolCalls`）。

在这个 task 中，确认 verifyFixIteration 期间的 tool call 不会错误递增 `consecutiveStormRounds` / `consecutiveRedundantRounds`。

- [ ] **Step 1: Verify existing isolation is correct**

在 `turn_loop.go` 中，我们已经将 verify-fix 迭代的 tool-use 处理放到了独立的 `if verifyFixIteration` 分支中，该分支不使用主 loop 的 `consecutiveStormRounds++` / `consecutiveRedundantRounds++` 逻辑。确认这一点即可。

```bash
grep -n "consecutiveStormRounds\|consecutiveRedundantRounds" internal/agent/turn_loop.go
```
验证这些计数器只在非 verifyFixIteration 路径中被修改。

- [ ] **Step 2: Build verification**

```bash
go build ./internal/agent/...
```
Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add internal/agent/turn_loop.go
git commit -m "fix(agent): ensure verify-fix iterations don't affect storm/redundant counters"
```

---

### Task 6: Add context headroom check to turn_loop.go

**Files:**
- Modify: `internal/agent/turn_loop.go`

- [ ] **Step 1: Add estimateContextHeadroom helper**

在 `turn_loop.go` 底部添加：

```go
// estimateContextHeadroom estimates how many tokens of context window remain.
// Returns an approximation using the compact.EstimateMessagesTokens helper.
func (a *Agent) estimateContextHeadroom(history []core.Message, rt *memory.RuntimeState) int {
	used := compact.EstimateMessagesTokens(rt.BuildProviderHistory())
	return max(0, a.contextWindow-used)
}
```

需要 import `"github.com/usewhale/whale/internal/memory"`（可能已有）。

- [ ] **Step 2: Already wired in Task 4**

Task 4 的 verify-fix loop 入口已经包含了 headroom 检查。验证其正确性。

- [ ] **Step 3: Build and verify**

```bash
go build ./internal/agent/...
```

- [ ] **Step 4: Commit**

```bash
git add internal/agent/turn_loop.go
git commit -m "feat(agent): add context headroom check before verify-fix loop"
```

---

### Task 7: Structured forced summary in force_summary.go

**Files:**
- Modify: `internal/agent/force_summary.go`

- [ ] **Step 1: Replace forcedSummaryBanner with structured template**

将现有的 `forcedSummaryBanner` 函数（line 67-75）替换为结构化模板：

```go
func forcedSummaryBanner(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "execution limit reached"
	}
	return fmt.Sprintf(`⚠️ This turn was auto-interrupted: %s

## Completed
- (see summary below)

## Remaining
- (see summary below)

## Next Step
- Send another message or use /retry to continue from the current context.

## Summary
`, reason)
}
```

- [ ] **Step 2: Update forceSummary prompt to request structured output**

修改 `forceSummary` 函数中的 prompt（line 82）：

```go
prompt := fmt.Sprintf(
    "The run stopped: %s. Summarize concisely in this format:\n"+
    "## Completed\n- [specific completed items]\n"+
    "## Remaining\n- [what still needs to be done]\n"+
    "## Next Step\n- [concrete next action]\n"+
    "## Key Findings\n- [important discoveries]\n\n"+
    "Do not call tools.",
    strings.TrimSpace(reason),
)
```

- [ ] **Step 3: Build and verify**

```bash
go build ./internal/agent/...
go test ./internal/agent/... -run "Force" -v -count=1
```

- [ ] **Step 4: Commit**

```bash
git add internal/agent/force_summary.go
git commit -m "feat(agent): structured forced summary with Completed/Remaining/NextStep sections"
```

---

### Task 8: Task-Driven Termination Guard in turn_loop.go (Feature B2)

**Files:**
- Modify: `internal/agent/turn_loop.go`

**Interfaces:**
- Consumes: existing todo tool state from session store
- Produces: pre-termination check for incomplete todos

- [ ] **Step 1: Add checkIncompleteTodos helper**

在 `turn_loop.go` 底部添加：

```go
// checkIncompleteTodos scans the session history for incomplete todo items
// and returns a reminder message if any are found. Returns "" if all todos
// are complete or no todos exist.
func (a *Agent) checkIncompleteTodos(ctx context.Context, sessionID string) string {
	msgs, err := a.store.List(ctx, sessionID)
	if err != nil {
		return ""
	}
	var lastTodoMsg *core.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == core.RoleTool {
			for _, tr := range msgs[i].ToolResults {
				if strings.HasPrefix(tr.Name, "todo_") {
					lastTodoMsg = &msgs[i]
					break
				}
			}
			if lastTodoMsg != nil {
				break
			}
		}
	}
	if lastTodoMsg == nil {
		return "" // no todos in this session
	}
	// Parse the most recent todo_list result for incomplete items.
	for _, tr := range lastTodoMsg.ToolResults {
		if tr.Name == "todo_list" {
			incomplete := countIncompleteTodos(tr)
			if incomplete > 0 {
				return fmt.Sprintf("You have %d incomplete task(s). Are you sure you're done?", incomplete)
			}
			return ""
		}
	}
	return ""
}

// countIncompleteTodos counts todo items with status != "completed" in a
// todo_list tool result. The result contains a JSON array of {status: ...} objects.
func countIncompleteTodos(tr core.ToolResult) int {
	payload, ok := tr.Payload.(map[string]any)
	if !ok {
		return 0
	}
	items, ok := payload["items"].([]any)
	if !ok {
		// Try the model-visible text as a fallback.
		var parsed []struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(core.ToolResultModelText(tr)), &parsed); err != nil {
			return 0
		}
		count := 0
		for _, item := range parsed {
			if item.Status != "completed" {
				count++
			}
		}
		return count
	}
	count := 0
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			if status, ok := m["status"].(string); ok && status != "completed" {
				count++
			}
		}
	}
	return count
}
```

需要 import `"encoding/json"` 和 `"strings"`（如尚未导入）。

- [ ] **Step 2: Wire into finalizeAndDone closure**

在 `finalizeAndDone` 闭包中，DeepSeek 模型发出 Done 之前添加 todo 检查：

```go
finalizeAndDone := func() {
    // Feature B2: task-driven termination guard.
    if reminder := a.checkIncompleteTodos(ctx, sessionID); reminder != "" {
        // Inject as a visible nudge in the final assistant text.
        if strings.TrimSpace(assistant.Text) != "" {
            assistant.Text = strings.TrimSpace(assistant.Text) + "\n\n" + reminder
        }
    }
    // ... existing plan mode + cleanup + Done emit ...
}
```

- [ ] **Step 3: Build verification**

```bash
go build ./internal/agent/...
```
Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add internal/agent/turn_loop.go
git commit -m "feat(agent): add task-driven termination guard (Feature B2)"
```

---

### Task 9: Integration tests in agent_discipline_test.go (was Task 8)

**Files:**
- Modify: `internal/agent/agent_discipline_test.go`

- [ ] **Step 1: Add TestVerifyFixLoopBasic — no issues found**

```go
// TestVerifyFixLoopBasic verifies that when all checks pass, the turn
// completes normally without fix iterations.
func TestVerifyFixLoopBasic(t *testing.T) {
	// Setup: agent with VerifyLoopConfig enabled, no real mutations
	a := setupDisciplineAgent(t)
	a.verifyLoopConfig = VerifyLoopConfig{
		Enabled:    true,
		MaxRounds:  1,
		SelfCheck:  false,
	}
	a.dirtySinceTurnTest = true

	// Simulate a turn where verification passes
	// (use mock provider that returns "All checks passed.")
	// Verify: Done event emitted, VerifyFixPassed emitted
}
```

- [ ] **Step 2: Add TestVerifyFixLoopWithFindings — P0/P1 issues trigger fix**

```go
// TestVerifyFixLoopWithFindings verifies that P0 findings cause the
// loop to inject verification results and re-enter the main loop.
func TestVerifyFixLoopWithFindings(t *testing.T) {
	a := setupDisciplineAgent(t)
	a.verifyLoopConfig = VerifyLoopConfig{Enabled: true, MaxRounds: 2, SelfCheck: false}
	a.dirtySinceTurnTest = true

	// First model turn: outputs text (claims done)
	// Verification finds P0 issue
	// Model gets verification results, issues fix tool calls
	// Fix succeeds, re-verification passes
	// Done emitted
}
```

- [ ] **Step 3: Add TestVerifyFixLoopMaxRounds — cap reached**

```go
// TestVerifyFixLoopMaxRounds verifies the round cap terminates the loop.
func TestVerifyFixLoopMaxRounds(t *testing.T) {
	a := setupDisciplineAgent(t)
	a.verifyLoopConfig = VerifyLoopConfig{Enabled: true, MaxRounds: 1, SelfCheck: false}
	a.dirtySinceTurnTest = true

	// Verification finds P0 issue
	// Model attempts fix, but verification still fails
	// Round cap reached → VerifyFixFailed event → Done
}
```

- [ ] **Step 4: Add TestVerifyFixLoopFlakyDetection — repeated findings skipped**

```go
func TestVerifyFixLoopFlakyDetection(t *testing.T) {
	a := setupDisciplineAgent(t)
	a.verifyLoopConfig = VerifyLoopConfig{
		Enabled:             true,
		MaxRounds:           2,
		IgnoreFlakyFindings: true,
	}
	a.prevRoundFindings = map[string]bool{"file.go\x00same problem text": true}
	a.dirtySinceTurnTest = true

	// Verification finds same problem as prevRoundFindings
	// Should emit VerifyFixSkipped with "repeated findings"
}
```

- [ ] **Step 5: Add TestVerifyFixLoopSubagentOnly — subagent findings don't trigger fix**

```go
func TestVerifyFixLoopSubagentOnly(t *testing.T) {
	a := setupDisciplineAgent(t)
	a.verifyLoopConfig = VerifyLoopConfig{Enabled: true, MaxRounds: 1}
	a.dirtySinceTurnTest = true
	a.mutationsFromSubagent = map[string]bool{"pkg/foo.go": true}

	// Verification finds P0 in pkg/foo.go (subagent origin)
	// hasP0P1FromOwn returns false
	// Should emit VerifyFixPassed (skipping own findings)
}
```

- [ ] **Step 6: Add TestSelfCheckNudge — nudge appended to final assistant text**

```go
func TestSelfCheckNudge(t *testing.T) {
	a := setupDisciplineAgent(t)
	a.verifyLoopConfig = VerifyLoopConfig{SelfCheck: true}

	nudge := a.buildSelfCheckNudge()
	if !strings.Contains(nudge, "all parts of the user's request") {
		t.Errorf("self-check nudge missing expected content: %s", nudge)
	}
}
```

- [ ] **Step 7: Run tests**

```bash
go test ./internal/agent/... -run "VerifyFix|Flaky|Subagent|SelfCheck" -v -count=1 -timeout 120s
```
Expected: all new tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/agent_discipline_test.go
git commit -m "test(agent): add verify-fix loop integration tests (basic, findings, cap, flaky, subagent)"
```

---

### Task 10: Wire VerifyLoopConfig through app_config

**Files:**
- Modify: `internal/app/app_config.go`
- Modify: `internal/app/app_types.go`

- [ ] **Step 1: Add VerifyLoop section to config types**

在 `app_types.go` 中添加：

```go
// VerifyLoopConfigTOML is the TOML representation of verify-feedback loop config.
type VerifyLoopConfigTOML struct {
	Enabled      bool `toml:"enabled"`
	MaxRounds    int  `toml:"max_rounds"`
	SelfCheck    bool `toml:"self_check"`
}
```

在 `GateConfigTOML` 中新增字段（查找现有 gate config struct）：

```go
VerifyLoop VerifyLoopConfigTOML `toml:"verify_loop"`
```

- [ ] **Step 2: Wire to Agent in config_apply.go**

在 `config_apply.go` 中找到 `WithGateConfig` 的调用处，添加 `WithVerifyLoopConfig` 的 wiring：

```go
if cfg.Gate.VerifyLoop.Enabled {
    agentOpts = append(agentOpts, agent.WithVerifyLoopConfig(agent.VerifyLoopConfig{
        Enabled:    cfg.Gate.VerifyLoop.Enabled,
        MaxRounds:  cfg.Gate.VerifyLoop.MaxRounds,
        SelfCheck:  cfg.Gate.VerifyLoop.SelfCheck,
        // Other fields use defaults
    }))
}
```

- [ ] **Step 3: Build**

```bash
go build ./...
```
Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add internal/app/
git commit -m "feat(app): wire VerifyLoopConfig through app config to Agent"
```

---

### Task 11: Final integration test and manual verification

**Files:**
- Create: (none — all tests in existing test files)

- [ ] **Step 1: Run full agent test suite**

```bash
go test ./internal/agent/... -count=1 -timeout 300s
```
Expected: all tests pass, no regressions.

- [ ] **Step 2: Run full project test suite**

```bash
go test ./... -count=1 -timeout 300s
```
Expected: all tests pass.

- [ ] **Step 3: Run vet and staticcheck**

```bash
go vet ./internal/agent/...
```
Expected: no warnings.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "chore: final integration test pass, vet clean"
```
