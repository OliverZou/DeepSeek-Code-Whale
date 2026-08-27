package team_engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// run_report.go — 事后可分析数据：计划执行结束后把每任务/每批次的耗时、
// token、重试、终态写进 <masterDir>/run_report.json。与 30s 心跳 logs 互补：
// 心跳供运行中监控，报告供结束后优化分析（哪类任务/角色烧时间或 token）。

// runReport is the JSON shape of the post-run dataset.
type runReport struct {
	MasterTaskID string    `json:"master_task_id"`
	Goal         string    `json:"goal"`
	Status       string    `json:"status"` // done / failed / error
	Summary      string    `json:"summary"`
	Error        string    `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	WallSeconds  float64   `json:"wall_seconds"`
	TotalTokens  int       `json:"total_tokens"`
	// Token split by DeepSeek prefix-cache billing: hit tokens replay at ~1/31
	// of miss price, so effective_tokens (= miss + completion + hit/31) tracks
	// real API cost while total_tokens counts raw replay volume.
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	EffectiveTokens       int `json:"effective_tokens"`
	// 事后分析定位：leader 会话、运行引擎版本、leader 轮 token。
	LeaderSessionID string      `json:"leader_session_id,omitempty"`
	EngineCommit    string      `json:"engine_commit,omitempty"`
	LeaderTokens    int         `json:"leader_tokens,omitempty"`
	Batches         []batchStat `json:"batches"`
}

type batchStat struct {
	ID       string     `json:"id"`
	Label    string     `json:"label,omitempty"`
	Status   string     `json:"status"`
	Duration float64    `json:"duration_seconds,omitempty"`
	Tasks    []taskStat `json:"tasks"`
}

type taskStat struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Role             string  `json:"role"`
	State            string  `json:"state"`
	RetryCount       int     `json:"retry_count"`
	WorkerDuration   float64 `json:"worker_duration_seconds,omitempty"`
	WorkerTokens     int     `json:"worker_tokens,omitempty"`
	VerifierDuration float64 `json:"verifier_duration_seconds,omitempty"`
	VerifierTokens   int     `json:"verifier_tokens,omitempty"`
	ToolCalls        int     `json:"tool_calls,omitempty"`
	TopTools         string  `json:"top_tools,omitempty"`
	// 事后分析：会话定位/验证结论/验证者 cap/工具等待。
	SessionID        string  `json:"session_id,omitempty"`
	Verdict          string  `json:"verdict,omitempty"`
	VerifierCapCount int     `json:"verifier_cap_count,omitempty"`
	ToolWaitSeconds  float64 `json:"tool_wait_seconds,omitempty"`
}

// writeRunReport writes the run_report.json into the master's whiteboard dir.
// Best-effort: failures are logged, never fatal — the run is already over.
//
// 全量账单从 store 的 per-task 持久字段聚合 + leader 分账（leaderHit/
// leaderMiss/leaderCompletion 由调用方传入本实例累计的 leader 轮用量）。
// worker/verifier 的执行可能发生在另一个引擎实例（leader-driven 双实例）：
// v12 实测 execute 侧累计 311K，而 leader 侧实例本地只有 33.8K——直接取
// 本地计数器会把真实成本少报一个数量级。store 无分账字段（旧数据）时
// 回退到本实例计数器（与旧行为一致）。
func (e *TeamEngine) writeRunReport(masterTaskID, goal, status, summary string, execErr error, started time.Time, batches []*Batch, leaderHit, leaderMiss, leaderCompletion int) {
	hit, miss, completion := leaderHit, leaderMiss, leaderCompletion
	storeRaw := 0
	for _, bch := range batches {
		for _, t := range bch.Tasks {
			cur, err := e.Store.GetTask(t.ID)
			if err != nil || cur == nil {
				continue
			}
			hit += cur.WorkerPromptHit + cur.VerifierPromptHit
			miss += cur.WorkerPromptMiss + cur.VerifierPromptMiss
			completion += cur.WorkerCompletion + cur.VerifierCompletion
			storeRaw += cur.WorkerTokens + cur.VerifierTokens
		}
	}
	leaderRaw := leaderHit + leaderMiss + leaderCompletion
	total := storeRaw + leaderRaw
	if total <= 0 {
		total = e.tokenTotal()
	}
	effective := miss + completion + hit/31
	if effective <= 0 {
		effective = total
	}
	report := runReport{
		MasterTaskID:          masterTaskID,
		Goal:                  goal,
		Status:                status,
		Summary:               summary,
		StartedAt:             started,
		FinishedAt:            time.Now(),
		WallSeconds:           time.Since(started).Seconds(),
		TotalTokens:           total,
		PromptCacheHitTokens:  hit,
		PromptCacheMissTokens: miss,
		CompletionTokens:      completion,
		EffectiveTokens:       effective,
		LeaderSessionID:       e.leaderSessionIDForReport(masterTaskID),
		EngineCommit:          buildRevision(),
		LeaderTokens:          e.leaderRunTokens,
	}
	if execErr != nil {
		report.Error = execErr.Error()
	}
	for _, bch := range batches {
		bs := batchStat{ID: bch.ID, Label: bch.Label, Status: string(bch.Status), Duration: bch.TotalDuration}
		for _, t := range bch.Tasks {
			cur, err := e.Store.GetTask(t.ID)
			if err != nil || cur == nil {
				continue
			}
			bs.Tasks = append(bs.Tasks, taskStat{
				ID:               cur.ID,
				Title:            cur.Title,
				Role:             string(cur.Role),
				State:            string(cur.State),
				RetryCount:       cur.RetryCount,
				WorkerDuration:   cur.WorkerDuration,
				WorkerTokens:     cur.WorkerTokens,
				VerifierDuration: cur.VerifierDuration,
				VerifierTokens:   cur.VerifierTokens,
				ToolCalls:        cur.ToolCalls,
				TopTools:         cur.TopTools,
				SessionID:        e.Store.SessionID(t.ID),
				Verdict:          cur.Verdict,
				VerifierCapCount: cur.VerifierCapCount,
				ToolWaitSeconds:  cur.ToolWaitSeconds,
			})
		}
		report.Batches = append(report.Batches, bs)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		Log("report", "marshal run report: %v", err)
		return
	}
	path := filepath.Join(e.Whiteboard.MasterDir(masterTaskID), "run_report.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		Log("report", "create report dir: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		Log("report", "write run report: %v", err)
		return
	}
	Log("report", "written run_report.json for %s (wall=%.1fs raw=%d eff=%d hit=%d miss=%d comp=%d batches=%d tasks=%d)",
		masterTaskID[:8], report.WallSeconds, report.TotalTokens, report.EffectiveTokens, report.PromptCacheHitTokens, report.PromptCacheMissTokens, report.CompletionTokens, len(report.Batches), countReportTasks(report.Batches))
}

// leaderSessionIDForReport resolves the Leader subagent session ID for a
// master: the engine-local record first (leader-driven run), then the
// persisted master meta (recovered/other-process runs).
func (e *TeamEngine) leaderSessionIDForReport(masterTaskID string) string {
	if e.leaderSessionID != "" {
		return e.leaderSessionID
	}
	if mt, err := e.GetMasterTask(masterTaskID); err == nil && mt != nil && mt.LeaderSessionID != "" {
		return mt.LeaderSessionID
	}
	return ""
}

// BuildRevision is injected at build time via -ldflags
// ("-X github.com/usewhale/whale/internal/team_engine.BuildRevision=<git rev>")
// so every run_report carries the exact engine revision (v45: "unknown" because
// plain builds do not embed vcs.revision).
var BuildRevision string

// buildRevision returns the VCS revision embedded at build time (vcs.revision
// from runtime/debug), or a buildID fallback — so an A/B run can always be
// traced back to the exact engine binary that produced it.
func buildRevision() string {
	if BuildRevision != "" {
		return BuildRevision
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return "unknown"
}

func countReportTasks(batches []batchStat) int {
	n := 0
	for _, b := range batches {
		n += len(b.Tasks)
	}
	return n
}

// refreshRunReportAfterLeaderReview rewrites run_report.json with the FINAL
// task states after the Leader's review turn.  The execute-side report is a
// pre-review snapshot: when the Leader redispatch a suspended verification
// task to PASS (v47), the snapshot still says "failed" while the run actually
// completed — a misleading tail for the user.  This reconciliation rewrites
// status/summary from the current store state and keeps the original
// timestamps/tokens.
func (e *TeamEngine) refreshRunReportAfterLeaderReview(masterTaskID, goal string, started time.Time, batches []*Batch, leaderTokens int) {
	if len(batches) == 0 {
		return
	}
	status := "done"
	failedBatches := 0
	taskState := func(t *Task) string { return string(e.Store.TaskState(t.ID)) }
	_ = taskState
	allTasksDone := true
	for _, b := range batches {
		if b.Status == BatchStatusFailed {
			failedBatches++
		}
		for _, t := range b.Tasks {
			if st := e.Store.TaskState(t.ID); st != TaskStateDone {
				allTasksDone = false
			}
		}
	}
	if !allTasksDone {
		status = "failed"
	} else if failedBatches > 0 {
		status = "done" // redispatch closed every task; batches were failed in the snapshot only
	}
	summary := fmt.Sprintf("%d/%d batches passed, %d failed", len(batches)-failedBatches, len(batches), failedBatches)
	e.leaderRunTokens = leaderTokens
	leaderHit, leaderMiss, leaderCompletion := e.usageSplit()
	e.writeRunReport(masterTaskID, goal, status, summary, nil, started, batches, leaderHit, leaderMiss, leaderCompletion)
}
