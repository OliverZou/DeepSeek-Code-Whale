# Whale Dashboard 需求规格

## 1. 概述

Whale Dashboard 是一个桌面应用（Wails v2），用于集中管理本机所有 Whale 工作区。展示各工作区的主任务（总任务）、子任务、对话记录和流程图，支持远程控制主任务的启停。

## 2. 工作区发现与注册

### 2.1 自动发现

Dashboard 启动时扫描本机所有运行中的 `whale.exe` 进程，通过读取进程 CWD 获取工作区路径，自动注册。

### 2.2 持久化

已注册的工作区路径持久化到 `workspaces.json`，下次启动时恢复。手动清理该文件可移除不需要的工作区。

### 2.3 手动注册

Whale CLI 通过 HTTP `POST /api/register` 主动注册，携带 `{"path": "..."}`。

### 2.4 去重

同一路径只注册一次。Windows 下路径比较大小写不敏感（`strings.EqualFold`），`E:\myai\fivego` 和 `e:\myai\fivego` 视为同一工作区。

### 2.5 通信

Whale CLI 通过 WebSocket `/ws?wsid=...` 与 Dashboard 保持长连接。连接状态决定工作区 `online` / `offline`。

## 3. 主任务列表（Sidebar）

### 3.1 数据来源

`GetMasterTasks()` 聚合所有已注册工作区的主任务。

### 3.2 显示规则

只显示有主任务的工作区。无 DB 或无主任务的工作区不占位、不显示。

| 工作区状态 | 显示 |
|-----------|------|
| 无 DB（engine == nil） | 不显示 |
| 有 DB，0 个主任务 | 不显示 |
| 有主任务 | 主任务卡片：目标摘要、进度条、运行/停止按钮 |

### 3.3 去重

按 master task ID（UUID）去重，不绑 workspace path。

### 3.4 离线显示

有主任务的离线工作区同样显示。`WorkspaceOnline` 字段标记连线状态，前端可据此置灰或禁用操作按钮。

## 4. 子任务列表

### 4.1 合成条目

有子任务时，列表顶部展示合成条目 `__leader__` — "📋 任务规划"，关联 TeamLeader 的分解计划与流程图。

### 4.2 子任务条目

每个子任务显示：状态圆点（pending/running/done/failed/suspended）、角色图标、标题、进度。

### 4.3 未读标记

子任务状态或进度变化时标记为未读（`unreadTasks` Set），对应条目显示脉冲动画和未读徽标。

### 4.4 自动选中

子任务列表加载后，如果没有已选中的条目，自动选中第一个。

## 5. 对话视图

### 5.1 TeamLeader 对话

选中 `__leader__` 时显示：
- 分解计划（`decompose_*.md`）
- 执行审查（`review_*.md`）
- 执行总结（`summary_*.md`）
- 流程图（SVG）

### 5.2 子任务对话

选中子任务时显示多轮 Worker ⟷ Verifier 对话：
- `input.md` — 任务输入
- `worker_NNN.md` — Worker 产出（按 round 编号）
- `verifier_NNN.md` — Verifier 审查
- `leader_feedback_NNN.md` — TeamLeader 反馈（轮间）
- `output.md` / `verifier.md` — 最终产出（无 round 时回退）

### 5.3 滚动行为

对话内容刷新时，只有用户原本在底部才自动滚到底部，保留主动上翻的阅读位置。

## 6. 实时更新

### 6.1 事件系统

使用进程内 EventBus（`internal/eventbus`），订阅 `team_engine` 和 `workspace` 两个 topic。

### 6.2 跨进程桥接

Dashboard 和 Whale CLI 的 EventBus 通过 WebSocket 桥接，实现事件互通：

```
Whale CLI 发布事件 → bridgeOut → WebSocket → Dashboard bridgeIn → PublishLocal
Dashboard 发布事件 → bridgeOut → WebSocket → Whale CLI bridgeIn → PublishLocal
```

- `Publish()` — 发布到本地订阅者 + 转发到桥接通道
- `PublishLocal()` — 仅本地订阅者（用于注入外部事件，防止回环）
- 桥接通道满时丢弃（非阻塞），日志记录丢包

### 6.3 Fan-out

Dashboard 侧一条 bridgeOut 事件广播到所有已连接 WebSocket 的 Whale CLI 进程。

### 6.4 去重

`emitUpdate()` 比较本次与上次 payload JSON，相同则跳过推送，避免前端无意义刷新。

### 6.5 前端事件

| 事件名 | 方向 | 说明 |
|--------|------|------|
| `update` | Go → JS | 全量主任务列表，前端直接替换 `state.masterTasks` |
| `task-event` | Go → JS | 单条任务事件，触发增量刷新 |

### 6.6 引擎就绪

Whale CLI 执行 `team_plan` 创建 DB 后发布 `engine_ready` 事件。Dashboard 收到后调用 `LoadEngineByPath` 关闭旧 engine、打开新 engine，确保读到 WAL 中的最新数据。

## 7. 操作

### 7.1 运行主任务

`POST /api/resume`：将 suspended/failed 子任务重置为 pending/assigned，通过 WebSocket（或 heartbeat 回退）通知 Whale CLI 拾取执行。

### 7.2 停止主任务

将主任务的所有非终态子任务 Kill，通知 Whale CLI 停止执行。

### 7.3 删除主任务

删除主任务及其所有子任务。删除后 Checkpoint WAL 并重新 Open engine，确保后续读取立即可见。

## 8. 前端架构

### 8.1 状态管理

```js
state = {
  masterTasks: [],   // 主任务列表
  selMtId: null,     // 当前选中的主任务 ID
  subtasks: [],      // 当前主任务的子任务列表
  selStId: null,     // 当前选中的子任务 ID
  activeTab: 'dialogue', // dialogue | flowchart
  unreadTasks: Set   // 有未读更新的子任务 ID
}
```

### 8.2 布局

三栏可拖拽调整宽度：
- 左侧：主任务列表（min 160px, max 500px）
- 中间：子任务列表（min 160px, max 500px）
- 右侧：对话/流程图视图

### 8.3 右键菜单

主任务右键 → 删除总任务（需确认）。

## 9. 配置

| 项 | 默认值 | 说明 |
|----|-------|------|
| 注册服务端口 | `127.0.0.1:8520` | HTTP + WebSocket |
| heartbeat 间隔 | 30s | Whale CLI → Dashboard WebSocket ping |
| 事件去抖间隔 | 500ms | eventLoop 收到事件后的 debounce |
| bridge 通道缓冲 | 128 | EventBus bridgeOut/bridgeIn 容量 |
| WebSocket 重连间隔 | 5s | 断线后自动重连 |

## 10. 日志

日志写入 `bin/whale-dashboard.teamlog`，关键标签：

| 标签 | 说明 |
|------|------|
| `[startup]` | 启动流程 |
| `[dashboard]` | 工作区注册、GetMasterTasks |
| `[frontend]` | emitUpdate、GetMasterTasks 调用 |
| `[bridge]` | EventBus 跨进程桥接事件流转 |

## 11. 约束

1. Dashboard 自身不执行子任务，只管理状态和发送命令
2. Engine 实例通过 bolt WAL 与 Whale CLI 并发读取同一 DB
3. 不做定时轮询 — 更新完全由事件驱动（EventBus + WebSocket 桥接）
4. 只显示有主任务的工作区，无主任务不占位
