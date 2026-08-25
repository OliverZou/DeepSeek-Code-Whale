# Team Engine 实现计划

> 文档版本：v1.1
> 编写日期：2026-08-21
> 依据：Agent Team 架构图（team-overview / engine / comm / lwv）
> v1.1：状态机描述与实现对齐（checking/checked 已从实现删除；produced/verified 为真实中间态，suspended/pending_confirmation 为辅助态）

---

## 一、目标概述

构建一个 **「Leader 编排 + Engine 确定性调度 + Worker/Verifier 对抗式执行」** 的多智能体（Multi-Agent）协作引擎。核心能力：

- 将用户目标拆解为 `Plan → Batch → Task` 的多层结构，同批并发、跨批串行。
- 每个 Task 由一对 Worker（产出）+ Verifier（校验）对抗式推进，直至校验通过。
- Engine 以确定性状态机驱动全流程，失败自动重试并复用 Session 上下文。
- 通过统一消息总线，实现 **Agent 与人类同权** 的跨时间双向通讯。

系统摒弃传统「一次函数调用（call → wait → return）」的 Task 工具模型，改用 **「状态机 + 消息总线」**（`dispatch ⇄ steer ⇄ verify ⇄ recover`）模型。

---

## 二、系统角色与职责

| 角色 | 形态 | 职责 |
|------|------|------|
| **User** | 人类 | 提出目标、接收交付；仅在高风险 / 模糊 / 成本扩张时介入决策 |
| **Leader** | 主 Agent（橙色大框） | 拆解 Plan、派发 Task、对每轮 CycleReport 决策（accept / reject / override_accept / manual_retry）；不直接管理 Worker |
| **Team Engine** | 确定性代码（扁长条通道） | 解析 plan.yaml、按 depends_on 切分 Batch、spawn task session、控制 max_concurrency / max_cycles、跟踪 board.md / deliverable.md、包装 CycleReport |
| **Worker** | Sub-agent（方框） | 每个 task 的产出方，独立 Session，Context 隔离 |
| **Verifier** | Sub-agent（六边形） | 每个 task 的校验方，与 Worker 成对（assigned_to + verified_by） |

> **配色约定**：Leader 与 Engine 同色（砖橙 C.agent），靠形状区分 —— Leader 为大 Box（主角），Engine 为扁长条 Channel（确定性调度通道）；Worker 与 Verifier 同色（焦糖 C.subagent），靠形状区分职责。

---

## 三、核心设计理念

1. **确定性调度通道**：Engine 是纯代码状态机，Leader / Worker / Verifier 不直接互相通信，全靠 Engine 中转，保证流程可控、可复现。
2. **对抗式执行**：Worker 想「完毕退出」，Verifier 想「挑出问题让 Worker 重做」；双方都以「运行结束」为目标，一方结束即触发另一方启动，多轮迭代直至 PASS。
3. **Session 生命周期状态机**（v1.1：与实现对齐）：
   `pending → assigned → producing → produced → verifying → verified → done`
   - 另设三个辅助态：`failed`（终端）、`suspended`（重试耗尽/取消，可恢复）、`pending_confirmation`（用户确认机制，confirmation.md 预留）。
   - 重试时回退到 `producing` 并**复用同一 Session**（`ContinueSubagent` 追加 Verifier 反馈轮），保留失败上下文。
   - `done` 后仍可接收消息、恢复工作或继续对话。
4. **Agent 与人类同权**：User / Other Agent / Team Engine 三种调用方平权，共享同一组动词与接口。
5. **每 Task 独立 Session**：Context 隔离，不污染其他 Task，节省宝贵的上下文窗口（Ralph-Loop / Harness 思路）。

---

## 四、模块拆分与实现步骤

### 模块 1：Session 生命周期状态机（基础层）
- 实现 Session 对象，状态流转 `pending → assigned → producing → produced → verifying → verified → done`（另有 `failed / suspended / pending_confirmation`），支持状态恢复。
- 支持同一 Session 重试复用、done 后继续接收消息。
- 每个 Task 独立 Session，Session 间 Context 隔离。

### 模块 2：消息总线 / 通信层（daemon）
- **daemon HTTP API**：绑定 `127.0.0.1:port`，按 profile / dataDir 隔离。
- **SSE 事件流**：实时回流 `status / progress / message / error`。
- **Auth**：token + scope 校验，调用前自动注入，调用方无感。（可暂时不做）
- 底层封装为 **CLI 包装**：`communication / session / agent / team / cron`。

### 模块 3：Skill + CLI 接口层（暴露给调用方的标准动词）
对外暴露 6 个标准动作（User / Other Agent / Engine 三者共用）：

| 动词 | 含义 |
|------|------|
| `prompt` | 发指令、追加要求 |
| `spawn` | 新开 Agent / Session |
| `abort` | 中止当前任务 |
| `kill` | 强制终止 |
| `summarize` | 让其总结当前状态 |
| `fork` | 从当前状态复制 Session |


> **分层约定**：调用方关心**动词**，skill 关心**契约**，daemon 关心**实现**，三层各司其职，调用方对底层切换无感。

### 模块 4：Team Engine 调度器（核心）
- **输入**：接收 Leader 的 `plan.yaml`。
- **Batch 划分**：按 `depends_on` 切分多 Batch，同批并发、跨批串行。
- **并发控制**：`max_concurrency` 控制同批最大并发；`max_cycles` 限制总轮次。
- **调度逻辑**：
  1. `spawn` 派发 task session。
  2. 等待 Worker 产出 → 触发 Verifier 校验。
  3. 校验 FAIL → 将错误注入 Worker 同 Session 自动重试（达到 `auto_reject_retries` 次后才升级到 Leader）。
  4. 一批全部 PASS → 才 spawn 下一批。
  5. 全部 PASS → 报 `plan_complete`，包装 `CycleReport` 交回 Leader。
- **产物跟踪**：维护 `board.md`（看板）与 `deliverable.md`（交付物）。

### 模块 5：角色 Agent 实现
- **Leader**：拆 Plan、切 Batch、决策每轮 CycleReport（`accept / reject / override_accept / manual_retry`）；高风险时 escalate 给 User。
- **Worker**：每 task 一个产出子代理，如 Researcher / Crawler / DB Query / Synthesizer / Writer 等。
- **Verifier**：与 Worker 成对的校验子代理，如 引用核查 / 数据核查 / 结果核查 / 一致性核查 / 格式核查。

### 模块 6：Memory 经验沉淀
- 失败上下文复用、多轮可修改。
- Leader / Worker 跨 Session 复用，用户可评价。
- 沉淀为 agent memory / MEMORY.md。

---

## 五、关键数据流（对应 team-overview 图 ①~⑦）

| 编号 | 流向 | 内容 |
|------|------|------|
| ① | User → Leader | 提目标 |
| ② | Leader → Engine | 传递 `plan.yaml` |
| ③ | Engine → Batch | `spawn` task session |
| ④ | Batch 1 → Batch 2 | Engine 等 Batch 1 全部 verify PASS 才 spawn Batch 2（跨批串行） |
| ⑤ | Engine → Leader | 全部 Batch PASS → 报 `plan_complete` |
| ⑥ | Engine → Leader | 回传 `CycleReport` |
| ⑦ | Leader → User | 合并交付（高风险则 escalate） |

> 实际 Plan 可含任意 Batch 与 Task；`max_concurrency` 控制同批最大并发，全程由 Engine 确定性驱动。本图示意 2 个 Batch（3 + 2 task）。

---

## 六、分阶段实现路线

| 阶段 | 内容 | 验收标准 |
|------|------|----------|
| **P0 地基** | Session 状态机 + daemon HTTP API + SSE + Auth | Session 能跑通 pending→done 全流程 |
| **P1 接口层** | 6 个动词的 skill + CLI 包装 | User / Agent / Engine 三方可平权调用 |
| **P2 Engine 调度** | plan.yaml 解析、Batch 划分、并发控制、自动重试、CycleReport | 能按 depends_on 串并行调度并自动重试 |
| **P3 对抗执行** | Worker / Verifier 成对对抗循环、Session 复用 | FAIL 能注入重试，PASS 才推进 |
| **P4 编排闭环** | Leader 决策逻辑 + escalate + Memory 沉淀 | 完整 ①~⑦ 闭环跑通 |
| **P5 打磨** | 文档跟踪（board/deliverable）、超时/降级、测试覆盖 | 稳定性与可观测性达标 |

---

## 七、测试与验证策略

- **状态机单测**：验证 pending→done 各态流转及恢复（含 failed / suspended / pending_confirmation）。
- **调度器测试**：depends_on 依赖图、max_concurrency 并发上限、max_cycles 终止。
- **重试测试**：验证 `auto_reject_retries` 次数阈值与升级 Leader 逻辑。
- **接口一致性测试**：三种调用方对同一动词得到相同语义。
- **端到端**：模拟一个含 2 Batch（3 + 2 task）的 Plan，验证全流程闭环。

---

## 八、设计取舍与边界

- **为何用状态机而非函数调用**：一次函数调用无法承载跨时间的多方实时双向通讯、失败上下文复用与中途补指令；状态机 + 消息总线弥补了这些短板。
- **为何 Agent 与人类同权**：统一动词与接口后，Any Agent 既能作为调用方发起动作，也能作为被作用对象被沟通与编排，降低系统复杂度。
- **适用边界**：本设计面向「多任务、多阶段、需校验」的复杂协作场景；对单步、无校验的简单任务，传统 Task 工具模型仍更轻量。
