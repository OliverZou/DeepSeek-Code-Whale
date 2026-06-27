# Hermes Agent 的 Skill 发现与获取机制

> 这是「方案 A：注册表发现」的完整实现描述。一个 agent 在没有任何 skill 的情况下，如何通过这套机制逐步获得工具和技能。

---

## 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                      启动时（一次性）                         │
│                                                             │
│  ~/AppData/Local/hermes/skills/  ──┐                        │
│  .hermes/skills/                 ──┤  扫描所有 SKILL.md       │
│  plugins/*/skills/               ──┘  提取 name + description │
│                         ↓                                   │
│           注入到 system prompt 的 <available_skills> 块       │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                      运行时（每次对话）                        │
│                                                             │
│  用户请求 ──→ agent 扫描 <available_skills> ──→ 匹配?        │
│                                                   │         │
│                                   ┌────────────────┤         │
│                                   ↓ yes            ↓ no      │
│                            skill_view(name)    用通用能力处理   │
│                            加载完整 SKILL.md    │             │
│                            按指令执行           │             │
│                                   │             │             │
│                                   └──────┬──────┘             │
│                                          ↓                   │
│                              任务完成后                       │
│                              skill_manage('create')          │
│                              固化经验 → 下次自动匹配           │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                      远端扩展（按需）                          │
│                                                             │
│  skillhub search <关键词>  ──→  发现远程 skill               │
│  skillhub install <slug>   ──→  下载到本地 skills/ 目录       │
│                         ↓                                   │
│              下次启动时自动进入 <available_skills>             │
└─────────────────────────────────────────────────────────────┘
```

---

## 第一层：本地预注入（启动时）

### 机制

Hermes 启动时扫描以下目录：

| 目录 | 说明 |
|------|------|
| `~/AppData/Local/hermes/skills/` | 用户级 skill（跨项目共享） |
| `<project>/.hermes/skills/` | 项目级 skill（仅当前项目可见） |
| 各 plugin 的 `skills/` 目录 | 插件附带的 skill |

对每个目录，递归查找所有 `SKILL.md` 文件，提取其 YAML frontmatter 中的 `name` 和 `description`，生成一个 `<available_skills>` 块注入到 system prompt。

### 示例

假设本地有这些 skill：

```
~/AppData/Local/hermes/skills/
├── productivity/
│   └── document-generation/
│       └── SKILL.md          # name: document-generation
│                              # description: Generate professional DOCX/MD/PDF
├── creative/
│   └── claude-design/
│       └── SKILL.md          # name: claude-design
│                              # description: Design HTML artifacts
```

则 system prompt 中注入：

```xml
<available_skills>
  productivity: Skills for document creation, presentations, spreadsheets.
    - document-generation: Generate and maintain professional documents (DOCX, MD, PDF).
  creative: Creative content generation — ASCII art, diagrams, visual design.
    - claude-design: Design one-off HTML artifacts (landing, deck, prototype).
</available_skills>
```

**关键设计**：只注入 name + description（每个约 100 字节），**不注入完整 skill 内容**。这保证了即使有几百个 skill，system prompt 也不会爆炸。

---

## 第二层：按需加载（运行时）

### System Prompt 指令

agent 的 system prompt 中明确要求：

> Before replying, scan the skills below. If a skill matches or is even partially relevant to your task, you MUST load it with `skill_view(name)` and follow its instructions. Err on the side of loading — it is always better to have context you don't need than to miss critical steps.

### 执行流程

```
1. agent 收到用户请求：「帮我生成一份 Word 报告」
2. agent 扫描 <available_skills>
3. 发现 document-generation: "Generate and maintain professional documents"
4. 调用 skill_view(name='document-generation')
5. 获得完整 SKILL.md 内容（字体要求、python-docx 用法、qn('w:eastAsia') 技巧）
6. 按 skill 指令执行
```

### 关键工具

| 工具 | 作用 | 场景 |
|------|------|------|
| `skills_list(category=...)` | 按分类列出所有 skill | agent 不确定用什么 skill 时浏览 |
| `skill_view(name)` | 加载一个 skill 的完整 SKILL.md | 匹配到 skill 后获取详细指令 |
| `skill_view(name, file_path=...)` | 加载 skill 目录下的附属文件 | 读取 skill 附带的模板/脚本/参考文档 |

### 匹配策略

system prompt 中要求 agent「宁可多加载，不可漏掉一个相关的」。实践中的启发式规则：

1. **关键词匹配**：任务描述中的词汇与 skill description 的交集
2. **领域匹配**：任务的领域（前端/后端/数据处理/安全）与 skill 的分类对应
3. **模糊关联**：即使不是精确匹配，只要可能有用就加载（"it is always better to have context you don't need than to miss critical steps"）

---

## 第三层：远端扩展（按需）

### Remote Registry 架构

Hermes 对接两个远程 skill 注册中心：

| 注册中心 | 命令 | 特点 |
|----------|------|------|
| **SkillHub** | `skillhub search/install` | 国内优化，速度快，合规 |
| **ClawHub** | `clawhub search/install` | 国际社区，5400+ skill |

### 发现与安装流程

```
1. agent 扫描 <available_skills>，没有匹配的
2. agent 调用 skillhub search "服务器安全审计"
3. skillhub 返回匹配的 skill 列表（name, description, source, version）
4. agent 向用户展示候选 skill
5. 用户确认 → agent 调用 skillhub install <slug>
6. skill 下载到本地 skills/ 目录
7. 下次启动时自动进入 <available_skills>
```

### 优先级策略

`skillhub-preference` skill 规定了策略：

1. 优先用 `skillhub`（国内用户速度和合规性更好）
2. `skillhub` 不可用或无匹配时，回退到 `clawhub`
3. 安装前报告来源、版本和值得注意的风险信号

---

## 第四层：自举闭环（学习）

这是让 agent 真正「越用越强」的机制——不是依赖别人写的 skill，而是**自己从经验中生成 skill**。

### 闭环流程

```
用户请求 → agent 执行（用通用能力 + 已有 skill）
                ↓
         任务成功完成（经过多次迭代、克服了问题）
                ↓
         agent 调用 skill_manage(action='create')
         将成功的流程固化为 SKILL.md
                ↓
         新 skill 进入本地 skills/ 目录
                ↓
         下次同类任务自动匹配 → 直接加载执行
```

### 实例

1. 用户第一次让我审计服务器 → 我没有任何审计 skill → 用通用能力逐项检查 → 成功交付报告
2. 这个过程如果被固化为 `server-security-audit` skill：
   - `name: server-security-audit`
   - `description: SSH远程安全审计：收集系统信息/端口/服务/权限，生成结构化报告`
   - `content: 完整的检查清单、命令集、报告模板`
3. 下次用户说「审计那台服务器」→ `<available_skills>` 中自动出现 → 直接加载 → 按上次验证过的流程执行

### 维护闭环

skill 不是静态的——如果 agent 使用一个 skill 时发现其中的命令过时、步骤有误、缺少关键陷阱提示，会主动用 `skill_manage(action='patch')` 修复它。

> System prompt 要求：**"When using a skill and finding it outdated, incomplete, or wrong, patch it immediately — don't wait to be asked."**

---

## 完整方案总结

### 一个裸 agent 获得能力的路径

```
初始状态：只有 agent.md，没有任何 skill

第1次对话：
  用户：「帮我设计一个登录页面」
  agent 扫描 <available_skills> → 空
  agent 用通用能力 + web 搜索 + 代码生成 → 完成
  agent 调用 skill_manage(action='create') → 固化为 claude-design skill

第2次对话：
  用户：「帮我设计一个注册页面」
  启动时 claude-design 已进入 <available_skills>
  agent 扫描 → 匹配 → skill_view('claude-design') → 按上次流程执行

第N次对话：
  用户：「有没有视频生成的 skill」
  agent 扫描 <available_skills> → 没有
  agent 调用 skillhub search → 找到 manim-video
  agent 调用 skillhub install → 下载到本地
  下次启动自动可用
```

### 与 agent.md 的关系

agent.md 不需要包含具体 skill 的内容。它只需要包含**元规则**：

```markdown
## Skill 发现规则
1. 每次收到请求，先扫描 <available_skills> 看有无匹配
2. 有匹配 → skill_view 加载后按指令执行
3. 无匹配 → 用通用能力执行；完成后考虑 skill_manage 固化
4. 本地没有且需要 → skillhub/clawhub 远端搜索安装
```

这四个规则就是 agent 自举的全部秘密。**agent.md 是第零个 skill——它教 agent 如何获得其他所有 skill。**

---

## 对比其他方案的适用场景

| 维度 | 方案 A（本方案） | 方案 B（MCP注入） | 方案 C（懒加载） | 方案 D（纯 agent.md） |
|------|-----------------|------------------|-----------------|---------------------|
| 离线可用 | ✅ 本地 cache | ❌ 依赖服务端 | 首次需要网络 | ✅ 完全离线 |
| 首次启动能力 | 只有基础工具 | 连上即全能力 | 需下载 | 全靠模型知识 |
| 学习能力 | ✅ 自动固化 | ❌ 被动接收 | 缓存命中后快 | agent.md 越来越长 |
| 生态依赖 | SkillHub/ClawHub | MCP 服务器 | Remote registry | 无 |
| 适合场景 | 个人开发者 | 企业统一管控 | 大模型 + 有限空间 | 极简/嵌入式 |

---

*文档基于 Hermes Agent 的实际实现。对应源码逻辑见 Hermes 的 `skills_list`、`skill_view`、`skill_manage` 工具定义及 system prompt 中的 skill 加载指令。*
