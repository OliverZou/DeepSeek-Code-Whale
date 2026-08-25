# Whale daemon 收敛为 Streamable HTTP 端点 + 优先支持 ACP 语义

> 状态：计划草案（仅文档，未实施）
> 日期：2026-08-25
> 关联：`internal/daemon`（现有 SSE 传输层）、`internal/runtime/protocol`（前端无关契约）、`cmd/whale-acp`（已接入的 ACP 协议）

---

## 1. 背景与目标

当前 agent 前端与核心的边界已经解耦：`internal/agent → internal/app/service.Service` 通过
`Events()` 通道 + `DispatchProtocol(Intent)` 对外，`internal/tui`（Bubble Tea）只是消费者之一，
SSE 传输层（`internal/daemon`）也已经存在。

痛点：

- 目前 daemon 是「自定义 SSE + 散落的 REST 端点」，为 agent 侧暴露的命令/查询能力有限，
  前端要覆盖全功能就得为一堆 pull 型能力（`SkillSuggestions`、`RunningBackgroundShells`、
  `ViewMode`/`SetViewMode`、`Model`/`ReasoningEffort`/`ThinkingEnabled`、`PrepareOpenCommand`）
  单独造 `/api/session/*` 端点。
- 这些语义（请求/响应 + 事件推送 + 通知）本是 MCP / ACP 已经标准化过的，自造一套浪费且不互通。

目标：

1. 把 `internal/daemon` 收敛成 **Streamable HTTP** 端点形态——单一 POST 端点承载请求/响应，
   一条 SSE 流承载事件；不再为每个能力自造 REST 端点。
2. **优先复用 `cmd/whale-acp` 已实现的 ACP 语义**（工具/通知/会话），前端可直接用现成的
   MCP/ACP 客户端库，避免重新发明。
3. 前端（TUI / Web / 桌面）成为该端点的客户端，解耦渲染与核心。

## 2. 现状（已确认）

```
internal/agent
   ↓
internal/runtime/protocol   ← 前端无关契约（Event / Intent / SkillView / Approval…）
   ↓
internal/app/service.Service  ← 前端边界（Events() 通道 + DispatchProtocol(Intent)）
   ├── 进程内 channel → internal/ui/tuiadapter → internal/tui
   └── HTTP/SSE        → internal/daemon
                           ├── GET  /events         SSE 事件流（已有）
                           ├── POST /api/intent      意图下发（已有）
                           └── POST /api/team/...    team 端点（已有）
```

已有产物：

- `internal/daemon`：`/events`（SSE，含 keepalive）、`/api/intent`、team 六动词 + DAG 快照、
  EventHub（广播/慢消费者丢弃）、Auth 中间件、回环绑定（127.0.0.1）。
- `internal/runtime/protocol`：`Event`（agent_turn/local_submit 等约 50 种）、`Intent`、
  `SkillView`、`ApprovalRequest`、`Message`/`ToolCall`、`ClientMessage`/`ServerMessage`。
- `cmd/whale-acp`：Agent Client Protocol 接入（现成 ACP 语义）。
- `cmd/whale app-server`：Whale app-server 协议（stdio，hidden 命令）。

## 3. 方案：Streamable HTTP + ACP 优先

采用 MCP / ACP 的 **Streamable HTTP** 传输形态（参考
[ACP streamable-http-websocket transport](https://agentclientprotocol.com/rfds/streamable-http-websocket-transport)、
[MCP transports](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)）：

- **单个 POST 端点** 接收 JSON-RPC 请求；服务端要么直接回 JSON（无状态请求），要么回一条
  SSE 流（产生持续事件时）。
- **GET 通道** 提供持续的服务器事件流。
- 语义统一为 request/response + notification/push，不再为每个 pull 能力单独开 REST 端点。

优先级：先保证 ACP 语义（`whale-acp` 已有），再考虑 MCP，避免双轨。

## 4. 阶段计划

> 只做增量、可验证、可回退。不改变 `internal/agent` / `service.Service` 核心。

### 阶段 0：基线确认
- `go test ./...`（离线全套）确认 GREEN。
- 手动打通 `whale daemon`：`POST /api/intent` + `GET /events` 事件流可达。

### 阶段 1：补 agent pull/命令能力（后端，纯增量）
- 在 `internal/daemon` 增加 agent 侧查询/命令收敛端点，直接复用 `service.Service` 已有方法：
  - 查询：`Model` / `ReasoningEffort` / `ThinkingEnabled` / `ViewMode` / `ShowReasoning` /
    `SkillSuggestions` / `RunningBackgroundShells`。
  - 命令：`SetViewMode` / `PrepareOpenCommand`（open 命令）/ `RequestSessions` 等。
- 为这些端点加 `internal/daemon/routes_test.go`。
- 到此处，SSE 已能覆盖「纯事件前端」所需的全部能力。

### 阶段 2：写一个 Streamable HTTP 客户端抽象（前端）
- 新包 `internal/ui/sseclient`（或并入 tuiadapter），实现对齐 `tui.Runtime` 接口的 `SseRuntime`：
  `Events()` 从 SSE 流解析；`Dispatch`/命令走 HTTP；pull 型走查询端点。
- 用 `httptest` 模拟 daemon 测试：事件解析、意图 POST、状态查询。

### 阶段 3：极简试点前端
- 一个 mini TUI（输入 → POST intent；渲染事件流）接到 `SseRuntime`，验证端到端
  （`whale daemon` + mini 前端）能完成一次真实对话与工具审批。
- 该前端独立成小包，不污染现有 `internal/tui`。

### 阶段 4：决策并替换前端
- 选项：重写 bubbletea / 换 `tview`·`gocui` / 复用现有 `internal/tui` 但把 `tuiadapter.Runtime`
  底层从 `service.Service` 换成 `SseRuntime`。
- 现有 TUI 的 UX（focus / composer / review / keymap / windows paste / scrollback）保留或分阶段迁移。
- 更新 `cmd/whale` 启动路径：默认起/连 daemon + 前端走 SSE；或保留 in-process 作 `--local` 快路径。
- 补 `internal/tui` / `cli` 级行为测试。

### 阶段 5：收尾
- 清理：若不再用进程内直连，可迁移/删除 `internal/ui/tuiadapter` 与 `internal/tui`；保留则作为 `--local`。
- 更新 `docs/` 文档；`make build` 出单二进制确认形态。
- 全量 `go test ./...` + `go test ./internal/tui/...` + `go test ./internal/evals/...`。

## 5. 风险

1. **SSE 单向、无内建确认**：所有“请求-响应”靠 HTTP 端点兜底——阶段 1 专门处理，避免前端用不了 pull 能力。
2. **终端 UX 重写成本**：最大工作量，与「SSE vs channel」无关——用阶段 3 的极简前端先验证传输，再投入 UX。
3. **默认入口 / 会话生命周期变化**：属于 broad command-surface / session-format 级改动，应先开 issue 明确迁移与回退。
4. **单二进制形态**：daemon 化可能改变「一条命令即用」；阶段 4 明确 `whale`（起 daemon+前端）vs `whale daemon`（仅服务）。

## 6. 待决策

- “更好的 TUI”具体选哪个框架（或复用现有 UI 只换传输层）。
- 是否保留 in-process `--local` 快路径。
- 是否同时接 MCP，还是只做 ACP（建议先只做 ACP，避免双轨成本）。
- 是否在 repo 开 issue（AGENTS.md 要求 broad change 先开 issue）。

## 7. 预计主要产出文件

- `internal/daemon/routes.go` / `routes_test.go`（补 pull/命令端点）
- `internal/ui/sseclient`（新包：Streamable HTTP+SSE 的 `Runtime` 实现）
- 一个极简试点终端前端
- （视选定方案）重写或复用 `internal/tui`，调整 `cmd/whale` 启动路径
