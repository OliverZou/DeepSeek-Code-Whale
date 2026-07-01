# Whale Pod 需求规格

## 1. 概述

Whale Pod 是 [whale](https://github.com/DeepSeek-Code-Whale/whale) 这个 AI 编程 agent 的**桌面 GUI 壳**（Wails v2 + React），无边框窗口。它本身不包含 Agent 逻辑、Team Engine 或任何 AI 能力——这些全部委托给 whale daemon。

用户通过 Pod 可以与 whale、各个专家（expert）、专家团（team）直接对话，体验与 whale TUI 完全一致。

### 核心架构

```
┌─────────────────────────────────────────────────────┐
│  Whale Pod (Wails v2)                               │
│  ┌─────────────┐  Wails binding   ┌──────────────┐  │
│  │  React 前端  │ ◄──────────────► │  Go 后端     │  │
│  │  (TypeScript)│  EventsEmit/On  │  (App struct)│  │
│  └─────────────┘                  └──────┬───────┘  │
│                                          │          │
│                                   WebSocket :18900  │
└──────────────────────────────────────────┼──────────┘
                                           │
                                ┌──────────▼──────────┐
                                │  Whale Daemon        │
                                │  (whale daemon start) │
                                │  – 聊天 (chat)        │
                                │  – 任务 (task.*)      │
                                │  – 专家团 (team.*)    │
                                │  – 审批 (approval.*)  │
                                │  – MCP 管理 (mcp.*)  │
                                │  – 文件 (file.*)      │
                                └─────────────────────┘
```

- **Go 后端**：Wails v2 App，提供 Bind 方法给前端调用，通过 WebSocket 与 whale daemon 通信
- **React 前端**：React 18 + TypeScript + Vite + Zustand 状态管理
- **WebSocket**：统一协议 (`protocol/messages.go`)，请求/响应 + 服务端推送模式
- **Whale Daemon**：可以是本地自动启动的进程，也可以是远程 daemon

## 2. 通信协议

### 传输

- WebSocket `ws://localhost:18900/ws`
- 消息格式 JSON

### 消息类型

**请求（Request）**：

| 字段 | 类型 | 说明 |
|------|------|------|
| `type` | string | 消息类型 |
| `id` | string | 请求 ID（UUID），响应会回传 |
| `payload` | object | 请求体 |

**推送（Push）**——无 ID 的服务器主动消息：

| 字段 | 类型 | 说明 |
|------|------|------|
| `type` | string | 事件类型 |
| `payload` | object | 事件数据 |

### 核心消息类型

| Type | 方向 | 说明 |
|------|------|------|
| `chat` | 前端→daemon | 发送聊天消息 |
| `chat.cancel` | 前端→daemon | 中断当前聊天 |
| `chat.stream` | daemon→前端 | 聊天流式输出 |
| `session.getMessages` | 前端→daemon | 获取历史消息 |
| `session.list` | 前端→daemon | 获取会话列表 |
| `session.delete` | 前端→daemon | 删除会话 |
| `session.listByAgent` | 前端→daemon | 按 Agent 列出会话（分页） |
| `task.create` | 前端→daemon | 创建任务（委托给 Team Engine） |
| `task.list` | 前端→daemon | 任务列表 |
| `task.cancel` | 前端→daemon | 取消任务 |
| `task.subtasks` | 前端→daemon | 获取子任务树 |
| `task.dialogue` | 前端→daemon | 获取子任务对话 |
| `task.plan` | 前端→daemon | 获取 Leader 规划 |
| `task.feedback` | 前端→daemon | 向任务发送反馈 |
| `task.confirm` | 前端→daemon | 审批/驳回任务 |
| `task.state_changed` | daemon→前端 | 任务状态变更推送 |
| `agent.list` | 前端→daemon | Agent 列表 |
| `expert.list` | 前端→daemon | 专家列表 |
| `team.list` | 前端→daemon | 团队列表 |
| `team.chat.send` | 前端→daemon | 向团队发送消息 |
| `team.chat.messages` | 前端→daemon | 获取团队聊天记录 |
| `approval.decision` | 前端→daemon | 审批决定 |
| `user_input.response` | 前端→daemon | 用户输入响应 |
| `approval.required` | daemon→前端 | 需要用户审批 |
| `user_input.required` | daemon→前端 | 需要用户输入 |
| `mcp.list` | 前端→daemon | MCP 服务器列表 |
| `mcp.setEnabled` | 前端→daemon | 启用/禁用 MCP 服务器 |
| `mcp.setEnv` | 前端→daemon | 设置 MCP 环境变量 |
| `file.read` | 前端→daemon | 读取文件内容 |

### 聊天流式推送 (`chat.stream`)

聊天回复以流式推送返回，一个 chunk 包含以下事件类型：

| Event | 说明 |
|-------|------|
| `assistant` | Markdown 文本 delta，追加到消息体 |
| `thinking` | 推理过程 delta，在可折叠区域展示 |
| `tool_call` | 工具即将执行（含 ToolCallID + ToolName + ToolInput） |
| `tool_result` | 工具执行完成（含 ToolCallID + Outcome + Status） |
| `plan` | 规划步骤 delta |
| `subagent` | 子 agent 生命周期通知 |
| `task` | 并行推理任务生命周期 |
| `hook` | Hook 生命周期通知 |
| `error` | 错误消息 |
| `done` | 本轮回复完成（Done=true） |

## 3. Go 后端架构

### 入口 (`main.go`)

Wails v2 启动，1200×800 无边框窗口，绑定 `App` 结构体。

依赖：`github.com/wailsapp/wails/v2`、`github.com/gorilla/websocket`、`github.com/google/uuid`

### App 结构体 (`app.go`)

核心结构，包含所有从前端暴露的 Bind 方法：

| 方法 | 说明 |
|------|------|
| `startup / shutdown` | 生命周期，自动启动/停止 whale daemon |
| `Chat` | 发送聊天，通过 Wails EventsEmit 推送流式结果 |
| `AbortChat` | 中断聊天 |
| `GetMessages` | 获取会话历史 |
| `CreateTask / ListTasks` | 创建/列出任务 |
| `CancelTask / DeleteTask` | 取消/删除任务 |
| `GetMasterTasks` | 获取会话列表（来自 daemon） |
| `GetSubtasksBySession` | 获取子任务树 |
| `GetAgentDialogue` | 获取 agent 对话 |
| `GetLeaderPlan` | 获取 Leader 规划 |
| `GetTeamChat / SendTeamChat` | 团队聊天 |
| `ReadFileContent` | 读取文件 |
| `SendFeedback` | 发送反馈 |
| `RenameMasterTask` | 重命名会话 |
| `ConfirmTask` | 审批/驳回任务 |
| `ListAgents / ListExperts / ListTeams` | 查询 daemon 的角色/团队列表 |
| `ListMCPServers / SetMCPServerEnabled / SetMCPServerEnv` | MCP 管理 |
| `RespondApproval / RespondUserInput` | 审批/用户输入响应 |
| `GetSettings / SaveSettings / TestConnection` | 设置管理 |
| `GetConnectionStatus` | 连接状态查询 |
| `WindowMinimize / WindowMaximize / WindowClose` | 窗口控制 |
| `StartWindowDrag` | Win32 原生窗口拖拽 |

### WebSocket 客户端 (`wsclient/client.go`)

- `Connect()` — 连接 daemon
- `Send(type, payload)` — 发送请求，等待响应（30s 超时）
- `SendAsync(type, payload)` — 发送请求，不等待响应
- `OnPush(fn)` — 注册推送消息回调
- `OnClose(fn)` — 注册断连回调
- 读循环自动路由：有 `id` → 响应分发给 pending map；无 `id` → 调用 OnPush

### 协议定义 (`protocol/messages.go`)

共享的 JSON 消息结构，与 whale daemon 的 `internal/server/ws.go` 保持同步。

定义了所有请求/响应/推送类型的 Go struct。

### 会话管理 (`session_ops.go`)

- `ListSessionsByAgent(agent, offset, limit)` — 按 Agent 分页列会话
- `DeleteSession / DeleteAllSessions / ClearEmptySessions` — 删除操作
- `SessionHasMore` — 分页检测

### 召唤管理 (`summoned.go`)

持久化用户召唤的专家/团队列表到 `~/.whale/pod/summoned.json`。

### 设置管理 (`settings.go`)

- 读写 `~/.whale/credentials.json`（API Key）
- 读写 `~/.whale/settings.json`（Model、Temperature、MaxTokens、Theme）
- `TestConnection` 直连 DeepSeek API 验证 Key 有效性

### 类型定义 (`types.go`)

前后端共享的 JSON 序列化类型：`AgentInfoJSON`、`TeamDetailJSON`。

### 平台相关

- `app_windows.go` — 隐藏 CMD 窗口、Win32 窗口拖拽（ReleaseCapture + SendMessage）
- `app_other.go` — 空实现（`hideCmdWindow` 和 `StartWindowDrag` 均为 no-op）

## 4. React 前端

### 技术栈

- **框架**：React 18 + TypeScript
- **构建**：Vite 5
- **状态管理**：Zustand 4
- **Markdown**：react-markdown + rehype-raw + remark-gfm
- **代码高亮**：react-syntax-highlighter
- **HTML 净化**：DOMPurify

### 状态管理 (`store.ts`)

Zustand `create<PodState>` 全局状态。核心数据流：

```
组件 ──调用──► api (wails.ts) ──调用──► Go Bind
                         ▲
                         │ EventsOn
  Go ──EventsEmit──► events.ts ──► store.handleStreamChunk()
                                      store.handleTaskEvent()
                                      store.handleChatAction()
```

关键状态：

| 状态 | 说明 |
|------|------|
| `workDir / openWorkspaces` | 工作空间管理 |
| `masterTasks` | 会话/任务列表 |
| `selMasterTaskId / selSubtaskId` | 当前选中 |
| `subtasks / dialogue / leaderPlan` | 子任务和对话数据 |
| `chatMessages / directMessages` | 聊天消息 |
| `isStreaming / streamingContent / streamingThinking / streamingTools` | 流式状态 |
| `teams / teamDetails / agentDetails / summonedItems` | 角色和团队列表 |
| `connected / connectionStatus` | 连接状态 |
| `openTabs / selAgentId` | 多标签页管理 |
| `pendingTaskAction` | 待确认的任务操作 |
| `permissionEscalation` | 权限升级请求 |
| `pinnedTaskIds` | 固定任务 |

### 事件系统 (`events.ts`)

通过 Wails runtime `EventsOn/EventsEmit` 实现前后端通信：

| 事件名 | 方向 | 说明 |
|--------|------|------|
| `chat-chunk` | Go→前端 | 聊天流式数据块 |
| `chat-action` | Go→前端 | AI 发起的行动请求 |
| `session-update` | Go→前端 | 会话更新通知 |
| `connection-status` | Go→前端 | 连接状态变更 |
| `connected` | Go→前端 | 连接成功 |
| `task-event` | Go→前端 | 任务状态变更/Agent 日志 |
| `approval-required` | Go→前端 | 需要用户审批 |
| `user-input-required` | Go→前端 | 需要用户输入 |
| `error` | Go→前端 | 错误消息 |

### 前端 API 封装 (`wails.ts`)

`api` 对象封装所有 `window.go.main.App.*` 调用，提供类型安全的 Promise 接口。

关键方法：`streamChat`（发送消息并启动流式接收）、`directChat`（同前）、`abortChat`、`getMessages`、`getMasterTasks`、`getSubtasksBySession`、`listAgents/Experts/Teams` 等。

### 组件架构

| 组件 | 说明 |
|------|------|
| **TitleBar** | 自定义标题栏（无边框窗口），拖拽、最小化/最大化/关闭按钮 |
| **Sidebar** | 左侧面板：搜索框 + Agent/专家/团队列表 + 召唤按钮，底部固定 |
| **DirectChatView** | 聊天主视图，支持多会话 Tab |
| **ChatArea** | 聊天消息展示区域，自动滚动 |
| **ChatContent** | Markdown 渲染 + Thinking 折叠 + 工具调用卡片 |
| **ToolCallCard** | 工具调用卡片（Read/Write/Run/Search），可展开查看输入输出 |
| **ChatTabBar** | 多会话 Tab 栏，激活绿色底边线，hover 显示关闭按钮 |
| **ChatHistoryPanel** | 从左侧滑出的半透明侧边栏，分页加载历史会话 |
| **RightPanel** | 右侧浮动按钮组（历史 + 新会话） |
| **TaskControlPanel** | 子任务 DAG/列表视图切换 |
| **TaskTree** | 子任务树形列表，状态圆点 + 进度 |
| **DagView** | SVG 任务 DAG 可视化 |
| **DialoguePanel** | Agent 对话详细视图 |
| **ObserverPanel** | 任务观察面板 |
| **TaskObserverView** | 任务观察综合视图 |
| **SettingsPanel** | 设置面板（API Key、Model、Temperature、Theme） |
| **ExpertPanel** | 专家列表 |
| **TeamList** | 团队列表 |
| **ExpertDrawer** | 专家召唤抽屉 |
| **FunctionList** | 功能列表（创建任务 → 聊天） |
| **CreateTaskView** | 创建任务表单 |
| **AgentChatView** | Agent 聊天视图（旧版兼容） |
| **CollapsibleSection** | 可折叠区域组件 |
| **ConfirmDialog** | 确认对话框 |
| **ContextMenu** | 右键菜单 |
| **ErrorBoundary** | React 错误边界 |
| **MarkdownMessage** | Markdown 消息渲染（带代码高亮） |
| **MinimalSelect** | 精简下拉选择器 |
| **PinnedTasks** | 固定任务列表 |
| **Resizer** | 可拖拽的分隔栏 |
| **WorkspaceList** | 工作空间列表 |
| **useKeyboard** | 键盘快捷键 Hook |
| **toolCallParser** | 工具调用解析器 |

### 布局

```
┌─────────────────────────────────────────────────────┐
│  TitleBar (自定义标题栏)                     ┌─┬─┬─┐ │
├──────────┬──────────────────────────────────────────┤
│          │  RightPanel                              │
│ Sidebar  │  ┌─── ChatTabBar ───────────────────┐   │
│          │  │  Tab1 | Tab2 | Tab3    [+][x]    │   │
│ 搜索框    │  ├──────────────────────────────────┤   │
│          │  │  DirectChatView / TaskPanel        │   │
│ Agent    │  │  ┌─ 聊天消息 ──────────────────┐  │   │
│ 列表     │  │  │  Markdown + Thinking        │  │   │
│          │  │  │  + ToolCalls                │  │   │
│ ● Whale  │  │  └────────────────────────────┘  │   │
│ ● 专家A  │  │  [输入框] [发送]                  │   │
│ ● 团队B  │  ├──────────────────────────────────┤   │
│          │  │  TaskControlPanel (DAG/List)     │   │
│ 召唤专家  │  └──────────────────────────────────┘   │
├──────────┴──────────────────────────────────────────┤
│  状态栏：连接状态 / 会话数                            │
└─────────────────────────────────────────────────────┘
```

## 5. Whale Daemon 自动管理

Pod 会自动管理 whale daemon 的启动/停止：

1. 启动时搜索 `whale.exe`（与 pod 同目录或 `PATH`）
2. 执行 `whale daemon stop`（清理残留）
3. 执行 `whale daemon start --port 18900`
4. 等待端口就绪（最长 15s）
5. WebSocket 连接

也支持连接远程 daemon，URL 保存在 `~/.whale-pod/connection.json`。

数据文件路径：
- 本地 daemon 数据：`~/.whale/`
- Pod 自身数据：`~/.whale/pod/`
- 连接设置：`~/.whale-pod/connection.json`

## 6. 界面交互规范

### 图标

- 统一使用单色线条 SVG 或 Lucide 图标
- Whale 头像：`whale.png`（512×512）
- 专家头像：绿色渐变首字母圆形
- 团队头像：紫色渐变首字母圆形
- 下拉箭头：统一三角形 SVG（9×5）

### 颜色

| 用途 | 色值 |
|------|------|
| 背景 | `#141414` |
| 面板背景 | `#1e1e1e` |
| 边框 | `#333` |
| 主文字 | `#e0e0e0` |
| 次要文字 | `#888` |
| 强调色（绿） | `#4CAF50` |
| 强调色（蓝） | `#2196F3` |

### 交互

- 按钮默认透明无边框，hover 浅灰背景+浅边框，选中绿色背景+绿色边框
- 浮动按钮（历史、新会话）：透明无边框，hover 才显示边框和高亮背景
- 折叠章节：点击标题行展开/收起
- 支持亮色/暗色主题切换

## 7. 构建

```bash
# 前端构建
cd frontend && npm run build    # tsc + vite build

# 后端构建
wails build                      # 产物：bin/whale-pod.exe
```

构建流程：
1. `whale.png` → `appicon.png`
2. `npm run build` 构建前端
3. `wails build` 打包 Go 二进制

## 8. 待细化

- [ ] 工作空间持久化与多 workspace 支持
- [ ] 消息搜索/过滤
- [ ] 离线消息缓存
