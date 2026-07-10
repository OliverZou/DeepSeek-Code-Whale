# 状态机与消息总线分析：论文设计 vs Whale 实现

基于识图结果（engine_state_analysis.md），逐项对照。

---

## 一、左右对比：传统 Task 工具 vs Mavis Agent Team

图的核心逻辑是用**右侧**替代**左侧**：

| 维度 | 左侧：传统 Task 工具 | 右侧：Mavis Agent Team |
|------|---------------------|----------------------|
| 模式 | call → wait → return | state machine + message bus |
| 生命周期 | 短（一次返回即销毁） | 长（Session 持续存活） |
| 通信 | 单向一次 | 双向异步（IM Channel） |
| 可重入 | ❌ 不可重入 | ✅ dispatch ⇄ steer ⇄ verify ⇄ recover |
| 失败恢复 | ❌ 无 | ✅ 状态机 + 重试 |
| 经验沉淀 | ❌ 无 | ✅ Memory |
| 公式 | Task 工具 = 函数调用 | Agent Team = 状态机 + 消息总线 |

---

## 二、右侧架构逐项对比

| # | 组件 | 论文要求 | 我们实现 | 状态 |
|---|------|---------|---------|------|
| 1 | **IM Channel (MessageBus)** | 双向异步消息通道，用户 ↔ 系统，消息·状态·ACK | `inbox/outbox` 文件总线 + `channel.go` AgentChannel | ✅ |
| 2 | **Leader (Orchestrator)** | 接收 IM Channel 消息 + 状态；向 Worker 发送 dispatch/steer/补充 prompt | `leader.go` 只做初始 Decompose，**运行中不接收状态更新** | ⚠️ |
| 3 | **Worker (Session)** | 持续运行，finished 后仍可接收新消息，可重试可恢复 | `spawn_subagent` 一次性 session，terminate 后不可恢复 | 🔴 |
| 4 | **Verifier (同 task 成对)** | produce ⇄ verify 对抗循环，验证失败可触发重试，复用原 session | `RunTask` produce → verify retry loop ✅，但每次 retry 是新 session ❌ | ⚠️ |
| 5 | **Memory** | 经验沉淀 —— Agent 自己的经验会沉淀，后续执行会收到提示 | ❌ 无此机制 | 🔴 |
| 6 | **状态机** | dispatch ⇄ steer ⇄ verify ⇄ recover | pending→assigned→producing→produced→verifying→verified→done/failed | ✅ |
| 7 | **跨时间消息交换** | Agent 之间可以跨时间收发消息 | `inbox/outbox` 支持，但**慢通信**（文件系统） | ✅ |
| 8 | **失败恢复** | 可重入，可恢复，Worker 不因一次失败而销毁 | retry loop ✅，但 session 不持久 ❌ | ⚠️ |

---

## 三、关键差距分析

### 差距 1：Worker Session 不持久 🔴

**论文要求**：
```
Worker (Session · 持续运行)
每个 Session = Worker 一个运行周期
finished 后仍可接收新消息
```

**目前**：每次 `RunTask` 通过 `spawn_subagent` 创建一个全新的 subagent session。任务完成后 (`done`/`failed`)，这个 session 就销毁了。即使重新 retry，也是新 session，没有之前的上下文。

**需要**：
```
Worker session 应该是持久化的：
  1. RunTask → 创建 Worker session
  2. session 持续存在，即使任务完成
  3. 可以通过 AgentChannel.Prompt 向 session 发新消息
  4. session 可以 resume（恢复执行）
```

### 差距 2：Memory 经验沉淀 🔴

**论文要求**：
```
Agent内记忆：Agent 自己的经验会沉淀，后续执行会收到提示
白板 (共享留言板文件)：支持保存大量信息
```

**目前**：Whiteboard 只有 per-task 的文件，没有跨任务的"经验"积累机制。

**需要**：
```
Agent Memory:
  每个 Agent (Worker/Verifier) 可以：
  - 记录自己的经验教训
  - 下次 spawn 时自动加载相关经验
  - 经验跨 session 持久化
```

### 差距 3：Leader 运行中不接收状态 🔴

**论文要求**：
```
Leader 接收来自 IM Channel 的消息和状态
向 Worker 发送 dispatch / steer / 补充 prompt
接收 Worker 的上报（包括阻塞、finished 仍可继续）
```

**目前**：`leader.Decompose()` 只在起始时调用一次，生成 plan。Leader 不参与后续的 CycleReport 以外的执行中交互。

### 差距 4：同 session 复用 retry ⚠️

**论文要求**：
```
若失败，可触发重试（verifying → producing），复用原 session
```

**目前**：每次 retry 是新的 `spawn_subagent` 调用，新 session。Worker 不记得上次失败的原因（除了通过 `[VERIFIER FEEDBACK]` 追加到 description）。

---

## 四、总体评价

| 维度 | 评分 | 说明 |
|------|------|------|
| 核心状态机 (7个状态) | ✅ | pending→done 完整，转换表正确 |
| Worker⇄Verifier 对抗 | ✅ | produce→verify retry loop |
| 消息总线 (inbox/outbox) | ✅ | 文件系统 + channel.go AgentChannel |
| 统一操作接口 | ✅ | prompt/spawn/abort/kill/resolve |
| **Worker Session 持久化** | **🔴** | **一次执行后销毁，不可 resume** |
| **Agent Memory** | **🔴** | **无跨 session 经验积累** |
| **Leader 运行时交互** | **🔴** | **Leader 只在起始参与** |
| **Session 复用 retry** | **⚠️** | **每次 retry 是新 session** |

### 一句话

**核心的状态机骨架和消息通道已经对了，但缺少了"Session 持久化"和"经验沉淀"这两个让 Agent 从"一次性工具"升级为"长期协作队友"的关键特征。**
