package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/usewhale/whale/internal/session"
)

type Result struct {
	Handled            bool
	ShouldExit         bool
	ClearScreen        bool
	SessionID          string
	Output             string
	ShowStatus         bool
	Mode               string
	AskPrompt          string
	PlanPrompt         string
	InitMemory         bool
	ShowSkills         bool
	ReviewPrompt       string
	AllowShellPrefixes []string
	ForkName           string
	TeamGoal           string // /team <goal> — start a team run for this goal
	TeamName           string // optional --team NAME selector for /team
	TeamInline         bool   // /team (default): current agent becomes the Leader (visible + interruptible)
	TeamList           bool   // /team (no args) or /team list — enumerate running team runs
	TeamSession        string // /team session [id] — view a member session (empty = list)
	TeamAbort          string // /team abort [masterID] — stop the run immediately
	TeamClean          bool   // /team clean [--yes] — retention cleanup (dry-run default)
	TeamCleanYes       bool
	Subagent           bool // /subagent — list active subagent sessions (switch picker)
	BtwQuestion        string
}

func NewSessionID(now time.Time) string {
	u, err := uuid.NewV7()
	if err != nil {
		return now.Format("20060102-150405")
	}
	return u.String()
}

func Parse(line, currentSessionID string, now time.Time) (Result, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return Result{}, nil
	}
	if trimmed == "/exit" {
		return Result{Handled: true, ShouldExit: true, SessionID: currentSessionID}, nil
	}
	if trimmed == "/status" {
		return Result{Handled: true, SessionID: currentSessionID, ShowStatus: true}, nil
	}
	fields := strings.Fields(trimmed)
	head := ""
	if len(fields) > 0 {
		head = fields[0]
	}
	if head == "/resume" && len(fields) > 1 {
		return Result{}, fmt.Errorf("usage: /resume")
	}
	if trimmed == "/subagent" {
		return Result{Handled: true, SessionID: currentSessionID, Subagent: true}, nil
	}
	if head == "/new" {
		next := ""
		if len(fields) > 2 {
			return Result{}, fmt.Errorf("usage: /new [id]")
		}
		if len(fields) == 2 {
			next = strings.TrimSpace(fields[1])
		}
		if next == "" {
			next = NewSessionID(now)
		}
		return Result{Handled: true, SessionID: next, Output: fmt.Sprintf("new session: %s", next)}, nil
	}
	if head == "/fork" {
		if len(fields) > 2 {
			return Result{}, fmt.Errorf("usage: /fork [name]")
		}
		name := ""
		if len(fields) == 2 {
			name = strings.TrimSpace(fields[1])
		}
		return Result{Handled: true, SessionID: currentSessionID, ForkName: name}, nil
	}
	if head == "/team" {
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "/team"))
		teamName := ""
		if i := strings.Index(rest, "--team"); i >= 0 {
			teamName = strings.TrimSpace(strings.TrimPrefix(rest[i:], "--team"))
			rest = strings.TrimSpace(rest[:i])
		}
		// 缺省 inline：当前 agent 直接成为 Leader（用户可见可插话）；
		// 显式 --subagent 才走后台 subagent leader（原行为）。
		subagentMode := false
		if i := strings.Index(rest, "--subagent"); i >= 0 {
			subagentMode = true
			rest = strings.TrimSpace(rest[:i] + rest[i+len("--subagent"):])
		}
		if rest == "" {
			if teamName != "" || subagentMode {
				return Result{}, fmt.Errorf("usage: /team <goal> [--team NAME] [--subagent]")
			}
			return Result{Handled: true, SessionID: currentSessionID, TeamList: true}, nil
		}
		if rest == "list" {
			return Result{Handled: true, SessionID: currentSessionID, TeamList: true}, nil
		}
		// /team session [sessionID|taskID]：查看团队成员的会话（列出/打开成员对话）。
		if strings.HasPrefix(rest, "session") {
			id := strings.TrimSpace(strings.TrimPrefix(rest, "session"))
			if id == "" {
				id = "picker"
			}
			return Result{Handled: true, SessionID: currentSessionID, TeamSession: id}, nil
		}
		// /team abort [masterID]：立即停止当前（或指定）run——不经模型，确定性操作。
		if rest == "clean --yes" {
			return Result{Handled: true, SessionID: currentSessionID, TeamClean: true, TeamCleanYes: true}, nil
		}
		if rest == "clean" {
			return Result{Handled: true, SessionID: currentSessionID, TeamClean: true}, nil
		}
		if strings.HasPrefix(rest, "abort") {
			id := strings.TrimSpace(strings.TrimPrefix(rest, "abort"))
			if id == "" {
				id = "picker" // 无参 → 选择器（列出 run 让用户选后再停）；/team abort current 或 <id> 才直接停
			}
			return Result{Handled: true, SessionID: currentSessionID, TeamAbort: id}, nil
		}
		return Result{Handled: true, SessionID: currentSessionID, TeamGoal: rest, TeamName: teamName, TeamInline: !subagentMode}, nil
	}
	if trimmed == "/clear" {
		return Result{Handled: true, SessionID: currentSessionID, ClearScreen: true}, nil
	}
	if trimmed == "/agent" {
		return Result{Handled: true, SessionID: currentSessionID, Mode: string(session.ModeAgent)}, nil
	}
	if trimmed == "/ask" {
		return Result{Handled: true, SessionID: currentSessionID, Mode: string(session.ModeAsk)}, nil
	}
	if strings.HasPrefix(trimmed, "/ask ") {
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "/ask"))
		return Result{Handled: true, SessionID: currentSessionID, Mode: string(session.ModeAsk), AskPrompt: payload}, nil
	}
	if trimmed == "/plan" {
		return Result{Handled: true, SessionID: currentSessionID, Mode: string(session.ModePlan)}, nil
	}
	if strings.HasPrefix(trimmed, "/plan ") {
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "/plan"))
		if payload == "" || payload == "show" || payload == "on" || payload == "off" {
			return Result{}, fmt.Errorf("usage: /plan [prompt]")
		}
		return Result{Handled: true, SessionID: currentSessionID, Mode: string(session.ModePlan), PlanPrompt: payload}, nil
	}
	if trimmed == "/init" {
		return Result{Handled: true, SessionID: currentSessionID, InitMemory: true}, nil
	}
	if trimmed == "/skills" || strings.HasPrefix(trimmed, "/skills ") {
		fields := strings.Fields(trimmed)
		if len(fields) == 1 && fields[0] == "/skills" {
			return Result{Handled: true, SessionID: currentSessionID, ShowSkills: true}, nil
		}
		return Result{}, fmt.Errorf("usage: /skills")
	}
	if head == "/review" {
		args := strings.TrimSpace(strings.TrimPrefix(trimmed, "/review"))
		prompt, err := ReviewPromptFromArgs(args)
		if err != nil {
			return Result{}, err
		}
		allowPrefixes, err := ReviewShellAllowPrefixesFromArgs(args)
		if err != nil {
			return Result{}, err
		}
		return Result{Handled: true, SessionID: currentSessionID, ReviewPrompt: prompt, AllowShellPrefixes: allowPrefixes}, nil
	}
	if head == "/btw" {
		question := strings.TrimSpace(strings.TrimPrefix(trimmed, "/btw"))
		if question == "" {
			return Result{}, fmt.Errorf("Usage: /btw <your question>")
		}
		return Result{Handled: true, SessionID: currentSessionID, BtwQuestion: question}, nil
	}
	return Result{}, nil
}

func PlanPromptFromSlash(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "/plan ") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "/plan"))
	if payload == "" || payload == "show" || payload == "on" || payload == "off" {
		return "", false
	}
	return payload, true
}

func AskPromptFromSlash(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "/ask ") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "/ask"))
	if payload == "" {
		return "", false
	}
	return payload, true
}
