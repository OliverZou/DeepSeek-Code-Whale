# Whale Pod 纯文件化任务状态 & Resume 方案

## 一、设计原则

> **任务不可变，历史永存。文档即状态，文件系统是唯一真源。**

核心理念：

- 不维护独立的"状态"字段（数据库或 meta.json 中的 `state`）
- 任务每个阶段的产出物落盘即为该阶段完成的证据
- **只有两个操作：New（新建任务）和 Resume（继续执行）**
- **没有 Restart。想重跑 = 新建一个同 goal 的任务**，新旧并存
- 旧任务是否删除，交给用户决定
- **产出隔离：任务产出留在自己的 task 目录中，不自动写入工作空间**

---

## 二、产出隔离：任务不污染工作空间

### 2.1 问题

同一 workspace 中执行两个 goal 相同的任务，后执行的产出文件会覆盖先执行的。或者任务跑了一半的脏文件残留在工作空间。

### 2.2 原则

```
任务产出 ──→ 留在 task 目录内
用户     ──→ 看到产出链接，自行审阅
用户     ──→ 主动选择"应用" → 复制到工作空间
用户     ──→ 主动指定输出目录 → 复制一份到目标目录
```

### 2.3 产出层级

```
{task_id}/
├── output.md       ← 任务执行摘要（Agent 自己写的报告）
├── out/            ← Agent 产出的实际代码/文件
│   ├── src/
│   │   └── login.go
│   └── test/
│       └── login_test.go
└── diff.patch      ← 如果启用了 git worktree，保留 diff
```

- `output.md`：人类可读的任务报告，描述做了什么、为什么这样做
- `out/`：Agent 产出的所有实际文件，读写都在这个目录下
- `diff.patch`：相对于工作空间的变更记录

### 2.4 用户交互

```
UI 中 task 完成后的展示：
┌─────────────────────────────────────────┐
│ ✅ 任务完成：实现用户登录接口              │
│                                         │
│ 📄 output.md — 查看执行摘要               │
│ 📁 out/ (5 个文件) — 查看产出文件          │
│                                         │
│ [应用到工作空间]  [复制到...]  [查看 Diff]  │
└─────────────────────────────────────────┘
```

- **应用到工作空间**：将 `out/` 内容复制到 workspace 对应路径
- **复制到...**：用户指定目录，复制一份过去
- **查看 Diff**：如果有 `diff.patch`，展示变更预览

### 2.5 Agent 视角

Agent 执行时，工作目录是 `{task_id}/out/` 而非 workspace。Agent 的 prompt 中明确：

```
你的工作目录: {task_dir}/out/
所有产出文件请写在这个目录下。
完成后在 output.md 中总结你的工作。
```

如果用户显式指定了输出目录（`--output /path/to/project`），引擎在任务完成后将 `out/` 内容复制一份到目标目录——是复制，不是移动。任务原稿始终保留。

---

## 三、两个核心操作

```
┌─────────────────────────────────────────────────────────────┐
│  New（新建）                                                 │
│  ─────────                                                  │
│  创建新的 session + master task + 从零 PlanAndRun            │
│  目标可以跟旧任务完全一样，但它是独立的、全新的执行            │
│                                                             │
│  Resume（继续）                                              │
│  ─────────────                                              │
│  对已存在的 master task，扫描文件系统                         │
│  推导所有 task 状态 → 跳过已完成的 → 从未完成处继续            │
│  同一个 task 目录，断点续跑，产出追加（或覆盖瞬态）            │
└─────────────────────────────────────────────────────────────┘
```

### 为什么没有 Restart

- Restart = 删除文件 + 重跑。但"删除"破坏历史，也引入复杂度
- 想重跑？复制 goal，新建一个 master task。新旧并存，历史完整
- loop 层（将来在 TE 之上）评估结果 → 不满意就新建一个 task 继续迭代
- TE 本身上升为 **可迭代执行的原子单元**，loop 层驱动多轮执行

---

## 四、当前文件结构

```
.whale/team_tasks/
├── plan.json                ← Leader 分解总计划（已存在）
├── {task_id}/
│   ├── meta.json            ← 结构元信息（state 字段 → 待移除）
│   ├── goal.md              ← 任务目标
│   ├── input.md             ← Worker 输入（Engine 写入）
│   ├── output.md            ← Worker 产出（人类可读摘要）
│   ├── out/                 ← Agent 实际产出的代码/文件
│   ├── verify.md            ← Verifier 产出
│   └── plan.json            ← 自分解子任务（可选）
├── masters/{master_id}/
│   ├── meta.json
│   └── goal.md
├── deliverable.md           ← 最终汇集产物
└── board/
    └── inbox/outbox/        ← Agent 间通信
```

### meta.json 字段分类

```json
{
  // === 结构属性：分解时确定，不可变 ===
  "id": "task_001",
  "title": "实现用户登录接口",
  "description": "...",
  "role": "backend-developer",
  "output": "output.md",
  "parent_ids": ["task_000"],
  "batch_id": "batch_1",
  "master_task_id": "m_123",
  "max_retries": 3,
  "created_at": "2026-06-21T10:00:00Z",

  // === 运行时状态：应从文件推导 → 移除 ===
  "state": "producing",
  "retry_count": 2
}
```

---

## 五、纯文件状态方案

### 5.1 文件 → 状态映射

```
文件存在情况                               → 推导状态
──────────────────────────────────────────────────────────
confirmation.md 存在                       → pending_confirmation
output.md + verify.md 都存在               → done
output.md 存在，verify.md 不存在            → produced
input.md 存在                              → assigned
以上都不存在                                → pending
```

### 5.2 deriveState() 改造

```go
func (fs *FileTaskStore) deriveState(dir string) TaskState {
    hasConfirmation := fileExists(dir, "confirmation.md")
    hasOutput := fileExists(dir, "output.md")
    hasVerify := fileExists(dir, "verify.md") || fileExists(dir, "verifier.md")
    hasInput := fileExists(dir, "input.md")

    if hasConfirmation { return TaskStatePendingConfirmation }
    if hasOutput && hasVerify { return TaskStateDone }
    if hasOutput { return TaskStateProduced }
    if hasInput { return TaskStateAssigned }
    return TaskStatePending
}
```

关键改动：

1. **不再 fallback 到 meta.State**：当前代码在文件不存在时会读 `meta.State` 作为 fallback，彻底去掉
2. **移除 meta.json 中的 state 字段**：`taskMeta` struct 去掉 `State`、`RetryCount`
3. **TransitionState 调用改为文件操作**：
   - `TaskStateProducing` / `TaskStateVerifying` → 瞬态，写 `.running`（含 PID）
   - `TaskStateDone` → 无文件操作（output.md + verify.md 已经存在）
   - `TaskStateFailed` → 写入 `error.md`
   - `TaskStateSuspended` → 写入 `suspended.md`
   - `TaskStatePendingConfirmation` → 写入 `confirmation.md`

### 5.3 中间瞬态不需要持久化

`producing`、`verifying`、`checking` 是进程正在运行的瞬态。Resume 不关心：

- `producing` 中崩溃 → output.md 不存在 → 重跑 produce
- `verifying` 中崩溃 → verify.md 不存在 → 重跑 verify

唯一需要持久化的运行时信息是 PID（用于清理孤儿进程），记录在 `.running` 中：

```json
{"pid": 12345, "started": "2026-06-21T10:05:00Z", "phase": "producing"}
```

### 5.4 锁与竞态

`.running` 同时充当互斥锁：

- Worker 启动时原子创建 `.running`（O_CREATE | O_EXCL）
- 创建成功 → 获得执行权
- 创建失败 → 检查 PID：存活则等待，不存活则清理后重新获取
- Worker 完成后删除 `.running`

---

## 六、Resume（继续执行）

### 6.1 核心逻辑

Resume = 读 `plan.json` → 遍历 batch → 推导每个 task 状态 → 跳过完成的 → 从未完成处继续。

### 6.2 PlanAndRun 改造

```go
func (e *TeamEngine) PlanAndRun(
    ctx context.Context, goal string, workdir string, mtID string,
    preDecomposed ...PlanTask,
) ([]*Batch, error) {

    // ── Step 1: 获取 Plan ──────────────────────────────────
    planTasks := preDecomposed
    if len(planTasks) == 0 {
        planTasks = e.readPlanJSON(workdir)   // 从文件恢复
    }
    if len(planTasks) == 0 {
        planTasks = e.decompose(goal)         // 首次 LLM 分解
        e.writePlanJSON(workdir, planTasks)
    }

    // ── Step 2: 创建 Task 目录（幂等） ──────────────────────
    for _, pt := range planTasks {
        if e.Store.TaskExistsByTitle(pt.Title, mtID) {
            continue   // 已存在，跳过
        }
        e.Store.InsertTask(pt, mtID)
    }

    // ── Step 3: Group → Batches ─────────────────────────────
    batches := groupByBatch(planTasks)

    // ── Step 4: 遍历 batch ──────────────────────────────────
    for _, batch := range batches {
        pendingTasks := e.resumeBatch(batch)
        if len(pendingTasks) == 0 {
            continue   // 本 batch 全部完成，跳过
        }
        e.executeBatch(ctx, pendingTasks)     // 只执行未完成的
        e.leaderReview(ctx, batch)            // 有新的完成 → 重新 review
    }

    // ── Step 5: Gather ──────────────────────────────────────
    e.gatherDeliverable(batches)
    e.Store.CompleteMasterTask(mtID)

    return batches, nil
}
```

### 6.3 resumeBatch — 筛选未完成任务

```go
func (e *TeamEngine) resumeBatch(batch *Batch) []*Task {
    var pending []*Task
    for _, t := range batch.Tasks {
        switch e.Store.DeriveState(e.Whiteboard.TaskDir(t.ID)) {
        case TaskStateDone:
            // 跳过
        case TaskStateProduced:
            // output 有但 verify 没有 → 补验证
            pending = append(pending, t)
        default:
            // pending / assigned → 完整执行
            pending = append(pending, t)
        }
    }
    return pending
}
```

### 6.4 executeBatch — 跳过已产出阶段

```go
func (e *TeamEngine) executeBatch(ctx context.Context, tasks []*Task) {
    for _, t := range tasks {
        switch e.Store.DeriveState(e.Whiteboard.TaskDir(t.ID)) {
        case TaskStateProduced:
            // 只跑 verify
            e.verifyTask(ctx, t)
        default:
            // 完整 produce + verify
            e.produceAndVerify(ctx, t)
        }
    }
}
```

### 6.5 孤儿进程处理

```go
func (e *TeamEngine) handleRunning(dir string) error {
    data, err := os.ReadFile(filepath.Join(dir, ".running"))
    if err != nil {
        return nil // 没有 .running，安全
    }
    var r struct { PID int `json:"pid"` }
    json.Unmarshal(data, &r)

    proc, err := os.FindProcess(r.PID)
    if err != nil {
        os.Remove(filepath.Join(dir, ".running")) // 孤儿，清理
        return nil
    }
    proc.Release()
    return fmt.Errorf("task still running: PID=%d", r.PID)
}
```

### 6.6 特殊情况

**同一 batch 内部分完成**：

```
Batch 2: [task_A: done] [task_B: produced] [task_C: pending]
         → resumeBatch 返回 [task_B, task_C]
         → task_B 只跑 verify，task_C 完整执行，task_A 跳过
         → Leader Review 涵盖全部三个 task
```

**Leader Review 断点**：batch 有新增完成的 task → 重新 Review；全部 done 且 review.md 存在 → 跳过。

**Gather 断点**：`deliverable.md` 已存在且所有 batch done → 跳过。

---

## 七、New（新建任务 = "重跑"）

### 7.1 语义

"重跑"不在 TE 内部实现。前端复制 goal，走 New 路径：

```go
// app.go：没有 RestartTask，只有 StartTask（= New）

func (a *App) StartTask(goal, teamName, workDir string) string {
    // 每次调用都是新的 session + 新的 master task
    sessionID := fmt.Sprintf("pod-%d", time.Now().UnixMilli())
    mt, _ := eng.CreateMasterTask(goal, taskWorkDir, sessionID)
    go eng.PlanAndRun(ctx, goal, taskWorkDir, mt.ID)
    return ""
}

func (a *App) ResumeTask(sessionID string) string {
    // 找到已有 session 对应的 master task，继续执行
    mt := ...
    planTasks := eng.ReadPlanJSON(mt.WorkspacePath)
    go eng.PlanAndRun(ctx, mt.Goal, mt.WorkspacePath, mt.ID, planTasks...)
    return ""
}
```

### 7.2 前端交互

```
任务列表，右键菜单：
  [继续执行]  — Resume，仅在中断的任务上可用
  [复制目标]  — 将 goal 复制到剪贴板 → 粘贴到新对话输入框 → StartTask
  [删除]      — 用户主动删除旧任务（删除整个 session + master task 目录）
```

"重跑"就是：复制旧任务的 goal → 点新建 → 粘贴 → 发送。两次执行产生两个独立的 master task，历史完整保留。

### 7.3 Loop 层的视角

将来在 TE 之上的 loop 层：

```go
func (loop *AgentLoop) Run(goal string) error {
    for attempt := 0; attempt < maxAttempts; attempt++ {
        mt := loop.createMasterTask(goal)        // 每次新建
        batches, err := loop.engine.PlanAndRun(ctx, goal, workdir, mt.ID)

        decision := loop.evaluate(batches)       // 评估结果
        if decision == Accept {
            return nil
        }
        // 不满意 → 下一轮循环创建新的 master task
        // 旧的 master task 保留，作为历史记录
    }
}
```

每次迭代产生一个独立的 `master_{id}/` 目录，loop 层只需管理这些目录的引用关系。

---

## 八、状态变迁图

```
                      ┌─────────────┐
                      │   pending   │  (无产出文件)
                      └──────┬──────┘
                             │ 写入 input.md
                             ▼
                      ┌─────────────┐
                      │  assigned   │  (input.md 存在)
                      └──────┬──────┘
                             │ Worker 开始执行
                             ▼
             ┌───────────────────────────────┐
             │         producing             │  (瞬态，.running 标记)
             │   Worker 写入 output.md       │
             └───────────────┬───────────────┘
                             │ output.md 完成
                             ▼
                      ┌─────────────┐
                      │  produced   │  (output.md 存在，verify.md 不存在)
                      └──────┬──────┘
                             │ Verifier 开始
                             ▼
             ┌───────────────────────────────┐
             │         verifying            │  (瞬态)
             │   Verifier 写入 verify.md    │
             └───────────────┬───────────────┘
                             │ verify.md 完成
                             ▼
             ┌───────────────┴───────────────┐
             │                               │
        verify PASS                    verify FAIL
             │                          (retry < max)
             ▼                               │
      ┌──────────┐                    ┌──────┴──────┐
      │   done   │                    │  重试       │
      │ output.md│                    │ 覆盖        │
      │ verify.md│                    │ output.md   │
      └──────────┘                    │ 重新 verify │
                                      └─────────────┘

             retry 耗尽 → 写入 error.md → failed
```

---

## 九、API 总览

```
TE 对外 API
────────────────────────────────────────────────
PlanAndRun(goal, workdir, mtID, preDecomposed?)
    — 幂等执行。New → 从头跑；Resume → 从断点续

ResumeTask(sessionID)
    — 对应前端"继续执行"按钮
    — 本质是 PlanAndRun(same_mtID) + plan.json 已存在

CreateMasterTask(goal, workdir, sessionID)
    — 创建新的执行单元。每次调用产生独立目录

ApplyOutput(taskID, targetDir)
    — 将 {task_id}/out/ 内容复制到 targetDir
    — 用户主动触发，不是自动

没有 Restart API
没有自动写入 workspace 的行为

Loop 层能力（将来）
────────────────────────────────────────────────
loop.evaluate(batches) → Accept | Retry
loop.createMasterTask(goal) → 新的执行单元
旧 master task 目录永不清除，loop 层记录迭代链
```

---

## 十、代码改动清单

### 10.1 filestore.go

| 改动 | 说明 |
|------|------|
| `deriveState()` 去掉 fallback | 不再读 `meta.State`，纯文件推导 |
| `taskMeta` struct 去掉 `State`、`RetryCount` | 只保留结构属性 |
| `writeMeta()` / `readMeta()` 不处理 state | |
| 新增 `fileExists()` | 封装 `os.Stat` |
| `TransitionState()` 改造 | 终态写标记文件，瞬态写 `.running` |
| `rebuildIndex()` 改用 `deriveState` | 初始化时从文件推导所有状态 |

### 10.2 team_engine.go

| 改动 | 说明 |
|------|------|
| `PlanAndRun()` 加 resume 入口 | 读 plan.json → 判断新/续 |
| 新增 `resumeBatch()` | 遍历 batch，过滤已完成 |
| `executeBatch()` 支持跳过 | produced → 只跑 verify |
| 产出目录改为 `{task_id}/out/` | Agent 工作目录隔离 |
| `ApplyOutput(taskID, targetDir)` | 新增，复制产出到目标 |
| `CancelTask()` → 写入 `suspended.md` | |
| `ConfirmTask()` → 删除 `confirmation.md` | |

### 10.3 app.go

| 改动 | 说明 |
|------|------|
| `ResumeTask(sessionID)` | 从断点继续执行 |
| `ApplyOutput(taskID, targetDir)` | 用户主动应用产出 |
| `GetMasterTasks()` 状态从 `deriveState` 推导 | 不再依赖 meta.state |
| **不添加** `RestartTask` | |
| 保留 `StartTask` / `StartExpertTask` | 语义不变：每次调用 = 新建任务 |

### 10.4 前端

| 改动 | 说明 |
|------|------|
| 右键菜单加 [继续执行] | 仅中断任务可见 |
| 右键菜单加 [复制目标] | 复制 goal 到剪贴板 |
| 右键菜单保留 [删除] | 用户主动清理旧任务 |
| Task 详情加 [应用到工作空间] 按钮 | 调用 ApplyOutput |
| Task 详情展示产出文件列表 | 从 `out/` 目录读取 |
| 去除与 Restart 相关的 UI | |

---

## 十一、实施计划

| 阶段 | 内容 | 预计工作量 |
|------|------|-----------|
| **Phase 1** | 纯文件状态：deriveState 去 fallback，meta.json 去 state | 半天 |
| **Phase 2** | 产出隔离：Agent 工作目录改为 `out/`，ApplyOutput API | 半天 |
| **Phase 3** | Resume：PlanAndRun 入口改造，resumeBatch，孤儿进程 | 1 天 |
| **Phase 4** | 前端：继续执行 + 应用产出 + 复制目标 | 半天 |
| **合计** | | ~2.5 天 |
