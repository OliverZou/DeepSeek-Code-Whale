package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cleanManifest records exactly what /team clean removed (and why), so the
// action is auditable: date, master, removed set, kept set.
type cleanManifest struct {
	At        string   `json:"at"`
	Master    string   `json:"master"`
	Removed   []string `json:"removed"`
	Kept      []string `json:"kept"`
	ByPolicy  string   `json:"by_policy"`
}

// cleanTeamData implements "/team clean [--yes]" with the RETENTION policy
// (v47): REMOVE redundancy (raw LLM dumps, stale board snapshot, volatile
// memory, temp residue) while KEEPING the minimal full evidence chain in
// place — plan/review_actions/task processes/run_report — so every run stays
// traceable for bug-hunting and optimization analysis. Dry-run by default.
func (a *App) cleanTeamData(confirm bool) (string, error) {
	root := filepath.Join(a.workspaceRoot, ".whale", "team_tasks")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return "没有团队数据（.whale/team_tasks 不存在）", nil
	}
	// 运行中的 master 判定（status=running 且无 run_report）→ 全部跳过。
	finished := func(dir string) bool {
		if _, err := os.Stat(filepath.Join(dir, "run_report.json")); err == nil {
			return true
		}
		data, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
		if len(data) == 0 {
			return false
		}
		var m struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(data, &m) != nil {
			return false
		}
		return m.Status == "done" || m.Status == "failed" || m.Status == "cancelled"
	}
	// 可删除的冗余模式（白名单，其余一律保留）。
	removable := func(name string) bool {
		lower := strings.ToLower(name)
		switch {
		case strings.HasPrefix(lower, "leader_decompose_"), strings.HasPrefix(lower, "leader_elaborate_"):
			return true // 原始 LLM 转储（结论已提取）
		case lower == "board.md":
			return true // 瞬时白板快照
		case strings.HasSuffix(lower, ".tmp"), strings.HasSuffix(lower, ".bak"), strings.HasSuffix(lower, ".swp"):
			return true
		case lower == "memory" || strings.HasPrefix(lower, "memory"):
			return true // worker volatile 记忆
		}
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil && fi.Size() == 0 {
			return true
		}
		return false
	}
	// 1) 收集旧格式平铺冗余（team_tasks 根下的 leader_*.md/board.md 等）。
	var remove []string
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if removable(e.Name()) {
			remove = append(remove, e.Name())
		}
	}
	// 2) masters/ 内每个完结 run 的冗余（board.md 等可能已写入 master 目录）。
	var mastersDone []string
	if mEntries, err := os.ReadDir(filepath.Join(root, "masters")); err == nil {
		for _, me := range mEntries {
			if !me.IsDir() {
				continue
			}
			dir := filepath.Join(root, "masters", me.Name())
			if !finished(dir) {
				continue
			}
			mastersDone = append(mastersDone, me.Name())
			fEntries, _ := os.ReadDir(dir)
			for _, fe := range fEntries {
				if removable(fe.Name()) {
					remove = append(remove, filepath.Join("masters", me.Name(), fe.Name()))
				}
			}
		}
	}
	// 3) memory/（引擎级 volatile）
	if _, err := os.Stat(filepath.Join(root, "memory")); err == nil {
		remove = append(remove, "memory")
	}
	// 4) 干跑
	if !confirm {
		var b strings.Builder
		b.WriteString("【/team clean 预览】（默认干跑；加 --yes 执行）\n\n")
		b.WriteString(fmt.Sprintf("将删除冗余（%d 项）：\n", len(remove)))
		for _, r := range remove {
			b.WriteString("  - " + r + "\n")
		}
		if len(mastersDone) > 0 {
			b.WriteString(fmt.Sprintf("\n已完结 run（不受影响）：%s\n", strings.Join(mastersDone, ", ")))
		}
		b.WriteString("\n保留：plan/plan.md、review_actions.jsonl、每任务过程（input/output/worker_*/verifier_*）、run_report.json、meta.json\n")
		return b.String(), nil
	}
	// 5) 执行 + manifest
	removed := []string{}
	for _, r := range remove {
		if err := os.RemoveAll(filepath.Join(root, r)); err != nil {
			removed = append(removed, r+"（失败: "+err.Error()+"）")
		} else {
			removed = append(removed, r)
		}
	}
	manifest := cleanManifest{
		At:       time.Now().Format(time.RFC3339),
		Master:   strings.Join(mastersDone, ", "),
		Removed:  removed,
		Kept:     []string{"plan.json", "plan.md", "review_actions.jsonl", "tasks/*", "run_report.json", "meta.json"},
		ByPolicy: "retention: remove raw LLM dumps / stale board / volatile memory / temp; keep full evidence chain in place",
	}
	if mb, err := json.MarshalIndent(manifest, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(root, "clean_manifest.json"), mb, 0644)
	}
	return fmt.Sprintf("已清理 %d 项冗余（见 clean_manifest.json 审计记录）。\n已完结 run 的证据链（任务过程/决策/统计）全部保留。", len(removed)), nil
}
