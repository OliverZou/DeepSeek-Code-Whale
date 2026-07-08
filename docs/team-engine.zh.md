# Team Engine — 多智能体编排引擎

## 概述

Team Engine 实现了**递归的 Plan → Work → Verify 协作模型**。每个任务环节都满足 Plan → Work → Verify 的闭环。Plan 是主动的规划者——理解目标、规划方案、分解任务、分配工作。这个 loop 是递归的——总任务和子任务遵循同一个模式，只是规划者、执行者和验证者的身份随任务性质变化。

### 有产出物 = 任务 = 走 Team Engine

**核心判断标准：有用户级产出物的请求就是任务，统一走 Team Engine 编排。** 与对话对象是 expert 还是 team 无关。

| 场景 | 是否创建任务 | Team Engine 配置 |
|------|------------|-----------------|
| 简单问答（无产出物） | 否，direct chat | — |
| Expert 执行任务（有产出物） | 是 | `mode: agent`，单 Worker，无 Verifier（仅 Checker） |
| Team 快捷模式 | 是 | `mode: team`，Worker + Verifier，2 轮 |
| Team 标准 SOP | 是 | `mode: team`，多 Worker + Verifier，完整轮次 |

**为什么统一走 Team Engine**：

1. **一套任务系统**：前端只有一个 TaskPanel，一套事件通道（`task.state_changed`），一套状态持久化（`FileTaskStore`）
2. **Expert 任务也是任务**：有产出物的工作应该可观测、可追踪、可暂停恢复——不因执行者是单 agent 就丢失这些能力
3. **渐进复杂度**：从单 Worker 到多 Worker+Verifier 是配置差异，不是架构差异。Expert 任务 = Team 任务的最简配置
4. **事件一致**：所有任务走同一套事件通道，前端只需一套渲染逻辑

**实现方式**：

- Agent（expert 或 team leader）在 direct chat 中接收用户请求
- Agent 判断请求是否需要产出物：是 → 创建 Team Engine 任务；否 → 直接回答
- Agent 通过 `team_create` 工具调用 `d.engine.CreateTask()` 创建任务
- Team Engine 接管后续编排：分配 Worker、调度 Verifier（如有）、管理轮次
- Team Engine 事件通过 `task.state_changed` 推送到 Pod
- Agent 监控 Team Engine 任务进度，向用户汇报

**Expert 任务的 Team Engine 配置**：

- `mode: agent`：单 Worker 执行，无 Verifier 角色
- Checker 仍然自动运行（基础完整性检查）
- 任务生命周期与 team 任务一致：pending → assigned → producing → produced → checking → checked → done
- 产出物通过 `task.subtasks` 的 `output` 字段呈现

### Expert 与 Team Leader 的任务创建权限

**Expert 只能创建 `mode: agent` 任务，不能创建 `mode: team` 任务。**

| Agent 角色 | `mode: agent` 任务 | `mode: team` 任务 |
|-----------|-------------------|-------------------|
| Expert | ✅ 可以创建 | ❌ 不可以 |
| Team Leader | ✅ 可以（快捷模式单 Worker） | ✅ 可以（标准 SOP 多 Worker） |

**理由**：

- Expert 是单一角色专家，职责是**执行或建议**，不是编排
- Team 任务需要 Leader 角色来编排多 agent 协作，Expert 不是 Leader
- 概念边界清晰：Expert = 执行者，Team Leader = 编排者

**Expert 遇到超出自身能力的任务时**，应建议用户使用 Team，而非自行创建 Team 任务。例如：

> "这个需求涉及架构设计和测试验证，建议使用软件开发团队来执行。"

这与现实世界一致：专科医生不会自己组建医疗团队，而是推荐患者去多学科会诊。

### 核心设计哲学

**Team Engine 是委托系统，不是工作流引擎。**

- Leader 的智能是核心驱动力——它决定做什么、谁来做、怎么验收
- Leader 根据目标自由编排，不受预设流程约束
- 所有改进围绕一个目标：**让 Leader 拥有正确的知识和约束**

### 递归的 Plan → Work → Verify Loop

```
总任务（Leader）
├── Plan: Leader 理解用户目标、规划方案、分解任务、分配工作
│   Verifier=用户确认目标 + 接收agent评估任务可执行性
├── 子任务1
│   ├── Plan: agent-A 理解任务、规划方案
│   ├── Work: agent-A 执行
│   └── Verify: agent-B 验证
├── 子任务2
│   ├── Plan: agent-C 理解任务、规划方案
│   ├── Work: agent-C 执行
│   └── Verify: agent-D 验证
├── Work: Leader 收集子任务结果
└── Verify: Checker + 用户确认
```

每一层都是同一个 loop，只是 worker 和 verifier 的身份随任务性质变化。**顶层 verifier 是用户，子任务 verifier 是 agent。**

#### 总任务层（Leader 执行）

| 环节 | Worker | Verifier | 说明 |
|------|--------|----------|------|
| 识别目标 | Leader 问用户 | **用户确认** | 来回几次对话确认目标 |
| 分解任务 | Leader + LLM | **接收 agent 评估** | agent 评估任务是否可执行 |
| 汇总交付 | Leader 收集子任务结果 | **Checker + 用户** | Checker 基础检查，用户最终确认 |

Leader 完成总任务**不需要 agent 作为 verifier**——顶层 verifier 是用户。

#### 子任务层（Agent 执行）

| 环节 | Worker | Verifier | 说明 |
|------|--------|----------|------|
| 执行任务 | 专业 agent | **专业 agent** | 由 Leader 在分解时指定 `VerifierRole` |

子任务的 worker 和 verifier 都是 agent。Verifier 不是通用检查器，而是**针对该任务产出的专业评判者**。

### Verifier vs Checker

| | Checker（系统机制） | Verifier（专业 agent） |
|---|---|---|
| 触发方式 | Engine 自动 | Leader 分配 |
| 做什么 | 基础完整性检查：产出是否存在、是否为空、是否截断、引用文件是否存在 | 专业质量验证：用角色知识判断产出是否合格 |
| 知识 | 通用规则 | 角色的核心能力 + 输出规范 |
| 产出 | PASS/FAIL 裁决 | 结构化反馈（FINDINGS JSON） |
| 位置 | Worker 产出后自动运行 | 作为一个子任务，由 Leader 编排 |

### Verifier 是针对任务的，不是针对角色的

同一个 tester，不同任务的 verifier 不同：

| 任务 | Worker | Verifier |
|------|--------|----------|
| 为 API 模块写单元测试 | tester | api-designer（覆盖关键接口？） |
| 执行集成测试 | tester | checker（跑一遍就知道） |
| 设计测试策略 | tester | developer（方向对不对？） |
| TDD 先写测试 spec | tester | — （测试本身就是 spec，developer 的代码过不过测试就是验证） |

**Leader 在分解任务时决定每个任务的 verifier**——它知道这个任务的产出该由谁来评判。

### 成员 Push Back 机制

团队成员不是被动执行者，有权质疑 Leader 的决定：

1. **Worker 接到任务后可以质疑**——"这个任务描述有歧义"、"上游产出还没到位，我无法开始"
2. **Worker 产出时可以附条件**——"我完成了 A 部分，但 B 部分需要架构师先确认"
3. **专业 Verifier 可以否决 Leader 的验收**——Leader 说"通过了"，代码审查员说"还有安全问题"

实现方式：

- **Worker Push Back**：Worker prompt 中注入 Push Back 指令，Worker 在产出中使用 `[PUSH_BACK]` 标记表示质疑。Engine 检测到 `[PUSH_BACK]` 后自动将任务转为 `pending_confirmation` 状态，等待用户确认后继续。
- **Verifier 否决**：专业 Verifier 验证不通过时，反馈追加到 task.Description，触发重试或重新分解。
- **Whiteboard 消息**：Worker 也可通过 Whiteboard 的 inbox/outbox 消息机制向 Leader 发送质疑消息。

### 协作铁律（Leader 内部约束）

所有 Leader 共有的行为约束，注入 Leader prompt：

1. 你是编排者，不是执行者——禁止自己写代码、写文档、做专业分析
2. 每个专业产出必须由对应角色输出后再采信，你只做编排与汇编
3. 未完成前序任务不可跳到后续任务
4. 验证不通过的任务必须回退重做，不可跳过
5. 禁止自己代写任何团队成员的专业产出

### 任务拆分原则

**一个 agent 只对应一个 role**——不是"一个 task 一个 role"。每个子任务应该由**一个内聚的 Worker agent** 完成产出，由**一个或多个正交的 Verifier agent** 评判，形成完整的"生产-评判"闭环。

**拆分应该持续到每个子任务可以被一个 Worker + N 个 Verifier 闭环覆盖。** 如果一个子任务还需要多个不同领域的 Worker 协同，说明拆得不够细。如果 Verifier 需要评判的维度太多导致评判变浅，就继续拆。

**Worker-Verifier 是上限，不是下限。** 有些子任务 Worker 干完就完了：

| 任务类型 | 需要 LLM Verifier？ |
|---------|-------------------|
| 生成代码 | ✓ 需要（代码审查） |
| 写文档 | ✓ 需要（准确性检查） |
| 数据格式转换 | ✗ 不需要（确定性任务，Checker 即可） |
| 汇总信息 | ✗ 不需要（事实性任务，源头即验证） |
| 翻译 | 视情况（专业翻译需要审校，日常翻译不需要） |

**当任务本身有模糊性或质量风险时，LLM Verifier 才有意义。** 输入输出一致、可自动验证的确定性任务不需要 LLM 验证。

**即使不用 LLM Verifier，Checker 也始终运行**——做最基本的输出检查（文件存在、无截断、引用完整等）。

### Verifier 角色决议

每个任务都有一个 Verifier。分两种形态：

1. **LLM Verifier**：Leader 在分解任务时指定 `verifier_role`（agent name），Engine 从 agent 定义库加载对应的 system prompt、tools、skills，生成独立的 Verifier agent
2. **内置 Verifier（Checker）**：Leader 不指定 `verifier_role` 时，Engine 用 Checker 做确定性验证兜底

决议规则：
```
Level 1: task.VerifierRole（Leader 显式指定）
    ↓ 为空
Level 2: Worker Role → Verifier Role 映射
    developer  → review    (代码审查)
    tester     → review
    researcher → verifier  (事实核查)
    writer     → verifier  (内容核查)
    formatter/evaluator/synthesizer → (无映射，Checker 兜底)
```

**Leader 通过是否设置 `VerifierRole` 来控制是否需要 LLM 验证。** 不设置 → Checker 即最终验证。

---

## 配置

### 模型配置

Team Engine 支持为不同角色配置不同的 LLM 模型，在 `.whale/team_engine.yaml` 中配置：

```yaml
routing:
  role_map:
    planner:              # Leader（任务分解 + 审查）
      profile: read_only
      timeout: 180
      model: deepseek-v4-flash
    developer:            # Worker（写代码）
      profile: default
      timeout: 1800
      model: deepseek-v4-pro
    tester:               # Worker（写测试）
      profile: test
      timeout: 1200
      model: deepseek-v4-flash
    reviewer:             # Worker（代码审查）
      profile: read_only
      timeout: 900
      model: deepseek-v4-flash
    researcher:           # Worker（调研分析）
      profile: research
      timeout: 900
      model: deepseek-v4-flash
    writer:               # Worker（文档撰写）
      profile: content
      timeout: 900
      model: deepseek-v4-flash
    formatter:            # Worker（格式化）
      profile: content
      timeout: 900
      model: deepseek-v4-flash
    evaluator:            # Worker（评估）
      profile: read_only
      timeout: 600
      model: deepseek-v4-flash
    synthesizer:          # Worker（综合）
      profile: read_only
      timeout: 600
      model: deepseek-v4-flash
```

### 默认值

| 角色 | 模型 | 超时 | 工具权限 |
|------|------|------|---------|
| planner (Leader) | `deepseek-v4-flash` | 180s | read_only |
| developer | `deepseek-v4-pro` | 1800s | default |
| tester | `deepseek-v4-flash` | 1200s | test |
| reviewer | `deepseek-v4-flash` | 900s | read_only |
| researcher | `deepseek-v4-flash` | 900s | research |
| writer | `deepseek-v4-flash` | 900s | content |
| formatter | `deepseek-v4-flash` | 900s | content |
| evaluator | `deepseek-v4-flash` | 600s | read_only |
| synthesizer | `deepseek-v4-flash` | 600s | read_only |

### Profile（工具权限）

| Profile | 可用工具 | 适用场景 |
|---------|---------|---------|
| `default` | 读文件 + 写文件 + shell | 开发编码 |
| `read_only` | 只读文件 + 网页搜索 | 审查、评估 |
| `research` | 读文件 + 网页搜索 | 调研分析 |
| `content` | 读文件 + 写文件（无 shell） | 文档写作 |
| `test` | 读/写文件 + shell（测试命令） | 测试执行 |
| `verify` | 读文件 + shell + 搜索（不能写） | Checker |

---

## 任务生命周期

### 状态机

```
pending → assigned → producing → produced → checking → checked → done
                                              ↓
                                         verifying → verified → done
                                              ↓
                                         failed / suspended
```

- **checking**：Checker（系统自动）执行基础完整性检查
- **verifying**：专业 Verifier（agent）执行质量验证（仅当 `task.VerifierRole` 指定时）

### 状态说明

| 状态 | 说明 | 是否可恢复 |
|------|------|-----------|
| `pending` | 等待执行 | — |
| `assigned` | 已分配 | — |
| `producing` | Worker 执行中 | — |
| `produced` | Worker 完成 | — |
| `checking` | Checker 基础检查中 | — |
| `checked` | Checker 通过 | — |
| `verifying` | 专业 Verifier 验证中 | — |
| `verified` | 专业验证通过 | — |
| `done` | 完成 | ❌ 终止状态 |
| `failed` | 失败 | ❌ 终止状态 |
| `suspended` | 用户主动停止 | ✅ 可通过 Resume 恢复 |
| `pending_confirmation` | 等待用户确认 | ✅ 用户确认后继续 |

---

## 暂停与恢复

### 停止任务

在 Dashboard 中点击 ⏹ 按钮会通过 `Kill()` 将运行中的任务置为 `suspended` 状态，并保存执行检查点（checkpoint）到数据库。

```
运行中 → 点击停止 → 任务状态变为 suspended → checkpoint 保存已完成批次
```

### 恢复任务

在 Dashboard 中点击 ▶ 恢复按钮会调用 `ResumeMasterTask()`：

1. 加载 checkpoint，跳过已完成的 batch
2. 重置 suspended 子任务为 pending
3. 从第一个未完成的 batch 继续执行

```
已暂停 → 点击恢复 → suspended 任务重置为 pending → 跳过已完成 batch → 继续执行
```

---

## Batch 并行执行

多个 batch 之间按依赖关系（DAG）并行执行：

- **无依赖的 batch** — 立即并行启动
- **有依赖的 batch** — 等待所有前置 batch 完成后自动启动

```
Batch 1 (调研) ──┐
                  ├── 同时执行
Batch 2 (设计) ──┤
                  │
Batch 3 (编码) ←──┘ 等待 Batch 1 + 2 完成后才开始
```

同一 batch 内的任务也支持并发（通过 `concurrency` 参数控制）。

---

## 事件推送

Engine 在任务状态变化时主动推送事件，替代轮询：

| 事件类型 | 触发时机 |
|---------|---------|
| `EventStateChanged` | 任何状态转换 |
| `EventWorkerOutput` | Worker 产出完成 |
| `EventCheckerResult` | Checker 得出结论 |
| `EventVerifierResult` | 专业 Verifier 得出结论 |
| `EventTaskDone` | 任务最终完成或失败 |
| `EventLeaderLog` | Leader 日志更新 |
| `EventAgentLog` | Agent 日志更新 |

Dashboard 通过 Wails `runtime.EventsEmit` 实时接收事件并更新界面。

---

## 恢复流程（技术细节）

```
用户点击 ▶ 恢复

1. ResumeMasterTask()
   ├─ 从 DB 加载 checkpoint JSON
   ├─ 解析 {"completed_batches": [...], "batch_cycles": {...}}
   ├─ 重置所有 suspended 子任务为 pending
   ├─ 按 batch_id 重新分组任务
   └─ 跳过已完成的 batch

2. 对未完成的 batch：
   ├─ runBatchWithCycles()
   │   ├─ RunBatch() → 并行执行 batch 内任务
   │   └─ Leader 审查 → accept/reject/escalate
   └─ 每完成一个 batch 保存 checkpoint

3. 全部完成后写入 deliverable.md + 执行总结
```

---

## Whiteboard 通信机制

### 任务目录结构

```
tasks/<task_id>/
├── input.md        # Leader 写入的任务描述 + 上下文
├── output.md       # Worker 写入的产出
├── verifier.md     # Verifier 写入的检查结果
├── confirmation.md # Agent 的确认请求（pending_confirmation 状态）
├── status.json     # 当前状态元数据
├── inbox/          # Agent 间通讯：发给本 Agent 的消息
│   ├── 001_from_human.json
│   └── 002_from_agent-B.json
├── outbox/         # Agent 间通讯：本 Agent 发出的消息
│   └── 001_to_agent-C.json
└── artifacts/      # Worker 产出的具体文件（代码等）
```

### 消息流

```
Leader ──WriteInboxFile──→ Worker 的 input.md
Worker ──WriteOutput───→ Worker 的 output.md
Checker ──WriteVerifier──→ Worker 的 verifier.md（基础检查）
Verifier ──WriteVerifier──→ Worker 的 verifier.md（专业验证）
Worker ──WriteMessage──→ Leader 的 inbox（push back / 质疑）
Leader ──WriteMessage──→ Worker 的 inbox（反馈 / 追加指令）
```

### Push Back 示例

Worker 发现问题时的消息流：

```
1. Worker 读 input.md → 发现"上游产出还没到位"
2. Worker 写 inbox 消息给 Leader："任务 #3 的上游依赖（任务 #1 产出）不存在，无法开始"
3. Leader 收到消息 → 检查任务 #1 状态 → 回复"任务 #1 已完成，产出路径为 X"
4. Worker 收到回复 → 重新开始执行
```

---

## 团队定义（team.yaml）

### 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `label` | string | 团队名称 |
| `roles` | []string | 角色 agent name 列表（对应 agents 目录下的 .md 文件名） |
| `capabilities` | []string | 能力范围声明（范围+技术栈+工作流阶段） |
| `routing` | []RoutingEntry | 意图路由表（可选） |

### Routing Entry

| 字段 | 类型 | 说明 |
|------|------|------|
| `intent` | string | 意图关键词（`|` 分隔） |
| `roles` | []string | 匹配的角色列表 |
| `mode` | string | `agent`（单角色直调）或 `team`（多角色协作） |

### 示例

```yaml
label: 软件开发团队
roles:
  - product-manager
  - requirements-analyst
  - ux-architect
  - ui-designer
  - software-architect
  - api-designer
  - implementation-planner
  - backend-engineer
  - frontend-engineer
  - coding-implementer
  - tdd-tester
  - code-reviewer
  - architecture-guardian
  - bug-analyst
capabilities:
  - "软件开发全流程：需求→设计→编码→测试→交付"
  - "技术栈：Go, TypeScript, React, Wails"
  - "工作流阶段：需求分析, 架构设计, API设计, 编码实施, 测试验证, 代码审查"
routing:
  - intent: "新功能|添加功能|新增|开发"
    roles: [product-manager, requirements-analyst]
    mode: team
  - intent: "bug|缺陷|异常|报错|修复"
    roles: [bug-analyst]
    mode: agent
  - intent: "API|接口设计|接口变更"
    roles: [api-designer]
    mode: agent
  - intent: "代码审查|review|代码质量"
    roles: [code-reviewer, architecture-guardian]
    mode: team
```

### 成员能力清单（自动提取）

Leader prompt 中的成员能力清单从 agent MD 文件自动提取，不需要在 team.yaml 中重复声明：

| 提取源 | 提取内容 | 注入位置 |
|--------|---------|---------|
| agent MD 的 `role` 字段 | 角色中文名 | 成员能力清单 |
| agent MD 的"核心能力"分区 | 擅长领域 | 成员能力清单 |
| agent MD 的"输出规范"分区 | 验证标准 | Checker/Verifier prompt |

---

## 循环记忆（Loop Memory）

理解债务管理机制，防止 Agent 在重试循环中重复犯相同错误：

- **存储**：`loop.md` 文件，保存在 Whiteboard 的 master task 目录下
- **写入时机**：RunTask 重试时，将前次失败原因摘要写入 loop.md
- **读取时机**：RunTask 重试时，从 loop.md 读取循环记忆注入 Worker prompt 的 feedback
- **注入方式**：当 `RetryCount > 0` 时，Worker prompt 中自动追加循环记忆内容

```
RunTask 重试流程：
1. 检测 RetryCount > 0
2. ReadLoopMemory() → 获取历史失败摘要
3. 将循环记忆注入 Worker prompt 的 feedback 区域
4. Worker 执行 → 产出
5. 如果再次失败 → WriteLoopMemory() → 追加本次失败摘要
```

---

## YAML Frontmatter 解析

Agent 定义文件（`.md`）的 YAML frontmatter 使用 `gopkg.in/yaml.v3` 解析，替代了早期手写解析器：

- 支持 YAML 标准列表格式和逗号分隔字符串格式（如 `tools: workspace.read, shell.run`）
- 支持 WorkBuddy 的 `displayName`/`profession` 嵌套字段映射
- 自定义 `yamlStringList` 类型处理逗号分隔字符串和 YAML 列表两种格式
