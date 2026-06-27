# WorkBuddy 团队体系分析及对 Whale 的启发

## 一、WorkBuddy 架构概览

### 1.1 三层结构：Scene → Plugin → Agent

| 层级 | 作用 | Whale 对应 |
|------|------|-----------|
| Scene | 场景路由，用户意图入口 | 无（Whale 用 agent 直接匹配） |
| Plugin | 专家包容器，声明 agent/skill 资源 | team 目录 |
| Agent | 具体角色定义（.md 文件） | agents 目录下的 .md 文件 |

Scene 是 WorkBuddy 独有的概念——它把用户意图映射到一组 Plugin，每个 Plugin 再包含若干 Agent。Whale 不需要 Scene 层，因为 Whale 的 Leader 自身做意图路由。

### 1.2 专家与专家团统计

#### Marketplace（`cb_teams_marketplace`）

| 类型 | 数量 |
|------|------|
| 插件（Plugin） | 30 个 |
| 其中有 agent MD 文件的 | 5 个 |
| Agent MD 文件总数 | 44 个 |
| Team 型插件（`expertType: "team"`） | 0 个 |

注意：WorkBuddy 的"团队"概念不在 `plugin.json` 的 `expertType` 里，而是通过 `scenes.json` 编排多个 agent 实现。例如 `a-share-analysis` 有 7 个 agent（含主理人 `a-share-advisor`），`ai-hedge-fund` 有 21 个，`trading-agent` 有 12 个——它们在功能上就是团队，但 `plugin.json` 里都标记为 `agent`。

#### 典型团队型 Plugin

| Plugin | Agent 数量 | 主理人 | 成员 |
|--------|-----------|--------|------|
| a-share-analysis | 7 | a-share-advisor | morning-briefing, stock-research, sector-screening, portfolio-diagnosis, thematic-hunter, smart-money-tracker |
| ai-hedge-fund | 21 | portfolio-manager | warren-buffett, charlie-munger, peter-lynch, phil-fisher, bill-ackman, cathie-wood, michael-burry, ... |
| trading-agent | 12 | research-manager | fundamentals-analyst, market-analyst, sentiment-analyst, news-analyst, risk-manager, trader, ... |

#### Whale 当前 agents 目录

| 类型 | 数量 |
|------|------|
| Agent MD 文件总数 | 289 个 |
| 可被当前解析器解析 | 254 个 |
| 无法解析（README/非 agent 文件） | 35 个 |
| 我们自己定义的专家 | 38 个（7 个子目录） |
| agency-agents-zh 引入的 | 251 个 |
| 专家团（teams） | 5 个 |

---

## 二、WorkBuddy Agent MD 规范分析

### 2.1 Frontmatter 字段对比

| 字段 | WorkBuddy | Whale | 兼容性 |
|------|-----------|-------|--------|
| `name` | 必填，与文件名一致 | 必填 | ✅ 兼容 |
| `description` | 必填，英文，AI 判断何时激活 | 必填 | ⚠️ WorkBuddy 用 `>-` 折叠语法 |
| `displayName` | `{en, zh}` 嵌套 | 无 | ❌ 不兼容 |
| `profession` | `{en, zh}` 嵌套 | `role`（单字符串） | ⚠️ 语义等价，格式不同 |
| `maxTurns` | 必填，默认 50 | 有，默认 0 | ✅ 兼容 |
| `tools` | 禁止声明（系统分配） | 可声明 | ⚠️ 策略不同 |
| `color` | 有 | 无 | ❌ 不兼容（可忽略） |
| `skills` | 可选 | 有 | ✅ 兼容 |
| `role` | 无 | 有 | Whale 独有 |
| `whenToUse` | 无 | 有 | Whale 独有 |

### 2.2 正文结构对比

**WorkBuddy 普通成员：**

```markdown
# {角色名称} - {人名}

{角色描述}

## 核心能力
1. **{能力1}**：{描述}
2. **{能力2}**：{描述}

## 工作流程
1. {步骤1}
2. {步骤2}

## 输出规范
- {规范1}
- {规范2}

## 注意事项
- {约束或边界条件}
```

**WorkBuddy 主理人：**

```markdown
# {团队名称} - 主理人

## 团队成员
| 成员 | 名字 | 职责 |
|------|------|------|

## 标准工作流程（SOP）
### Phase 1: {阶段名}
### Phase 2: {阶段名}

## 团队协作机制（铁律）
1. 建立团队
2. 调度成员
3. 消息中转
4. 成员结论为准

## 协作规则
1-5 条
```

**Whale 当前：** 正文包含"核心能力"、"输出规范"等结构化分区（如 `architecture-guardian.md`），但缺少"典型问法"。

### 2.3 解析兼容性问题

当前 `parseMarkdownAgentDefinition` 使用手写的逐行 YAML 解析器，存在以下兼容性问题：

| 问题 | 影响 | 受影响数量 |
|------|------|-----------|
| YAML `>-` 折叠语法 | `description` 丢失 | 40/44 WorkBuddy agent |
| 嵌套 YAML（`displayName`/`profession`） | 字段丢失 | 1/44 |
| 额外字段（`color` 等） | 忽略 | 全部 |

**建议**：改用 `gopkg.in/yaml.v3` 解析 frontmatter（已在 `team.go` 中使用），映射 `displayName.zh` → `role`，忽略不兼容字段。

---

## 三、WorkBuddy Team 型规范核心要素

### 3.1 意图路由表

WorkBuddy 主理人 prompt 中包含明确的意图→角色映射：

```markdown
## 工作流程

### 第一步：识别用户意图

| 意图类型 | 典型问法 | 路由目标 |
|---------|---------|---------|
| 个股能不能买 | "宁德时代能不能买？" | stock-research agent |
| 个股估值判断 | "贵不贵？" | valuation-framework skill |
| 每日策略 | "今天怎么看？" | morning-briefing agent |
| 持仓诊断 | "帮我看看持仓" | portfolio-diagnosis agent |
```

**对 Whale 的启发**：当前 Whale 的 Leader 只知道"团队能力范围"和"有哪些角色"，但不知道"什么问题该找谁"。意图路由表把这种知识从隐式推断变为显式查表。Whale 在 `team.yaml` 中增加 `routing` 字段实现。

### 3.2 SOP Phase 编排

WorkBuddy 主理人 prompt 中包含 Phase 编排。

**对 Whale 的启发**：**不采用。** Pipeline/SOP 是固定工作流的思维，和 Team Engine 的委托本质矛盾。Leader 应该根据目标自由编排，不受预设流程约束。去掉 `pipeline.yaml`。

### 3.3 协作铁律（4 正则 + 5 红线）

WorkBuddy 的协作铁律是最值得借鉴的设计：

**4 条正则：**

1. **建立团队**：任务开始时由主理人亲自创建团队，明确协作边界。团队创建必须且只能由主理人执行，严禁委派任何成员创建团队
2. **调度成员**：按 SOP 阶段将成员拉入协作、下发独立任务；成员作为独立协作方输出专业产出，不得由主理人代写
3. **消息中转**：成员产出回传给主理人，由主理人汇总、转交下一阶段；所有跨成员信息流必须经主理人中转，不得互相直连
4. **成员结论为准**：任何专业产出必须由对应成员输出后再采信，主理人只做编排与汇编

**5 条红线：**

- ❌ 禁止跳过 TeamCreate，直接自己模拟成员发言或并行写出多角色内容
- ❌ 禁止自己代写任何团队成员的专业产出
- ❌ 禁止未完成前序阶段就跳到后续阶段
- ❌ 禁止让成员互相直连通信，所有跨成员信息流必须经主理人中转
- ❌ 禁止 spawn 主理人自己（编排、汇总、决策由主理人亲自完成）

**对 Whale 的启发**：协作铁律作为 **Leader 内部约束**注入 Leader prompt，不写在 team.yaml 里。这是所有 Leader 共有的行为约束。

### 3.4 成员能力清单

WorkBuddy 规范要求主理人 prompt 中列出每个成员的 Agent ID + 擅长领域 + 典型问法。

**对 Whale 的启发**：Whale 的 agent MD 文件已有"核心能力"和"输出规范"分区，从中自动提取即可。**不需要新增 `expertise`/`typicalQuestions` 字段**。"典型问法"由 Leader 从角色的 `role` + `核心能力` 推断，不需要 agent 自我声明。

### 3.5 成员命名规范

WorkBuddy 要求 Team 型专家团的每个成员有"谐音花名"。

**对 Whale 的启发**：趣味性设计，不采用。Whale 用 agent name 直接标识。

### 3.6 单 Agent 直调路由表

WorkBuddy 规范要求主理人包含一个路由表：单一维度问题→对应成员，综合性问题→走 Workflow。

**对 Whale 的启发**：Whale 已实现 `capabilities` 决策规则（不在能力范围→通用助理；单一维度→agent 模式；多维度→team 模式），在 `team.yaml` 的 `routing` 字段中显式声明。

---

## 四、对 Whale Team Engine 的改进建议

### 4.1 核心认知

Team Engine 是**委托系统**，不是工作流引擎。Leader 的智能是核心驱动力——它决定做什么、谁来做、怎么验收。所以所有改进都应该围绕一个目标：**让 Leader 拥有正确的知识和约束**。

### 4.2 递归的 Plan → Work → Verify Loop

**核心设计哲学**：每个任务环节都满足 Plan → Work → Verify 的闭环。Plan 是主动的规划者——理解目标、规划方案、分解任务、分配工作。这个 loop 是递归的——总任务和子任务遵循同一个模式，只是规划者、执行者和验证者的身份随任务性质变化。

#### 总任务层（Leader 执行）

| 环节 | Planner/Worker | Verifier | 说明 |
|------|----------------|----------|------|
| 识别目标 | Leader 问用户 | **用户确认** | 来回几次对话确认目标 |
| 分解任务 | Leader + LLM | **接收 agent 评估** | agent 评估任务是否可执行 |
| 汇总交付 | Leader 收集子任务结果 | **Checker + 用户** | Checker 基础检查，用户最终确认 |

Leader 完成总任务**不需要 agent 作为 verifier**——顶层 verifier 是用户。

#### 子任务层（Agent 执行）

| 环节 | Planner/Worker | Verifier | 说明 |
|------|----------------|----------|------|
| 规划+执行 | 专业 agent（Plan + Work） | **专业 agent** | 由 Leader 在分解时指定 `VerifierRole` |

子任务的 Plan 和 Work 通常由同一个 agent 完成——它先理解任务、规划方案，然后执行。Verifier 是**针对该任务产出的专业评判者**。

#### Verifier vs Checker 的区分

| | Checker（系统机制） | Verifier（专业 agent） |
|---|---|---|
| 触发方式 | Engine 自动 | Leader 分配 |
| 做什么 | 基础完整性检查：产出是否存在、是否为空、是否截断、引用文件是否存在 | 专业质量验证：用角色知识判断产出是否合格 |
| 知识 | 通用规则 | 角色的核心能力 + 输出规范 |
| 产出 | PASS/FAIL 裁决 | 结构化反馈（FINDINGS JSON） |
| 位置 | Worker 产出后自动运行 | 作为一个子任务，由 Leader 编排 |

当前 `verifier.go` 中的 `BuildVerifierPrompt` / `BuildContentVerifierPrompt` 实质上是 **Checker**——用通用 checklist 检查，不注入 Worker 角色的专业知识。

#### Verifier 是针对任务的，不是针对角色的

同一个 tester，不同任务的 verifier 不同：

| 任务 | Worker | Verifier |
|------|--------|----------|
| 为 API 模块写单元测试 | tester | api-designer（覆盖关键接口？） |
| 执行集成测试 | tester | checker（跑一遍就知道） |
| 设计测试策略 | tester | developer（方向对不对？） |
| TDD 先写测试 spec | tester | — （测试本身就是 spec，developer 的代码过不过测试就是验证） |

**Leader 在分解任务时决定每个任务的 verifier**——它知道这个任务的产出该由谁来评判。

### 4.3 成员 Push Back 机制

当前团队成员是被动执行者——Leader 给什么任务就做什么，没有渠道质疑。

**改进**：让团队成员有能力 push back：

1. **Worker 接到任务后可以质疑**——"这个任务描述有歧义"、"上游产出还没到位，我无法开始"
2. **Worker 产出时可以附条件**——"我完成了 A 部分，但 B 部分需要架构师先确认"
3. **专业 Verifier 可以否决 Leader 的验收**——Leader 说"通过了"，代码审查员说"还有安全问题"

实现方式：利用 Whiteboard 已有的 inbox/outbox 消息机制。Worker 执行中发现问题，写 inbox 消息给 Leader，Leader 必须响应后才能继续。

### 4.4 改进方案

#### P0：Leader prompt 加协作铁律（内部约束）

防止 Leader 代写所有角色的产出。作为所有 Leader 共有的行为约束注入，不写在 team.yaml 里。

```
## 协作铁律

1. 你是编排者，不是执行者——禁止自己写代码、写文档、做专业分析
2. 每个专业产出必须由对应角色输出后再采信，你只做编排与汇编
3. 未完成前序任务不可跳到后续任务
4. 验证不通过的任务必须回退重做，不可跳过
5. 禁止自己代写任何团队成员的专业产出
```

#### P0：Leader prompt 加成员能力清单

从 agent 定义文件自动提取"核心能力"+"输出规范"，注入到 Leader prompt。

不需要新增 `expertise`/`typicalQuestions` 字段——agent MD 已有"核心能力"和"输出规范"分区，解析提取即可。"典型问法"由 Leader 从 `role` + `核心能力` 推断。

注入格式：

```
## 团队成员能力清单

| Agent ID | 角色 | 核心能力 | 输出规范 |
|----------|------|---------|---------|
| architecture-guardian | 架构守护者 | 架构合规检查、设计期审查、编码后复检、技术债识别 | 审查报告含合规状态+偏离项+风险等级+修复建议 |
| api-designer | API设计师 | ... | ... |
```

#### P0：重命名 Verifier → Checker，专业验证由 agent 承担

1. 当前 `verifier.go` 的 `BuildVerifierPrompt` / `BuildContentVerifierPrompt` 重命名为 `BuildCheckerPrompt` / `BuildContentCheckerPrompt`
2. 新增专业验证路径：当 `task.VerifierRole` 指定了专业 agent 时，不运行通用 Checker，而是由该 agent 执行专业验证
3. 专业 Verifier 的 prompt 注入 Worker 角色的"核心能力"+"输出规范"——**用角色自己的承诺来检查它是否兑现**

#### P1：team.yaml 加 `routing` 字段

意图路由从推断变为查表。从 agent 定义自动提取能力，不需要在 team.yaml 中重复声明。

```yaml
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
```

#### P1：分解任务后增加 agent 确认环节

Leader 分解完任务后不是直接创建执行，而是先发"任务意向"给对应 agent：

1. Leader 输出分解方案（PlanTasks）
2. Engine 对每个 PlanTask，向对应 agent 发送确认请求
3. Agent 评估：任务描述是否清晰？上游依赖是否满足？能力是否匹配？
4. Agent 确认可执行 → 正式创建任务
5. Agent 质疑 → 反馈给 Leader 重新分解

这是 goal-worker-verifier loop 在"分解任务"环节的体现：Worker=Leader+LLM，Verifier=接收 agent。

#### P1：去掉 pipeline.yaml

Pipeline 是固定工作流的思维，和委托本质矛盾。Leader 根据目标自由编排，不受预设流程约束。

#### P2：YAML frontmatter 改用 yaml.v3 解析

替换 `agent_library.go` 中的手写解析器，改用 `gopkg.in/yaml.v3`：

- 支持 `>-` 折叠语法和 `|` 字面量块
- 支持嵌套字段（`displayName.zh` → `role`）
- 兼容 WorkBuddy 的 44 个 marketplace agent 和 251 个 agency-agents-zh

### 4.5 改动优先级总结

| 优先级 | 改动 | 效果 |
|--------|------|------|
| **P0** | Leader prompt 加协作铁律（内部约束） | 防止 Leader 代写 |
| **P0** | Leader prompt 加成员能力清单（从 agent MD 自动提取） | Leader 知道谁擅长什么 |
| **P0** | 重命名 Verifier → Checker，专业验证由 agent 承担 | 验证用专业标准，不是通用模板 |
| **P1** | team.yaml 加 `routing` 字段 | 意图路由查表 |
| **P1** | 分解任务后增加 agent 确认环节 | 任务可执行性验证 |
| **P1** | 去掉 pipeline.yaml | 回归委托本质 |
| **P2** | YAML frontmatter 改用 yaml.v3 解析 | 兼容 WorkBuddy agent 生态 |

---

## 五、WorkBuddy 参考文件索引

| 文件 | 路径 | 内容 |
|------|------|------|
| Agent MD 规范 | `C:\Users\oliver-PC\AppData\Local\Programs\WorkBuddy\resources\app.asar.unpacked\resources\builtin-skills\expert-manager\references\agent-md-spec.md` | Agent MD frontmatter 字段、正文结构、主理人模板 |
| Plugin JSON 规范 | `...\references\plugin-json-spec.md` | plugin.json 字段、Agent 型/Team 型模板 |
| Team 型规范 | `...\references\team-spec.md` | 成员命名规范、协作铁律、SOP 编排、settings.json |
| A股分析团队 | `C:\Users\oliver-PC\.workbuddy\plugins\marketplaces\cb_teams_marketplace\plugins\a-share-analysis\agents\` | 7 个 agent MD 文件（含意图路由表） |
| AI对冲基金团队 | `...\plugins\ai-hedge-fund\agents\` | 21 个 agent MD 文件（含 Phase 编排） |
| 交易 Agent | `...\plugins\trading-agent\agents\` | 12 个 agent MD 文件 |
| 场景路由 | `...\cb_teams_marketplace\scenes.json` | 26 个场景，映射到 plugin |
| 运行时团队实例 | `C:\Users\oliver-PC\.workbuddy\teams\trading-moutai\config.json` | 实际运行时创建的团队配置 |
