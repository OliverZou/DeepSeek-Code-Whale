# Team Engine — Architecture

## 核心设计哲学

**递归的 Plan → Work → Verify Loop**：每个任务环节都满足 Plan → Work → Verify 的闭环。Plan 是主动的规划者——理解目标、规划方案、分解任务、分配工作。这个 loop 是递归的——总任务和子任务遵循同一个模式，只是规划者、执行者和验证者的身份随任务性质变化。

**委托系统，不是工作流引擎**：Leader 的智能是核心驱动力，它决定做什么、谁来做、怎么验收。Leader 根据目标自由编排，不受预设流程约束。

**Verifier vs Checker**：
- **Checker**（系统机制）：基础完整性检查——产出是否存在、是否为空、是否截断、引用文件是否存在
- **Verifier**（专业 agent）：专业质量验证——用角色知识判断产出是否合格
- Verifier 是**针对任务的**，不是针对角色的——同一个 agent 在不同任务中可能是 worker 或 verifier

**成员 Push Back**：团队成员不是被动执行者，有权质疑 Leader 的决定。

## 全景

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              ENTRY POINTS                                    │
│                                                                              │
│  whale team_plan        whale resume        Dashboard ▶运行 / ⏹停止          │
│  (新目标)               (续跑)              (远程控制)                        │
│       │                     │                     │                          │
│       └─────────────────────┼─────────────────────┘                          │
│                             ▼                                                │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                          TeamEngine.New(db, wb, spawner)              │   │
│  │  cleanupInterruptedTasks (spawner != nil 时重置断点任务)              │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
│                             │                                                │
│              ┌──────────────┴──────────────┐                                 │
│              ▼                              ▼                                │
│  ┌─────────────────────────┐   ┌─────────────────────────┐                  │
│  │      Leader             │   │      PlanAndRun          │                  │
│  │  .Decompose(goal)       │──▶│  .DecomposeFull()        │                  │
│  │  .DecomposeTask(failed) │   │  → CreateTask ×N          │                  │
│  │  .ReviewCycle(report)   │   │  → RunBatch ×N (DAG)      │                  │
│  └─────────────────────────┘   │  → Leader.ReviewCycle     │                  │
│                                 └────────────────┬────────┘                  │
│                                                  │                           │
│                    ┌─────────────────────────────┼─────────────────────┐     │
│                    ▼                             ▼                     ▼     │
│            ┌──────────────┐          ┌──────────────┐      ┌──────────────┐ │
│            │  Batch A     │  ─DAG─▶ │  Batch B     │ ──▶  │  Batch C     │ │
│            │  concurrency │          │  depends: A  │      │  depends: B  │ │
│            └──────┬───────┘          └──────┬───────┘      └──────┬───────┘ │
│                   │                         │                     │         │
│                   ▼                         ▼                     ▼         │
│            ┌──────────────────────────────────────────────────────────┐    │
│            │                     RunBatch(batch)                       │    │
│            │  for each task: sem ← 1; go RunTask(ctx, task)            │    │
│            └──────────────────────────┬───────────────────────────────┘    │
└───────────────────────────────────────┼─────────────────────────────────────┘
                                       │
                                       ▼
┌───────────────────────────────────────────────────────────────────────────────┐
│                              RunTask(task) — 单任务生命周期                     │
│                                                                                │
│  for attempt < maxRetries:                                                     │
│                                                                                │
│    ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐           │
│    │ pending  │ ──▶ │ assigned │ ──▶ │producing │ ──▶ │produced  │           │
│    └──────────┘     └──────────┘     └────┬─────┘     └────┬─────┘           │
│                                          │                  │                 │
│                                     Worker              ┌───▼──────────┐      │
│                                     spawn               │ checking     │      │
│                                     subagent            │ Checker      │      │
│                                     ┌─┴──┐              │ (系统自动)    │      │
│                                     │output│             └──┬───┬──────┘      │
│                                     │.md  │                │   │             │
│                                     └────┘           PASS   FAIL             │
│                                                        │     │             │
│                                                        ▼     ▼             │
│    ┌──────────────────────────────────────────┐   checked  (空/截断/      │
│    │  Verifier (专业 agent)                    │     │      引用缺失)     │
│    │  仅当 task.VerifierRole 指定时触发         │     │       │            │
│    │  prompt 注入 Worker 角色的核心能力+输出规范  │     │       ▼            │
│    │  用角色自己的承诺来检查是否兑现              │     │   重试++           │
│    │  ┌─┴──┐                                   │     │                    │
│    │  │verif│                                   │     │                    │
│    │  │ier.md│                                  │     │                    │
│    │  └────┘                                   │     │                    │
│    └──────────────────┬───────────────────────┘     │                    │
│                       │                              │                    │
│              ┌────────┴────────┐                     │                    │
│              ▼                 ▼                     │                    │
│           PASS              FAIL                     │                    │
│              │                 │                     │                    │
│              ▼                 ▼                     │                    │
│          verified      反馈追加到 task.Description    │                    │
│            │           retries 耗尽 → suspended       │                    │
│            │           Leader.DecomposeTask()         │                    │
│            ▼           → 拆成 2-3 个小任务             │                    │
│        done ✅         → 替换原任务, 继续执行           │                    │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                    递归 Plan → Work → Verify Loop                              │
│                                                                                │
│  总任务（Leader 执行）                                                          │
│  ├── Plan: Leader 理解用户目标、规划方案、分解任务、分配工作                       │
│  │   Verifier=用户确认目标 + 接收agent评估任务可执行性                            │
│  ├── 子任务1                                                                    │
│  │   ├── Plan: agent-A 理解任务、规划方案                                        │
│  │   ├── Work: agent-A 执行                                                     │
│  │   └── Verify: agent-B 验证                                                   │
│  ├── 子任务2                                                                    │
│  │   ├── Plan: agent-C 理解任务、规划方案                                        │
│  │   ├── Work: agent-C 执行                                                     │
│  │   └── Verify: agent-D 验证                                                   │
│  ├── Work: Leader 收集子任务结果                                                │
│  └── Verify: Checker + 用户确认                                                 │
│                                                                                │
│  顶层 verifier 是用户，子任务 verifier 是 agent                                 │
│  Leader 完成总任务不需要 agent 作为 verifier                                    │
│                                                                                │
│  Verifier 是针对任务的，不是针对角色的：                                         │
│  ┌──────────────────────┬──────────┬──────────────────────────────┐           │
│  │ 任务                  │ Worker   │ Verifier                     │           │
│  ├──────────────────────┼──────────┼──────────────────────────────┤           │
│  │ 为API模块写单元测试    │ tester   │ api-designer（覆盖关键接口？）│           │
│  │ 执行集成测试          │ tester   │ checker（跑一遍就知道）       │           │
│  │ 设计测试策略          │ tester   │ developer（方向对不对？）     │           │
│  │ TDD先写测试spec       │ tester   │ — （测试本身就是spec）        │           │
│  └──────────────────────┴──────────┴──────────────────────────────┘           │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                        Batch Cycle — Loop-Until-Dry                            │
│                                                                                │
│  for cycle < maxCycles (安全帽 10):                                            │
│                                                                                │
│    RunBatch(batch)                                                             │
│         │                                                                      │
│         ▼                                                                      │
│    collectCycleFindings(batch)  ←── 读取每个 task 的 Verifier 输出             │
│         │                                                                      │
│         ▼                                                                      │
│    ParseFindings(verifierOutput) ──▶ [{id, title, severity, evidence}]         │
│         │                                                                      │
│         ▼                                                                      │
│    HasNewFindings(prevFindings) ?                                               │
│         │                                                                      │
│    ┌────┴────┐                                                                 │
│    │ 有新发现 │          │ 无新发现 ──▶ dryCount++                              │
│    │ dryCount=0│         │   dryCount ≥ 2 ?                                    │
│    │ 继续下一轮│         │    YES ──▶ ✅ DRY — exit cycle loop                  │
│    └─────────┘          │    NO  ──▶ 继续下一轮                                │
│                         └──────────────────────┘                                │
│                                                                                │
│  → Leader.ReviewCycle(report) → accept/reject/escalate                         │
│  → CycleAccept: batch 完成, 进入下一个 batch (DAG 依赖满足则放行)              │
│  → CycleReject: 重试当前 batch                                                 │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                     成员 Push Back — 质疑与回应                                 │
│                                                                                │
│  Worker 发现问题时：                                                            │
│                                                                                │
│  1. Worker 读 input.md → 发现问题                                               │
│     "上游产出还没到位" / "任务描述有歧义" / "能力不匹配"                          │
│                                                                                │
│  2. Worker 写 inbox 消息给 Leader                                               │
│     WriteMessage(leaderTaskID, msg)                                             │
│                                                                                │
│  3. Leader 收到消息 → 响应                                                      │
│     - 补充说明 → Worker 重新开始                                                 │
│     - 调整任务描述 → Worker 重新开始                                             │
│     - 重新分解 → 替换原任务                                                      │
│                                                                                │
│  Verifier 否决 Leader 验收时：                                                  │
│                                                                                │
│  1. Leader 说 "通过了"                                                          │
│  2. Verifier 写 inbox 消息 → "还有安全问题"                                     │
│  3. Leader 必须响应 → 回退重做或调整验收标准                                      │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                     分解任务确认 — Agent 评估可执行性                            │
│                                                                                │
│  Leader 分解完任务后：                                                           │
│                                                                                │
│  1. Leader 输出分解方案（PlanTasks）                                             │
│  2. Engine 对每个 PlanTask，向对应 agent 发送确认请求                             │
│  3. Agent 评估：                                                                │
│     - 任务描述是否清晰？                                                         │
│     - 上游依赖是否满足？                                                         │
│     - 能力是否匹配？                                                             │
│  4. Agent 确认可执行 → 正式创建任务                                              │
│  5. Agent 质疑 → 反馈给 Leader 重新分解                                          │
│                                                                                │
│  这是 Plan → Work → Verify loop 在"分解任务"环节的体现：                        │
│  Planner = Leader+LLM，Verifier = 接收 agent                                   │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                           WebSocket — 实时通讯                                  │
│                                                                                │
│  Whale CLI ─────────── WS ──────────▶ Dashboard                                │
│  ├─ SendTaskEvent(event)              ├─ HandleWebSocket → fireEngineEvent     │
│  │  (worker/checker/verifier/leader)  │  → EventsEmit("task-event")            │
│  │                                    │  → 前端 onTaskEvent                    │
│  │                                    │     ├─ EventLeaderLog → 标记未读       │
│  │                                    │     └─ EventAgentLog → 刷新对话       │
│  │                                    │                                        │
│  Dashboard ───────── WS ──────────▶ Whale CLI                                  │
│  ├─ cancel_master ─▶ OnCancel       ←──▶ CancelAutoExecute (context.Cancel)   │
│  └─ resume        ─▶ OnResume       ←──▶ AutoExecuteMasterTask               │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                              PERSISTENCE                                       │
│                                                                                │
│  FileTaskStore (文件系统)              Whiteboard (文件系统)                     │
│  ├─ master_tasks/                      ├─ {taskID}/input.md                    │
│  │   {id}/meta.json                    ├─ {taskID}/output.md                   │
│  │   {id}/goal.md                      ├─ {taskID}/verifier.md                  │
│  │   {id}/output.md                    ├─ {taskID}/confirmation.md              │
│  │   {id}/verify.md                    ├─ {taskID}/artifacts/                  │
│  │                                     ├─ {taskID}/inbox/                     │
│  ├─ tasks/                             ├─ {taskID}/outbox/                    │
│  │   (同上结构)                         ├─ masters/{masterID}/chat/            │
│  │                                     ├─ logs/leader/decompose_*.md          │
│  │                                     ├─ logs/leader/review_*.md             │
│  │                                     └─ logs/tasks/{taskID}/worker_*.md     │
│  │                                                                             │
│  ├─ checkpoint (JSON in master_task meta)                                      │
│  │   completed_batches, batch_cycles,                                        │
│  │   batch_depends_on, completed_outputs                                     │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                          协作铁律（Leader 内部约束）                             │
│                                                                                │
│  所有 Leader 共有的行为约束，注入 Leader prompt：                                │
│                                                                                │
│  1. 你是编排者，不是执行者——禁止自己写代码、写文档、做专业分析                     │
│  2. 每个专业产出必须由对应角色输出后再采信，你只做编排与汇编                       │
│  3. 未完成前序任务不可跳到后续任务                                                │
│  4. 验证不通过的任务必须回退重做，不可跳过                                        │
│  5. 禁止自己代写任何团队成员的专业产出                                           │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                     循环记忆（Loop Memory）— 理解债务管理                        │
│                                                                                │
│  防止 Agent 在重试循环中重复犯相同错误：                                          │
│                                                                                │
│  存储位置：Whiteboard master task 目录下的 loop.md                               │
│                                                                                │
│  RunTask 重试流程：                                                              │
│                                                                                │
│  1. 检测 RetryCount > 0                                                        │
│  2. ReadLoopMemory() → 获取历史失败摘要                                         │
│  3. 将循环记忆注入 Worker prompt 的 feedback 区域                                │
│  4. Worker 执行 → 产出                                                          │
│  5. 如果再次失败 → WriteLoopMemory() → 追加本次失败摘要                          │
│                                                                                │
│  关键：循环记忆是追加式的，每次重试都会累积更多上下文，                             │
│  帮助 Agent 避免重复已尝试过的失败路径                                            │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                     Push Back 实现细节                                          │
│                                                                                │
│  Worker Push Back：                                                             │
│  - Worker prompt 注入 Push Back 指令                                            │
│  - Worker 在产出中使用 [PUSH_BACK] 标记表示质疑                                  │
│  - Engine 检测到 [PUSH_BACK] → 自动转为 pending_confirmation                    │
│  - 等待用户确认后继续                                                            │
│                                                                                │
│  Verifier 否决：                                                                │
│  - 专业 Verifier 验证不通过 → 反馈追加到 task.Description                       │
│  - 触发重试或重新分解                                                            │
│                                                                                │
│  Whiteboard 消息：                                                               │
│  - Worker 也可通过 inbox/outbox 向 Leader 发送质疑消息                           │
│  - Leader 必须响应后 Worker 才能继续                                              │
└───────────────────────────────────────────────────────────────────────────────┘
