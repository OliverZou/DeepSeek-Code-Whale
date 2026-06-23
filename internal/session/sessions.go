package session

import (
	"bufio"
	"encoding/json"
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

	type entryInfo struct {
		id      string
		modTime time.Time
	}
	candidates := make([]entryInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !core.IsSessionJSONLName(e.Name()) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if id == "" || isSubagentSessionID(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, entryInfo{id: id, modTime: info.ModTime()})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	need := len(candidates)
	if limit > 0 && limit < need {
		need = limit
	}

	out := make([]SessionSummary, 0, need)
	for _, c := range candidates {
		if limit > 0 && len(out) >= limit {
			break
		}
		meta, err := LoadSessionMeta(sessionsDir, c.id)
		if err != nil {
			continue
		}
		if strings.TrimSpace(meta.Kind) == "subagent" {
			continue
		}
		out = append(out, SessionSummary{
			ID:      c.id,
			ModTime: c.modTime,
			Meta:    meta,
		})
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
