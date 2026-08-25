package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/usewhale/whale/internal/core"
)

type SessionSummary struct {
	ID           string
	ModTime      time.Time
	Size         int64
	Meta         SessionMeta
	Conversation string
}

func ListSessions(sessionsDir string, limit int) ([]SessionSummary, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]SessionSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !core.IsSessionJSONLName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if id == "" {
			continue
		}
		if isSubagentSessionID(id) {
			continue
		}
		meta, err := LoadSessionMeta(sessionsDir, id)
		if err == nil && strings.TrimSpace(meta.Kind) == "subagent" && strings.TrimSpace(meta.Team) == "" {
			// Plain subagents stay hidden from the picker; team members carry
			// Team meta so the user can switch into a Leader/worker/verifier
			// session to observe or redirect it (P3).
			continue
		}
		out = append(out, SessionSummary{
			ID:      id,
			ModTime: info.ModTime(),
			Size:    info.Size(),
			Meta:    meta,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ModTime.After(out[j].ModTime)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i].Conversation = SessionConversationTitle(sessionsDir, out[i].ID, out[i].Meta)
	}
	return out, nil
}

func SessionConversationTitle(sessionsDir, sessionID string, meta SessionMeta) string {
	if title := strings.TrimSpace(meta.Title); title != "" {
		return singleLine(title)
	}
	// Team member sessions have no Title but carry Team/Role/Task meta — render
	// a distinguishable entry for the session picker (P3).
	if meta.Kind == "subagent" && strings.TrimSpace(meta.Team) != "" {
		role := strings.TrimSpace(meta.Role)
		task := strings.TrimSpace(meta.Task)
		switch {
		case task != "":
			return singleLine(fmt.Sprintf("[%s] %s — %s", meta.Team, role, task))
		case role != "":
			return singleLine(fmt.Sprintf("[%s] %s", meta.Team, role))
		default:
			return singleLine(fmt.Sprintf("[%s] team member", meta.Team))
		}
	}
	if title, err := FirstVisibleUserMessage(sessionsDir, sessionID); err == nil && title != "" {
		return title
	}
	return "(no message yet)"
}

func FirstVisibleUserMessage(sessionsDir, sessionID string) (string, error) {
	path := FindSessionPathByID(sessionsDir, sessionID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg struct {
			Role   string
			Text   string
			Parts  []core.MessagePart `json:"parts,omitempty"`
			Hidden bool
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Role != "user" || msg.Hidden {
			continue
		}
		text := msg.Text
		if len(msg.Parts) > 0 {
			text = core.MessagePartsPlainText(msg.Parts)
		}
		if text := strings.TrimSpace(text); text != "" {
			return singleLine(text), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

func singleLine(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func FindSessionPathByID(sessionsDir, sessionID string) string {
	id := core.SanitizeSessionID(sessionID)
	return filepath.Join(sessionsDir, id+".jsonl")
}

func isSubagentSessionID(id string) bool {
	id = strings.TrimSpace(id)
	return strings.Contains(id, "--subagent-") || strings.HasPrefix(id, "subagent-")
}

// SubagentSession is one selectable subagent (child) session in the
// /subagent picker. Unlike ListSessions — which deliberately hides subagents
// from the /resume picker — this surface enumerates them so the user can
// switch into one and observe or continue it (P3). Once opened, a subagent
// session behaves like any other session.
type SubagentSession struct {
	SessionID       string
	ParentSessionID string
	ParentTitle     string // 主会话名（父会话 Title，回退到父会话首条消息）
	Team            string
	Role            string
	Task            string // 会话名（成员任务描述）
	Status          string
	Model           string
	ModTime         time.Time
}

// ListSubagentSessions enumerates subagent sessions in sessionsDir. activeOnly
// keeps only sessions that have not yet been closed (CompletedAt unset). The
// result is ordered by parent session title, then most-recent first within a
// parent.
func ListSubagentSessions(sessionsDir string, activeOnly bool) ([]SubagentSession, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]SubagentSession, 0)
	for _, e := range entries {
		if e.IsDir() || !core.IsSessionJSONLName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if id == "" {
			continue
		}
		meta, err := LoadSessionMeta(sessionsDir, id)
		if err != nil || !strings.EqualFold(strings.TrimSpace(meta.Kind), "subagent") {
			continue
		}
		if activeOnly && !meta.CompletedAt.IsZero() {
			continue
		}
		out = append(out, SubagentSession{
			SessionID:       id,
			ParentSessionID: strings.TrimSpace(meta.ParentSessionID),
			ParentTitle:     parentSessionTitle(sessionsDir, meta.ParentSessionID),
			Team:            strings.TrimSpace(meta.Team),
			Role:            strings.TrimSpace(meta.Role),
			Task:            strings.TrimSpace(meta.Task),
			Status:          strings.TrimSpace(meta.Status),
			Model:           strings.TrimSpace(meta.Model),
			ModTime:         info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := out[i].ParentTitle, out[j].ParentTitle
		if pi == "" {
			pi = out[i].ParentSessionID
		}
		if pj == "" {
			pj = out[j].ParentSessionID
		}
		if pi != pj {
			return pi < pj
		}
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out, nil
}

// parentSessionTitle resolves a parent session's display title, falling back to
// its first visible user message when no explicit title is recorded.
func parentSessionTitle(sessionsDir, parentID string) string {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return ""
	}
	if meta, err := LoadSessionMeta(sessionsDir, parentID); err == nil {
		if title := strings.TrimSpace(meta.Title); title != "" {
			return singleLine(title)
		}
		return SessionConversationTitle(sessionsDir, parentID, meta)
	}
	return parentID
}
