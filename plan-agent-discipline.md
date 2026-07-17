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

2. 在 `stream.go` 的 `appendDispatchedToolResult` 中，工具执行成功后记录 read_file 和成功 mutation 的目标路径：

```go
// stream.go — 工具执行成功后
if call.Name == "read_file" && primarySucceeded {
    a.recordFileRead(call)
}
// P1: successful mutation also counts as having "read" the file,
// so a subsequent edit in the same turn is not blocked.
if isMutationTool(call.Name) && primarySucceeded {
    a.recordFileRead(call)
}
```

3. 在 `stream_dispatch.go` 的 `dispatchToolCalls` 中，edit/write/multi_edit 执行前增加检查。**门控拦截结果必须走 `appendToolResult`**（与 hook 拦截、policy 拒绝等路径一致），确保事件通道、WarnPrefix、telemetry 不丢失：

```go
// stream_dispatch.go — 在 runPreToolUseHook 之后、实际执行之前
if isMutationTool(call.Name) && a.gateReadBeforeEdit {
    if blocked := a.checkReadBeforeEditGate(ctx, &sc, call, &results); blocked {
        continue
    }
}
```

`checkReadBeforeEditGate` 内部使用 `appendToolResult` 而非直接 `results = append(...)`，保证 UI 事件流、telemetry、autoDenyCounts 统计完整。

4. Turn 开始时通过 `resetTurnState()` 重置 `filesReadThisTurn`（在 `turn_loop.go` 的循环顶部调用）。

5. `isMutationTool` 判断 `edit`/`write`/`multi_edit` 三个工具名。`extractFilePath` 从 tool call input 中解析 `file_path` 字段。

6. 路径归一化：`normalizeWorkspacePath` 在 Windows 上做 `strings.ToLower`，避免大小写不一致导致门控误判。

**豁免**：
- `write` 创建新文件（文件不存在时）不需要先读。检查 `os.Stat` 判断文件是否存在。
- MCP 工具的写操作暂不纳入（无法统一提取文件路径）。
- `workspaceRoot == ""` 时跳过门控（测试环境兼容）。

**改动文件**：
- `internal/agent/agent.go` — 新增 `filesReadThisTurn` 字段、`recordFileRead`、`normalizeWorkspacePath`
- `internal/agent/turn_loop.go` — turn 开始时 `resetTurnState()`
- `internal/agent/stream_dispatch.go` — 新增 `checkReadBeforeEditGate`（使用 `appendToolResult`）
- `internal/agent/stream.go` — read_file 和 mutation 成功后 `recordFileRead`

### P2: 编辑后自动验证（Post-edit Auto-verify）

**原理**：edit/write/multi_edit 成功后，自动运行三层验证——构建/lint 命令（机器验证）、测试命令（测试验证）、第三方 review agent（语义验证），将结果合并后注入模型上下文。

**设计原则**：语言无关。核心机制是通用的"运行命令→收集输出→合并结果"，不绑定任何语言。验证命令由用户配置或从项目文件自动推断。

**与现有 `/review` 的关系**：

| | 现有 `/review` | P2 自动验证 |
|---|---|---|
| 触发方式 | 用户手动 | 编辑后自动 |
| 审查范围 | 整体改动 | 当前 dispatch 批次的 diff |
| 审查深度 | 全量 | 构建错误 + 测试失败 + P0-P3 严重度 findings |
| 时机 | 改完之后 | dispatch 批次结束后立即 |

#### Debbounce 机制

LLM 可能在同一 dispatch 批次中连续调用多次 edit/multi_edit。用"脏标记"延迟验证，在 dispatch 批次结束后统一执行一次：

```go
// stream.go — mutation 工具成功后设置脏标记
if isMutationTool(call.Name) && primarySucceeded {
    a.dirtySinceVerify = true
}
```

验证在 `stream_dispatch.go` 的 `dispatchToolCalls` 末尾执行（在 `flushPendingParallelBatches` 之后、`createDispatchToolMessage` 之前）。

#### 第一层：构建/lint 验证

1. 配置在 `[verify]` 段：

```toml
[verify]
commands = ["go build ./..."]  # 用户显式配置
timeout = "30s"
review_threshold = 20
```

2. 自动推断（用户未配置时）：从项目特征文件检测验证命令。**安全机制**：如果配置文件（package.json、Makefile 等）在当前 turn 被修改过，跳过自动推断的命令（`autoDetectedConfigDirty`），防止权限升级攻击。

| 项目特征文件 | 推断命令 |
|-------------|---------|
| `go.mod` | `go build ./...` |
| `package.json`（含 `scripts.build` + `scripts.lint`） | `npm run build && npm run lint` |
| `package.json`（仅含 `scripts.build`） | `npm run build` |
| `Makefile`（含 `lint` target） | `make lint` |
| `pyproject.toml` | `ruff check .` |

3. `execVerifyCommand` 使用 `os/exec` 直接执行（不走审批通道），设 `WaitDelay=5s` 防止孙进程挂死，`CombinedOutput` 收集输出。

4. 错误分支同时输出 `CombinedOutput` 诊断内容和 `err.Error()`，确保模型能看到编译错误详情。

#### 第二层：测试验证

1. 配置在 `[verify]` 段：

```toml
[verify]
test_commands = ["go test ./... -count=1"]  # 用户显式配置
test_timeout = "60s"
```

2. **自动推断默认关闭**：全量测试太慢，用户需显式配置 `[verify] test_commands`。

#### 第三层：第三方 Review Agent

1. 独立 HTTP 客户端（与 Classifier 同模式），不复用主 agent 的 provider：

```toml
[verify]
review_agent = true
review_model = "deepseek-v4-pro"
# review_api_key 优先使用 DEEPSEEK_API_KEY 环境变量
review_base_url = "https://api.deepseek.com"  # 优先使用 DEEPSEEK_BASE_URL 环境变量
```

2. API key 优先使用环境变量（`DEEPSEEK_API_KEY`、`DEEPSEEK_BASE_URL`），配置文件作为 fallback。避免在项目 TOML 中明文存储密钥。

3. Review agent 输出 P0-P3 严重度的 findings，每个 finding 包含文件、行号、问题描述和修复建议。

#### 并行执行与结果合并

三层验证在 dispatch 批次结束后并行执行（3 个 goroutine），全部完成后合并：

1. `parseReviewFindings` 解析 review agent 的 FINDING 输出（兼容 em-dash `—` 和普通 dash `-`）
2. `extractFileRefs` 从 verify/test 输出中提取文件引用（仅从含错误指示的行提取，防止成功输出误报；使用正则捕获组而非 `SplitN`，Windows 路径安全）
3. `mergeVerificationResults` 合并去重：统一编号、按严重度排序、来源标注、未覆盖错误自动提升
4. 验证结果作为**独立的合成 tool result** 追加到 results 中（不修改已有 result 的 ModelText），保持 PostToolUse hook 不变量

**Fallback**：如果 findings 为空但 verify/test 含输出，原样透传原始输出，确保模型能看到构建失败。

#### Diff 自审提示

每次成功 mutation 后，根据 diff 行数追加精简审查引导：

- > 20 行：完整审查提示（3 项检查）
- > 0 行：精简提示（1 项检查）
- 0 行：不追加

#### Test Reminder

如果当前 turn 修改了源文件但未修改测试文件，追加提醒。

**改动文件**：
- `internal/agent/agent.go` — `runAutoVerify`、`runAutoTest`、`runCommands`、`execVerifyCommand`、`runReviewAgent`、`autoDetectVerifyCommands`、`autoDetectTestCommands`、`autoDetectedConfigDirty`、`resolveVerifyCommands`、`resolveTestCommands`、`VerifyConfig` struct
- `internal/agent/stream_dispatch.go` — debounce 验证执行点、并行 goroutine、结果合并、diff 自审
- `internal/agent/stream.go` — `dirtySinceVerify`、`sourceFilesThisTurn`/`testFilesThisTurn`、`mutationsChangeCountThisTurn`、diff 自审提示
- `internal/agent/verification_merge.go` — `mergeVerificationResults`、`parseReviewFindings`、`extractFileRefs`、`extractLinesForFile`
- `internal/agent/reviewer_prompt.go` — review agent 独立 system prompt
- `internal/app/config_file.go` — `FileVerifyConfig`、`FileGateConfig`
- `internal/app/config_apply.go` — `applyVerifyConfig`、`applyGateConfig`
- `internal/app/app_types.go` — Config 全部 verify/gate 字段
- `internal/app/app_config.go` — `DefaultConfig()` 中 gate 默认值 true
- `internal/app/runtime.go` — `WithVerifyConfig`、`WithGateConfig`
- `internal/tasks/runner.go` — `RunnerConfig` verify/gate/review 字段
- `internal/tasks/subagent.go` — `newChild` 传递 `WithVerifyConfig` + `WithGateConfig`

### P3: 最小变更原则（Minimal Change Principle）

**原理**：通过系统提示 + 工具描述，双重软约束 LLM 的修改范围。P3 是纯提示层改动，无运行时状态。

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

在 `buildImmutableSystemBlocksWithTools` 中追加此 block。

2. **write 工具降级提示**（`catalog_files.go`）：修改 write 工具的 description，在开头加警告。

**改动文件**：
- `internal/agent/system_prompt.go` — 新增 `renderMinimalChangeBlock`
- `internal/tools/catalog_files.go` — write 工具描述修改

### P4: 结构化根因分析（Structured Root Cause Analysis）

**原理**：在 Plan 模式的基础上，为 Agent 模式增加轻量级的"分析阶段"——edit 工具解锁前，LLM 必须先输出对问题的理解。

**实现**：

1. 新增 turn 级别的状态标记 `analysisProvidedThisTurn`：

```go
// agent.go
type Agent struct {
    // ...
    analysisProvidedThisTurn  bool
    skipAnalysisThisTurn      bool // 用户明确跳过时设置
    mutationsChangeCountThisTurn int // P4: 累计变更行数（用于 analysis_threshold）
}
```

2. 新增"分析工具" `analyze_problem`（只读工具），**必须标记 capabilities** 以确保子代理能获取：

```go
toolFn{
    name:        "analyze_problem",
    description: "Record your analysis of the root cause before making changes...",
    readOnly:     true,
    capabilities: []string{"workspace.read", "workspace.write"},
    fn:           b.analyzeProblem,
}
```

`workspace.read` + `workspace.write` 双标记确保无论子代理拥有哪种 capability 都能获取此工具，避免死锁。

3. 在 `stream_dispatch.go` 中，mutation 工具执行前检查。**门控拦截结果必须走 `appendToolResult`**：

```go
if isMutationTool(call.Name) && !a.analysisProvidedThisTurn && !a.skipAnalysisThisTurn && a.gateAnalyzeBeforeEdit && a.workspaceRoot != "" {
    belowThreshold := a.analysisThreshold > 0 && a.mutationsChangeCountThisTurn > 0 && a.mutationsChangeCountThisTurn < a.analysisThreshold
    if !belowThreshold {
        if err := appendToolResult(ctx, &sc, &results, core.ToolResult{...}); err != nil {
            return nil, false, err
        }
        continue
    }
}
```

4. Turn 开始时通过 `resetTurnState()` 重置。

**豁免**：
- 用户明确说"直接改"、"skip analysis"、"just fix it"等关键词时跳过（`skipAnalysisThisTurn`）。关键词检测在 `turn_loop.go` 中，**跳过 hidden 消息**（`!msg.Hidden`）。
- `analysis_threshold`：当累计变更行数低于阈值时豁免。首次编辑时 `mutationsChangeCountThisTurn == 0`，不触发豁免（`mutationsChangeCountThisTurn > 0` 检查），确保首次大修改也被分析。
- `workspaceRoot == ""` 时跳过门控。

**改动文件**：
- `internal/agent/agent.go` — 新增 `analysisProvidedThisTurn`、`skipAnalysisThisTurn`、`mutationsChangeCountThisTurn` 字段
- `internal/agent/turn_loop.go` — turn 开始时重置 + skip 关键词检测（跳过 hidden）
- `internal/agent/stream_dispatch.go` — analysis gate 检查（使用 `appendToolResult`）+ skip 关键词列表
- `internal/agent/stream.go` — analyze_problem 成功后设置标记 + 变更行数累计
- `internal/tools/catalog_runtime.go` — 新增 `analyze_problem` 工具（含 capabilities 标签）

## 跨领域问题

### Subagent / spawn_subagent 的门控

子代理（spawn_subagent）的编辑也受 P1/P4 的门控。

**结论：子代理独立维护门控状态，同样遵守先读再改、先分析再改。**

- `spawn_subagent` 创建的子 `Agent` 实例通过 `newChild` 传递 `WithVerifyConfig` + `WithGateConfig`，自然继承门控配置。
- 子代理在自己的 turn 循环中独立遵守门控规则。
- `analyze_problem` 工具标记了 `workspace.read` + `workspace.write` capabilities，确保有写权限的子代理能获取此工具，不会死锁。

### 配置统一

P1、P2、P4 的门控和验证都可在 `~/.whale/config.toml` 中统一配置：

```toml
[gate]
# P1: 编辑前是否必须先读文件
read_before_edit = true    # 默认 true
# P4: 编辑前是否必须先调用 analyze_problem
analyze_before_edit = true # 默认 true
# P4: 变更行数低于此值时豁免分析（0 = 始终要求分析）
analysis_threshold = 0     # 默认 0

[verify]
# P2: 编辑后自动运行的验证命令（用户显式配置）
commands = ["go build ./..."]
timeout = "30s"
# P2: 超过此行数时追加审查提示
review_threshold = 20
# P2: 编辑后自动运行的测试命令（默认关闭自动推断）
test_commands = []
test_timeout = "60s"
# P2: 第三方 review agent
review_agent = false
review_model = ""          # 默认使用主模型
review_api_key = ""        # 优先使用 DEEPSEEK_API_KEY 环境变量
review_base_url = ""       # 优先使用 DEEPSEEK_BASE_URL 环境变量
```

配置链路：`FileConfig` → `Config` → `AgentOption`（`WithVerifyConfig`/`WithGateConfig`），Agent 自身不读配置文件。

## 安全考量

### 自动验证命令的权限边界

自动推断的 verify/test 命令使用 `os/exec` 直接执行，不走 Policy.Decide 审批通道。潜在风险：模型通过 write 写入含恶意 script 的 package.json → 同批 dispatch 结束时 `runAutoTest` 自动执行该 script。

**缓解**：`autoDetectedConfigDirty()` 检测配置文件（package.json、Makefile、go.mod 等）是否在当前 turn 被修改过。如果被修改过，跳过自动推断的命令，只执行用户显式配置的命令。用户显式配置的命令被视为用户主动授权。

### Review Agent API Key

`review_api_key` 可写入项目级 TOML，存在泄露风险。缓解：
- 环境变量优先：`DEEPSEEK_API_KEY` > 配置文件中的 `review_api_key`
- `DEEPSEEK_BASE_URL` > 配置文件中的 `review_base_url`
- 配置字段注释引导用户使用环境变量

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
| P2 自动推断命令被投毒 | `autoDetectedConfigDirty` 检测配置文件修改，跳过自动推断 |
| P2 轻量审查提示被忽略 | 提示是软约束，硬约束靠 P1（先读）+ P4（先分析） |
| P3 提示被 LLM 忽略 | 提示只是软约束，硬约束靠 P1/P2/P4 |
| P4 增加交互轮次 | `analysis_threshold` 豁免小变更；用户可显式跳过 |
| P4 analyze_problem 被滥用为形式主义 | 要求填写 observed/expected/root_cause 三个字段，不允许留空 |
| P4 子代理死锁 | `analyze_problem` 标记 `workspace.read` + `workspace.write` capabilities |
| Subagent 绕过门控 | 子代理独立维护门控状态，同样遵守先读再改、先分析再改 |
| P2 debounce 期间 LLM 已基于旧状态继续推理 | 验证结果作为独立 tool result 追加，LLM 下一轮可见 |
| P2 review agent API key 泄露 | 环境变量优先于配置文件 |
| P1/P4 门控拦截丢失事件 | 门控结果走 `appendToolResult`（事件通道 + WarnPrefix + telemetry） |
