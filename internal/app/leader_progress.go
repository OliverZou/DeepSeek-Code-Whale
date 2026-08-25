package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/team_engine"
)

// leaderProgress bridges team-engine task events to the INLINE leader session:
// when a member task reaches done/failed, the app fires an injected (hidden)
// turn so the leader narrates the milestone to the user automatically — the
// WorkBuddy-style progress stream (v45: leader had to poll; storm-blocked
// loops burned tokens and narrated internals).
type leaderProgress struct {
	mu         sync.Mutex
	masters    map[string]bool      // master id → started inline in THIS app session
	lastNotify map[string]time.Time // per-master throttle
	sink       inlineLeaderTurnSink
	memberSink func(TeamMemberInfo) // TUI 成员卡片事件（producing/done/fail/verifier）
	pending    string               // 最新一条事件叙述（leader turn 活跃时暂存，不丢失）
	finished   bool                 // 当前 run 已完结（终态后不再注入）
	doneCount  int                  // 完成任务数（状态行统计）
	failCount  int                  // 失败任务数（状态行统计）
	statusText string               // 团队运行状态（显示层："团队执行中：已 N 个任务完成"）
}

// inlineLeaderTurnSink starts an injected leader turn rendering to the user.
// Provided by the service layer (desktop/TUI), which owns turn plumbing.
type inlineLeaderTurnSink func(visibleInput, hiddenInput string)

func (a *App) leaderProgressInit() {
	a.leaderProgressState = &leaderProgress{
		masters:    map[string]bool{},
		lastNotify: map[string]time.Time{},
	}
	team_engine.SetDefaultEventSink(a.onTeamEngineEvent)
}

// registerInlineMaster marks a master as driven by this app session's inline
// leader, so its task events become leader narrations (not other runs').
func (a *App) registerInlineMaster(masterID string) {
	s := a.leaderProgressState
	if s == nil || masterID == "" {
		return
	}
	s.mu.Lock()
	s.masters[masterID] = true
	// run 一经创建状态行即存在（即使 0 个任务完成）——用户时刻知道团队在干活。
	if s.statusText == "" {
		s.statusText = "团队正在干活：安排任务分工…"
	}
	s.mu.Unlock()
}

// RegisterMasterForInlineForTest marks a master inline (test helper for the service
// layer, which lives in a different package).
func (a *App) RegisterMasterForInlineForTest(masterID string) {
	a.registerInlineMaster(masterID)
}

// FinishInlineRunForTest marks the current run finished (same test-helper purpose).
func (a *App) FinishInlineRunForTest(masterID string) {
	s := a.leaderProgressState
	if s == nil {
		return
	}
	s.mu.Lock()
	s.finished = true
	s.statusText = ""
	s.mu.Unlock()
}

// SetInlineLeaderTurnSink wires the service layer's injected-turn runner.
func (a *App) SetInlineLeaderTurnSink(fn func(visibleInput, hiddenInput string)) {
	s := a.leaderProgressState
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sink = inlineLeaderTurnSink(fn)
	s.mu.Unlock()
}

// onTeamEngineEvent receives every engine event process-wide. Only task done/
// failed transitions of THIS session's inline masters become leader turns;
// throttled to at most one notification per master per 30s.
func (a *App) onTeamEngineEvent(ev team_engine.TaskEvent) {
	trunc := func(s string, n int) string {
		if runes := []rune(s); len(runes) > n {
			return string(runes[:n]) + "…"
		}
		return s
	}
	// 计划级事件（异步分解完成/失败/运行完结）：与“团队已经建立，正在规划任务”形成先后时间线。
	if ev.Type == team_engine.EventLeaderLog && ev.MasterID != "" && ev.Title != "" {
		if strings.HasPrefix(ev.Title, "计划分解失败") {
			a.notifyLeader(ev.MasterID, "【团队进展】"+ev.Title+"。请向用户说明并给出下一步建议。")
		} else if strings.Contains(ev.Title, "停止") {
			s := a.leaderProgressState
			if s != nil {
				s.mu.Lock()
				s.finished = true
				s.statusText = ""
				s.mu.Unlock()
			}
			a.notifyLeader(ev.MasterID, "【团队进展】已按你的指令停止全部任务。请向用户确认：已停止（已执行产出保留在工作区），并说明哪些任务已完成、哪些被中止。")
		} else if strings.Contains(ev.Title, "完结") {
			s := a.leaderProgressState
			if s != nil {
				s.mu.Lock()
				s.finished = true
				s.statusText = ""
				s.mu.Unlock()
			}
			a.notifyLeader(ev.MasterID, "【团队进展】运行已完结。请向用户说明最终状态与交付（收尾：概览/文件链接/耗时与 token）。")
		} else {
			if s := a.leaderProgressState; s != nil {
				s.mu.Lock()
				s.statusText = "团队正在干活：任务已开工"
				s.mu.Unlock()
			}
			a.notifyLeader(ev.MasterID, "【团队进展】任务分工已经排好。分工如下："+ev.Data+"。请照此简明转述（谁做什么、产出文件是什么），并说明你在等什么（例：“我等寇豆码完成后转交严过关独立验证”）；不要编造分工之外的任务。")
		}
		return
	}
	// 完成后不再注入新事件（终态）。
	if s := a.leaderProgressState; s != nil {
		s.mu.Lock()
		done := s.finished
		s.mu.Unlock()
		if done {
			return
		}
	}
	// 验证结论事件：leader 叙述“严过关完成独立验证，判定 PASS/FAIL”。
	if ev.Type == team_engine.EventVerifierResult && ev.MasterID != "" {
		verdict := "PASS"
		if ev.Data != "" {
			verdict = ev.Data
		}
		t := trunc(ev.Title, 60)
		next := "现在收尾：汇总交付"
		if verdict != "PASS" {
			next = "判定未通过：请向用户说明问题，并按修复流程（team_feedback/team_run 重派）处理"
		}
		a.notifyLeader(ev.MasterID, "【团队进展】「"+t+"」验证结论："+verdict+"。请向用户转述（例：“严过关完成独立验证，判定 "+verdict+"。”），并说明你现在的动作与等待（“"+next+"”）。")
		a.emitMemberCard(TeamMemberInfo{ToolCallID: ev.TaskID, Role: ev.Role, Status: "completed", SessionID: a.readTaskSession(ev.Workdir, ev.TaskID), Summary: "验证结论：" + verdict})
		return
	}
	if ev.Type != team_engine.EventStateChanged {
		return
	}
	// 启动叙述：成员已开始工作时 leader 说“XX 已启动，正在…，我等他…”。
	if ev.NewState == "producing" {
		a.emitMemberCard(TeamMemberInfo{ToolCallID: ev.TaskID, Role: ev.Role, Status: "running", Detail: trunc(ev.Title, 60)})
		a.notifyLeader(ev.MasterID, "【团队进展】「"+trunc(ev.Title, 60)+"」已开始执行（成员已启动）。请向用户转述：XX 已启动，正在编写/验证…；并说明你等待什么（例：“我等他完成后再转交严过关验证”）。")
		return
	}
	if ev.NewState != "done" && ev.NewState != "failed" {
		return
	}
	if s := a.leaderProgressState; s != nil {
		s.mu.Lock()
		if ev.NewState == "done" {
			s.doneCount++
		} else {
			s.failCount++
		}
		s.statusText = fmt.Sprintf("团队正在干活：已 %d 个任务完成%s", s.doneCount, func() string {
			if s.failCount > 0 {
				return fmt.Sprintf("（含 %d 个失败）", s.failCount)
			}
			return ""
		}())
		s.mu.Unlock()
	}
	title := trunc(ev.Title, 60)
	stateText := "完成"
	if ev.NewState == "failed" {
		stateText = "失败"
	}
	short := ev.TaskID
	if len(short) > 8 {
		short = short[:8]
	}
	excerpt := trunc(strings.TrimSpace(ev.Data), 80)
	deliveries := ""
	if len(ev.Deliverables) > 0 {
		var b strings.Builder
		for _, f := range ev.Deliverables {
			name := filepath.Base(f)
			abs := f
			if ev.Workdir != "" && !filepath.IsAbs(f) {
				abs = filepath.Join(ev.Workdir, f)
			}
			b.WriteString(fmt.Sprintf("- [%s](%s)\n", name, abs))
		}
		deliveries = "\n交付文件：\n" + b.String()
	}
	hidden := fmt.Sprintf("【团队进展】任务「%s」已%s（%s）。请向用户转述交付结果（例：\u201cXX 已交付：%s\u201d，附交付文件链接）；然后说明下一步与你在等什么（例：\u201c现在转交严过关以全新视角独立验证，我等他判定后再汇总交付\u201d）；若失败说明原因与决策；若全部完成进入收尾。%s", title, stateText, short, excerpt, deliveries)
	status := "completed"
	if ev.NewState == "failed" {
		status = "failed"
	}
	a.emitMemberCard(TeamMemberInfo{ToolCallID: ev.TaskID, Role: ev.Role, Status: status, SessionID: a.readTaskSession(ev.Workdir, ev.TaskID), Summary: excerpt})
	a.notifyLeader(ev.MasterID, hidden)
}

// readTaskSession reads the member session id from the task meta file
// (.whale/team_tasks/<taskid>/meta.json) for the member card.
func (a *App) readTaskSession(workdir, taskID string) string {
	if workdir == "" || taskID == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(workdir, ".whale", "team_tasks", taskID, "meta.json"))
	if err != nil {
		return ""
	}
	var m struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return m.SessionID
}

// TeamMemberInfo is the TUI member-card event payload (rendered by the
// existing spawn_subagent card path — no new TUI renderer needed).
type TeamMemberInfo struct {
	ToolCallID string // task id（卡片替换语义）
	Role       string
	Status     string // running | completed | failed
	SessionID  string
	Detail     string // 正在做什么（如 "写入 index.html"）
	Summary    string // 完成摘要（worker 报告截断）
	DurationMS int64
}

// SetTeamMemberSink wires the service layer's member-card emitter.
func (a *App) SetTeamMemberSink(fn func(TeamMemberInfo)) {
	s := a.leaderProgressState
	if s == nil {
		return
	}
	s.mu.Lock()
	s.memberSink = fn
	s.mu.Unlock()
}

// emitMemberCard routes a member progress event to the TUI card sink.
func (a *App) emitMemberCard(info TeamMemberInfo) {
	s := a.leaderProgressState
	if s == nil {
		return
	}
	s.mu.Lock()
	fn := s.memberSink
	s.mu.Unlock()
	if fn != nil {
		fn(info)
	}
}

// QueueLeaderProgress stores the latest pending progress narration while a
// leader turn is active (events must not be lost during a long turn); Flush
// delivers it once the turn ends.
func (a *App) QueueLeaderProgress(hidden string) {
	s := a.leaderProgressState
	if s == nil || strings.TrimSpace(hidden) == "" {
		return
	}
	s.mu.Lock()
	s.pending = hidden
	s.mu.Unlock()
}

// FlushLeaderProgress delivers the queued progress narration (called by the
// service layer when the active turn ends).
func (a *App) FlushLeaderProgress() {
	s := a.leaderProgressState
	if s == nil {
		return
	}
	s.mu.Lock()
	pending := s.pending
	s.pending = ""
	sink := s.sink
	s.mu.Unlock()
	if pending == "" || sink == nil {
		return
	}
	sink("", pending)
}

// LeaderRunFinished reports whether the current run reached its terminal
// state (used by the service to emit a final "finished" status line).
func (a *App) LeaderRunFinished() bool {
	s := a.leaderProgressState
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// LeaderRunStatus returns the team run status line for the UI (empty when
// no run is active or the run finished). Shown after every user-visible turn
// so the user always knows the team is still working.
func (a *App) LeaderRunStatus() string {
	s := a.leaderProgressState
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || len(s.masters) == 0 || s.statusText == "" {
		return ""
	}
	return s.statusText
}

// notifyLeader injects a hidden leader narration for an inline master.
// Throttled to at most one per master per 30s; no-op for non-inline masters
// or when no turn sink is wired.
func (a *App) notifyLeader(masterID, hidden string) {
	s := a.leaderProgressState
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.masters[masterID] {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	if last, ok := s.lastNotify[masterID]; ok && now.Sub(last) < 30*time.Second {
		s.mu.Unlock()
		return
	}
	s.lastNotify[masterID] = now
	sink := s.sink
	s.mu.Unlock()
	if sink == nil {
		return
	}
	sink("", hidden)
}
