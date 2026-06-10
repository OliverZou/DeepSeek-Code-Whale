这张图详细对比了“传统 Task 工具”与“Mavis Agent Team”的架构差异，核心在于从**一次性函数调用（one-shot）**演进到**跨时间、多角色、状态机驱动的消息协作系统**。以下是对图中每一个组件、状态、箭头、数据流的逐项详解，特别聚焦你提出的四个重点：

---

## 一、整体结构概览

图分为左右两大板块：

- **左侧：① 传统 Task 工具**
  - 简单线性流程：Caller Agent → Sub-Agent → one-shot text
  - 强调“短生命周期”、“不可重入”、“单向通信”

- **右侧：② Mavis Agent Team**
  - 复杂协同系统：IM Channel ↔ Leader ↔ Worker ↔ Verifier + Memory + Team Engine 状态机
  - 强调“跨时间消息交换”、“状态推进”、“失败恢复”、“经验沉淀”

底部总结公式：
> Task 工具 = 函数调用 (call → wait → return)  
> Agent Team = 状态机 + 消息总线 (dispatch ⇄ steer ⇄ verify ⇄ recover)

---

## 二、左侧：传统 Task 工具详解

### 1. 组件

#### Caller Agent（主 Agent · Tool Caller）
- 角色：发起者，调用子任务。
- 行为：发送 prompt，等待返回结果。

#### Sub-Agent（一次性派出 · 短生命周期）
- 角色：执行具体任务的子代理。
- 特点：被 spawn 后运行一次即销毁，无持久状态。

#### one-shot text（一段文本 / 摘要 · 不可重入）
- 输出产物：Sub-Agent 返回的结果，是静态文本或摘要。
- 特性：不可重复使用，无法中途干预或修改。

### 2. 数据流与连接

- **① 单次 prompt**：Caller Agent → Sub-Agent
  - 内容：task / dispatch / spawn（模型工具调用层）
  - 含义：启动一个子任务，传入指令或上下文。

- **② 一次性返回（文本/摘要）**：Sub-Agent → Caller Agent
  - 内容：执行结果，通常是自然语言描述或结构化摘要。
  - 含义：任务结束，返回最终输出。

- **指向 one-shot text 的箭头**：Sub-Agent → one-shot text
  - 表示 Sub-Agent 的输出就是这个文本对象，它本身不参与后续交互。

### 3. 能力边界（Capability boundary）

列出传统工具的局限性：

- 短生命周期 — 工具返回即结束，子 Agent session 销毁
- 单次输入输出 — 调用是函数，不是对话，子 Agent 不能反问主 Agent
- 不可重入 — 中途遇到阻塞或矛盾，无法实时上报，只能在最终 return 体现
- 即使后台 SubAgent 长跑，通讯仍是一次输入输出，无双向消息通道
- 适用：短任务 · 局部探索 · 检查思路 · 候选答案生成

→ 总结：这是典型的“请求-响应”模式，缺乏持续性和反馈机制。

---

## 三、右侧：Mavis Agent Team 详解

这是一个基于**状态机 + 消息总线 + 多角色协作**的智能体团队架构。

### 1. 核心组件

#### IM Channel（双向 · async）
- 角色：消息总线（MessageBus），负责用户与系统之间的异步通信。
- 输入：user / 用户
- 输出：消息 · 状态 · ACK（确认）
- 作用：作为外部接口，接收用户指令并推送系统状态更新。

#### Leader（Orchestrator · 控制面）
- 角色：协调器、调度中心。
- 功能：
  - 接收来自 IM Channel 的消息和状态。
  - 向 Worker 发送 dispatch / steer / 补充 prompt。
  - 接收 Worker 的上报（包括阻塞、finished 仍可继续）。
  - 管理整个 Session 的生命周期。

#### Worker（Session · 持续运行）
- 角色：实际执行任务的工作节点。
- 特点：
  - 每个 Session = Worker 一个运行周期。
  - finished 后仍可接收新消息，支持重试和恢复。
  - 与 Verifier 成对工作（produce ⇄ verify）。

#### Verifier（同 task 成对）
- 角色：校验器，验证 Worker 的输出是否符合预期。
- 功能：
  - 接收 Worker 的 produce 输出。
  - 进行 verify 校验。
  - 若失败，可触发重试（verifying → producing），复用原 session。

#### Memory（经验沉淀 ·
