# 架构图对比分析：论文设计 vs Whale 实现

基于识图结果（vision_analysis.md），逐项对照 MiniMax 架构图与我们的实现。

---

## 一、整体角色

| 图中角色 | 职责 | 实现状态 | 对应代码 |
|---------|------|---------|---------|
| **User** | 提目标 + 收交付 | ✅ | `whale team plan --goal "..."` |
| **Leader** | 编排者·拆任务·派发·交付 | ✅ | `leader.go` — `Decompose()` |
| **Worker** | 产出 | ✅ | `team_engine.go` — `RunTask()` producing 阶段 |
| **Verifier** | 验证·退回 | ✅ | `verifier.go` — `Verify()` |

---

## 二、六步主流程

```
① User → Leader:   提目标            ✅ PlanAndRun(goal)
② Leader → Plan:   拆 Stage / Task   ✅ leader.Decompose() → []PlanTask
③ Stage 1 PASS → 推进               ⚠️ 无显式 Stage 概念，通过 ParentIDs 隐式
④ Stage 2 → Worker/Verifier 循环    ✅ RunTask() produce→verify retry loop
⑤ Stage 2 PASS → Leader 汇总        ✅ RunPipeline() 返回 results map
⑥ Leader → User:   交付              ✅ PlanAndRun() 返回 []*Task
```

---

## 三、核心技术特征

### 3.1 同 Stage 内多 Task · 真并行 🔴 未实现

**图的要求**：
```
Stage 1 · 并行 cycles
同 Stage 内多 Task · 各自独立 cycle · 真并行
Task A ── Worker A ── Verifier A
Task B ── Worker B ── Verifier B  (同时运行)
```

**当前实现**：
```go
// team_engine.go PlanAndRun() — 顺序执行
for _, task := range tasks {
    e.RunTask(task.ID)  // 一个跑完才跑下一个
}
```

**需要改为**：
```go
var wg sync.WaitGroup
for _, task := range readyTasks {
    wg.Add(1)
    go func(t *Task) {
        defer wg.Done()
        e.RunTask(t.ID)
    }(task)
}
wg.Wait()
// 全部 PASS 后才进入下一 Stage
```

### 3.2 多阶段链 · 阶段间依赖 🟡 部分实现

**图的要求**：
- Stage 1 产物的交付物是 Stage 2 的输入
- Stage 1 全 PASS 才推进到 Stage 2

**当前实现**：
- `ParentIDs` 字段支持上游依赖
- `RunPipeline()` 检查依赖是否完成
- **缺少**：显式的 Stage 数据结构、Stage gate 检查、产物传递

### 3.3 Worker ⇄ Verifier 多轮对抗 ✅ 已实现

**图的要求**：
```
Worker 产出 → "停" → Verifier 验证
Verifier FAIL → "改" → Worker 修改
              → 循环直到 PASS
```

**当前实现**：
```go
for task.RetryCount < task.MaxRetries {
    // Producing → Produced
    // Verifying → Verified or Producing (retry)
}
```
✅ 完整实现，包括 exponential backoff、Context isolation

### 3.4 对抗式协作 · Context 隔离 ✅ 已实现

**图的要求**：
- Verifier 只看到 Worker 的产出，看不到内部推理
- 各 Worker 之间互相隔离

**当前实现**：
```go
// 每个 SpawnSubagent 创建全新 session
childSessionID()      → 新 session
BuildAgentRegistry()  → 新工具集
NewTurnLoop()         → 新 Agent 循环
```
✅ 通过 `WhaleNativeSpawner` + `SpawnSubagentWithProgress` 保证

### 3.5 Agent 与人类同权 ✅ 已实现（刚补齐）

**图的要求**：
- prompt / spawn / abort / kill 统一接口
- 任何渠道（用户、Agent、Engine）同权

**当前实现**：
```go
type AgentChannel interface {
    Prompt(ctx, PromptRequest) (*Message, error)
    Spawn(ctx, SpawnRequest) (*Task, error)
    Abort(ctx, taskID) error
    Kill(ctx, taskID) error
}
```
✅ 通过 `channel.go` + inbox/outbox 消息总线实现

---

## 四、差距总结

| 特征 | 图的要求 | 当前状态 | 优先级 |
|------|---------|---------|--------|
| 真并行（同 Stage 多 Task） | Stage 1 内 Task A/B 同时跑 | ❌ 顺序执行 | **高** |
| 显式 Stage 概念 | Stage 1 → Stage 2 分阶段推进 | ❌ 无 Stage 类型 | **高** |
| Stage gate | Stage 1 全 PASS 才推进 | ❌ 无 gate 检查 | **高** |
| 跨 Stage 产物传递 | Stage 1 输出是 Stage 2 输入 | ⚠️ 只有 ParentIDs | **中** |
| Worker ⇄ Verifier 对抗 | 多轮产出→验证→修改 | ✅ | - |
| Context 隔离 | 各角色独立上下文 | ✅ | - |
| 统一操作接口 | prompt/spawn/abort/kill | ✅ | - |
| Agent 间通讯 | 主动推送 + 按需查询 | ✅ | - |

---

## 五、待办

- [ ] 引入 `Stage` 数据结构（Stage 1, Stage 2, ...）
- [ ] `PlanAndRun` 改为按 Stage 并行执行
- [ ] 同一 Stage 内的 Task 用 goroutine + WaitGroup 并发
- [ ] Stage gate：本 Stage 所有 Task 都 `done` 后才推进
- [ ] 跨 Stage 产物传递：前一 Stage 的输出目录传给下一 Stage
