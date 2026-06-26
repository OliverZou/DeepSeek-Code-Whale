# Loop Engineering 角度审查 Team Engine

> 分析日期：2026-06-25
> 参考文章：
> - "AI Agent Loop Engineering 深度解析" (AI砖家, 2026-06-16)
> - "超 350 万浏览：一文讲清 Loop Engineering" (老年人学AI, 2026-06-10)
> - Addy Osmani (Google Cloud AI 工程总监) 的长文定义
> - Lance Martin (Anthropic) 的 Fable 5 循环实验

---

## 一、Loop Engineering 核心概念

Loop Engineering 的核心洞察：**用设计的系统替代人提示 Agent，实现自动化循环**。

### 1.1 五步闭环

```
Discover → Plan → Execute → Verify → Iterate
   ↑___________________________________________|
```

| 步骤 | 大白话 | 文章关键描述 |
|------|--------|-------------|
| **Discover** | 先弄清楚现在最该处理什么 | 失败测试、待办、Issue、用户反馈 |
| **Plan** | 决定先做哪一步，怎样做 | 任务清单、修改范围、风险判断 |
| **Execute** | 调用工具把工作真正做出来 | 代码、文档、报告、配置修改 |
| **Verify** | 对照标准检查结果是否合格 | 测试、评分表、规则、人工检查点 |
| **Iterate** | 不合格就根据反馈继续修正 | 报错、差异、审查意见、下一轮计划 |

### 1.2 五块拼图 + 一个记忆本

| 拼图 | 作用 | 文章原文描述 |
|------|------|-------------|
| **Automation** | 自动化触发器 | 定时启动（每天早上6点）、事件触发（CI失败/Issue新建/PR提交）、周期性巡检 |
| **Worktree** | 隔离工作树 | 多个 Agent 同时修改项目时各自在独立目录和分支里工作，避免彼此覆盖 |
| **Skill** | 可复用技能 | 项目规则、启动方法、测试方式和历史经验被写下来，Agent 每轮开始时直接读取 |
| **Connector** | 外部连接器 | 对接 GitHub/Linear/Slack/数据库，MCP 协议让 AI 能操作真实世界工具 |
| **Sub-agent** | 子代理分工 | 一个 Agent 负责探索或执行，另一个 Agent 负责检查。写答案的人不再自己给自己判满分 |
| **记忆本** | 外部持久记忆 | LOOP.md/Kanban/Issue/DB：记住目标、尝试、失败原因、当前状态、下一步 |

### 1.3 关键设计原则

- **执行者与检查者分离**：避免"自己给自己打分"的盲区
- **工具化验证**：能写成测试的尽量用测试；能写成数字的尽量给数字
- **封闭循环优先**：对大多数场景，先写清楚范围、动作、验证、预算、停止条件
- **模型会忘记，仓库不会忘记**：把状态和经验写到外面，不依赖无限延长聊天记录
- **Comprehension Debt**：循环产出代码的速度 > 人理解代码的速度，必须保留人工检查点

---

## 二、Team Engine 逐项对比

### 2.1 五步闭环

| 步骤 | 文章要求 | Team Engine 现状 | 评分 |
|------|---------|-----------------|:----:|
| **Discover** | 从失败测试、Issue、CI、用户反馈中自动发现待办 | 无主动发现机制。只能通过 `PlanAndRun(goal)` 被动接收目标，没有从外部系统扫描问题的能力 | ❌ 0% |
| **Plan** | 系统自动拆解目标为子任务，决定顺序和范围 | `Planner.Decompose()` 强实现：结构化输出 + JSON 修复 + output schema + ConsensusDecompose 多模型投票 + Escalator 重分解 + BuildLeaderPrompt 团队角色路由 | ✅ 95% |
| **Execute** | 调用工具写出代码/文档/配置 | `AgentRunner.RunWithContext()` 完整实现：隔离子代理 + git worktree 隔离 + 流式输出 + 7 种 ToolProfile + PID 追踪 + stdin 实时消息 + context 取消 | ✅ 90% |
| **Verify** | 对照标准检查，不合格就反馈回去 | 三层验证链：Checker(工具化验证 + lazy-verdict 自动重试) + Verifier Focus 多维度(correctness/security/completeness/sources/plausibility) + Leader ReviewCycle(accept/reject/escalate/escalated) + 独立校验(必须使用 ≥2 种工具，禁止信任 Worker 自评) | ✅ 95% |
| **Iterate** | 不合格→反馈→重试→直到达标或停止 | 完整闭环：retry loop + verifier feedback 累积到 prompt + loop memory 注入 + loop-until-dry(连续2轮无新finding即退出) + Escalator 重分解 + batch cycle 扩展 + 同finding检测提前中止 | ✅ 90% |

### 2.2 五块拼图 + 记忆本

| 拼图 | 文章定义 | Team Engine 现状 | 评分 |
|------|---------|-----------------|:----:|
| **Automation** | 定时启动、事件触发、周期性巡检 | **完全缺失**。没有 cron 调度器、没有 webhook 监听、没有事件驱动的任务触发机制。整个 engine 只能通过 API 主动调用 `PlanAndRun()` / `RunTask()` 启动 | ❌ 0% |
| **Worktree** | 隔离并行开发环境，支持同时运行几十个 Agent | `EnableWorktree()` + `createWorktree()` + `cleanupWorktree()` + `collectWorktreeDiff()` — 为 coding 角色自动创建 git worktree + 分支，完成后收集 diff 并清理。`isCodingRole()` 判断是否启用 | ✅ 85% |
| **Skill** | 项目知识文档化、编码规范固化、最佳实践沉淀为可复用模块 | **部分实现**。`TeamConfig` 有 `MemoryDir`/`TemplatesDir`、agent definition markdown 文件、`RoleCapabilities`/`RoleOutputSpecs` 注入到 prompt。但**没有正式的可加载/可组合/可版本化 Skill 模块** | ⚠️ 50% |
| **Connector** | GitHub/Linear/Slack/数据库对接，MCP 协议操作外部世界 | **完全缺失**。Team Engine 完全运行在本地文件系统内，没有外部系统连接器，没有 MCP 协议集成 | ❌ 0% |
| **Sub-agent** | 执行者与检查者分离，不同模型分工，避免"自己给自己打分" | **核心设计原则已完整实现**。Leader/Worker/Checker 完全上下文隔离（`Context Isolation Principle` 注释明确写了设计哲学），不同角色可用不同模型(Leader=v4-pro, Worker=v4-pro, Checker=v4-flash)，7 种独立 ToolProfile 控制权限 | ✅ 95% |
| **记忆本** | LOOP.md/Kanban/DB，记住目标、尝试、失败、当前状态 | `Whiteboard` 完整实现：inbox/outbox(Agent 间消息)、loop.md(循环记忆)、board.md(全局进度看板)、deliverable.md(交付物汇总)、checkpoint 持久化 + `ResumeMasterTask`(断点续传)、chat 消息系统(Agent↔Agent 同权通讯) | ✅ 90% |

### 2.3 关键模式深度对比

#### Self-Correction Loop (Fable 5 文章核心实验)

Fable 5 实验的关键链路：**失败并记录 → 调查失败原因 → 验证自己的猜测 → 把确认过的经验提炼成规则 → 下一次任务先读取规则**

| 能力 | Team Engine 现状 |
|------|-----------------|
| 失败记录 | ✅ `WriteLoopMemory()` 每轮记录时间戳 + 失败原因 |
| 反馈传递 | ✅ Verifier feedback 累积到 `task.Description` 的 `[VERIFIER FEEDBACK]` 块中，Worker 重试时完整看到历史 |
| 循环记忆注入 | ✅ `ReadLoopMemory()` 在 retryCount>0 时注入 prompt：`## 循环记忆（之前尝试的记录）\n请避免重复之前的错误` |
| 独立评估 | ✅ Checker 与 Worker 完全上下文隔离，强制使用 ≥2 种工具验证，lazy-verdict 检测（无 TOOLS USED 段落则重做） |
| 提炼规则 | ⚠️ `recordLesson()` 记录简短 feedback，但没有正式的规则提炼流程（从多次失败中自动提取模式 → 生成 rule） |
| 跨任务复用 | ❌ Lesson 只按 role 存储，下一次不同任务中不会主动注入。缺少文章描述的 "上一次任务的经验自动成为下一次的 Skill" |

#### Open Loop vs Closed Loop

| 类型 | 文章描述 | Team Engine 现状 |
|------|---------|-----------------|
| **Closed Loop** | 提前写清楚范围、动作、验证、预算、停止条件 | ✅ 完整支持：Batch 结构 + MaxCycles + Dependency gates + VerifierFocus + 多种停止条件 |
| **Open Loop** | 只给目标，系统自己决定尝试什么、调用多少工具、做多少轮实验 | ⚠️ 可通过 MaxCycles=0 实现，但缺少探索预算控制、方向漂移检测 |

#### Loop-Until-Dry (收敛检测)

文章描述的核心收敛模式：**持续产生 findings 直到连续 K 轮没有新发现**

✅ **已完整实现**：
- `CycleFindingsSet` + `HasNewFindings()` — 结构化 finding 跨轮比较
- `dryCount >= 2` — 连续 2 轮无新 finding 即自动退出
- Finding 使用稳定 ID（如 `missing-section-3`），支持跨轮去重
- 同时用于普通模式（`batchCycleLoop`）和 DW 模式（`runDWCycle`）

#### Consensus / Judge Panel (多模型协作)

文章描述：**N 个独立方案 → 并行评判 → 从最佳方案合成**

✅ **已实现**：`ConsensusDecompose()` — 两个模型并行分解同一个 goal，第三个模型（selector）评估并选择最佳方案。

#### Concurrency & Fleet (Agent 小队)

文章描述：**调度 Agent 管目标，探索 Agent 查资料，执行 Agent 做修改，验证 Agent 查结果，整理 Agent 汇总状态**

✅ **已实现**：
- `RunBatch()` — semaphore 控制的并发执行
- `Batch.DependsOn` — 跨阶段依赖门控
- `Cross-stage artifact passing` — 场景4：Stage 1 产出自动注入 Stage 2 的 prompt
- `leaderWatchLoop` — Leader 实时监控执行进度，主动介入卡住的任务

#### Context Isolation (上下文隔离)

文章描述：**Worker 看不到 Verifier 的批判，Verifier 看不到 Worker 的内部推理**

✅ **核心设计已完整实现**（`spawner.go` 中有完整的 `Context Isolation Principle` 文档注释）：
> The Worker NEVER sees the Verifier's critique. The Verifier NEVER sees the Worker's internal reasoning — only the final output. This adversarial isolation is what prevents context pollution and drives quality.

每个 `SpawnSubagent` 调用创建全新的隔离子代理会话，拥有自己的 LLM 上下文、工具注册表和 agent loop。

### 2.4 成本控制

| 能力 | 文章要求 | 现状 |
|------|---------|------|
| 时间限制 | 最长时间 | ✅ Role 级别 timeout（Router.ResolveTimeout），默认 300s，research 角色 1800s |
| 轮数限制 | 最大轮数 | ✅ `MaxRetries`(per task) + `MaxCycles`(per batch) + `DefaultMaxCycles=10` |
| 并行限制 | 最大并行数 | ✅ `Batch.Concurrency` + `Config.Batch.MaxAgents=9` |
| 预算限制 | 最大预算 | ⚠️ 仅有粗略估算（`TotalTokens += (t.RetryCount + 1) * 10000`），无硬性 token 预算上限 |
| 停止条件 | 完成/超标/卡住 | ✅ Loop-until-dry + CycleAccept/Reject/Escalate + 同finding检测提前中止 + cancel 机制 |

### 2.5 停止条件

文章强调停止条件的重要性："这是防止无限烧 Token 的关键"

| 停止条件 | 文章标准 | 现状 |
|---------|---------|------|
| 所有测试通过 | ✅ 好的停止条件 | ✅ Checker 工具化验证 + Verifier PASS |
| 验收标准全部满足 | ✅ 好的停止条件 | ✅ Verifier Focus 多维度检查 |
| 达到最大迭代次数 | ✅ 好的停止条件 | ✅ MaxRetries + MaxCycles + DefaultMaxCycles |
| Token 消耗达到预算 | ✅ 好的停止条件 | ⚠️ 仅估算，无硬性上限 |
| 连续失败同一问题 | ✅ 好的停止条件 | ✅ `findingsAreSame()` 检测，连续 2 轮同 finding → 提前中止 |
| 用户手动取消 | ✅ 好的停止条件 | ✅ context 取消 + PID kill + stdin close |
| "做得足够好" | ❌ 坏的停止条件 | N/A — 使用明确标准 |

---

## 三、差距总结

### 🔴 关键缺失（必须补）

**1. Automation 自动化触发器**
- 需要：cron-like 定时调度器、webhook/事件驱动的任务触发、周期性巡检
- 参考：文章中的 `/loop --schedule "0 6 * * *"` 和 GitHub Actions cron 集成
- 建议实现：
  - 新增 `Scheduler` 组件，支持 cron 表达式注册定时任务
  - 新增 `Trigger` 接口，支持 webhook、文件监听、事件源等触发方式
  - 与 `PlanAndRun()` 对接：触发器事件 → 自动生成 goal → 送入 engine 执行

**2. Connector 连接器体系**
- 需要：GitHub connector（Issue/PR/CI 读写）、MCP 协议集成、通知渠道（Slack 等）
- 参考：Addy Osmani 原文将 Plugin/Connector 作为独立拼图，是 Agent 操作真实世界的基础
- 建议实现：
  - 定义 `Connector` 接口（`Connect()` / `Read()` / `Write()` / `Subscribe()`）
  - 首批实现：`GitHubConnector`（读 Issue/PR、写 PR 评论、读 CI 状态）
  - 对接 MCP 协议，使 connector 可作为 MCP server 暴露给 agent

**3. 主动 Discover 能力**
- 需要：从 GitHub Issues、CI 失败日志、代码扫描结果、用户反馈中自动生成待办
- 现有 `PlanAndRun(goal)` 只能被动接收目标字符串
- 建议实现：
  - 定义 `Discovery` 接口：定期扫描外部信号源 → 输出 `[]DiscoveredTask`
  - 与 Automation 联动：每天 6 点触发 Discover → 发现的问题自动送入 PlanAndRun

### 🟡 重要增强（应该做）

**4. Skill 系统正式化**
- 现有：`TeamConfig.MemoryDir`/`TemplatesDir`、agent definition markdown 文件、`RoleCapabilities`/`RoleOutputSpecs`
- 缺失：可加载、可组合、可版本化的 Skill 模块（如 `coding-standards`、`ci-triage`、`security-review`）
- 建议实现：
  - 定义 `Skill` 结构体：name、description、prompt、allowedTools、rules、examples
  - 支持 `LoadSkill(name string) (*Skill, error)` 从 `.whale/skills/` 目录加载
  - Leader prompt 和 Worker prompt 中注入当前任务相关的 skills

**5. Token 预算硬控制**
- 现有：粗略的 token 估算（`TotalTokens += (t.RetryCount + 1) * 10000`），无硬性预算上限
- 需要：`--max-tokens 500000` 式的每会话预算，达到上限后优雅停止
- 建议实现：
  - `SubagentResponse` 已携带 `UsagePrompt`/`UsageCompletion`（但当前未累加使用）
  - 在 `PlanAndRun` 入口添加 `BudgetLimit int64` 参数
  - `AgentRunner` 累加实际 token 消耗并暴露 `RemainingBudget()`
  - 预算耗尽时触发 `CycleEscalate` 或按 checkpoint 优雅停止

**6. 跨任务经验复用（Lesson → Rule 提炼）**
- 现有：`recordLesson()` 记录但不在后续任务中注入；loop memory 只在同任务内循环
- 需要：Fable 5 实验描述的 "把验证过的经验提炼成规则 → 下一次任务先读取规则"
- 建议实现：
  - `BuildMemoryContext()` 扩展：除了当前 role 的 lessons，注入相关 role 和相似任务类型的历史 lessons
  - 新增 `LessonAnalyzer`：定期分析 lessons，将重复出现 ≥3 次的 finding 模式自动提升为 `Rule`
  - Rule 存储在 team 的 memory 目录中，作为 `BuildLeaderPrompt` 的一部分注入

### 🟢 可选优化

**7. Comprehension Debt 管理**
- 文章提醒：循环每天都在产出代码，人越来越不知道项目为什么变成这样
- 建议：在 `buildExecutionSummary()` 中添加 "人类必读变更摘要" 章节，用自然语言描述架构级变更、新增模块、删除的功能，标记需要人工 review 的关键决策

**8. 项目级 LOOP.md**
- 文章描述：`LOOP.md` 作为项目级循环状态文件，包含目标、完成条件、允许范围、当前状态、下一步、尝试记录
- 现有 Whiteboard 的 `board.md`（进度看板）和 `deliverable.md`（交付物）接近但不完全等同
- 建议：新增 `WriteLoopState()`/`ReadLoopState()`，生成文章描述的 LOOP.md 格式

**9. 验证 Agent 盲点检测**
- 文章提醒：验证 Agent 也可能判断错误，两个 Agent 也可能共享同样的盲点
- 现有 Checker 的 lazy-verdict 检测是一个好的起点
- 建议：引入 Checker 多样性（不同模型/不同 focus），对关键任务使用多 Checker 投票

---

## 四、总体评分

```
Loop Engineering 成熟度: ████████░░ 75%

五步闭环:
  Discover    ░░░░░░░░░░   0%  ← 最大短板
  Plan        ██████████  95%
  Execute     ██████████  90%
  Verify      ██████████  95%
  Iterate     ██████████  90%

五块拼图 + 记忆:
  Automation  ░░░░░░░░░░   0%  ← 最大短板
  Worktree    ████████░░  85%
  Skill       █████░░░░░  50%
  Connector   ░░░░░░░░░░   0%  ← 最大短板
  Sub-agent   ██████████  95%
  Memory      ██████████  90%
```

## 五、结论

**Team Engine 在核心循环引擎（Plan → Execute → Verify → Iterate）方面已经达到了 Loop Engineering 的标准**，甚至在一些方面超出了文章描述的典型做法：

- **Consensus Decompose** —— 多模型并行规划 + 第三个模型评判选择，是文章未提及的高级模式
- **Loop-Until-Dry** —— 结构化 finding 跨轮比较 + 收敛检测，完整实现了文章中最先进的循环退出策略
- **Context Isolation Principle** —— 完整的文档化设计哲学，执行者/检查者上下文完全隔离
- **Verifier Focus 多维度** —— correctness/security/completeness/sources/plausibility 等多维度验证
- **Inbox/Outbox Agent 通讯** —— Agent 间消息系统，与人类同权的通讯模型
- **Checkpoint/Resume** —— 跨进程的断点续传，支持 dashboard 停止后恢复

**但它目前是一个"封闭的循环引擎"**——循环在内部运转得很好，却缺少与外部世界的三个关键连接：

1. **Automation**：没有定时/事件驱动的自动启动能力
2. **Connector**：没有与 GitHub/Slack/MCP 等外部系统的对接
3. **Discover**：没有从外部信号源主动发现问题、自动生成任务的能力

这三个维度是 Team Engine 从 "内部循环引擎" 进化为 "完整的 Loop Engineering 系统" 的必要条件。此外，Skill 系统的正式化和 Token 预算硬控制也应作为第二优先级的增强项。
