package team_engine

import (
	"fmt"
	"path/filepath"
	"strings"
)

// output_conflict.go — 计划合法性的机械门:同一个交付文件只能有一个任务负责。
// worker 直接写 workspace(交付模型 ab07b1b)后,两个任务输出同一路径会在并行
// 执行时互相覆盖、后完成者胜——这是确定性的计划错误。不能等运行时撞车,也
// 不该交给 leader 临场裁决:在计划生成(decompose)与执行提交(StartPlanRun)
// 时直接拒绝并提示重新分解,保证输出所有权在机制层唯一。
// 路径按大小写不敏感比较(Windows/macOS 文件系统默认不区分),拼写变体也判冲突。

// checkOutputConflicts 校验一批任务的输出文件集合无重复;冲突返回带明细的错误。
func checkOutputConflicts(tasks []*Task) error {
	owner := map[string]*Task{}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		for _, o := range splitOutputEntries(t.Output) {
			if prev, dup := owner[o]; dup && prev.ID != t.ID {
				return fmt.Errorf(
					"plan output conflict: %q is owned by both task %s (%q) and task %s (%q) — each deliverable must have exactly one owner",
					o, prev.ID, prev.Title, t.ID, t.Title)
			}
			owner[o] = t
		}
	}
	return nil
}

// checkBatchOutputConflicts 与 checkOutputConflicts 相同,接受批次集合(恢复路径)。
func checkBatchOutputConflicts(batches []*Batch) error {
	var tasks []*Task
	for _, b := range batches {
		tasks = append(tasks, b.Tasks...)
	}
	return checkOutputConflicts(tasks)
}

// checkPlanTaskOutputConflicts 在分解层(源头)校验收纳计划:同名交付 = 分解
// 问题,由 decompose 承担,不落到执行。冲突时 plan.json 不落盘、Task 记录不
// 创建、批次不执行——decompose 以明确错误终止,无任何半成品状态。
func checkPlanTaskOutputConflicts(planTasks []PlanTask) error {
	owner := map[string]string{} // output → task title
	for _, pt := range planTasks {
		for _, o := range splitOutputEntries(pt.Output) {
			if prev, dup := owner[o]; dup {
				return fmt.Errorf(
					"plan output conflict: %q is owned by both task %q and task %q — each deliverable must have exactly one owner",
					o, prev, pt.Title)
			}
			owner[o] = pt.Title
		}
	}
	return nil
}

// splitOutputEntries 把 task.Output 拆成去重、归一化(小写 + clean + slash)的条目。
func splitOutputEntries(output string) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range strings.Split(output, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		key := strings.ToLower(filepath.ToSlash(filepath.Clean(o)))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}
