这张图展示了一个名为 **MiniMax Agent Team** 的多智能体协作系统架构，其核心设计理念是：

> **并发批 + 对抗循环 + Engine 驱动**

即：任务被拆分为多个批次（Batch），每个批次内任务并行执行；Worker 与 Verifier 形成“对抗”关系（一个生产、一个验证）；整个流程由确定性代码引擎（Engine）驱动，直到所有任务通过验证或达到最大重试次数。

---

## 一、整体架构概览

系统从上到下分为四个主要层级：

1. **User（用户层）**
2. **Leader（主决策Agent）**
3. **Team Engine（调度与控制引擎）**
4. **Task Batch Execution Layer（任务执行层，含 Worker & Verifier）**

底部还有详细的注释说明各组件职责和交互规则。

---

## 二、各组件详解

### 1. User（用户）

- **角色**：提出目标、必要时参与决策（如高风险/模糊/成本扩张时介入）
- **不参与日常任务执行**，只在关键节点干预。
- **交互方式**：
  - 向 Leader 提交目标（① 提目标）
  - 接收 Leader 的合并交付结果（⑦ 合并·交付）
  - 在 escalate 场景下介入（如风险过高、预算超支等）

> 📌 用户不是直接操作者，而是“战略制定者 + 最终验收者”。

---

### 2. Leader（主 Agent）

- **职责**：
  - 拆解 Plan → 生成 plan.yaml
  - 决策每轮 CycleReport 是否接受
  - 不直接管理 Worker，只看 Engine 汇报
  - 控制全局节奏（max_concurrency, depends_on, max_cycles）

- **形状**：橙色矩形（与 Engine 同色，但形状不同 —— Leader 是 Boss，Engine 是 Channel）

- **输入输出**：
  - 输入：用户目标（①）、CycleReport（⑥）、决策回填（来自用户）
  - 输出：plan.yaml（②）、escalate 请求（⑦）、接受/拒绝指令（隐含在 CycleReport 处理后）

> 📌 Leader 是“大脑”，负责宏观规划与最终裁决，不碰具体执行细节。

---

### 3. Team Engine（团队引擎）

- **本质**：确定性代码（deterministic code），非 AI Agent
- **职责**：
  - spawn task session（启动任务会话）
  - retry on FAIL（失败自动重试）
  - 跟踪 board.md / deliverable.md（记录进度与产出）
  - 包装 CycleReport（汇总当前周期报告）
  - 控制 max_concurrency / depends_on / max_cycles（并发数、依赖关系、最大循环数）

- **形状**：浅橙色长条矩形（强调它是“通道”而非“思考者”）

- **关键动作**：
  - ③ spawn：根据 plan.yaml 启动第一批任务
  - ④ depends_on：等待 Batch 1 全部 PASS 后才 spawn Batch 2
  - ⑤ all batches PASS → 报 plan_complete → 走通道⑥回 Leader

> 📌 Engine 是“神经系统”，负责精确调度、状态追踪、错误恢复，确保流程可控可复现。

---

### 4. Task Batch Execution Layer（任务执行层）

这是实际工作的地方，包含多组 **Worker + Verifier** 对。

#### 结构特点：

- 每个 Task 对应一个 Worker（生产者）和一个 Verifier（验证者）
- Worker 和 Verifier 颜色相同（紫糖 C.agent），表示它们是配对工作的“代理对”
- 每个 pair 独立 session，互不污染上下文
- 使用 “produce” 箭头连接 Worker → Verifier，表示数据流向
- Verifier 成功后才允许进入下一阶段（如 Batch 2 依赖 Batch 1 输出）

#### 示例 Batch 1：

| Task | Worker 类型 | Verifier 类型 |
|------|-------------|----------------|
| A1: 文献调研 | Researcher（阅读资料、引用搜索） | 引用核查（事实来源、引用边界） |
| A2: 网页爬取 | Crawler（调用爬虫工具） | 数据核查（完整性、时效性） |
| A3: 数据库查询 | DB Query（调 SQL/API） | 结果核查（行数、字段对齐） |

#### 示例 Batch 2：

| Task | Worker 类型 | Verifier 类型 |
|------|-------------|----------------|
| B1: 
