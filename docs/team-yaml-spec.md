# Team, Expert & Agent 定义规格

## 概述

本文档定义 Whale Team Engine 的三种配置文件格式：

1. **Agent 定义**（`.md` + YAML frontmatter）— 定义单个智能体的能力、工具和运行时行为
2. **Expert 定义**（`experts/*.yaml`）— 定义专家的身份、领域、显示名，并引用底层 agent
3. **Team 定义**（`team.yaml`）— 定义专家团的成员、能力范围、意图路由和 Leader 配置

核心关系：**Team → Expert → Agent**。Team 引用 Expert（领域/名称），Expert 引用 Agent（能力/工具/prompt）。Expert 层解耦了"身份"和"能力"，使外部 agent 文件无需修改即可使用。

```
team.yaml ──roles──→ experts/*.yaml ──agent──→ agents/*.md
  "软件工程/后端工程师"    name: 后端工程师         backend-engineer.md
                          agent: backend-engineer
```

---

## 1. Agent 定义文件

### 1.1 文件位置

| 位置 | 适合谁 | 是否建议提交 | 优先级 |
|------|--------|-------------|--------|
| `{workspace}/.whale/agents/<name>.md` | 当前项目或团队共享 | 是 | 高 |
| `~/.whale/agents/<name>.md` | 个人所有项目通用 | 否 | 低 |
| `{workspace}/agents/<category>/<name>.md` | Team Engine 专用 agent 目录 | 是 | — |

同名时，项目级优先于全局。agent 目录支持子目录分类（如 `agents/engineering/`、`agents/quality/`），按 name 匹配时不关心子目录结构。

文件名即默认 name。也可在 frontmatter 里显式写 `name` 覆盖。

### 1.2 文件格式

Agent 定义文件由两部分组成：

1. **YAML frontmatter**（`---` 之间）：结构化配置
2. **Markdown 正文**（`---` 之后）：角色说明和工作方式

```markdown
---
name: backend-engineer
role: 后端工程师
description: 负责后端服务 API 开发和业务逻辑实现
whenToUse: 需要后端功能开发、API实现、业务逻辑编码时使用
model: deepseek-v4-pro
effort: high
permissionMode: auto
maxTurns: 50
memory: project
tools:
  - workspace.read
  - workspace.write
  - shell.run
---

你是资深后端开发工程师。按照 TDD 流程工作：
1. 先根据 API 设计编写单元测试
2. 再实现功能使测试通过
3. 重构代码保持简洁

## 核心能力

- 功能开发：按照设计文档高质量实现功能
- Bug修复：快速定位和修复问题
- 代码重构：提升代码可读性、可维护性和性能

## 输出规范

- 代码可编译、测试通过
- 遵循项目 lint 规则
- 函数不超过50行，类不超过300行
```

### 1.3 Frontmatter 字段

#### 必填字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `description` | string | 一句话说明这个角色做什么。`description` 和 `whenToUse` 至少填一个 |

#### 推荐字段

| 字段 | 类型 | 说明 | 默认值 |
|------|------|------|--------|
| `name` | string | 显式指定 agent name；不写时用文件名（不含 `.md`） | 文件名 |
| `role` | string | 角色中文名，显示在 UI 和 Leader prompt 能力清单中 | — |
| `whenToUse` | string | 什么时候应该使用它，帮助主 agent 判断调度时机 | 同 `description` |
| `tools` | yamlStringList | 允许使用的工具能力 | `[]`（model-only） |
| `permissionMode` | string | 权限模式 | `read_only` |

#### 运行时字段

| 字段 | 类型 | 说明 | 默认值 |
|------|------|------|--------|
| `model` | string | 指定 LLM 模型 | 全局默认 |
| `effort` | string | 推理强度：`low`/`medium`/`high`/`max` | — |
| `maxTurns` | int | 子会话最多轮数 | — |
| `maxToolIters` | int | 工具调用最大迭代次数 | 全局默认 |
| `maxToolCalls` | int | 工具调用最大次数 | 全局默认 |
| `memory` | string | 可用记忆范围：`user`/`project`/`local` | — |
| `background` | bool | 是否后台运行 | `false` |
| `isolation` | string | 工作隔离：`none`/`worktree` | `none` |
| `initialPrompt` | string | 子会话开始前先注入的提示 | — |

#### 高级字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `disallowedTools` | yamlStringList | 从允许工具里排除某些能力或工具 |
| `skills` | yamlStringList | 给这个 agent 加载的技能名 |
| `mcpServers` | yamlStringList | 给这个 agent 暴露的 MCP server |
| `hooks` | any | 钩子配置（PreToolUse / SubagentStart 等） |
| `generation` | object | 生成控制：`assistantPrefix` + `prefixCompletion` |

#### 兼容性字段（WorkBuddy 格式映射）

| 字段 | 类型 | 映射规则 |
|------|------|---------|
| `displayName` | map[string]string | `displayName.zh` → `role`（当 `role` 为空时） |
| `profession` | map[string]string | `profession.zh` → `role`（当 `role` 为空时，优先级低于 `displayName.zh`） |

### 1.4 `tools` 字段格式

`tools` 支持两种格式，解析器自动兼容：

**YAML 列表格式**（推荐）：

```yaml
tools:
  - workspace.read
  - workspace.write
  - shell.run
```

**逗号分隔字符串格式**（兼容 Claude Code / WorkBuddy）：

```yaml
tools: workspace.read, workspace.write, shell.run
```

### 1.5 可用工具能力

| 工具能力 | 能做什么 | 风险级别 |
|---------|---------|---------|
| `workspace.read` | 读取项目文件、搜索代码 | 只读 |
| `workspace.write` | 修改文件 | 写入 |
| `shell.read` | 运行偏只读的 shell 命令 | 只读 |
| `shell.run` | 执行命令 | 写入 |
| `web.search` | 搜索网页 | 只读 |
| `web.fetch` | 抓取网页内容 | 只读 |
| `mcp.read` | 使用已配置 MCP 工具 | 只读 |

### 1.6 权限模式

| `permissionMode` | 含义 | 适合场景 |
|------------------|------|---------|
| `read_only` | 只读，最安全 | 审查、评估、调研 |
| `ask` | 需要敏感操作时询问 | 偶尔修改或执行命令 |
| `auto` | 自动接受部分操作 | 信任的编辑任务 |
| `trusted` | 更高信任级别 | 非常明确、受控的角色 |

### 1.7 Markdown 正文结构

正文是 agent 的角色说明和工作方式，作为 `prompt` 注入到 LLM。Team Engine 会从中自动提取两个关键分区：

| 分区 | 标题格式 | 提取用途 |
|------|---------|---------|
| **核心能力** | `## 核心能力` | 注入 Leader prompt 的成员能力清单表格 |
| **输出规范** | `## 输出规范` | 注入 Checker/Verifier prompt 作为验证标准 |

推荐正文结构：

```markdown
[角色概述 — 1-2 句话说明身份和核心职责]

## 核心能力

- 能力1：简述
- 能力2：简述
- ...

## 工作原则

1. 原则1
2. 原则2
- ...

## 输出规范

- 规范1
- 规范2
- ...
```

- **核心能力**和**输出规范**是 Team Engine 自动提取的依据，标题必须精确匹配
- 其他分区（工作原则、编码规范等）不参与自动提取，但会完整包含在 agent prompt 中
- 正文使用中文或英文均可，`role` 字段建议用中文以便 UI 显示

### 1.8 业界格式兼容性

Whale 的 agent MD 格式与以下业界格式兼容：

| 来源 | 格式 | 兼容方式 |
|------|------|---------|
| **Claude Code** | `.md` + YAML frontmatter（`description`/`whenToUse`/`tools`/`permissionMode`） | 完全兼容，Whale 是 Claude Code 格式的超集 |
| **WorkBuddy** | `.md` + YAML frontmatter（`displayName`/`profession` 嵌套字段、`tools` 逗号分隔） | 兼容，`displayName.zh`/`profession.zh` 自动映射到 `role`，逗号分隔 tools 自动解析为列表 |
| **OpenAI Assistants** | JSON API（`instructions`/`tools`/`model`） | 不兼容，需要转换工具 |

**Claude Code 格式**的 agent 文件可以直接放入 `.whale/agents/` 使用，无需修改。Whale 额外支持的字段（`role`、`memory`、`isolation`、`hooks`、`generation`）是可选扩展。

**WorkBuddy 格式**的 agent 文件也可以直接使用。解析器会自动处理：
- `tools: workspace.read, shell.run` → `["workspace.read", "shell.run"]`
- `displayName.zh: 后端工程师` → `role: 后端工程师`
- `profession.zh: 后端工程师` → `role: 后端工程师`（`displayName` 优先）

### 1.9 校验规则

| 规则 | 说明 |
|------|------|
| `name` 只能包含字母、数字和连字符 | kebab-case，最长 64 字符 |
| `description` 和 `whenToUse` 至少填一个 | 都为空时解析失败 |
| `name` 省略时取文件名 | 去掉 `.md` 后缀 |
| frontmatter 必须闭合 | 缺少结束 `---` 时报错 |
| `tools` 为空列表时 agent 为 model-only | 无工具可用，只能从 prompt 和模型知识回答 |

### 1.10 完整示例

#### 最小定义（Claude Code 兼容格式）

```markdown
---
description: Review local code changes for bugs, regressions, and missing tests.
whenToUse: Use when the user asks for a code review or before merging local changes.
tools: workspace.read
permissionMode: read_only
---

You are a focused code reviewer.
Prioritize concrete bugs, behavior regressions, security risks, and missing tests.
```

#### Team Engine 标准定义

```markdown
---
name: backend-engineer
role: 后端工程师
description: 负责后端服务 API 开发和业务逻辑实现
whenToUse: 需要后端功能开发、API实现、业务逻辑编码时使用
model: deepseek-v4-pro
effort: high
permissionMode: auto
maxTurns: 50
memory: project
tools:
  - workspace.read
  - workspace.write
  - shell.run
---

你是资深后端开发工程师。按照 TDD 流程工作：
1. 先根据 API 设计编写单元测试
2. 再实现功能使测试通过
3. 重构代码保持简洁

## 核心能力

- 功能开发：按照设计文档高质量实现功能
- Bug修复：快速定位和修复问题
- 代码重构：提升代码可读性、可维护性和性能
- 性能优化：识别性能瓶颈并实施优化方案

## 工作原则

1. 先读后写：修改代码前必须完整阅读相关上下文
2. 最小变更：只做必要的修改，不做无关重构
3. 测试驱动：核心逻辑先写测试，再写实现

## 输出规范

- 代码可编译、测试通过
- 遵循项目 lint 规则
- 函数不超过50行，类不超过300行
```

#### WorkBuddy 兼容格式

```markdown
---
name: 模型 QA 专家
description: 独立模型 QA 专家，端到端审计机器学习和统计模型
emoji: ✅
color: "#B22222"
---

# 模型 QA 专家

你是**模型 QA 专家**，一位独立的 QA 专家...

## 核心能力

- 文档与治理审查
- 数据重建与质量
- 模型复现与构建

## 输出规范

- 每项分析必须从原始数据到最终输出完全可复现
- 每个发现必须包含：观察、证据、影响评估和建议
```

---

## 2. Expert 定义文件

### 2.1 概述

Expert 层是 Team 和 Agent 之间的桥梁，解耦了"身份"和"能力"：

- **Team 引用 Expert**（`软件工程/后端工程师`），不直接引用 Agent
- **Expert 引用 Agent**（`backend-engineer`），提供中文显示名和领域归属
- **Agent 文件不需要修改** — Claude Code、WorkBuddy 的 agent MD 原封不动

### 2.2 文件位置

```
experts/
├── 软件工程.yaml       ← 一个文件 = 一个领域 = 一组专家
├── 架构设计.yaml
├── 质量保障.yaml
├── 产品设计.yaml
├── 交互设计.yaml
├── 基础设施.yaml
└── 数据分析.yaml
```

文件名即领域标识，文件内部的 `domain` 字段值也用中文。

### 2.3 文件格式

```yaml
# experts/软件工程.yaml
domain: 软件工程
domain_en: software-engineering
icon: 💻
experts:
  - name: 后端工程师
    name_en: backend-engineer
    agent: backend-engineer
    description: 负责后端服务 API 开发和业务逻辑实现
    tags: [Go, Python, API, 微服务]

  - name: 代码审查员
    name_en: code-reviewer
    agent: code-reviewer
    description: 审查代码质量和架构合规性
    tags: [审查, 质量]
```

### 2.4 字段详解

#### 文件级字段

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `domain` | string | 是 | 领域中文名，也是 team.yaml 引用时的前缀 |
| `domain_en` | string | 否 | 领域英文名，用于英文环境 |
| `icon` | string | 否 | 领域图标（emoji），UI 展示用 |
| `experts` | []ExpertEntry | 是 | 该领域下的专家列表 |

#### ExpertEntry 字段

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 是 | 专家中文名，team.yaml 引用时的后半段 |
| `name_en` | string | 否 | 专家英文名，fallback 到 `agent` 字段值 |
| `agent` | string | 是 | 引用的 agent name（对应 agents 目录下的 MD 文件名） |
| `description` | string | 否 | 专家描述 |
| `tags` | []string | 否 | 标签，用于搜索和分类 |

### 2.5 引用方式

Team 通过 `领域/名称` 引用专家：

```yaml
# team.yaml
roles:
  - 软件工程/后端工程师
  - 软件工程/代码审查员
  - 数据科学/模型QA专家
```

解析优先级：

1. 精确匹配：`软件工程/后端工程师` → 查找 domain="软件工程" 且 name="后端工程师" 的专家
2. 名称匹配：`后端工程师`（无 `/`）→ 在所有领域中查找 name="后端工程师" 的专家
3. Agent 匹配：`backend-engineer` → 查找 agent="backend-engineer" 的专家

### 2.6 显示名优先级

```
expert.name（中文）→ agent MD 的 role → agent MD 的 name → expert.name_en → agent name
```

| 来源 | expert.name | agent MD role | 最终显示 |
|------|------------|---------------|---------|
| Whale 自有 | 后端工程师 | 后端工程师 | 后端工程师 |
| WorkBuddy | 模型QA专家 | — | 模型QA专家 |
| Claude Code | 安全审查员 | — | 安全审查员 |
| Claude Code（无 expert） | — | — | security-reviewer |

### 2.7 同一 Agent 被多个专家复用

```yaml
# experts/软件工程.yaml
experts:
  - name: Go后端工程师
    agent: backend-engineer
    tags: [Go]

  - name: Python后端工程师
    agent: backend-engineer
    tags: [Python]
```

同一个 `backend-engineer` agent 被两个专家引用，各有不同的中文名和标签。Team 根据需要选择合适的专家。

### 2.8 完整示例

```yaml
# experts/软件工程.yaml
domain: 软件工程
domain_en: software-engineering
icon: 💻
experts:
  - name: 后端工程师
    name_en: backend-engineer
    agent: backend-engineer
    description: 负责后端服务 API 开发和业务逻辑实现
    tags: [Go, Python, API, 微服务]

  - name: 前端工程师
    name_en: frontend-engineer
    agent: frontend-engineer
    description: 负责前端界面开发和用户体验实现
    tags: [TypeScript, React, Vue, CSS]

  - name: 代码实现者
    name_en: code-implementer
    agent: code-implementer
    description: 按照设计方案实现代码，遵循 TDD 流程
    tags: [编码, 实现, TDD]

  - name: 代码审查员
    name_en: code-reviewer
    agent: code-reviewer
    description: 审查代码质量和架构合规性
    tags: [审查, 质量, 最佳实践]
```

---

## 3. Team 定义文件

### 2.1 文件位置

| 方式 | 路径 | 适用场景 |
|------|------|---------|
| 扁平文件 | `.whale/teams/{name}.yaml` | 简单团队，无额外配置 |
| 目录结构 | `.whale/teams/{name}/team.yaml` | 需要附加 config.yaml、memory/、templates/ |

同名时，项目级 `.whale/teams` 优先于全局 `~/.whale/teams`。目录结构优先于扁平文件。

### 2.2 完整字段

```yaml
# ── 必填 ──────────────────────────────────────

label: 软件开发团队          # string — 团队显示名称

leader:
  role: product-manager      # string — 必填，Leader 对应的 agent name
  description: ...           # string — 可选，Leader 角色描述
  model: deepseek-v4-flash   # string — 可选，Leader 使用的模型
  prompt: |                  # string — 可选，注入到 Leader 的额外指令
    你是一位经验丰富的技术项目经理...
  rules:                     # []string — 可选，团队级规则，注入到所有子任务
    - 所有代码必须通过 go vet
    - 测试覆盖率不低于 80%

roles:                       # []string — 必填，团队成员 agent name 列表
  - product-manager
  - software-architect
  - backend-engineer
  - tdd-tester
  - code-reviewer

# ── 可选 ──────────────────────────────────────

category: 开发               # string — 团队分类标签

capabilities:                # []string — 能力范围声明（范围+技术栈+工作流阶段）
  - "软件开发全流程：需求→设计→编码→测试→交付"
  - "技术栈：Go, TypeScript, React, Wails"
  - "工作流阶段：需求分析, 架构设计, API设计, 编码实施, 测试验证, 代码审查"

routing:                     # []RoutingEntry — 意图路由表
  - intent: "新功能|添加功能|新增|开发"
    roles: [product-manager, requirements-analyst]
    mode: team
  - intent: "bug|缺陷|异常|报错|修复"
    roles: [bug-analyst]
    mode: agent
```

### 2.3 字段详解

#### `label` — 团队名称（必填）

- 类型：`string`
- 显示在 UI 和日志中
- 扁平文件方式时如果省略，取文件名（去掉 `.yaml` 后缀）

#### `leader` — Leader 配置（必填）

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `role` | string | **是** | Leader 对应的 agent name，必须能在 agents 目录中找到 |
| `description` | string | 否 | Leader 角色描述，注入到 Leader prompt 的 "Your Role" 区域 |
| `model` | string | 否 | Leader 使用的 LLM 模型，覆盖默认值 |
| `prompt` | string | 否 | 注入到 Leader 的额外指令，追加在 base prompt 之后 |
| `rules` | []string | 否 | 团队级规则，Leader 分解任务时会写入每个子任务的描述 |

#### `roles` — 成员列表（必填）

- 类型：`[]string`
- 每个元素是一个 agent name，对应 `agents/` 目录下的 `.md` 文件名（不含扩展名）
- Leader 的 `role` 通常也出现在 `roles` 中（Leader 本身也是团队成员）
- Leader prompt 中的角色列表会替换 base prompt 中的通用角色列表
- 成员的能力清单（核心能力、输出规范）从 agent MD 文件自动提取，无需在此重复声明

#### `capabilities` — 能力范围（可选）

- 类型：`[]string`
- 描述团队能做什么，由三个维度组成：
  - **范围**：团队覆盖的业务领域（如"软件开发全流程"）
  - **技术栈**：团队擅长的技术（如"Go, TypeScript, React"）
  - **工作流阶段**：团队可参与的开发阶段（如"需求分析, 架构设计, 编码实施"）
- Leader 根据能力范围自动判断是否启动团队任务：
  - 不在能力范围 → 通用助理直接回答
  - 单一维度 → agent 模式（单角色直调）
  - 多维度 → team 模式（多角色协作）

#### `routing` — 意图路由表（可选）

- 类型：`[]RoutingEntry`
- Leader 参考但不强制，最终决策仍由 Leader 自主判断

每个 RoutingEntry：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `intent` | string | 是 | 意图关键词，用 `|` 分隔（如 `"bug|缺陷|修复"`） |
| `roles` | []string | 是 | 匹配意图时激活的角色列表 |
| `mode` | string | 是 | `agent`（单角色直调）或 `team`（多角色协作） |

#### `category` — 分类标签（可选）

- 类型：`string`
- 用于 UI 分组显示

### 2.4 目录结构方式

使用目录结构时，可以附加运行时配置：

```
.whale/teams/全栈开发团队/
├── team.yaml           # 团队定义（必填）
├── config.yaml         # 运行时配置（可选）
├── memory/             # 团队记忆目录（可选）
└── templates/          # 团队模板目录（可选）
```

#### config.yaml（可选）

```yaml
max_agents: 10
default_timeout: 1800
model:
  leader: deepseek-v4-flash
  worker_default: deepseek-v4-pro
  verifier_default: deepseek-v4-flash
workdir: .
deploy:
  host: ""           # 空 = 本地执行
  port: 22
  user: ""
  keyfile: ""
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `max_agents` | int | 最大并发 agent 数 |
| `default_timeout` | int | 默认超时（秒） |
| `model.leader` | string | Leader 模型 |
| `model.worker_default` | string | Worker 默认模型 |
| `model.verifier_default` | string | Verifier 默认模型 |
| `workdir` | string | 工作目录 |
| `deploy.host` | string | 远程执行地址（空 = 本地） |
| `deploy.port` | int | SSH 端口 |
| `deploy.user` | string | SSH 用户 |
| `deploy.keyfile` | string | SSH 密钥路径 |

### 2.5 Leader Prompt 注入

Team Engine 加载 `team.yaml` 后，会自动构建增强版 Leader prompt，注入以下内容：

| 注入内容 | 来源 | 条件 |
|---------|------|------|
| Leader 自定义指令 | `leader.prompt` | 非空时 |
| 团队角色列表 | `roles` | 非空时，替换 base prompt 的通用角色列表 |
| 成员能力清单 | agent MD 自动提取 | `roles` 非空时 |
| 协作铁律 | 硬编码 | 始终注入 |
| 意图路由表 | `routing` | 非空时 |
| 团队规则 | `leader.rules` | 非空时 |
| 能力范围 | `capabilities` | 非空时 |

#### 协作铁律（始终注入）

```
0. 简单任务只用最相关角色，不必全员出动。1个角色能完成就只用1个。
1. 你是编排者，不是执行者——禁止自己写代码、写文档、做专业分析
2. 分配给某角色的任务必须由该角色输出后采信，你只做编排与汇编
3. 未完成前序任务不可跳到后续任务
4. 验证不通过的任务必须回退重做，不可跳过
5. 禁止自己代写任何团队成员的专业产出
```

#### 成员能力清单（自动提取）

从 agent MD 文件自动提取，注入为 Markdown 表格：

```
| Agent ID | 角色 | 核心能力 | 输出规范 |
|----------|------|---------|---------|
| backend-engineer | 后端工程师 | Go/Python API 开发、数据库设计... | 代码可编译、测试通过... |
| tdd-tester | 测试工程师 | 单元测试、集成测试、TDD... | 覆盖率 ≥ 80%... |
```

提取源：

| 提取源 | 提取内容 | 注入列 |
|--------|---------|--------|
| agent MD 的 `role` 字段 | 角色中文名 | 角色 |
| agent MD 的"核心能力"分区 | 擅长领域 | 核心能力 |
| agent MD 的"输出规范"分区 | 验证标准 | 输出规范 |

### 2.6 校验规则

| 规则 | 说明 |
|------|------|
| `label` 必填 | 省略时取文件名，但建议显式声明 |
| `leader.role` 必填 | 缺少时 LoadTeamConfig 返回错误 |
| `roles` 必填 | 至少包含 1 个 agent name |
| `leader.role` 应在 `roles` 中 | Leader 本身也是团队成员 |
| `roles` 中的 name 必须能在 agents 目录中找到 | 找不到时 ResolveRoles 跳过，该角色无能力清单 |
| `routing.mode` 只能是 `agent` 或 `team` | 其他值会被忽略 |
| `routing.intent` 用 `\|` 分隔关键词 | 匹配时按关键词 OR 逻辑 |

### 2.7 完整示例

#### 示例 1：软件开发团队（目录结构）

`.whale/teams/全栈开发团队/team.yaml`：

```yaml
label: 软件开发团队
category: 开发

leader:
  role: product-manager
  description: 经验丰富的技术项目经理，负责需求拆解和任务编排
  model: deepseek-v4-flash
  prompt: |
    你是一位经验丰富的技术项目经理。
    优先考虑方案的可行性和可测试性。
    对于不确定的需求，先向用户确认再分解。
  rules:
    - 所有代码必须通过 go vet
    - 测试覆盖率不低于 80%

roles:
  - product-manager
  - requirements-analyst
  - ux-architect
  - software-architect
  - api-designer
  - backend-engineer
  - frontend-engineer
  - tdd-tester
  - code-reviewer
  - architecture-guardian

capabilities:
  - "软件开发全流程：需求→设计→编码→测试→交付"
  - "技术栈：Go, TypeScript, React, Wails"
  - "工作流阶段：需求分析, 架构设计, API设计, 编码实施, 测试验证, 代码审查"

routing:
  - intent: "新功能|添加功能|新增|开发"
    roles: [product-manager, requirements-analyst]
    mode: team
  - intent: "bug|缺陷|异常|报错|修复"
    roles: [bug-analyst]
    mode: agent
  - intent: "API|接口设计|接口变更"
    roles: [api-designer]
    mode: agent
  - intent: "代码审查|review|代码质量"
    roles: [code-reviewer, architecture-guardian]
    mode: team
  - intent: "架构|系统设计|技术方案"
    roles: [software-architect, ux-architect]
    mode: team
```

#### 示例 2：内容创作团队（扁平文件）

`.whale/teams/content-team.yaml`：

```yaml
label: 内容创作团队
category: 内容

leader:
  role: content-strategist

roles:
  - content-strategist
  - technical-writer
  - copy-editor
  - seo-specialist

capabilities:
  - "技术内容创作：文档→博客→教程"
  - "语言：中文, English"
  - "工作流阶段：选题, 大纲, 撰写, 编辑, 发布"

routing:
  - intent: "文档|教程|指南"
    roles: [technical-writer]
    mode: agent
  - intent: "博客|文章|内容策略"
    roles: [content-strategist, technical-writer]
    mode: team
```

#### 示例 3：最小团队定义

```yaml
label: 代码审查
leader:
  role: code-reviewer
roles:
  - code-reviewer
  - architecture-guardian
```

---

## 4. Team 对 Agent 的要求

Team Engine 在运行时对引用的 agent 有以下要求：

### 3.1 必须满足

| 要求 | 说明 | 不满足时 |
|------|------|---------|
| agent 文件必须存在 | `roles` 中的 name 必须能在 agents 目录中找到 | ResolveRoles 跳过，该角色无能力清单，Leader prompt 中显示为 `—` |
| `description` 必填 | agent MD 的 frontmatter 必须有 `description` 或 `whenToUse` | 解析失败，该 agent 不可用 |
| `name` 合法 | 只能包含字母、数字和连字符，最长 64 字符 | 解析失败 |

### 3.2 强烈建议

| 要求 | 原因 | 缺失影响 |
|------|------|---------|
| `role` 字段非空 | Leader prompt 的成员能力清单需要中文角色名 | 表格中角色列显示 agent name（英文 ID） |
| 正文包含 `## 核心能力` 分区 | Leader prompt 需要提取能力清单 | 能力清单表格中核心能力列显示 `—` |
| 正文包含 `## 输出规范` 分区 | Checker/Verifier prompt 需要提取验证标准 | 能力清单表格中输出规范列显示 `—`，专业 Verifier 无法用角色承诺验证 |
| `tools` 与 `permissionMode` 匹配 | 有 `workspace.write`/`shell.run` 时应设为 `ask` 或更高 | 安全风险：写权限 agent 以 `read_only` 运行 |

### 3.3 Leader 角色特殊要求

Leader 是团队的编排者，对其 agent 定义有额外约束：

| 要求 | 说明 |
|------|------|
| `permissionMode` 建议 `read_only` 或 `ask` | Leader 是编排者不是执行者，不应有写权限 |
| 正文应强调编排职责 | Leader prompt 会被追加协作铁律，正文应与之呼应 |
| `model` 可用较轻量模型 | Leader 做规划和审查，不需要最强模型 |

### 3.4 Verifier 角色要求

当 Leader 在子任务中指定 `verifier_role` 时，该 agent 作为专业 Verifier：

| 要求 | 说明 |
|------|------|
| 正文必须包含 `## 输出规范` | Verifier 用 Worker 角色的输出规范来验证，而不是自己的 |
| `permissionMode` 建议 `read_only` | Verifier 只验证不修改 |
| `tools` 包含 `workspace.read` | 需要读取 Worker 产出来验证 |

---

## 5. 文件体系总览

```
experts/                             agents/                              teams/
├── 软件工程.yaml                    ├── engineering/                     ├── 全栈开发团队/
├── 架构设计.yaml                    │   ├── backend-engineer.md          │   ├── team.yaml
├── 质量保障.yaml                    │   ├── frontend-engineer.md         │   └── config.yaml
├── 产品设计.yaml                    │   └── code-reviewer.md             └── content-team.yaml
├── 交互设计.yaml                    ├── architecture/
├── 基础设施.yaml                    │   ├── software-architect.md
└── 数据分析.yaml                    │   └── api-designer.md
                                     ├── quality/
                                     │   └── tdd-tester.md
                                     ├── product/
                                     │   └── product-manager.md
                                     └── agency-agents-zh/        ← WorkBuddy agent，不用改
                                         └── specialized/
                                             └── specialized-model-qa.md
```

引用关系：

```
team.yaml ──roles──→ experts/*.yaml ──agent──→ agents/*.md
   │                      │                       │
   │                      ├── name → Leader prompt 成员显示名
   │                      │                       ├── frontmatter.role → 显示名 fallback
   │                      │                       ├── 正文"核心能力" → Leader prompt 成员能力清单
   │                      │                       └── 正文"输出规范" → Checker/Verifier prompt
   │                      │
   ├── leader.role → Leader expert → Leader agent MD
   ├── capabilities → Leader prompt 能力范围
   ├── routing → Leader prompt 意图路由表
   └── leader.rules → 所有子任务描述
```

---

## 6. 已废弃字段

| 字段 | 原用途 | 废弃原因 |
|------|--------|---------|
| `pipeline` | 预设任务流水线 | Leader 根据目标自由编排，不受预设流程约束 |
| `pipeline_file` | 引用外部 pipeline.yaml | 同上 |

Team Engine 的核心哲学是**委托系统，不是工作流引擎**。Leader 的智能是核心驱动力，它决定做什么、谁来做、怎么验收。
