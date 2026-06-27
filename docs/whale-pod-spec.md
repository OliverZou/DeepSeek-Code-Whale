# Whale Pod 需求规格

## 1. 概述

Whale Pod 是一个桌面应用（Wails v2），内嵌 Team Engine，独立管理任务编排。无边框窗口，左侧 Sidebar + 右侧面板的两栏布局。

### 核心架构

Whale Pod 基于**递归的 Plan → Work → Verify 协作模型**：

- 每个任务环节都满足 Plan → Work → Verify 的闭环
- Plan 是主动的规划者——理解目标、规划方案、分解任务、分配工作
- 总任务的 verifier 是用户，子任务的 verifier 是专业 agent
- Leader 是编排者（主持者），不是独裁者——团队成员可以 push back
- Checker 做基础完整性检查，专业验证由 agent 承担

### 以 Agent 为中心的 UI

- Sidebar 只有搜索框 + Agent 列表 + 召唤专家（固定底部）
- 点击 Agent → 高亮 → 激活最后一个对话 → 无对话则自动创建
- 对话输入框只显示"和 XXX 对话"，不再有专家 combobox
- 对话历史面板：从左侧滑出的半透明侧边栏，带动画
- 同 Agent 多对话以 tab 形式叠加，可切换/关闭

## 2. 界面框架

### 2.1 整体布局

```
┌─────────────────────────────────────────────┐
│  🐳 Whale Pod                    ─  □  ✕   │  ← frameless 自绘标题栏
├────────────┬────────────────────────────────┤
│  Sidebar   │        Right Panel             │
│  (可拖拽)  │                                │
│            │   对话视图 / 任务观察           │
│  搜索框    │                                │
│  Agent列表 │   浮动按钮：历史 | 新会话      │
│            │                                │
│  ────────  │   ChatTabBar（多对话 tab）     │
│  召唤专家  │                                │
└────────────┴────────────────────────────────┘
```

### 2.2 Sidebar 结构（从上到下）

| 区块 | 说明 | 滚动 |
|------|------|------|
| 搜索框 | 搜索 Agent 名称和对话 goal | — |
| Agent 列表 | 自动从对话提取有会话的 Agent，显示最后一条消息摘要 | 在滚动区内 |
| 召唤专家 | 固定底部，不在滚动区内 | — |

### 2.3 Right Panel 路由

| 状态 | 面板内容 |
|------|---------|
| 选中 Agent + 有对话 | DirectChatView（对话视图） |
| 选中 Agent + 无对话 | 新建对话模式（输入 goal 自动创建） |
| 对话中有任务 | DirectChatView + TaskControlPanel（DAG/列表） |

### 2.4 对话视图组件

| 组件 | 说明 |
|------|------|
| `DirectChatView` | 对话主视图，agent 图标根据类型显示（whale.png / 首字母头像） |
| `ChatTabBar` | 紧凑 28px tab 栏，活跃标签绿色底边线，hover 显示 ✕ 关闭 |
| `ChatHistoryPanel` | 从左侧滑出的半透明侧边栏，0.25s 动画，分页加载（每页 20 条） |
| `RightPanel` | 浮动按钮组（历史+新会话），透明无边框，hover 高亮 |
| `TaskControlPanel` | DAG/列表 tab 切换，展示子任务执行状态 |
| `ConfirmDialog` | 自定义确认弹窗（删除对话等操作） |
| `DagView` | SVG 任务 DAG 可视化，按 batch 分层，节点颜色按状态区分 |

## 3. 对话-任务关联

- MasterTask 有 SessionID，sidebar 只显示对话（session），任务通过 SessionID 关联
- 删除对话 = 删 session + 关联的 master task 及子任务（自定义 ConfirmDialog 确认）
- 关闭 tab 不删除对话，删除对话另外处理
- 第一次点击 agent，只列出最近的那次会话，需要其他对话点击历史按钮弹出对话列表选择

## 4. 能力范围自动触发

### 决策规则

- 不在能力范围 → 通用助理直接回答
- 单一维度 → agent 模式（单角色直调）
- 多维度 → team 模式（多角色协作）

### 实现方式

1. `team.yaml` 的 `capabilities` 声明能力范围
2. `team.yaml` 的 `routing` 声明意图→角色映射（可选，Leader 参考但不强制）
3. Leader prompt 注入能力范围 + 决策规则
4. 对话中 AI 回复包含 `<!-- ACTION:{...} -->` 注释，前端解析后自动触发 startTaskInSession / startExpertTaskInSession

## 5. 专家和专家团文件体系

### 专家定义

- 格式：`.md` + YAML frontmatter
- 目录：`agents/` 下支持子目录分类
- `use_agent` 只用 name 按名称匹配
- 正文包含结构化分区：核心能力、输出规范等（用于自动提取注入 Leader prompt 和 Checker/Verifier prompt）

### 专家团定义

- 目录：`teams/` 下每个团队一个子目录
- `team.yaml`：label + roles（agent name 数组）+ capabilities + routing
- 不再使用 `pipeline.yaml`（Leader 根据目标自由编排）

### 成员能力清单

从 agent 定义文件自动提取，注入 Leader prompt：

| 提取源 | 提取内容 | 注入位置 |
|--------|---------|---------|
| agent MD 的 `role` 字段 | 角色中文名 | Leader prompt 成员能力清单 |
| agent MD 的"核心能力"分区 | 擅长领域 | Leader prompt 成员能力清单 |
| agent MD 的"输出规范"分区 | 验证标准 | Checker/Verifier prompt |

## 6. Team Engine 架构

详见 `docs/team-engine.zh.md`。

核心要点：

- **递归 Plan → Work → Verify Loop**：总任务 verifier=用户，子任务 verifier=专业 agent
- **Checker vs Verifier**：Checker 做基础完整性检查（系统自动），Verifier 做专业质量验证（agent）
- **成员 Push Back**：Worker 可以质疑任务描述（`[PUSH_BACK]` 标记），Verifier 可以否决 Leader 验收
- **协作铁律**：Leader 内部约束，禁止代写、跳步
- **循环记忆**：RunTask 重试时通过 loop.md 累积失败摘要，注入 Worker prompt 防止重复犯错
- **YAML 解析**：Agent 定义文件使用 yaml.v3 解析，支持逗号分隔字符串和 YAML 列表两种格式

## 7. 设计规范

### 7.1 图标

- 统一使用单色线条 SVG
- 下拉/折叠箭头：统一三角 SVG（8×5），闭合向右旋转、展开向下
- 文件夹：平面线条 SVG
- 应用图标：`whale.png`（512×512）→ `appicon.png` → 构建时生成 ICO
- Agent 图标：whale 显示 whale.png，专家显示绿色渐变首字母，团队显示紫蓝渐变首字母

### 7.2 交互

- 按钮默认透明无边框，hover 时浅灰背景+淡边框，选中时绿色背景+绿色边框
- 浮动按钮（历史、新会话）：透明无边框，hover 时才显示边框和高亮背景
- 折叠章节：点击标题行展开/收起
- Combobox：点击展开下拉菜单，外部点击关闭

### 7.3 色彩

| 用途 | 色值 |
|------|------|
| 背景 | `#141414` |
| 面板背景 | `#1e1e1e` |
| 边框 | `#333` |
| 主文字 | `#e0e0e0` |
| 次要文字 | `#888` |
| 强调色（绿） | `#4CAF50` |
| 强调色（蓝） | `#2196F3` |

## 8. 构建

```batch
build_whale_pod.bat:
  Step 0: whale.png → appicon.png（gen_icon.go）
  Step 1: npm build 前端
  Step 2: wails build  Go 二进制
```

产物：`bin/whale-pod.exe`

## 9. 待细化

- [ ] 分解任务确认环节的 UI 交互
- [ ] 成员 Push Back 的 UI 展示
- [ ] 专业 Verifier 结果的 UI 展示
- [ ] 任务右键菜单
- [ ] 工作空间持久化存储
