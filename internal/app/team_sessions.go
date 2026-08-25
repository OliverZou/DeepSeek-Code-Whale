package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/usewhale/whale/internal/runtime/protocol"
	"github.com/usewhale/whale/internal/session"
)

// ListSubagentPicks returns the active subagent sessions for the /subagent
// picker. The returned SessionID is what gets switched into via the resume
// path (subagent sessions behave like any other session once opened).
func (a *App) ListSubagentPicks() []protocol.SubagentPick {
	subs, err := session.ListSubagentSessions(a.sessionsDir, true)
	if err != nil || len(subs) == 0 {
		return nil
	}
	picks := make([]protocol.SubagentPick, 0, len(subs))
	for _, s := range subs {
		picks = append(picks, protocol.SubagentPick{
			SessionID:   s.SessionID,
			Title:       s.Task,
			ParentTitle: s.ParentTitle,
			Role:        s.Role,
			Status:      s.Status,
		})
	}
	return picks
}

// TeamSessionPicks returns the member sessions of the current run for the
// TUI picker (called by the service layer on /team session).
func (a *App) TeamSessionPicks() []protocol.SessionPick {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return nil
	}
	defer eng.Close()
	masters, _ := eng.Store.ListMasterTasksBySession(a.sessionID)
	if len(masters) == 0 {
		return nil
	}
	mt := masters[len(masters)-1]
	tasks, _ := eng.Store.ListTasksByMasterTask(mt.ID)
	if len(tasks) == 0 {
		return nil
	}
	picks := make([]protocol.SessionPick, 0, len(tasks))
	for _, t := range tasks {
		picks = append(picks, protocol.SessionPick{
			TaskID:    t.ID,
			Role:      string(t.Role),
			Title:     t.Title,
			State:     string(t.State),
			SessionID: eng.Store.SessionID(t.ID),
		})
	}
	return picks
}

// ReadMemberSession returns the member transcript text for a session id.
func (a *App) ReadMemberSession(sessionID string) string {
	return a.readSessionTranscript(sessionID)
}

// teamSessionView implements "/team session [id]": list the member sessions
// of the current team run, or open one member session transcript (text view).
func (a *App) teamSessionView(sel string) string {
	eng, err := a.teamEngineForGoal()
	if err != nil {
		return "团队引擎不可用: " + err.Error()
	}
	defer eng.Close()

	masters, _ := eng.Store.ListMasterTasksBySession(a.sessionID)
	if len(masters) == 0 {
		return "当前会话没有团队 run。先 /team <目标> 启动一个。"
	}
	mt := masters[len(masters)-1] // 最近一个 run

	tasks, _ := eng.Store.ListTasksByMasterTask(mt.ID)
	if sel == "list" {
		if len(tasks) == 0 {
			return fmt.Sprintf("master %s 还没有任务记录（规划中/未执行）", sid8(mt.ID))
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("成员会话（master %s，%d 个任务）：\n\n", sid8(mt.ID), len(tasks)))
		for _, t := range tasks {
			sid := eng.Store.SessionID(t.ID)
			b.WriteString(fmt.Sprintf("- %s [%s] %s | session: %s\n", sid8(t.ID), t.State, truncT(t.Title, 40), sid))
		}
		b.WriteString("\n查看某成员完整对话：/team session <session id 或任务 id>\n")
		return b.String()
	}

	// 按 session id 或任务 id 前缀匹配。
	sid := sel
	for _, t := range tasks {
		ts := eng.Store.SessionID(t.ID)
		if strings.HasPrefix(t.ID, sel) || strings.HasPrefix(ts, sel) {
			sid = ts
			break
		}
	}
	if sid == "" || sid == "list" {
		return "未找到该成员会话（可先 /team session 列出）"
	}
	return a.readSessionTranscript(sid)
}

// readSessionTranscript prints the most recent dialogue lines of a member
// subagent session (jsonl transcript). Text view — enough to inspect what a
// member did without leaving the leader conversation.
func (a *App) readSessionTranscript(sessionID string) string {
	path := filepath.Join(a.sessionsDir, sessionID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return "读取会话失败: " + err.Error()
	}
	lines := strings.Split(string(data), "\n")
	const maxShown = 24
	var b strings.Builder
	b.WriteString(fmt.Sprintf("成员会话「%s」最近对话：\n\n", sessionID))
	shown := 0
	for i := len(lines) - 1; i >= 0 && shown < maxShown; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var m struct {
			Role string `json:"Role"`
			Text string `json:"Text"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		txt := strings.TrimSpace(m.Text)
		if txt == "" {
			continue
		}
		txt = strings.ReplaceAll(txt, "\n", " ")
		if r := []rune(txt); len(r) > 220 {
			txt = string(r[:220]) + "…"
		}
		who := "成员输入"
		if m.Role == "assistant" {
			who = "成员回复"
		}
		b.WriteString(fmt.Sprintf("· [%s] %s\n", who, txt))
		shown++
	}
	if shown == 0 {
		return "会话无文本记录（可能只有工具调用）"
	}
	return b.String()
}

func sid8(id string) string {
	if id == "" {
		return ""
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncT(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
