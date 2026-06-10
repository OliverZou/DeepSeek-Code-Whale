# 实战架构对比：MiniMax Agent Team Overview 图 vs Whale 实现

基于识图结果（team_overview_analysis.md），逐项对比。

---

## 一、架构层级对比

### 论文架构图

```
User ──→ Leader ──→ Team Engine ──→ Batch 1: [Worker+Verifier] × N
                    (deterministic)    Batch 2: [Worker+Verifier] × N (depends_on Batch 1)
                    board.md            ...
                    deliverable.md    all PASS → plan_complete → CycleReport → Leader
```

### 我们的实现

```
User ──→ Leader ──→ TeamEngine ──→ Task 1: [Worker → Verifier]
                    (Go code)        Task 2: [Worker → Verifier] (parent=Task1)
                    whiteboard/       ...
                    input/output     all done → return results
```

---

## 二、逐项对比表

| # | 论文组件 | 职责 | 我们实现 | 状态 |
|---|---------|------|---------|------|
| 1 | **User** | 提目标 + 收交付 + escalate 介入 | `whale team plan --goal` / `whale team feedback` | ✅ |
| 2 | **Leader** | 拆 Plan → plan.yaml；审 CycleReport；决策 | `leader.go` Decompose() → `[]PlanTask` | ✅ 无 plan.yaml |
| 3 | **Team Engine** | 确定性代码，非 AI | `team_engine.go` 纯 Go 状态机 | ✅ |
| 4 | **Batch** | 同批次 Task 并行执行 | ❌ 无 Batch 概念，任务平铺 | 🔴 |
| 5 | **depends_on** | Batch 2 等 Batch 1 全 PASS | `ParentIDs` 字段 | ⚠️ per-task 级别 |
| 6 | **max_concurrency** | 控制并发数 | ❌ 无此参数 | 🔴 |
| 7 | **max_cycles** | 全局最大循环数 | ❌ 只有 per-task max_retries | 🔴 |
| 8 | **plan.yaml** | Leader 产出的正式计划文件 | ❌ 只有内存中 `[]PlanTask` | 🔴 |
| 9 | **board.md** | 团队共享进度白板 | ❌ 只有 per-task input/output | 🔴 |
| 10 | **deliverable.md** | 最终交付物汇总 | ❌ 不存在 | 🔴 |
| 11 | **CycleReport** | 每轮周期汇报给 Leader | ❌ Leader 不参与执行中反馈 | 🔴 |
| 12 | **Escalation** | 高风险/模糊/成本超支时请示用户 | ❌ 无此机制 | 🔴 |
| 13 | **Worker → Verifier** | 对抗验证对 | `RunTask` retry loop | ✅ |
| 14 | **独立 session** | 每 Task 独立上下文 | `spawn_subagent` 新 session | ✅ |
| 15 | **prompt/spawn/abort/kill** | 统一操作接口 | `channel.go` AgentChannel | ✅ |

---

## 三、关键差距详解

### 差距 1：缺少 Batch 概念 🔴

**论文要求**：
```yaml
# plan.yaml
batches:
  - id: batch-1
    tasks: [A1, A2, A3]      # 并行执行
    depends_on: []            # 无依赖
  - id: batch-2
    tasks: [B1]
    depends_on: [batch-1]     # 等 batch-1 全 PASS
```

**目前**：任务只有 `ParentIDs` 字段，无 Batch 分组。

**需要增加**：
```go
type Batch struct {
    ID         string
    Tasks      []*Task
    DependsOn  []string  // batch IDs
    Status     BatchStatus // pending → running → passed → failed
    Concurrency int      // max_concurrency
}
```

### 差距 2：缺少 CycleReport 🔴

**论文流程**：
```
Leader 拆 Plan → Engine 执行 Batch
                 → 每轮 CycleReport 回 Leader
                 → Leader 决定：接受 / 拒绝 / 改方向
                 → 可 escalate 给用户
```

**目前**：`PlanAndRun` 一次性跑完全部任务，Leader 不参与执行中决策。

**需要增加**：
```go
type CycleReport struct {
    BatchID    string
    TaskStates []TaskState
    Artifacts  []string  // board.md, deliverable.md 路径
    Decision   string    // "accept" | "reject" | "escalate"
}
```

### 差距 3：缺少正式产出追踪 (board.md / deliverable.md) 🔴

**论文要求**：Engine 负责跟踪 `board.md`（团队白板）和 `deliverable.md`（交付物）。

**目前**：Whiteboard 只有 per-task 的 `input.md` + `output.md` + `verifier.md`，没有跨任务的汇总板。

**需要增加**：
```
team_tasks/
├── board.md          ← 全局进度：所有 Batch/Task 状态一览
├── deliverable.md    ← 最终交付物清单
├── batch-1/
│   ├── A1/          ← 现有 per-task 结构
│   ├── A2/
│   └── A3/
└── batch-2/
    └── B1/
```

### 差距 4：缺少 Escalation 机制 🔴

**论文要求**：Engine 遇到"风险过高、需求模糊、成本超支"等场景时，应 suspend 当前流程，向用户请示。

**目前**：只有 `SendFeedback`（用户主动发消息给 Agent），没有 Engine 主动 suspend 等用户决策的机制。

**需要增加**：
```go
type EscalationRequest struct {
    Reason   string   // "high_risk" | "ambiguous" | "over_budget"
    Context  string
    Decision chan EscalationDecision  // 阻塞等待用户决策
}
```

---

## 四、总体评价

| 维度 | 评分 | 说明 |
|------|------|------|
| 核心状态机 | ✅ | pending→done 全链路正确 |
| Context 隔离 | ✅ | 每个 spawn 全新 session |
| 对抗验证 | ✅ | Worker⇄Verifier 多轮 retry |
| 统一操作接口 | ✅ | AgentChannel prompt/spawn/abort/kill |
| Agent 间通讯 | ✅ | inbox/outbox 消息总线 |
| **Batch 并行** | ❌ | **缺 Batch 概念，需新增 Stage 分组** |
| **CycleReport** | ❌ | **缺执行中汇报，Leader 一次决策到底** |
| **board/deliverable** | ❌ | **缺全局白板和交付物汇总** |
| **Escalation** | ❌ | **缺 Engine 主动暂停等用户决策** |

### 一句话

**核心的 LWV 三角色对抗模型和 Context 隔离是对的，但缺少了论文中的"工业化"特征：Batch 编排、CycleReport 反馈闭环、board.md 跟踪、Escalation 决策断点。** 这些是让 Team Engine 从"demo 级"到"生产级"的关键缺失。
