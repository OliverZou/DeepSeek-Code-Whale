# Whale Agent 纪律性改进计划

## 问题

Whale 单 Agent 模式缺少"理解→修改→验证"的强制闭环。三个核心差距：

1. **无"先理解再修改"的门控** — LLM 可以不读文件就直接编辑
2. **无"改完必须验证"的闭环** — 编辑后不自动运行编译/测试/linter
3. **无"最小变更"的约束** — 系统提示没有明确的最小变更原则

## 改进方案

### P1: 编辑前必须先读（Pre-edit Read Gate）

**原理**：在 `stream_dispatch.go` 的工具调度链中，edit/write/multi_edit 执行前检查目标文件是否在当前 turn 被读取过。未读则拦截，返回错误提示。

**实现**：

1. 在 `Agent` 结构体（`agent.go`）中新增 turn 级别的文件读取追踪：

```go
// agent.go
type Agent struct {
    // ...
    filesReadThisTurn map[string]bool // turn 级别，turn 开始时重置
}
```

注意：使用普通 `map` 而非 `sync.Map`，因为每个 session 的 turn 是串行的，无并发访问。

2. 在 `stream_dispatch.go` 的工具执行成功后（而非 `catalog_files.go` 内部），记录 read_file 的目标路径到 `filesReadThisTurn`。原因是 `Toolset` 无法访问 `Agent` 的字段，而 `stream_dispatch.go` 中有 `Agent` 的访问权：

```go
// stream_dispatch.go — 工具执行成功后
if call.Name == "read_file" && primarySucceeded {
    if fp := extractFilePath(call); fp != "" {
        a.filesReadThisTurn[fp] = true
    }
}
```

3. 在 `stream_dispatch.go` 的 `dispatchToolCalls` 中，edit/write/multi_edit 执行前增加检查：

```go
// stream_dispatch.go — 在 runPreToolUseHook 之后、实际执行之前
if isMutationTool(call.Name) {
    filePath := extractFilePath(call)
    if _, read := a.filesReadThisTurn[filePath]; !read {
        results = append(results, core.ToolResult{
            ToolCallID: call.ID,
            Name:       call.Name,
            ModelText:  fmt.Sprintf("File %q has not been read in this turn. Use read_file first to inspect the current content before editing.", filePath),
            Outcome:    core.OutcomeFailure,
            Code:       "read_before_edit_required",
        })
        continue
    }
}
```

4. Turn 开始时重置 `filesReadThisTurn`（在 `turn_loop.go` 的循环顶部）：

```go
a.filesReadThisTurn = make(map[string]bool)
```

5. `isMutationTool` 判断 `edit`/`write`/`multi_edit` 三个工具名。`extractFilePath` 从 tool call input 中解析 `file_path` 字段。

**豁免**：
- `write` 创建新文件（文件不存在时）不需要先读。检查 `os.Stat` 判断文件是否存在。
- MCP 工具的写操作暂不纳入（无法统一提取文件路径）。

**改动文件**：
- `internal/agent/agent.go` — 新增 `filesReadThisTurn` 字段
- `internal/agent/turn_loop.go` — turn 开始时重置
- `internal/agent/stream_dispatch.go` — 新增 pre-edit gate 检查 + read_file 成功后记录

### P2: 编辑后自动验证（Post-edit Auto-verify）

**原理**：edit/write/multi_edit 成功后，自动运行两层验证——先跑构建/测试命令（机器验证），再对 diff 做轻量审查（语义验证），将结果注入工具返回值，LLM 无需主动调用 shell 即可看到验证结果。

**设计原则**：语言无关。核心机制是通用的"运行命令→收集输出→注入结果"，不绑定任何语言。验证命令由用户配置或从项目文件自动推断，whale 本身不假设项目使用什么语言或构建工具。

**与现有 `/review` 的关系**：

| | 现有 `/review` | P2 轻量自动审查 |
|---|---|---|
| 触发方式 | 用户手动 | 编辑后自动 |
| 审查范围 | 整体改动（多文件、多 commit） | 单次编辑的 diff |
| 审查深度 | correctness + security + maintainability 全量 | 仅检查"是否最小变更"+"是否引入明显问题" |
| 时机 | 改完之后 | 改完立即 |

`/review` 的 prompt 生成逻辑（`internal/commands/review.go` 的 `buildReviewPrompt`）和审查维度不直接复用，但其核心思路——用 diff 驱动审查——是 P2 的基础。

**实现**：

#### 第一层：构建/测试验证（机器验证）

1. 在 `~/.whale/config.toml` 中新增 `[verify]` 段（复用现有配置文件，不新建）：

```toml
[verify]
# 编辑后自动运行的验证命令，按优先级排列
# 任何 shell 命令均可，不限于特定语言
commands = ["go build ./...", "go test ./..."]
# 最大等待时间
timeout = "30s"
# 是否在验证失败时自动回滚（默认 false）
rollback_on_failure = false
```

2. 在 `stream.go` 的 `appendDispatchedToolResult` 中，PostToolUse hook 之前，对成功的 mutation 工具结果追加自动验证：

```go
// stream.go — appendDispatchedToolResult 中
if isMutationTool(call.Name) && primarySucceeded {
    if verifyResult := a.runAutoVerify(ctx); verifyResult != "" {
        finalRes.ModelText += "\n\n--- Auto-verify ---\n" + verifyResult
    }
}
```

3. `runAutoVerify` 读取配置，依次执行命令，收集输出。超时则截断。

4. **Debounce**：LLM 可能在同一 turn 连续调用多次 edit/multi_edit，每次都跑构建会严重拖慢速度。解决方案：用"脏标记"延迟验证，不在每次编辑后立即执行，而是在 turn 内最后一次编辑完成后、下一轮 LLM 推理前统一执行一次。

```go
// agent.go
type Agent struct {
    // ...
    dirtySinceVerify bool // 本 turn 是否有未验证的编辑
}

// stream_dispatch.go — mutation 工具成功后设置脏标记
if isMutationTool(call.Name) && primarySucceeded {
    a.dirtySinceVerify = true
}

// turn_loop.go — 下一轮 LLM 推理前，如果有脏标记则执行验证
if a.dirtySinceVerify {
    if result := a.runAutoVerify(ctx); result != "" {
        // 将验证结果作为系统消息注入到历史中
        history = append(history, core.Message{
            Role:    core.RoleTool,
            Content: "--- Auto-verify ---\n" + result,
        })
    }
    a.dirtySinceVerify = false
}
```

5. 如果用户未配置 `[verify]`，从项目特征文件自动推断（仅作为便利默认值，用户配置优先）：

| 项目特征文件 | 推断命令 |
|-------------|---------|
| `go.mod` | `go build ./...` |
| `package.json`（含 `scripts.test`） | `npm test` |
| `Cargo.toml` | `cargo check` |
| `pyproject.toml` | `ruff check . && pytest` |
| `pom.xml` / `build.gradle` | `mvn compile` / `gradle build` |
| `Makefile`（含 `check`/`test` target） | `make check` 或 `make test` |

推断结果可被用户显式配置覆盖。推断逻辑可扩展，不硬编码在 whale 核心中，而是作为内置的"推断策略"注册表。

#### 第二层：轻量 diff 审查（语义验证）

复用 whale 已有的 diff 基础设施（`internal/tools/diff_metadata.go` 的 `unifiedDiff` + `fileDiffCounts`），在 mutation 工具成功后自动生成 diff，并注入精简审查提示：

1. **diff 已有**：`fileDiffMetadata` 已经在 edit/multi_edit/write 的工具结果中生成 unified diff（含 additions/deletions 计数）。P2 直接复用这个 diff，不需要重新计算。

2. **轻量审查提示**：在 diff 注入工具结果时，追加精简的审查引导（不调用 LLM，仅注入提示文本让当前 LLM 在下一轮自行判断）：

```go
// stream.go — appendDispatchedToolResult 中，在第一层验证之后
if isMutationTool(call.Name) && primarySucceeded {
    additions, deletions := diffCountsFromResult(finalRes)
    if additions+deletions > 20 {
        finalRes.ModelText += fmt.Sprintf(
            "\n\n--- Change summary ---\n%d additions, %d deletions. Verify: (1) every change traces to the user's request, (2) no unrelated refactoring, (3) no missing error handling for new paths.",
            additions, deletions,
        )
    }
}
```

3. **阈值可配置**：审查提示的触发行数阈值（默认 20 行）可在 `[verify]` 中配置：

```toml
[verify]
review_threshold = 20  # 超过此行数时追加审查提示
```

4. **不做完整 review**：P2 的轻量审查只关注"最小变更"和"明显问题"，不替代 `/review` 的全量审查。完整审查维度（correctness/security/maintainability）仍由用户手动触发 `/review`。

**改动文件**：
- `internal/agent/agent.go` — 新增 `runAutoVerify` 方法、`dirtySinceVerify` 字段、配置加载
- `internal/agent/turn_loop.go` — debounce 验证执行点
- `internal/agent/stream.go` — mutation 工具成功后调用验证 + 轻量审查提示
- `internal/tools/diff_metadata.go` — 复用现有 diff 计算（无需修改）
- `internal/tools/file_mutation.go` 或 `internal/tools/file_state.go` — 验证配置解析

### P3: 最小变更原则（Minimal Change Principle）

**原理**：通过系统提示 + 工具描述，双重软约束 LLM 的修改范围。diff 大小警告已合并到 P2 第二层。

**实现**：

1. **系统提示**（`system_prompt.go`）新增不可变 block：

```go
func renderMinimalChangeBlock() string {
    return strings.TrimSpace(`
Minimal change principle.

- Every line you modify must be directly traceable to the user's request.
- Do not refactor, reformat, or "improve" adjacent code unless the user asks.
- Do not add flexibility, configurability, or error handling for scenarios the user did not mention.
- If you can solve the problem in 50 lines, do not write 200.
- Prefer edit/multi_edit over write for existing files. Use write only for new files or intentional full rewrites.
- After editing, verify the change is correct before moving on.
`)
}
```

在 `buildImmutableSystemBlocksWithTools` 中追加此 block，与 `renderToolPolicyBlock()`、`renderDelegationPolicyBlock()` 同级——都是行为策略类的不可变约束：

```go
// system_prompt.go — buildImmutableSystemBlocksWithTools
systemBlocks = append(systemBlocks, renderMinimalChangeBlock())
```

2. **write 工具降级提示**（`catalog_files.go`）：修改 write 工具的 description，在开头加警告：

```
"WARNING: This tool overwrites the entire file. For existing files, prefer multi_edit for surgical changes. Use write only for new files or when you intentionally need a full rewrite."
```

**改动文件**：
- `internal/agent/system_prompt.go` — 新增 `renderMinimalChangeBlock`
- `internal/tools/catalog_files.go` — write 工具描述修改

### P4: 结构化根因分析（Structured Root Cause Analysis）

**原理**：在 Plan 模式的基础上，为 Agent 模式增加轻量级的"分析阶段"——edit 工具解锁前，LLM 必须先输出对问题的理解。

**实现**：

1. 新增 turn 级别的状态标记 `analysisProvided`：

```go
// agent.go
type Agent struct {
    // ...
    analysisProvidedThisTurn bool
    skipAnalysisThisTurn     bool // 用户明确跳过时设置
}
```

2. 新增"分析工具" `analyze_problem`（只读工具），LLM 调用它来声明对问题的理解。工具本身只做记录，门控逻辑在 `stream_dispatch.go` 中：

```go
// catalog_runtime.go — 工具定义
toolFn{
    name:        "analyze_problem",
    description: "Record your analysis of the root cause before making changes. Required before edit/write/multi_edit in a turn. State: what is the observed behavior, what is the expected behavior, and what is the root cause.",
    parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "observed":   map[string]any{"type": "string", "description": "What is happening (the bug or problem)"},
            "expected":   map[string]any{"type": "string", "description": "What should happen instead"},
            "root_cause": map[string]any{"type": "string", "description": "Why the observed behavior differs from expected"},
        },
        "required": []string{"observed", "expected", "root_cause"},
    },
    readOnly: true,
    fn:       b.analyzeProblem,
}
```

```go
// tool_fn.go 或 catalog_runtime.go — 工具实现
func (b *Toolset) analyzeProblem(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
    var args struct {
        Observed  string `json:"observed"`
        Expected  string `json:"expected"`
        RootCause string `json:"root_cause"`
    }
    if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
        return core.ToolResult{ToolCallID: call.ID, Name: call.Name, ModelText: "Invalid input", Outcome: core.OutcomeFailure, Code: "invalid_input"}, nil
    }
    if strings.TrimSpace(args.Observed) == "" || strings.TrimSpace(args.Expected) == "" || strings.TrimSpace(args.RootCause) == "" {
        return core.ToolResult{
            ToolCallID: call.ID, Name: call.Name,
            ModelText: "All three fields (observed, expected, root_cause) must be non-empty.",
            Outcome: core.OutcomeFailure, Code: "empty_field",
        }, nil
    }
    return core.ToolResult{
        ToolCallID: call.ID, Name: call.Name,
        ModelText: "Analysis recorded. You may now use edit/write/multi_edit tools.",
        Outcome: core.OutcomeSuccess, Code: "ok",
    }, nil
}
```

注意：工具函数内部不设置 `Agent.analysisProvidedThisTurn`，因为 `Toolset` 无法访问 `Agent`。标记的设置在 `stream_dispatch.go` 中完成。

3. 在 `stream_dispatch.go` 中，edit/write/multi_edit 执行前检查 `analysisProvidedThisTurn`；`analyze_problem` 执行成功后设置标记：

```go
// stream_dispatch.go — analyze_problem 成功后设置标记
if call.Name == "analyze_problem" && primarySucceeded {
    a.analysisProvidedThisTurn = true
}

// stream_dispatch.go — mutation 工具执行前检查
if isMutationTool(call.Name) && !a.analysisProvidedThisTurn && !a.skipAnalysisThisTurn {
    results = append(results, core.ToolResult{
        ToolCallID: call.ID,
        Name:       call.Name,
        ModelText:  "You must call analyze_problem first to record your root cause analysis before making changes.",
        Outcome:    core.OutcomeFailure,
        Code:       "analysis_required_before_edit",
    })
    continue
}
```

4. Turn 开始时重置 `analysisProvidedThisTurn = false`、`skipAnalysisThisTurn = false`。

**豁免**：
- 如果用户明确说"直接改"或"不要分析"，跳过此检查。实现方式：在 `UserPromptSubmit` hook 中检测用户消息中的关键词（如"直接改"、"skip analysis"、"just fix it"），设置 `a.skipAnalysisThisTurn = true`。门控检查时如果此标记为 true 则放行。
- 用户可在 `[gate]` 配置中设置 `analysis_threshold = 5`，当 LLM 请求编辑的文件总变更行数（由 LLM 在 analyze_problem 中预估）低于此值时自动豁免。

**改动文件**：
- `internal/agent/agent.go` — 新增 `analysisProvidedThisTurn`、`skipAnalysisThisTurn` 字段
- `internal/agent/turn_loop.go` — turn 开始时重置
- `internal/agent/stream_dispatch.go` — 新增 analysis gate 检查 + analyze_problem 成功后设置标记
- `internal/tools/catalog_runtime.go` — 新增 `analyze_problem` 工具

## 跨领域问题

### Subagent / spawn_subagent 的门控

子代理（spawn_subagent）的编辑是否也受 P1/P4 的门控？如果不受，LLM 可以通过 spawn_subagent 绕过所有门控。

**结论：子代理应继承父代理的门控状态，但独立维护自己的追踪。**

- `spawn_subagent` 创建的子 `Agent` 实例应初始化 `filesReadThisTurn` 和 `analysisProvidedThisTurn` 为空/false。
- 子代理在自己的 turn 循环中独立遵守门控规则——它也必须先读再改、先分析再改。
- 这意味着子代理会自然地多花 1-2 轮工具调用，但换来的是同样的纪律性保障。
- `parallel_reason` 不涉及文件编辑，无需门控。

### 配置统一

P1、P2、P4 的门控都应可在 `~/.whale/config.toml` 中统一配置：

```toml
[gate]
# P1: 编辑前是否必须先读文件
read_before_edit = true    # 默认 true
# P4: 编辑前是否必须先调用 analyze_problem
analyze_before_edit = true # 默认 true
# P4: 变更行数低于此值时豁免分析
analysis_threshold = 5     # 默认 5

[verify]
# P2: 编辑后自动运行的验证命令
commands = []
timeout = "30s"
rollback_on_failure = false
# P2: 超过此行数时追加审查提示
review_threshold = 20      # 默认 20
```

## 优先级与依赖

```
P3 (最小变更原则) ── 独立，可立即实施，零风险
    │
P1 (编辑前必须先读) ── 独立，可立即实施，低风险
    │
P2 (编辑后自动验证) ── 依赖 P1（先读再改再验，形成闭环）
    │
P4 (结构化根因分析) ── 依赖 P1，与 P2 并行，形成完整闭环
```

**建议实施顺序**：P3 → P1 → P2 → P4

- P3 是纯提示层改动，零代码风险，立竿见影
- P1 是代码层门控，但逻辑简单，不影响现有功能
- P2 在 P1 基础上补齐验证闭环
- P4 最复杂，需要新增工具和交互模式，放最后

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| P1 拦截合法的快速修改 | 新文件豁免；提供 `[gate] read_before_edit = false` 配置项关闭 |
| P2 构建验证耗时过长 | 30s 超时；可配置关闭；debounce 避免重复执行 |
| P2 自动推断错误 | 用户可在 `[verify]` 中显式指定，覆盖推断 |
| P2 轻量审查提示被忽略 | 提示是软约束，硬约束靠 P1（先读）+ P4（先分析） |
| P3 提示被 LLM 忽略 | 提示只是软约束，硬约束靠 P1/P2/P4 |
| P4 增加交互轮次 | 简单修改豁免（`analysis_threshold`）；用户可显式跳过 |
| P4 analyze_problem 被滥用为形式主义 | 要求填写 observed/expected/root_cause 三个字段，不允许留空 |
| Subagent 绕过门控 | 子代理独立维护门控状态，同样遵守先读再改、先分析再改 |
| P2 debounce 期间 LLM 已基于旧状态继续推理 | 验证结果作为系统消息注入历史，LLM 下一轮可见 |