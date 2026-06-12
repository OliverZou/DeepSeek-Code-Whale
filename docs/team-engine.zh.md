# Team Engine — 多智能体编排引擎

## 概述

Team Engine 实现了 Leader-Worker-Verifier 多智能体协作模型，将复杂目标分解为子任务，通过专用 Worker 执行，再通过 Verifier 进行对抗性审查。

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
| `verify` | 读文件 + shell + 搜索（不能写） | Verifier |

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

### 状态说明

| 状态 | 说明 | 是否可恢复 |
|------|------|-----------|
| `pending` | 等待执行 | — |
| `assigned` | 已分配 | — |
| `producing` | Worker 执行中 | — |
| `produced` | Worker 完成 | — |
| `verifying` | Verifier 审查中 | — |
| `verified` | 审查通过 | — |
| `done` | 完成 | ❌ 终止状态 |
| `failed` | 失败 | ❌ 终止状态 |
| `suspended` | 用户主动停止 | ✅ 可通过 Resume 恢复 |

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

## 事件推送

Engine 在任务状态变化时主动推送事件，替代轮询：

| 事件类型 | 触发时机 |
|---------|---------|
| `EventStateChanged` | 任何状态转换 |
| `EventWorkerOutput` | Worker 产出完成 |
| `EventVerifierResult` | Verifier 得出结论 |
| `EventTaskDone` | 任务最终完成或失败 |

Dashboard 通过 Wails `runtime.EventsEmit` 实时接收事件并更新界面。

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
