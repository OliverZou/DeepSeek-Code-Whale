# 持久化 Session 和状态历史 — 基于 team-engine-go 分析的改进计划

## 一、team-engine-go 持久化分析结果

原始 team-engine-go 有以下持久化机制，我们需要移植或借鉴：

### 1.1 双表持久化

```sql
-- 任务主表
CREATE TABLE tasks (
    id, title, description, role, backend, tools, state,
    max_retries, retry_count, workdir, parent_ids,
    artifact_path, verifier_feedback, verifier_backend, verifier_focus,
    created_at, updated_at
);

-- ★ 状态变更历史（完整审计）★
CREATE TABLE state_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL,
    old_state  TEXT,
    new_state  TEXT NOT NULL,
    error_msg  TEXT NOT NULL DEFAULT '',
    changed_at TEXT NOT NULL,
    FOREIGN KEY (task_id) REFERENCES tasks(id)
);
```

### 1.2 实时流式输出 (RunStreaming)

```go
// team-engine-go 的 runner 是这样做的：
//
// 1. 启动 agent 子进程
// 2. 通过 stdout pipe 逐行读取
// 3. 每读一行立即写入 whiteboard.AppendOutput(taskID, line)
// 4. 同时累积完整输出用于最终返回
//
// 效果：dashboard 可以实时看到 agent 输出，而不是等到全部跑完
```

### 1.3 累积式重试上下文

```go
// 重试时，VERIFIER FEEDBACK 被追加到 task.Description：
task.Description += fmt.Sprintf(
    "\n\n[VERIFIER FEEDBACK - Attempt %d]\n%s",
    task.RetryCount, feedback,
)

// 效果：Worker 总是看到完整历史（原始任务 + 之前所有 feedback）
//     不需要持久 session → 通过累积 description 实现等价效果
```

### 1.4 自动 Schema 迁移

```go
// 向后兼容：老数据库自动加新列
db.Exec(`ALTER TABLE tasks ADD COLUMN tools TEXT NOT NULL DEFAULT ''`)
db.Exec(`ALTER TABLE tasks ADD COLUMN verifier_backend TEXT NOT NULL DEFAULT ''`)
db.Exec(`ALTER TABLE tasks ADD COLUMN verifier_focus TEXT NOT NULL DEFAULT ''`)
```

---

## 二、我们需要补齐的内容

### 计划 A：state_history 审计表

**新增 `state_history` 表**，记录每一次状态迁移：
```go
// db.go 新增
func (tdb *TaskDB) RecordStateHistory(taskID, oldState, newState, errorMsg string) error {
    _, err := tdb.db.Exec(
        `INSERT INTO state_history (task_id, old_state, new_state, error_msg, changed_at)
         VALUES (?, ?, ?, ?, ?)`,
        taskID, oldState, string(newState), errorMsg, time.Now().UTC().Format(time.RFC3339),
    )
    return err
}
```

- 修改 `TransitionState` 在每次迁移后自动调用 `RecordStateHistory`
- 新增 `GetTaskHistory(taskID)` 查询方法
- 新增 `whale team history <task-id>` CLI

### 计划 B：实时流式输出到 Whiteboard

当前 `WhaleNativeSpawner` 调用 `SpawnSubagentWithProgress`，这个 API **本身就支持 progress callback**。我们只需要：

```go
// spawner.go 中修改 WhaleNativeSpawner
func (s *WhaleNativeSpawner) SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
    resp, err := s.runner.SpawnSubagentWithProgress(ctx, tasksReq, func(p core.ToolProgress) {
        // ★ 关键改动：每次 tool 事件时更新 whiteboard
        if req.OnProgress != nil {
            req.OnProgress(p)
        }
    })
    ...
}

// runner.go 中的 SubagentRequest 新增
type SubagentRequest struct {
    ...
    OnProgress func(core.ToolProgress) // 实时进度回调
}
```

- `SubagentRequest` 新增 `OnProgress` 回调
- `engine.RunTask` 传入 whiteboard 写入回调
- 每行 agent 输出立即追加到 `output.md`

### 计划 C：Schema 自动迁移

```go
// db.go 新增
func (tdb *TaskDB) migrate() error {
    // 已有的 initSchema
    initSchema(tdb.db)
    
    // 自动迁移：未来新增的列
    tdb.db.Exec(`ALTER TABLE tasks ADD COLUMN batch_id TEXT NOT NULL DEFAULT ''`)
    tdb.db.Exec(`ALTER TABLE tasks ADD COLUMN profile TEXT NOT NULL DEFAULT 'default'`)
    
    return nil
}
```

### 计划 D：Agent Memory（经验沉淀）

按论文要求，每个 Agent 应该有可跨 session 复用的经验记忆。设计为：

```go
// models.go 新增
type AgentMemory struct {
    ID          string    `json:"id"`
    AgentRole   string    `json:"role"`
    Key         string    `json:"key"`      // 经验标签
    Content     string    `json:"content"`   // 经验内容
    SourceTask  string    `json:"source_task"` // 来源任务
    CreatedAt   string    `json:"created_at"`
}

// db.go 新增表
// CREATE TABLE agent_memory (
//     id TEXT PRIMARY KEY,
//     agent_role TEXT NOT NULL,
//     key TEXT NOT NULL,
//     content TEXT NOT NULL,
//     source_task TEXT NOT NULL,
//     created_at TEXT NOT NULL
// );
```

和 whiteboard 配合：
```go
// whiteboard.go 新增
func (wb *Whiteboard) WriteMemory(role string, key string, content string) error
func (wb *Whiteboard) ReadMemory(role string, key string) (string, error)
func (wb *Whiteboard) BuildMemoryContext(role AgentRole) (string, error)
```

---

## 三、执行顺序

| 顺序 | 计划 | 文件 | 工作量 |
|------|------|------|--------|
| A | state_history 审计表 | `db.go`, `team_engine.go` | 小 |
| B | 实时流式输出 | `runner.go`, `spawner.go`, `team_engine.go` | 中 |
| C | Schema 自动迁移 | `db.go` | 小 |
| D | Agent Memory | `models.go`, `db.go`, `whiteboard.go` | 中 |

**推荐顺序：C → A → B → D（从最基础到最复杂）**

---

## 四、完成后的持久化全貌

```
执行任一 task 时：
  1. SQLite 记录 task 创建（tasks 表）
  2. 每次状态变更 → state_history 表记录（谁 + 什么时候 + 从哪 → 到哪）
  3. agent 执行中 → 逐行实时写入 whiteboard/output.md
  4. 重试时 → description 累积 VERIFIER FEEDBACK
  5. 完成后 → whiteboard 保存完整产出（output.md + verifier.md + artifacts/）
  6. Agent 经验 → memory 表跨 session 复用
  7. board.md → 全局进度汇总
  8. deliverable.md → 最终交付物清单
```
