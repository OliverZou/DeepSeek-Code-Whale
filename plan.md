# Whale Pod — Phase 1 实施方案

## 目标

将 whale-pod 从功能原型打磨为**可日常使用的 AI 编程桌面助手**，补齐核心体验缺口，达到内部试用标准。

## 当前状态总览

| 维度 | 状态 | 说明 |
|------|------|------|
| 窗口框架 | ✅ 完成 | 无边框窗口 + 自定义 TitleBar + 拖拽 |
| Agent 系统 | ✅ 完成 | expert/team/whale 三类，缓存定义，角色注入 |
| 会话管理 | ✅ 完成 | JSONL 存储，session-task 关联，分页列表 |
| 聊天系统 | ⚠️ 基础 | 非流式 HTTP 调用，60s 超时，纯文本展示 |
| Team Engine | ✅ 完成 | PlanAndRun，Leader-Worker-Verifier，持久化 |
| 任务面板 | ✅ 完成 | DAG 视图，子任务树，确认机制，对话日志 |
| 侧边栏 | ✅ 完成 | Agent 分组、搜索、工作区管理 |
| 多标签 | ✅ 完成 | 按 Agent 分组标签，关闭/切换 |
| 错误处理 | ❌ 薄弱 | 无重试、无离线提示、错误信息粗糙 |
| 设置/配置 | ❌ 缺失 | 无设置 UI，API Key 需手动配置文件 |
| Markdown 渲染 | ❌ 缺失 | AI 回复纯文本，无代码高亮 |
| 流式输出 | ❌ 缺失 | 阻塞等待完整响应 |
| 工具调用 | ❌ 缺失 | 聊天模式无工具执行能力 |

---

## Phase 1 任务分解

### Batch 1: 流式聊天引擎 (3-4 天)

**目标**: 将阻塞式 HTTP 调用替换为 SSE 流式，用户可以看到 AI 逐字输出。

#### Task 1.1: Go 后端 — SSE 流式调用

- 替换 `callLLM` 的同步 HTTP 为 SSE 流式读取
- 新增 `StreamChat` API，通过 Wails Events 向前端推送 chunk
- 支持 `reasoning_content` (DeepSeek-R1 思维链) 的流式推送
- 增加 AbortController 机制：用户可中断生成
- **文件**: `app.go` — 新增 `StreamChat`、`AbortChat` 方法

#### Task 1.2: 前端 — 流式消息渲染

- DirectChatView 接入 Wails `EventsOn("chat-chunk")` 事件
- 显示 typing indicator（生成中动画）
- 逐 chunk 追加到消息末尾，避免整段替换
- thinking 内容折叠显示（collapsible "思考过程"）
- **文件**: `DirectChatView.tsx`、`store.ts`

#### Task 1.3: 停止生成按钮

- 发送消息后，发送按钮变为「停止」按钮
- 点击后调用 `AbortChat`，后端取消 HTTP 请求
- 保留已生成的部分内容
- **文件**: `DirectChatView.tsx`

---

### Batch 2: Markdown 渲染 & 代码高亮 (1-2 天)

**目标**: AI 回复支持完整的 Markdown 渲染，代码块语法高亮。

#### Task 2.1: 集成 react-markdown + syntax highlighter

- 安装 `react-markdown`、`react-syntax-highlighter`、`remark-gfm`
- 自定义渲染组件：
  - 代码块：语法高亮 + 一键复制按钮
  - 表格：响应式滚动
  - 链接：外部浏览器打开
  - 图片：懒加载
- **文件**: `DirectChatView.tsx`（或新建 `MarkdownMessage.tsx`）

#### Task 2.2: 代码块操作

- 一键复制代码块内容
- "应用" 按钮：将代码块内容写入文件（需要用户确认）
- 语言标签显示
- **文件**: 新建 `CodeBlock.tsx`

---

### Batch 3: 设置面板 (2-3 天)

**目标**: 用户可通过 UI 配置所有必要参数，无需手动编辑配置文件。

#### Task 3.1: Go 后端 — 配置读写 API

- 新增 `GetSettings` / `SaveSettings` 方法
- 读写 `~/.whale/credentials.json` 和 `~/.whale/settings.json`
- 支持字段：API Key、Model、Temperature、MaxTokens、Theme
- 保存后重新加载引擎配置
- **文件**: `app.go` — 新增 Settings 相关方法

#### Task 3.2: 前端 — 设置面板 UI

- 右上角齿轮图标 → 滑出设置面板
- 表单字段：
  - API Key（密码框，带显示/隐藏切换）
  - 模型选择（deepseek-chat / deepseek-reasoner）
  - Temperature 滑块 (0-2)
  - MaxTokens 输入
  - 主题选择 (light / dark / system)
- 保存 / 取消按钮
- 连接测试按钮：发送 test ping 验证 API Key
- **文件**: 新建 `SettingsPanel.tsx`

#### Task 3.3: 主题系统

- CSS 变量定义亮/暗色板
- 跟随系统主题（`prefers-color-scheme`）
- 所有组件适配主题变量
- **文件**: `index.css`、各组件

---

### Batch 4: 工具调用 & 行动执行 (3-4 天)

**目标**: 聊天模式下 AI 可以建议并执行具体操作（创建文件、运行命令等）。

#### Task 4.1: 操作意图识别增强

- 改进 `detectIntent`：用正则 + 关键词识别 AI 建议的具体操作
- 解析操作类型：
  - `create_file`: 创建/修改文件 → 展示 diff 预览
  - `run_command`: 执行命令 → 展示命令预览
  - `install_package`: 安装依赖 → 展示包名
- **文件**: `app.go` — `detectIntent` 增强

#### Task 4.2: 操作卡片 UI

- AI 消息中嵌入操作卡片（非纯文本）
- 「创建文件」卡片：显示文件路径 + 内容预览
- 「运行命令」卡片：显示命令行 + 工作目录
- 每个卡片有「执行」/「跳过」按钮
- **文件**: 新建 `ActionCard.tsx`

#### Task 4.3: 操作执行 & 结果反馈

- 前端点击「执行」→ 调用后端 API 执行操作
- 后端新增 `ExecuteAction` 方法：写文件 / 执行命令
- 执行结果以新消息形式返回给 AI（作为 context 继续对话）
- 文件变更后刷新工作区状态
- **文件**: `app.go` — `ExecuteAction`；`store.ts` — `executeAction`

#### Task 4.4: Diff 预览

- 文件修改时显示 before/after diff
- 使用简单的行级 diff 算法（或集成 `diff` 库）
- 用户确认后写入
- **文件**: 新建 `DiffPreview.tsx`

---

### Batch 5: 会话体验优化 (2-3 天)

**目标**: 打磨聊天体验细节，接近主流 Chat 应用水平。

#### Task 5.1: 消息操作

- 每条 AI 消息：复制、重新生成、删除
- 用户消息：编辑（进入编辑模式）、删除
- 重新生成：删除最后一条 AI 消息后重新请求
- 操作按钮 hover 时显示（三点菜单）
- **文件**: `DirectChatView.tsx`

#### Task 5.2: 会话管理增强

- 会话列表显示最后一条消息预览
- 未读消息标记（蓝点）
- 批量删除会话
- 会话搜索（标题 + 内容）
- **文件**: `ChatHistoryPanel.tsx`、`Sidebar.tsx`

#### Task 5.3: 输入框增强

- Shift+Enter 换行，Enter 发送
- 输入框自动调整高度（最小 1 行，最大 8 行）
- @ 提及：输入 @ 触发 Agent 选择面板
- / 命令：输入 / 触发快捷命令面板
- 粘贴图片支持（剪贴板 + 拖拽）
- **文件**: `DirectChatView.tsx`

#### Task 5.4: 快捷键系统

- `Ctrl+N`：新建对话
- `Ctrl+Shift+N`：新建任务
- `Ctrl+K`：命令面板
- `Ctrl+,`：打开设置
- `Ctrl+W`：关闭当前标签
- `Ctrl+Tab` / `Ctrl+Shift+Tab`：切换标签
- `Escape`：关闭面板 / 取消操作
- **文件**: 新建 `useKeyboard.ts` hook

---

### Batch 6: 稳定性 & 错误处理 (2 天)

**目标**: 应用在各种异常情况下不崩溃，给出清晰的错误提示和恢复路径。

#### Task 6.1: 全局错误边界

- React Error Boundary 捕获组件崩溃
- 显示友好的错误页面 + 「刷新」按钮
- Go 后端 panic recovery（已有部分，需补全）
- **文件**: `App.tsx`、`app.go`

#### Task 6.2: 网络错误处理

- HTTP 请求超时 → 显示「请求超时，点击重试」
- API 认证失败 → 引导到设置页配置 Key
- 网络断开 → 显示离线状态指示器
- 自动重试：可重试错误（5xx、超时）自动重试 2 次
- **文件**: `app.go`、`DirectChatView.tsx`

#### Task 6.3: 数据完整性

- 会话文件损坏检测 + 自动跳过
- localStorage 满了的降级策略
- 定期自动清理过期会话（可配置保留天数）
- **文件**: `app.go`、`store.ts`

---

### Batch 7: 构建 & 发布 (2 天)

**目标**: 产出可分发的 Windows 安装包 / 便携版。

#### Task 7.1: 构建脚本优化

- 清理 `build.bat` / 新增 `build.ps1`
- 支持 `-dev` / `-release` 模式
- 版本号自动注入（从 git tag）
- 构建产物：`.exe` 便携版 + NSIS 安装包
- **文件**: `cmd/whale-pod/build.ps1`

#### Task 7.2: 自动更新机制

- 应用启动时检查 GitHub Releases 最新版本
- 提示用户下载更新
- （可选）自动下载并替换 exe
- **文件**: `app.go` — 新增 `CheckUpdate` 方法

#### Task 7.3: 图标 & 品牌

- 应用图标 `.ico` 多尺寸 (16/32/48/256)
- 任务栏 / 窗口标题图标
- About 对话框
- **文件**: `whale.png` → 多尺寸 ico

---

## 里程碑

| 里程碑 | 完成标准 | 预计日期 |
|--------|---------|---------|
| M1: 流式聊天可用 | 用户发送消息后逐字显示回复，可按停止 | Batch 1 完成 |
| M2: 体验基准达标 | Markdown 渲染 + 代码高亮 + 基本设置 | Batch 2-3 完成 |
| M3: 可操作 | AI 可创建文件 / 运行命令，用户确认后执行 | Batch 4 完成 |
| M4: 内部试用 | 全部 7 个 Batch 完成，可打包分发 | Batch 5-7 完成 |

---

## 不在此 Phase 的范围

以下功能有意识地在 Phase 1 中排除，留待后续：

- **WhaleNativeSpawner 集成** — 需要先解决接口 mismatch（见 [[team-engine-architecture]]）
- **Team Engine Pipeline 去 Barrier** — 架构级改造，影响面大（见 [[team-engine-vs-dynamic-workflow]]）
- **多窗口支持** — 单窗口先行
- **插件系统** — 等稳定后再设计
- **语音输入** — 需求不明确
- **移动端适配** — 仅桌面端
- **会话导入/导出** — Phase 2
- **Token 用量统计 & 计费** — Phase 2

---

## 风险 & 依赖

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| DeepSeek API 不支持 SSE 流式 | 流式功能受阻 | 降级为轮询模式或模拟流式 |
| Wails v2 事件性能瓶颈 | 高频 chunk 推送卡顿 | 前端 throttle (50ms) 合并 chunk |
| Windows Defender 误报 | 用户无法安装 | 代码签名证书 / 提交误报审查 |
| Team Engine 并发稳定性 | 多任务同时运行时崩溃 | 单任务锁已存在，加强测试覆盖 |

---

## 技术决策记录

1. **流式方案**: SSE over HTTP（DeepSeek API 原生支持 `stream: true`），后端通过 Wails Events 推送，前端 `EventsOn` 接收。不引入 WebSocket 避免增加复杂度。

2. **Markdown 渲染**: `react-markdown` + `react-syntax-highlighter`，服务端不做渲染。安全：禁止 HTML 标签，仅允许 Markdown。

3. **主题**: CSS 变量方案，3 个主题（light/dark/system），存储到 `localStorage`。不做主题编辑器。

4. **代码 diff**: 前端执行简单的行级 diff（使用 `diff` npm 包），不做语义 diff。

5. **构建**: 保持 Wails 原生构建流程，`build.ps1` 作为薄封装，添加版本注入和产物整理。
