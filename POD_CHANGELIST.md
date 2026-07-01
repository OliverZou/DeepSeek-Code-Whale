# whale daemon 改名 — pod 端适配清单

## 核心结论：pod 端零改动

**WS 协议完全不变。** 所有消息类型保持 `task.*`，JSON 字段保持 `master_task_id`。

daemon 侧改动全部在 Go 内部标识符层面，不影响协议通信。

---

## 不变的（pod 无需修改）

| 项目 | 说明 |
|------|------|
| WS 消息类型 | `task.create` / `task.list` / `task.cancel` / `task.delete` / `task.subtasks` / `task.dialogue` / `task.plan` / `task.feedback` / `task.confirm` / `task.confirmations` / `task.rename` / `task.state_changed` / `task.log` |
| JSON key | `master_task_id` 全部保持不变 |
| `session.*` 消息 | 聊天会话管理不变 |
| `team.chat.*` 消息 | 团队聊天不变 |

---

## daemon 侧 Go 代码改动（仅供查阅）

### 为什么改名
消除 `MasterTask`（任务容器）与 `Task`（子任务）、`Session`（对话记录）之间的概念混淆。

### 类型/方法对照

| 旧 | 新 | 备注 |
|----|----|------|
| `MasterTask` struct | `TaskSession` | 区分于 chat `Session` |
| `Task.MasterTaskID` | `Task.TaskSessionID` | JSON tag `master_task_id` 不变 |
| `CreateMasterTask()` | `CreateTaskSession()` | |
| `GetMasterTask()` | `GetTaskSession()` | |
| `ListMasterTasks()` | `ListTaskSessions()` | |
| `ResumeMasterTask()` | `ResumeTaskSession()` | |
| `CancelMasterTaskExecution()` | `CancelTaskSessionExecution()` | |
| `DeleteMasterTaskAndChildren()` | `DeleteTaskSessionAndTasks()` | |
| `MasterTaskJSON` (bridge) | `TaskSessionJSON` | |
| `EventSyncMasterTasks` | `EventSyncTaskSessions` | |

---

## 如果 pod 直接引用了 daemon Go 包

```go
// 旧
import "github.com/usewhale/whale/internal/bridge"
var mts []bridge.MasterTaskJSON

// 新
var mts []bridge.TaskSessionJSON
```

---

## 验证

- [x] WS 消息类型 `task.*` 全部保持
- [x] JSON key `master_task_id` 全部保持
- [x] daemon 编译通过
- [x] daemon 测试通过
