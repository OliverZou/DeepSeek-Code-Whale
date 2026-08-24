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

**3. [major] verifier 持久会话空闲期被 kill（`#10a`）— 根因已找到并修复（d9f2d25）**

**根因**：旧版 `SpawnPersistent` 用 `exec.CommandContext(runCtx, …)`，其中 `runCtx = context.WithTimeout(req.Timeout)`，而 `req.Timeout` 对 verifier 是 300s。这个 deadline **覆盖整个持久会话生命周期**（从 spawn 到 CloseSession）。当 verifier 第一次验证返回 FAIL、worker 进入重试（重新产出，耗时可能 > 300s）时，verifier 子进程在空闲中命中 300s deadline，被 `CommandContext` 自动 kill。下一个 `ContinueSession` 时子进程已死 → `stdin` 写失败 → 立即返回空 `[FAIL]`（`Duration 0.0s`）。

**证据**（engine.log 时间线精确对应 300s 阈值）：
- `f2119309`：verifier 第一次验证后空闲 **171s**（< 300s）→ CONTINUE 成功（46.6s）。
- `af207e0c`：verifier 第一次验证后空闲 **497s**（> 300s）→ CONTINUE 立即失败（0.0s）。

**修复**（d9f2d25，`spawner.go`）：`exec.CommandContext` → `exec.Command`（无 context）；会话级 timeout 改为 `sendAndReceive` 内的 per-call `time.AfterFunc`，只在该次 round-trip 未按时返回 EOT 时 kill。空闲期不再有 timer 悬挂。

**验证**：`persist_idle_repro_test.go` 用最新二进制实测空闲 480s 后 round2 仍正常返回（`STILL_ALIVE`），确认空闲期不再退出。2048 测试当时跑的是 d9f2d25 提交前编译的旧二进制，故仍暴露该 bug——重新编译后不复现。

**4. [major] 测试项目产物 bug：game.js 的 `window` 无守卫（`#10e`）**

`game.js` 第 371 行 UI 层 `var Core = window.Game2048Core;` 缺少 `typeof window !== 'undefined'` 守卫，Node 环境 `require()` 执行全文件时抛 `ReferenceError: window is not defined`，导致 42 个单元测试全部无法运行。此 bug 被 `bf63d390` 任务的 verifier 第 3 轮**正确发现**（证明 verifier 有价值），但根因是 UI 集成任务的 worker 在追加 UI 层时引入了回归——这本身暴露了「worker 追加改动缺少回归验证」的薄弱环节。

### 工程/可观测性问题

**5. [minor] 日志为空（`#11`）— 已修复（移除 build tag 门槛）**

`team_engine.log` 与 `engine.log` 均为 0 字节。根因：`internal/team_engine/log/teamlog.go` 带 `//go:build teamlog` 编译标签，默认构建（`go build` 无 `-tags teamlog`）时编译的是 `teamlog_off.go` 的 **no-op 实现**，`NewTeamLog` 返回空 `&TeamLog{}`，所有 `WorkerStart`/`VerifierDone`/`BatchDone` 日志方法都是空函数。因此即便 `app_new.go` 已接线 `SetLogger(NewTeamLog(workspaceRoot))`，默认二进制也不落任何日志。

修复：删除 `teamlog.go` 的 `//go:build teamlog` 标签（改为无条件编译真实实现），删除 `teamlog_off.go`（no-op 实现，避免符号重复）。默认构建现在会真实写入 `team_engine.log`，排障有据可查。`go build ./...`、`go vet`、`gofmt -l` 均通过。

**6. [minor] 工件泄漏（`#12`）**

`.whale_verify_check.html` 泄漏到 workdir 根目录（推测为某 verifier 的临时验证产物未清理）。

**7. [major] worker 产物硬编码沙箱深度 → 传播后测试失效（`#24`）**

**澄清**：之前怀疑「verifier 没真正运行测试就 pass」。核对 `0547e017`（单元测试任务）的 `verify.md` 后，**verifier 确实运行了测试**（`node --test test/game.test.js` → 45/45 pass，`node test/game.test.js` → 45/45 pass），且正确判断了沙箱内 5 层相对路径指向 repo root 的 `game.js`。verifier 的 PASS 在**沙箱语境**下是对的。

**真正的 bug 在传播后**：worker 在沙箱 `out/` 目录写 `test/game.test.js` 时，用 `require(path.join(__dirname, '..','..','..','..','..','game.js'))` 硬编码 5 层深度。沙箱内 `out/test/` 向上 5 层正好到 workspace root 的 `game.js`（测试通过）。但 `propagateTaskOutput`（纯 `copyDir`，不做路径修正）把文件复制到最终 workspace 后，`__dirname` 变成 `<workspace>/test`，向上 5 层 = 盘符根，`require('D:\game.js')` → `MODULE_NOT_FOUND`（实测复现）。

**根因**：sandbox 隔离下，worker 用相对路径「逃逸」沙箱引用 workspace root 的跨任务产物（`game.js` 是 `f2119309` 的产物），路径深度依赖沙箱层级；传播改变层级后失效。且传播后无任何验证。

**已修复（两者都做）**：
- **提示层预防**：worker prompt 追加「【路径约束】」，明示产物会被复制到 workspace 根目录、严禁用相对路径（如 `../../game.js`）引用沙箱外文件。治本，但依赖 LLM 遵从。
- **机械验证兜底**：`propagateTaskOutput` 提前到 done 标记之前，传播后调用 `runMechanicalVerify` 在最终 workspace 检测并运行标准测试/构建（`go test ./...`、`node --test`、`npm test`），exit≠0 降级 FAIL 触发重试，反馈含失败输出。未检测到自动化测试则跳过（不误判）。治标兜底，确保传播后失效必然被发现。

**未做（依赖显式化）**：③ 跨任务依赖显式化——依赖产物通过 input/inbox 传入沙箱而非靠 worker 猜路径。这是更彻底的根治，但需改动 whiteboard/leader 分解逻辑，作为后续项。

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
| `internal/team_engine/run_task.go` | 计算 `verifyWorkdir`（非 worktree → 沙箱 `out/`），三处 verifier 创建与 `req.Workdir` 均指向它；worker prompt 追加路径约束；`propagateTaskOutput` 提前到 done 前，传播后 `runMechanicalVerify` 机械验证（exit≠0 降级 FAIL） |
| `internal/ui/cli/cmd/team_cmd.go` | `suspended` 显示 `⚠️`（任务与批次）、`statusIcon` 补分支 |

验证：`go build ./...` 通过；`go test ./internal/team_engine/ ./internal/ui/cli/cmd/` 全部通过。
