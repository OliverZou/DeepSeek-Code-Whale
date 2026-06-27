# 任务重命名功能实现计划

## 概述
在任务三点上下文菜单中添加"重命名"功能，支持内联编辑，全栈实现（Go 后端 + Wails 桥接 + React 前端）。

## 任务列表

### Task 1: 后端 RenameMasterTask API (app.go)
**状态**: ✅ COMPLETE

在 `app.go` 中新增 `RenameMasterTask(taskID, newGoal string) string` 方法：
- 验证 newGoal 非空
- 定位 `masters/{id}/meta.json`，更新 `title` 字段
- 重写 `masters/{id}/goal.md`
- 更新内存索引 `mt.Goal = newGoal`
- 发出 `update` 事件刷新前端

### Task 2: 前端桥接层
**状态**: ✅ COMPLETE

- `wails.ts`: 添加 `renameMasterTask(taskId, newGoal)` API 封装
- `App.js`: 添加 `RenameMasterTask` JS 绑定
- `App.d.ts`: 添加 `RenameMasterTask` 类型声明

### Task 3: 前端 UI - TaskItem 重命名
**状态**: ✅ COMPLETE

- 三点菜单新增「重命名」选项
- 点击后切换为内联 `<input>`（替代原有 label）
- Enter 提交、Escape 取消、onBlur 自动提交
- 提交后调用 `api.renameMasterTask` 并刷新列表
- 修复 `App.d.ts` / `App.js` 中 `RenameMasterTask` 重复声明

### Task 4: 编译验证
**状态**: ✅ COMPLETE

- TypeScript (`npx tsc --noEmit`): 通过
- Go (`go build ./...`): 通过
