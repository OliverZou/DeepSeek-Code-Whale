# Team Engine 端到端测试观察报告（2048 小游戏）

**日期**：2026-08-16
**分支**：team-engine
**测试目标**：用 team engine 端到端产出「2048 数字合并小游戏」（纯 HTML5/CSS3/原生 JS，浏览器直接打开），全程观察并修复发现的问题。

## 一、测试结果

- 进程退出码 `0`，所有交付文件已产出（DESIGN.md、game.js、index.html、style.css、game.test.js）。
- **但实际完成度远低于表面**：6 个任务中 2 个 `done`、4 个 `suspended`，而结果输出把**所有批次都显示为 `✅ [passed]`**，误导用户以为全部成功。

```
✅ Batch 核心与静态资源并行 [passed]
    ✅ 8d06fc05 [suspended] 实现 game.js 纯核心逻辑 Game2048Core
    ✅ 63d46e59 [done]     编写 index.html 页面结构
    ✅ b919aa1d [suspended] 编写 style.css 响应式样式
✅ Batch UI 集成与单元测试 [passed]
    ✅ c07879d5 [suspended] 实现 game.js UI 控制器与输入/持久化
    ✅ bf63d390 [suspended] 编写核心逻辑单元测试
```

## 二、发现的问题

### 已修复

**1. [critical] verifier 工作目录不一致（`#10d`）**

verifier 在 `task.Workdir`（原始 workspace，如 `D:\src\whale_test_2048`）验证，但 worker 实际在沙箱 `out/` 目录（`<whiteboard>/<task_id>/out/`）产出文件，且产出只有在 **PASS 之后** 才 `propagateTaskOutput` 回原始 workspace。结果：verifier 第 1 轮在原始 workspace 必然找不到交付物 → 误判 `MISSING` → 触发一整轮不必要的重试。

修复：给 `Verifier` 增加 `WithWorkdir` 覆盖；非 worktree 模式下，`RunTask` 把 verifier 目录指向 worker 的沙箱 `out/`（worktree 模式仍用 worktree 路径）。

**2. [major] 批次/任务 `suspended` 被显示为 ✅（`#10c` UI 层）**

结果打印逻辑只区分 `failed`（❌）与其它（一律 ✅），`suspended` 任务和含 `suspended` 的批次都显示绿勾。修复：`suspended` 任务显示 `⚠️`，含 `suspended` 任务的批次显示 `⚠️`；`statusIcon` 补上 `TaskStateSuspended` 分支。

### 已定位、待进一步调查

**3. [major] verifier 持久会话复用失效（`#10a`）**

部分任务第 2/3 轮 verifier 返回空 `[FAIL]`（第 2 轮 `Duration 0.0s` 立即返回，第 3 轮 `300.0s` 超时）。现象指向：持久会话跨重试复用时上下文膨胀，verifier 第 3 轮处理复杂任务超时，或 `ContinueSession` 后 app 层 turn 管理未产生有意义的验证输出。需深入调查 `RunTurnWithContentOptions` 与持久会话的上下文/turn 管理。

**4. [major] 测试项目产物 bug：game.js 的 `window` 无守卫（`#10e`）**

`game.js` 第 371 行 UI 层 `var Core = window.Game2048Core;` 缺少 `typeof window !== 'undefined'` 守卫，Node 环境 `require()` 执行全文件时抛 `ReferenceError: window is not defined`，导致 42 个单元测试全部无法运行。此 bug 被 `bf63d390` 任务的 verifier 第 3 轮**正确发现**（证明 verifier 有价值），但根因是 UI 集成任务的 worker 在追加 UI 层时引入了回归——这本身暴露了「worker 追加改动缺少回归验证」的薄弱环节。

### 工程/可观测性问题

**5. [minor] 日志为空（`#11`）**

`team_engine.log` 与 `engine.log` 均为 0 字节。Escalator 的 `"task … suspended … needs user intervention"` 日志、DW 批次日志等都无处可查，导致排障只能靠读白板文件。

**6. [minor] 工件泄漏（`#12`）**

`.whale_verify_check.html` 泄漏到 workdir 根目录（推测为某 verifier 的临时验证产物未清理）。

## 三、根因链（一次失败是如何放大成 4 个 suspended 的）

1. verifier 查错目录（`#10d`）→ 第 1 轮误判 `FAIL`。
2. worker 收到反馈后重试，作为 workaround 把文件复制到原始 workspace。
3. verifier 持久会话复用失效（`#10a`）→ 第 2/3 轮返回空 `[FAIL]`。
4. 空 feedback 触发 `run_task.go` 的默认文案 guard，worker 拿到的是无信息量的通用提示。
5. `RetryCount` 达到 `MaxRetries`(3) → 任务 `suspended`。
6. `plan_run.go` 批次判定把 `suspended` 视作 `passed`（`#10c`）→ 批次显示通过。

修复 `#10d` 能切断第 1 环（避免无谓重试），修复 `#10c` 能暴露真实完成度（不再全绿误导）。`#10a` 是剩余的核心顽疾，值得优先调查。

## 四、修复清单

| 文件 | 改动 |
|---|---|
| `internal/team_engine/verifier.go` | `Verifier` 增加 `workdir` 字段与 `WithWorkdir`；`BuildPrompt`/`Verify` 优先用覆盖的 workdir |
| `internal/team_engine/run_task.go` | 计算 `verifyWorkdir`（非 worktree → 沙箱 `out/`），三处 verifier 创建与 `req.Workdir` 均指向它 |
| `internal/ui/cli/cmd/team_cmd.go` | `suspended` 显示 `⚠️`（任务与批次）、`statusIcon` 补分支 |

验证：`go build ./...` 通过；`go test ./internal/team_engine/ ./internal/ui/cli/cmd/` 全部通过。
