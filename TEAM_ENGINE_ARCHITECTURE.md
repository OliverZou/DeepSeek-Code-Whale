# Team Engine — Architecture

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
│                                 └────────────┬────────────┘                  │
│                                              │                               │
│                    ┌─────────────────────────┼─────────────────────┐         │
│                    ▼                         ▼                     ▼         │
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
│  for attempt < maxRetries (3):                                                 │
│                                                                                │
│    ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐           │
│    │ pending  │ ──▶ │ assigned │ ──▶ │producing │ ──▶ │produced  │           │
│    └──────────┘     └──────────┘     └────┬─────┘     └────┬─────┘           │
│                                          │                  │                 │
│                                     Worker              ┌───▼──────────┐      │
│                                     spawn               │ verifying    │      │
│                                     subagent            │ Verifier     │      │
│                                     ┌─┴──┐              │ spawn        │      │
│                                     │output│             │ subagent     │      │
│                                     │.md  │             └──┬───┬───┬───┘      │
│                                     └────┘                │   │   │           │
│                                                  ┌────────┘   │   └──────┐    │
│                                                  ▼            ▼           ▼    │
│                                               PASS         FAIL       RETRY   │
│                                                 │            │           │    │
│                                                 ▼            ▼           │    │
│                                             verified     重试++       反馈    │
│                                               │       retries≥3?      追加    │
│                                               ▼            │           │    │
│                                           done ✅    suspended   ◀─────┘    │
│                                                       (等待 re-decompose)      │
│                                                              │                 │
│                                                  Leader.DecomposeTask()        │
│                                                  → 拆成 2-3 个小任务           │
│                                                  → 替换原任务, 继续执行        │
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
│                           WebSocket — 实时通讯                                  │
│                                                                                │
│  Whale CLI ─────────── WS ──────────▶ Dashboard                                │
│  ├─ SendTaskEvent(event)              ├─ HandleWebSocket → fireEngineEvent     │
│  │  (worker/verifier/leader log)      │  → EventsEmit("task-event")            │
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
│  SQLite (WAL mode)                    Whiteboard (文件系统)                     │
│  ├─ master_tasks                      ├─ {taskID}/input.md                    │
│  │   id, goal, status, progress       ├─ {taskID}/output.md                   │
│  │                                    ├─ {taskID}/verifier.md                  │
│  ├─ tasks                             ├─ {taskID}/artifacts/                  │
│  │   id, title, state, batch_id,     ├─ {taskID}/inbox/                     │
│  │   retry_count, verifier_feedback   ├─ {taskID}/outbox/                    │
│  │                                    ├─ logs/leader/decompose_*.md          │
│  ├─ state_history                     ├─ logs/leader/review_*.md             │
│  │                                    └─ logs/tasks/{taskID}/worker_*.md     │
│  ├─ checkpoint (JSON in master_tasks)                                         │
│  │   completed_batches, batch_cycles,                                        │
│  │   batch_depends_on, completed_outputs                                     │
└───────────────────────────────────────────────────────────────────────────────┘
