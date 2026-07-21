# Agent Verify-Feedback Loop Design

## Overview

在现有 discipline 系统（P1-P4）基础上，将 turn-level verification 从"turn 结束后的一次性检查"升级为"turn 内的验证-修复闭环"，使模型在同一个 turn 内看到验证结果并自动修复问题，然后再输出最终答案。

架构师审查后修订。关键设计决策：
- 验证-修复轮次作为主 loop 的一等迭代（而非嵌套子循环），由 `verifyFixIteration` flag 标记
- 子 agent 产生的 mutation 跳过自动修复（主模型无上下文修复子 agent 的 bug）
- 内置 flaky test 检测（同一 finding 连续出现则跳过）
- 拆分两个 feature：代码级验证闭环 + turn 结束质量门

## Current State (post-discipline merge)

```
Turn N:   模型干活 → 输出答案 → Turn 结束
           ↓
         验证运行（test + review agent）→ 结果写入 history
          
Turn N+1: 模型看到验证结果 → 修复问题
```

**问题**：验证结果在下个 turn 才可见，用户看到的"完成"实际上是"未验证的完成"。

## Target State

```
Turn N:   模型干活 → 认为完成（输出文本而非 tool call）
           ↓
         验证运行（test + review agent）
           ↓
         有 P0/P1 issue？→ 注入结果到 history → 模型修复（作为主 loop 迭代）
           ↓                    ↓
         无关键 issue？      tool calls dispatched → 验证再次运行
           ↓                    ↓
         最终输出答案          循环直到通过或达上限(3轮)
```

---

## Feature A: 代码级验证闭环（Agent mode only）

### A1. Verify-Fix 作为主 Loop 一等迭代 (`turn_loop.go`)

**核心思路**：验证-修复轮次不创建嵌套子循环，而是作为主 `for` 循环的一个标记迭代。`verifyFixIteration` flag 区分"正常迭代"和"修复迭代"，避免与现有 guard 计数器冲突。

```
// 在 turn_loop.go 主循环中新增状态
verifyFixRound     := 0
verifyFixIteration := false

// 当模型输出文本（非 tool call）且准备 emit Done 时：
if dirtySinceTurnTest && !verifyFixIteration:
    verifyFixIteration = true  // 标记进入验证-修复模式
    // 运行验证（test + review agent）
    results := runTurnLevelVerification()
    if results has P0/P1 findings:
        if !isRepeatedFinding(results):
            inject results as tool message into history
            verifyFixRound++
            continue  // 回到主 loop 顶部，模型处理验证结果
        // else: 同一 finding 连续出现，跳过修复
    // 验证通过或无关键 issue
    verifyFixIteration = false
    dirtySinceTurnTest = false
    emit Done

if verifyFixIteration:
    // 修复迭代：模型看见验证结果后的响应
    if assistant has tool calls:
        // 正常 dispatch，但 storm/redundant 计数器用独立状态
        verifyFixRound++
        if verifyFixRound >= maxVerifyFixRounds:
            force summary  // 达到上限
            return
        continue  // 回到主 loop 顶部，重新验证
    else:
        // 模型选择不修复（输出文本）
        verifyFixIteration = false
        dirtySinceTurnTest = false
        emit Done
```

**关键约束**：
- `verifyFixIteration` 期间不递增 `modelTurns`、不触发 wrap-up nudge、不响应 plan loop nudge
- `verifyFixIteration` 期间的 tool calls 不递增主 loop 的 `consecutiveStormRounds` / `consecutiveRedundantRounds`
- `maxVerifyFixRounds = 3`（硬上限），默认从 1 开始

### A2. 子 Agent Mutation 归属 (`stream.go`)

子 agent（spawn_subagent）的结果通过 `appendDispatchedToolResult` 回传时会设置 `dirtySinceTurnTest = true`。但主模型没有子 agent 的完整上下文，无法修复其 bug。

**方案**：
- 在 `appendDispatchedToolResult` 中区分 "own mutation" 和 "subagent mutation"
- `runTurnLevelVerification` 的返回结果中标记每个 finding 的来源（`own` vs `subagent`）
- 验证闭环只对 `own` 来源的 P0/P1 findings 触发修复
- `subagent` 来源的 findings 在 Done 事件中报告但不触发自动修复

实现方式：新增 `mutationsFromSubagent` map，在 subagent 结果处理时标记文件路径。

### A3. Flaky Test 检测 (`turn_loop.go`)

如果同一 finding（相同 file + 相同 problem 文本的前 80 个字符）在连续验证轮次中出现 ≥2 次，跳过自动修复并 emit Done，附带 "possible flaky test or pre-existing issue" 标记。

```
type verifyFindingFingerprint struct {
    file    string
    problem string  // 前 80 字符
}
prevRoundFindings map[verifyFindingFingerprint]bool

// 在验证闭环开始前：
if isRepeatedFinding(currentFindings, prevRoundFindings):
    emit Done with warning: "finding persists across rounds — may be pre-existing"
    return
prevRoundFindings = fingerprint(currentFindings)
```

### A4. Context Headroom Check (`turn_loop.go`)

进入验证闭环前检查 context 余量。每个修复轮次预估 ~2000 tokens。如果 `(contextWindow - estimatedUsage) < maxVerifyFixRounds * 2000`，缩减 `maxVerifyFixRounds` 或跳过验证。

### A5. 验证闭环事件类型 (`agent.go`)

```go
AgentEventTypeVerifyFixStarted     AgentEventType = "verify_fix_started"
AgentEventTypeVerifyFixRoundStart  AgentEventType = "verify_fix_round_start"
AgentEventTypeVerifyFixRoundResult AgentEventType = "verify_fix_round_result"
AgentEventTypeVerifyFixPassed      AgentEventType = "verify_fix_passed"
AgentEventTypeVerifyFixFailed      AgentEventType = "verify_fix_failed"
AgentEventTypeVerifyFixSkipped     AgentEventType = "verify_fix_skipped"
```

UI 层可以用这些事件展示进度（spinner / "verifying..." / "fixing issues..." / "retrying..."）。

### A6. VerifyLoopConfig (`agent.go`)

```go
type VerifyLoopConfig struct {
    Enabled             bool
    MaxRounds           int           // 最大修复轮数，默认 1，最大 3
    SelfCheck           bool          // Pre-Done 自检 nudge
    PerRoundTimeout     time.Duration // 单轮验证超时，默认 120s
    TotalTimeout        time.Duration // 验证闭环总超时，默认 300s
    DegradeOnFailure    bool          // review agent 连续失败时降级
    IgnoreFlakyFindings bool          // 检测并跳过重复 finding
}

func DefaultVerifyLoopConfig() VerifyLoopConfig {
    return VerifyLoopConfig{
        Enabled:             false, // 默认关闭，通过配置显式启用
        MaxRounds:           1,
        SelfCheck:           true,
        PerRoundTimeout:     120 * time.Second,
        TotalTimeout:        300 * time.Second,
        DegradeOnFailure:    true,
        IgnoreFlakyFindings: true,
    }
}
```

---

## Feature B: Turn 结束质量门（所有 mode）

### B1. Pre-Done Self-Check Nudge (`turn_loop.go`)

在模型输出最终文本且准备 emit `Done` 时，追加一条内部检查消息：

```
"Before finalizing, verify: (1) all parts of the user's request addressed?
(2) tests pass? (3) output complete and correct?"
```

在 Plan mode 中，检查内容不同：
```
"Before finalizing the plan, verify: (1) all user requirements addressed?
(2) each step is concrete and actionable? (3) dependencies noted?"
```

通过 `VerifyLoopConfig.SelfCheck` 控制。

### B2. Task-Driven Termination Guard (`turn_loop.go`)

在准备 emit `Done` 前：
- 通过 `a.store.List` 检查当前 session 中是否有未完成的 todo 项
- 如果有，注入提醒消息但不强制阻止
- 模型可以选择忽略（有些 todo 是故意的占位符）或处理

### B3. Structured Forced Summary (`force_summary.go`)

改进 forced summary 的输出格式：

```
⚠️ This turn was auto-interrupted: [reason]

## Completed
- [具体完成事项]

## Remaining
- [剩余工作]

## Next Step
- [建议的续接步骤]

## Key Findings
- [重要信息]
```

### B4. 结构化验证结果 (`verification_merge.go`)

`mergeVerificationResults` 从返回纯文本改为返回结构化数据：

```go
type MergedVerification struct {
    Passed   bool
    Findings []VerificationFinding
    RawText  string  // 保留原始渲染文本用于注入 history
}

type VerificationFinding struct {
    Severity string   // P0, P1, P2, P3
    File     string
    Line     string
    Problem  string
    Fix      string
    Sources  []string // verify, test, review
    Origin   string   // "own" or "subagent"
}
```

验证闭环通过 `Findings` 字段做结构化判断（是否有 P0/P1、是否重复、来源归属），`RawText` 用于注入 history 供模型阅读。

---

## Files Changed

| 文件 | 改动 |
|------|------|
| `internal/agent/turn_loop.go` | Feature A: verify-fix 作为主 loop 一等迭代、flaky 检测、context headroom check；Feature B: self-check nudge、task guard |
| `internal/agent/agent.go` | 新增 `VerifyLoopConfig`、`WithVerifyLoopConfig` option、事件类型、verify-fix 状态字段 |
| `internal/agent/stream.go` | 子 agent mutation 归属标记、`dirtySinceTurnTest` 来源区分 |
| `internal/agent/stream_dispatch.go` | verifyFixIteration 期间跳过 storm/redundant 计数 |
| `internal/agent/verification_merge.go` | 返回 `MergedVerification` 结构化数据 |
| `internal/agent/force_summary.go` | 结构化 forced summary 模板 |
| `internal/agent/agent_discipline_test.go` | 验证闭环测试用例 |

## Non-Goals

- 不改变现有 P1-P4 discipline gates 的触发逻辑
- 不改变 review agent 的 prompt 或模型选择
- 不改变 tool dispatch 的并行模型
- 不改变 hook 系统
- review agent 降级状态机留到第二轮

## Risks & Mitigations (updated)

| 风险 | 缓解 |
|------|------|
| Token 消耗增加 | 只在 `dirtySinceTurnTest == true` 时触发；默认 `MaxRounds=1`；context headroom check |
| 验证循环死锁 | `MaxRounds` 硬上限；flaky finding 检测跳过重复 |
| 子 agent 归因错误 | 区分 own/subagent mutation，subagent 来源不触发自动修复 |
| 简单任务变慢 | 只在有代码变更时触发；默认 1 轮 |
| review agent 不可用 | 降级：只跑 test；事件通知 UI |
| force summary 冲突 | 验证闭环在 force summary 触发前检查，force summary 时跳过验证 |
| context 压缩打断验证 | 进入验证闭环前做 headroom check，不足时缩减轮数 |
