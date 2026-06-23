package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/pod"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/tasks"
	teamlog "github.com/usewhale/whale/internal/team_engine/log"
	"github.com/usewhale/whale/internal/team_engine"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx          context.Context
	workDir      string
	teamsDir     string
	sessionsDir  string
	sessionStore *store.JSONLStore
	engine       *team_engine.TeamEngine
	mu           sync.Mutex
	running      bool
}

func NewApp() *App {
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	teamsDir := filepath.Join(exeDir, "teams")
	// Dev: search upward for bin\teams（wails dev / go run 时自动定位项目 bin 目录）
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if fi, err := os.Stat(filepath.Join(dir, "bin", "teams")); err == nil && fi.IsDir() {
				binDir := filepath.Join(dir, "bin")
				teamsDir = filepath.Join(binDir, "teams")
				exeDir = binDir
				break
			}
		}
	}
	return &App{workDir: exeDir, teamsDir: teamsDir}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Initialize global sessions store (CLI-compatible)
	dataDir := store.DefaultDataDir()
	a.sessionsDir = store.DefaultSessionsDir(dataDir)
	sessStore, err := store.NewJSONLStore(a.sessionsDir)
	if err != nil {
		pod.Log("startup", "session store: %v", err)
	} else {
		a.sessionStore = sessStore
	}
	pod.Log("startup", "whale-pod workDir=%s teamsDir=%s sessionsDir=%s", a.workDir, a.teamsDir, a.sessionsDir)
	a.openEngine()
}

func (a *App) openEngine() {
	wbDir := filepath.Join(a.workDir, ".whale", "team_tasks")
	teamlogDir := filepath.Join(a.workDir, ".whale", "team_tasks", "logs")
	os.MkdirAll(teamlogDir, 0755)
	if tl := teamlog.NewTeamLog(a.workDir); tl != nil {
		tl.AddLog(filepath.Join(teamlogDir, "team_engine.log"))
		team_engine.SetLogger(tl)
	}
	spawner := team_engine.NewShellSubagentSpawner()
	eng, err := team_engine.New(wbDir, wbDir, "", spawner)
	if err != nil {
		pod.Log("startup", "open engine: %v", err)
		return
	}
	a.mu.Lock()
	if a.engine != nil { a.engine.Close() }
	a.engine = eng
	a.mu.Unlock()

	// Forward events to frontend.
	eng.OnEvent(func(evt team_engine.TaskEvent) {
		runtime.EventsEmit(a.ctx, "task-event", pod.TaskEvent{
			Type:     pod.TaskEventType(evt.Type),
			TaskID:   evt.TaskID,
			Title:    evt.Title,
			Progress: evt.Progress,
			NewState: evt.NewState,
		})
	})
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	if a.engine != nil { a.engine.Close() }
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Wails-callable: workspace
// ---------------------------------------------------------------------------

// SetWorkDir sets the working directory and re-opens the engine.
func (a *App) SetWorkDir(path string) string {
	path = filepath.Clean(path)
	if path == "" || path == "." { return "" }
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("目录不存在: %s", path)
	}
	a.workDir = path
	a.openEngine()
	pod.Log("workdir", "switched to %s", path)
	return ""
}

// GetWorkDir returns the current working directory.
func (a *App) GetWorkDir() string { return a.workDir }

func (a *App) PickFolder() string {
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择工作空间",
	})
	if err != nil || dir == "" {
		return ""
	}
	return dir
}

// ListTeams returns available team names from whale-pod.exe所在目录\teams
func (a *App) ListTeams() []string {
	seen := map[string]bool{}
	var teams []string
	for _, dir := range []string{
		a.teamsDir,
	} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && !seen[e.Name()] {
				seen[e.Name()] = true
				teams = append(teams, e.Name())
			}
		}
	}
	return teams
}

func (a *App) LoadSummonedItems() []pod.SummonedItemJSON {
	data, err := os.ReadFile(filepath.Join(a.workDir, "summoned.json"))
	if err != nil {
		return nil
	}
	var items []pod.SummonedItemJSON
	json.Unmarshal(data, &items)
	return items
}

func (a *App) SaveSummonedItems(items []pod.SummonedItemJSON) {
	data, _ := json.Marshal(items)
	os.WriteFile(filepath.Join(a.workDir, "summoned.json"), data, 0644)
}

// agentNameResolver implements team_engine.AgentInfoProvider by scanning
// the agents directory and mapping agent file names to their role titles.
type agentNameResolver struct {
	roleMap map[string]string
}

func (r *agentNameResolver) AgentRole(name string) string { return r.roleMap[name] }
func (r *agentNameResolver) AgentDesc(name string) string  { return "" }

// loadAgentNameResolver scans the agents directory and returns a resolver
// that maps agent name → role title (e.g. "backend-engineer" → "后端工程师").
func (a *App) loadAgentNameResolver() *agentNameResolver {
	agentsDir := filepath.Join(filepath.Dir(a.teamsDir), "agents")
	m := make(map[string]string)
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return &agentNameResolver{roleMap: m}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		subEntries, err := os.ReadDir(filepath.Join(agentsDir, e.Name()))
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if se.IsDir() || !strings.HasSuffix(se.Name(), ".md") {
				continue
			}
			path := filepath.Join(agentsDir, e.Name(), se.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			def, ok, _ := tasks.ParseMarkdownAgentDefinition(string(data), se.Name(), "")
			if !ok {
				continue
			}
			m[def.Name] = def.Role
		}
	}
	return &agentNameResolver{roleMap: m}
}

func (a *App) ListTeamDetails() []pod.TeamDetailJSON {
	teamsDir := a.teamsDir
	entries, err := os.ReadDir(teamsDir)
	if err != nil {
		pod.Log("teams", "ListTeamDetails read %s: %v", teamsDir, err)
		return nil
	}
	resolver := a.loadAgentNameResolver()
	var result []pod.TeamDetailJSON
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tc, err := team_engine.LoadTeamConfig(filepath.Join(teamsDir, e.Name(), "team.yaml"))
		if err != nil {
			tc, err = team_engine.LoadTeamConfig(filepath.Join(teamsDir, e.Name()+".yaml"))
			if err != nil {
				continue
			}
		}
		tc.ResolveRoles(resolver)
		roles := make([]string, len(tc.Roles))
		for i, name := range tc.Roles {
			roles[i] = tc.RoleDisplayName(name)
		}
		result = append(result, pod.TeamDetailJSON{
			Name:        e.Name(),
			Label:       tc.Label,
			Category:    tc.Category,
			Description: tc.Leader.Description,
			Roles:       roles,
		})
	}
	pod.Log("teams", "ListTeamDetails found %d teams in %s", len(result), teamsDir)
	return result
}

// ListAgents returns all available agent definitions from the agents directory.
func (a *App) ListAgents() []pod.AgentInfoJSON {
	agentsDir := filepath.Join(filepath.Dir(a.teamsDir), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		pod.Log("agents", "ListAgents read %s: %v", agentsDir, err)
		return nil
	}
	var result []pod.AgentInfoJSON
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		subEntries, err := os.ReadDir(filepath.Join(agentsDir, e.Name()))
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if se.IsDir() || !strings.HasSuffix(se.Name(), ".md") {
				continue
			}
			path := filepath.Join(agentsDir, e.Name(), se.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			def, ok, _ := tasks.ParseMarkdownAgentDefinition(string(data), se.Name(), "")
			if !ok {
				continue
			}
			result = append(result, pod.AgentInfoJSON{
				Name:        def.Name,
				Role:        def.Role,
				Description: def.Description,
				WhenToUse:   def.WhenToUse,
				Category:    e.Name(),
				Tools:       def.Tools,
				Skills:      def.Skills,
			})
		}
	}
	seen := make(map[string]bool, len(result))
	deduped := make([]pod.AgentInfoJSON, 0, len(result))
	for _, a := range result {
		if !seen[a.Name] {
			seen[a.Name] = true
			deduped = append(deduped, a)
		}
	}
	pod.Log("agents", "ListAgents found %d agents (%d unique) in %s", len(result), len(deduped), agentsDir)
	return deduped
}

// ---------------------------------------------------------------------------
// Wails-callable: tasks
// ---------------------------------------------------------------------------

// StartTask creates a master task and runs PlanAndRun.
// workDir: 空字符串表示 exeDir\chats
func (a *App) StartTask(goal, teamName, workDir string) string {
	if strings.TrimSpace(goal) == "" { return "goal 不能为空" }

	a.mu.Lock()
	if a.running { a.mu.Unlock(); return "已有任务正在执行" }
	a.running = true
	a.mu.Unlock()

	go func() {
		defer func() { a.mu.Lock(); a.running = false; a.mu.Unlock() }()

		a.mu.Lock()
		eng := a.engine
		a.mu.Unlock()
		if eng == nil { a.openEngine(); a.mu.Lock(); eng = a.engine; a.mu.Unlock() }
		if eng == nil { return }

		// Use exeDir\chats if no workspace specified
		taskWorkDir := workDir
		if taskWorkDir == "" {
			taskWorkDir = filepath.Join(a.workDir, "chats")
			os.MkdirAll(taskWorkDir, 0755)
		}

		// Load team config if specified.
		if teamName != "" {
			tc, err := team_engine.FindTeam(a.teamsDir, teamName)
			if err == nil { eng.SetTeam(tc) }
		}

		// Create session first
		sessionID := fmt.Sprintf("pod-%s", time.Now().Format("20060102-150405"))
		now := time.Now()
		session.SaveSessionMeta(a.sessionsDir, sessionID, session.SessionMeta{
			Title: goal, Kind: "pod-chat", Workspace: taskWorkDir,
			Agent: "team:" + teamName, Status: "active", StartedAt: now, UpdatedAt: now,
		})

		mt, err := eng.CreateMasterTask(goal, taskWorkDir, sessionID)
		if err != nil { pod.Log("task", "create master: %v", err); return }

		ctx := context.Background()
		batches, err := eng.PlanAndRun(ctx, goal, taskWorkDir, mt.ID)
		pod.Log("task", "done: batches=%d err=%v", len(batches), err)
		eng.CompleteMasterTask(mt.ID)
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}()

	return ""
}

// StartExpertTask creates a single-agent team engine task.
// It builds a synthetic TeamConfig with one role (the selected agent)
// and runs PlanAndRun, which will produce a single task executed by that agent.
func (a *App) StartExpertTask(goal, agentName, workDir string) string {

	if strings.TrimSpace(goal) == "" { return "goal 不能为空" }
	if strings.TrimSpace(agentName) == "" { return "agentName 不能为空" }

	a.mu.Lock()
	if a.running { a.mu.Unlock(); return "已有任务正在执行" }
	a.running = true
	a.mu.Unlock()

	go func() {
		defer func() { a.mu.Lock(); a.running = false; a.mu.Unlock() }()

		a.mu.Lock()
		eng := a.engine
		a.mu.Unlock()
		if eng == nil { a.openEngine(); a.mu.Lock(); eng = a.engine; a.mu.Unlock() }
		if eng == nil { return }

		taskWorkDir := workDir
		if taskWorkDir == "" {
			taskWorkDir = filepath.Join(a.workDir, "chats")
			os.MkdirAll(taskWorkDir, 0755)
		}

		// Build a synthetic single-agent team.
		tc := &team_engine.TeamConfig{
			Label: agentName,
			Leader: team_engine.TeamLeaderConfig{
				Role:        agentName,
				Description: "Single expert agent",
			},
			Roles: []string{agentName},
		}
		eng.SetTeam(tc)

		// Create session first
		sessionID := fmt.Sprintf("pod-%s", time.Now().Format("20060102-150405"))
		now := time.Now()
		session.SaveSessionMeta(a.sessionsDir, sessionID, session.SessionMeta{
			Title: goal, Kind: "pod-chat", Workspace: taskWorkDir,
			Agent: "expert:" + agentName, Status: "active", StartedAt: now, UpdatedAt: now,
		})

		mt, err := eng.CreateMasterTask(goal, taskWorkDir, sessionID)
		if err != nil { pod.Log("task", "create master: %v", err); return }

		// Skip Leader decomposition — create a single pre-decomposed task
		// so the expert agent executes directly without going through the
		// Leader→Worker→Verifier chain.
		preDecomposed := []team_engine.PlanTask{
			{
				Title:       goal,
				Description: goal,
				Role:        agentName,
				BatchID:     "execute",
				BatchLabel:  "Execution",
			},
		}

		ctx := context.Background()
		batches, err := eng.PlanAndRun(ctx, goal, taskWorkDir, mt.ID, preDecomposed...)
		pod.Log("task", "expert done: batches=%d err=%v", len(batches), err)
		eng.CompleteMasterTask(mt.ID)
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}()

	return ""
}

// CreateDirectTask creates a master task and stores the initial prompt as first message.
// deepThink: use deepseek-reasoner model with thinking chain.
// Returns the task ID, or empty string on error.
func (a *App) CreateDirectTask(goal, workDir, agent string, deepThink bool) string {
	if strings.TrimSpace(goal) == "" {
		return ""
	}
	if a.sessionStore == nil {
		pod.Log("task", "CreateDirectTask: session store not ready")
		return ""
	}
	taskWorkDir := workDir
	// Keep empty if user chose no workspace (don't default to a.workDir)

	// Create a session in ~/.whale/sessions/ (CLI-compatible)
	sessionID := fmt.Sprintf("pod-%s", time.Now().Format("20060102-150405"))

	// Save session metadata
	now := time.Now()
	if err := session.SaveSessionMeta(a.sessionsDir, sessionID, session.SessionMeta{
		Title:     goal,
		Kind:      "pod-chat",
		Workspace: taskWorkDir,
		Agent:     agent,
		Status:    "active",
		StartedAt: now,
		UpdatedAt: now,
	}); err != nil {
		pod.Log("task", "CreateDirectTask: save meta: %v", err)
		return ""
	}

	// NOTE: User message is written by DirectChat, not here, to avoid duplication

	runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	return sessionID
}

// DirectChatResult is returned from DirectChat as JSON.
type DirectChatResult struct {
	Reply       string `json:"reply"`
	Thinking    string `json:"thinking"`
	DurationMs  int64  `json:"durationMs"`
	NeedsAction bool   `json:"needsAction"`
	ActionType  string `json:"actionType,omitempty"` // "plan" or "agent"
}

// DirectChat sends a user message and returns the AI reply with thinking info.
// deepThink: use deepseek-reasoner model with thinking chain.
func (a *App) DirectChat(taskID, message string, deepThink bool) string {
	if taskID == "" || strings.TrimSpace(message) == "" {
		return ""
	}

	// Pod session (JSONL-based)
	if isPodSession(taskID) && a.sessionStore != nil {
		return a.directChatSession(taskID, message, deepThink)
	}

	// Legacy team_task fallback
	return a.directChatLegacy(taskID, message, deepThink)
}

// isPodSession checks if a task ID is a pod session (stored in ~/.whale/sessions/).
func isPodSession(id string) bool {
	return strings.HasPrefix(id, "pod-")
}

// directChatSession handles chat via JSONL sessions store (CLI-compatible).
func (a *App) directChatSession(sessionID, message string, deepThink bool) string {
	// Write user message
	_, err := a.sessionStore.Create(context.Background(), core.Message{
		SessionID: sessionID,
		Role:      core.RoleUser,
		Text:      message,
	})
	if err != nil {
		pod.Log("chat", "write user msg: %v", err)
	}

	// Read full conversation history
	msgs, _ := a.sessionStore.List(context.Background(), sessionID)
	var history []chatMsg
	for _, m := range msgs {
		from := "human"
		if m.Role == core.RoleAssistant {
			from = "agent"
		}
		history = append(history, chatMsg{From: from, Content: m.Text})
	}

	// Call LLM
	start := time.Now()
	aiReply, thinking := a.callLLM(history, deepThink)
	duration := time.Since(start).Milliseconds()

	// Store AI reply
	if aiReply != "" {
		_, err := a.sessionStore.Create(context.Background(), core.Message{
			SessionID:  sessionID,
			Role:       core.RoleAssistant,
			Text:       aiReply,
			Reasoning:  thinking,
			DurationMs: duration,
		})
		if err != nil {
			pod.Log("chat", "write ai reply: %v", err)
		}
		// Update session meta
		session.UpdateSessionMeta(a.sessionsDir, sessionID, func(m *session.SessionMeta) {
			m.TurnCount++
		})
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}

	needsAction, actionType := detectIntent(aiReply)
	result, _ := json.Marshal(DirectChatResult{
		Reply: aiReply, Thinking: thinking, DurationMs: duration,
		NeedsAction: needsAction, ActionType: actionType,
	})
	return string(result)
}

// directChatLegacy handles chat via team_engine (old MasterTask-based).
func (a *App) directChatLegacy(taskID, message string, deepThink bool) string {
	a.mu.Lock()
	eng := a.engine
	workDir := a.workDir
	a.mu.Unlock()
	if eng == nil || eng.Store == nil {
		return ""
	}

	mts, _ := eng.Store.ListMasterTasks()
	var taskWorkDir string
	for _, mt := range mts {
		if mt.ID == taskID {
			taskWorkDir = mt.WorkspacePath
			break
		}
	}
	if taskWorkDir == "" {
		taskWorkDir = filepath.Join(workDir, "chats")
	}

	a.storeMessage(taskWorkDir, taskID, "human", message)
	history := a.readTaskMessages(taskWorkDir, taskID)

	start := time.Now()
	aiReply, thinking := a.callLLM(history, deepThink)
	duration := time.Since(start).Milliseconds()

	if aiReply != "" {
		a.storeMessage(taskWorkDir, taskID, "agent", aiReply)
		eng.Store.UpdateMasterTaskStatus(taskID, "done")
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}

	needsAction, actionType := detectIntent(aiReply)
	result, _ := json.Marshal(DirectChatResult{
		Reply: aiReply, Thinking: thinking, DurationMs: duration,
		NeedsAction: needsAction, ActionType: actionType,
	})
	return string(result)
}

func (a *App) storeMessage(taskWorkDir, taskID, from, content string) {
	msgDir := filepath.Join(taskWorkDir, ".whale", "team_tasks", taskID, "messages")
	os.MkdirAll(msgDir, 0755)
	msgFile := filepath.Join(msgDir, fmt.Sprintf("%d_%s.md", time.Now().UnixNano(), from))
	os.WriteFile(msgFile, []byte(content), 0644)
}

type chatMsg struct {
	From    string `json:"from"`
	Content string `json:"content"`
}

func (a *App) readTaskMessages(taskWorkDir, taskID string) []chatMsg {
	msgDir := filepath.Join(taskWorkDir, ".whale", "team_tasks", taskID, "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		return nil
	}
	var msgs []chatMsg
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(msgDir, e.Name()))
		if err != nil {
			continue
		}
		from := "human"
		if strings.Contains(e.Name(), "_agent") {
			from = "agent"
		}
		msgs = append(msgs, chatMsg{From: from, Content: string(content)})
	}
	return msgs
}

func detectIntent(reply string) (needsAction bool, actionType string) {
	lower := strings.ToLower(reply)
	// Agent intent: writing code, modifying files, running commands
	agentKeywords := []string{
		"创建文件", "写入", "修改代码", "运行命令", "执行", "生成代码",
		"写一个", "实现", "部署", "编译", "安装",
	}
	for _, kw := range agentKeywords {
		if strings.Contains(lower, kw) {
			return true, "agent"
		}
	}
	// Plan intent: suggesting designs, architecture, strategies
	planKeywords := []string{
		"建议方案", "设计", "架构", "规划", "应该采用",
		"推荐使用", "考虑以下", "步骤如下", "流程如下",
	}
	for _, kw := range planKeywords {
		if strings.Contains(lower, kw) {
			return true, "plan"
		}
	}
	return false, ""
}

func resolveAPIKey() string {
	if v := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); v != "" {
		return v
	}
	dataDir := store.DefaultDataDir()
	credPath := filepath.Join(dataDir, "credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return ""
	}
	var creds struct {
		DeepSeekAPIKey string `json:"deepseek_api_key"`
	}
	if json.Unmarshal(data, &creds) != nil {
		return ""
	}
	return strings.TrimSpace(creds.DeepSeekAPIKey)
}

func (a *App) callLLM(messages []chatMsg, deepThink bool) (reply string, thinking string) {
	apiKey := resolveAPIKey()
	if apiKey == "" {
		return "未找到 DeepSeek API Key。请设置 DEEPSEEK_API_KEY 环境变量或在 ~/.whale/credentials.json 中配置", ""
	}

	model := "deepseek-chat"
	if deepThink {
		model = "deepseek-reasoner"
	}

	systemPrompt := "你是 Whale Pod，一个强大的 AI 编程助手。回复时遵循以下规则：\n\n" +
		"1. 简洁直接地回答问题\n" +
		"2. 如果需要执行具体操作（创建文件、修改代码、运行命令等），用清晰的编号列表描述每个操作步骤\n" +
		"3. 在列出操作后，明确询问用户\"需要我执行以上操作吗？\"，等待用户确认后再行动\n" +
		"4. 如果只是建议或讨论，只需给出方案说明，不需要列出操作步骤"

	msgs := []map[string]string{
		{"role": "system", "content": systemPrompt},
	}
	for _, m := range messages {
		role := "user"
		if m.From == "agent" {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": m.Content})
	}

	body := map[string]interface{}{
		"model":       model,
		"messages":    msgs,
		"max_tokens":  4096,
		"temperature": 0.7,
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.deepseek.com/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Sprintf("请求失败: %v", err), ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Sprintf("请求超时: %v", err), ""
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Sprintf("解析失败: %v", err), ""
	}
	if result.Error.Message != "" {
		return fmt.Sprintf("API 错误: %s", result.Error.Message), ""
	}
	if len(result.Choices) == 0 {
		return "未收到回复", ""
	}
	return result.Choices[0].Message.Content, result.Choices[0].Message.ReasoningContent
}

// GetMasterTasks returns sessions from ~/.whale/sessions/.
// All conversations (direct chat, expert, team) are sessions.
// Team engine tasks are linked via SessionID.
func (a *App) GetMasterTasks() []pod.MasterTaskJSON {
	var result []pod.MasterTaskJSON

	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()

	if a.sessionStore != nil {
		sessions, err := session.ListSessions(a.sessionsDir, 50)
		if err == nil {
			for _, s := range sessions {
				if s.Meta.Kind == "subagent" {
					continue
				}
				goal := s.Meta.Title
				if goal == "" {
					goal = s.Conversation
				}
				wp := s.Meta.Workspace
				status := "done"
				if s.Meta.Status == "active" { status = "running" }

				taskCount := 0
				doneCount := 0
				if eng != nil && eng.Store != nil {
					mts, _ := eng.Store.ListMasterTasksBySession(s.ID)
					for _, mt := range mts {
						tasks, _ := eng.Store.ListTasksByMasterTask(mt.ID)
						taskCount += len(tasks)
						for _, t := range tasks {
							if t.State == team_engine.TaskStateDone || t.State == team_engine.TaskStateFailed { doneCount++ }
						}
					}
					if taskCount > 0 && doneCount < taskCount { status = "running" }
				}

				result = append(result, pod.MasterTaskJSON{
					ID: s.ID, Goal: goal,
					Agent: s.Meta.Agent,
					WorkspacePath: wp, WorkspaceLabel: filepath.Base(wp),
					Status: status, CreatedAt: s.Meta.StartedAt.Format(time.RFC3339),
					TaskCount: taskCount, DoneCount: doneCount,
					WorkspaceOnline: true,
				})
			}
		}
	}

	return result
}

// GetSubtasksBySession returns subtasks for all master tasks linked to a session.
func (a *App) GetSubtasksBySession(sessionID string) []pod.SubtaskJSON {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil { return nil }

	mts, _ := eng.Store.ListMasterTasksBySession(sessionID)
	if len(mts) == 0 { return nil }

	var allTasks []*team_engine.Task
	for _, mt := range mts {
		tasks, _ := eng.Store.ListTasksByMasterTask(mt.ID)
		allTasks = append(allTasks, tasks...)
	}
	if len(allTasks) == 0 { return nil }

	nodeMap := make(map[string]*pod.SubtaskJSON)
	taskList := make([]*pod.SubtaskJSON, 0, len(allTasks))
	for _, t := range allTasks {
		sj := &pod.SubtaskJSON{
			ID: t.ID, Title: t.Title, Description: t.Description,
			Output: t.Output, Role: string(t.Role), State: string(t.State),
			Progress: pod.GetProgress(t.State), CreatedAt: t.CreatedAt,
			ParentIDs: t.ParentIDs, BatchID: t.BatchID,
			RetryCount: t.RetryCount, MaxRetries: t.MaxRetries,
		}
		nodeMap[t.ID] = sj; taskList = append(taskList, sj)
	}
	for _, child := range taskList {
		for _, pid := range child.ParentIDs {
			if parent, ok := nodeMap[pid]; ok { parent.Children = append(parent.Children, *child) }
		}
	}
	for _, sj := range taskList {
		if len(sj.Children) > 0 {
			sum := 0; for _, c := range sj.Children { sum += c.Progress }
			sj.Progress = sum / len(sj.Children)
		}
	}
	var roots []pod.SubtaskJSON
	for _, sj := range taskList {
		hasParent := false
		for _, pid := range sj.ParentIDs {
			if _, ok := nodeMap[pid]; ok { hasParent = true; break }
		}
		if !hasParent { roots = append(roots, *sj) }
	}
	if len(allTasks) > 0 {
		leader := pod.SubtaskJSON{ID: "__leader__", Title: "📋 任务规划", Role: "teamleader", State: "done", Progress: 100}
		return append([]pod.SubtaskJSON{leader}, roots...)
	}
	return roots
}

// GetSubtasks returns subtasks for a master task.
func (a *App) GetSubtasks(mtID string) []pod.SubtaskJSON {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil { return nil }

	tasks, _ := eng.Store.ListTasksByMasterTask(mtID)
	if len(tasks) == 0 { return nil }

	nodeMap := make(map[string]*pod.SubtaskJSON)
	taskList := make([]*pod.SubtaskJSON, 0, len(tasks))
	for _, t := range tasks {
		sj := &pod.SubtaskJSON{
			ID: t.ID, Title: t.Title, Description: t.Description,
			Output: t.Output, Role: string(t.Role), State: string(t.State),
			Progress: pod.GetProgress(t.State), CreatedAt: t.CreatedAt,
			ParentIDs: t.ParentIDs, BatchID: t.BatchID,
			RetryCount: t.RetryCount, MaxRetries: t.MaxRetries,
		}
		nodeMap[t.ID] = sj; taskList = append(taskList, sj)
	}
	for _, child := range taskList {
		for _, pid := range child.ParentIDs {
			if parent, ok := nodeMap[pid]; ok { parent.Children = append(parent.Children, *child) }
		}
	}
	for _, sj := range taskList {
		if len(sj.Children) > 0 {
			sum := 0; for _, c := range sj.Children { sum += c.Progress }
			sj.Progress = sum / len(sj.Children)
		}
	}
	var roots []pod.SubtaskJSON
	for _, sj := range taskList {
		hasParent := false
		for _, pid := range sj.ParentIDs {
			if _, ok := nodeMap[pid]; ok { hasParent = true; break }
		}
		if !hasParent { roots = append(roots, *sj) }
	}
	if len(tasks) > 0 {
		leader := pod.SubtaskJSON{ID: "__leader__", Title: "📋 任务规划", Role: "teamleader", State: "done", Progress: 100}
		return append([]pod.SubtaskJSON{leader}, roots...)
	}
	return roots
}

// GetAgentDialogue returns worker/verifier round logs.
func (a *App) GetAgentDialogue(taskID string) []pod.AgentDialogueJSON {
	return readDialogue(a.workDir, taskID)
}

// GetLeaderPlan returns leader decompose/review logs.
func (a *App) GetLeaderPlan() []pod.AgentDialogueJSON { return readLeaderPlan(a.workDir) }

// SendFeedback writes a human message to the task's messages/ directory.
func (a *App) SendFeedback(taskID, message string) string {
	msgDir := filepath.Join(a.workDir, ".whale", "team_tasks", taskID, "messages")
	os.MkdirAll(msgDir, 0755)
	msgFile := filepath.Join(msgDir, fmt.Sprintf("%d.md", time.Now().UnixNano()))
	os.WriteFile(msgFile, []byte(message), 0644)
	return ""
}

// RunSubtask resets a task to pending for re-execution.
func (a *App) RunSubtask(taskID string) string { updateMetaState(a.workDir, taskID, "pending"); return "" }

// CancelSubtask suspends a task.
func (a *App) CancelSubtask(taskID string) string { updateMetaState(a.workDir, taskID, "suspended"); return "" }

func (a *App) DeleteSession(sessionID string) string {
	// Delete session files
	sanitized := core.SanitizeSessionID(sessionID)
	for _, suffix := range []string{".jsonl", ".meta.json", ".approvals.json", ".mode.json", ".goal.json", ".todo.json"} {
		os.Remove(filepath.Join(a.sessionsDir, sanitized+suffix))
	}

	// Delete associated team engine tasks
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng != nil && eng.Store != nil {
		mts, _ := eng.Store.ListMasterTasksBySession(sessionID)
		for _, mt := range mts {
			eng.DeleteMasterTaskAndChildren(mt.ID)
		}
	}

	runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	return ""
}

// RenameMasterTask renames a master task's goal/title.
func (a *App) RenameMasterTask(taskID, newGoal string) string {
	if strings.TrimSpace(newGoal) == "" {
		return "goal 不能为空"
	}

	// Pod session: update SessionMeta.Title
	if isPodSession(taskID) {
		_, err := session.UpdateSessionMeta(a.sessionsDir, taskID, func(m *session.SessionMeta) {
			m.Title = newGoal
		})
		if err != nil {
			return err.Error()
		}
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
		return ""
	}

	// Legacy team_task
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil {
		return "engine not ready"
	}
	mts, _ := eng.Store.ListMasterTasks()
	var found bool
	for _, mt := range mts {
		if mt.ID == taskID {
			found = true
			break
		}
	}
	if !found {
		return "task not found"
	}
	metaPath := filepath.Join(a.workDir, ".whale", "team_tasks", "masters", taskID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return err.Error()
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err != nil {
		return err.Error()
	}
	meta["title"] = newGoal
	newData, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, newData, 0644); err != nil {
		return err.Error()
	}
	goalPath := filepath.Join(a.workDir, ".whale", "team_tasks", "masters", taskID, "goal.md")
	goalContent := fmt.Sprintf("# %s\n\n**角色**: teamleader\n\n## 任务描述\n\n%s\n\n## 产出\n\n```\n", newGoal, newGoal)
	os.WriteFile(goalPath, []byte(goalContent), 0644)
	if mt, _ := eng.Store.GetMasterTask(taskID); mt != nil {
		mt.Goal = newGoal
	}
	runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	return ""
}

// OpenTerminal launches whale TUI in the working directory.
func (a *App) OpenTerminal() string { pod.OpenTerminal(a.workDir); return "" }

// WindowMinimize minimizes the window.
func (a *App) WindowMinimize() { runtime.WindowMinimise(a.ctx) }

// WindowMaximize toggles maximized state.
func (a *App) WindowMaximize() { runtime.WindowToggleMaximise(a.ctx) }

// WindowClose closes the window.
func (a *App) WindowClose() { runtime.Quit(a.ctx) }

// GetChatMessages returns human-agent chat history.
func (a *App) GetChatMessages(taskID string) []pod.ChatMessageJSON {
	// Pod session: read from JSONL
	if isPodSession(taskID) && a.sessionStore != nil {
		msgs, err := a.sessionStore.List(context.Background(), taskID)
		if err != nil {
			return nil
		}
		var result []pod.ChatMessageJSON
		for _, m := range msgs {
			from := "human"
			if m.Role == core.RoleAssistant {
				from = "agent"
			}
			result = append(result, pod.ChatMessageJSON{
				Time:       m.CreatedAt.Format(time.RFC3339),
				From:       from,
				Content:    m.Text,
				Thinking:   m.Reasoning,
				DurationMs: m.DurationMs,
			})
		}

	return result
	}

	// Legacy team_task fallback
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil {
		return readChatFallback(a.workDir, taskID)
	}
	mts, _ := eng.Store.ListMasterTasks()
	for _, mt := range mts {
		if mt.ID == taskID && mt.WorkspacePath != "" {
			return readChat(mt.WorkspacePath, taskID)
		}
	}
	return readChatFallback(a.workDir, taskID)
}

func readChatFallback(workDir, taskID string) []pod.ChatMessageJSON {
	// Try task's own workspace first, then chats/ fallback
	msgs := readChat(workDir, taskID)
	if len(msgs) > 0 {
		return msgs
	}
	return readChat(filepath.Join(workDir, "chats"), taskID)
}

// ---------------------------------------------------------------------------
// Team Chat — @角色对话路由
// ---------------------------------------------------------------------------

// TeamChatMessage is a single message in the team chat conversation.
type TeamChatMessage struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// SendTeamChat sends a message in the master task's team chat.
// If targetRole is empty, the message is broadcast to the Leader.
// If targetRole is set (e.g. "后端工程师"), the message is routed
// to the corresponding task's inbox.
func (a *App) SendTeamChat(masterTaskID, message, targetRole string) string {
	if strings.TrimSpace(message) == "" {
		return "消息不能为空"
	}

	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Whiteboard == nil {
		return "engine not ready"
	}

	from := "human"
	to := targetRole
	if to == "" {
		to = "leader"
	}

	if err := eng.Whiteboard.WriteChatMessage(masterTaskID, from, to, message); err != nil {
		return err.Error()
	}

	if to != "" && to != "leader" {
		tasks, _ := eng.Store.ListTasksByMasterTask(masterTaskID)
		for _, t := range tasks {
			if string(t.Role) == to || t.Title == to {
				msg := team_engine.NewMessage(t.ID, "human", message, "")
				if err := eng.Whiteboard.WriteMessage(t.ID, msg); err != nil {
					pod.Log("chat", "write inbox for task %s: %v", t.ID, err)
				}
				break
			}
		}
	}

	runtime.EventsEmit(a.ctx, "team-chat-update", masterTaskID)
	return ""
}

// GetTeamChat returns the team chat messages for a master task.
func (a *App) GetTeamChat(masterTaskID string) []TeamChatMessage {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Whiteboard == nil {
		return nil
	}

	messages, err := eng.Whiteboard.ReadChatMessages(masterTaskID)
	if err != nil {
		return nil
	}

	var result []TeamChatMessage
	for _, m := range messages {
		result = append(result, TeamChatMessage{
			From:      m.From,
			To:        m.To,
			Content:   m.Content,
			Timestamp: m.Timestamp,
		})
	}
	return result
}

// ---------------------------------------------------------------------------
// Task Confirmation — 人工确认机制
// ---------------------------------------------------------------------------

// TaskConfirmation represents a confirmation request from an agent.
type TaskConfirmation struct {
	TaskID      string `json:"task_id"`
	TaskTitle   string `json:"task_title"`
	Role        string `json:"role"`
	Content     string `json:"content"`
	State       string `json:"state"`
}

// GetTaskConfirmation returns the confirmation request for a task, if any.
func (a *App) GetTaskConfirmation(taskID string) *TaskConfirmation {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Whiteboard == nil {
		return nil
	}

	if !eng.Whiteboard.HasConfirmation(taskID) {
		return nil
	}

	content, err := eng.Whiteboard.ReadConfirmation(taskID)
	if err != nil || content == "" {
		return nil
	}

	t, _ := eng.Store.GetTask(taskID)
	role := ""
	title := ""
	state := ""
	if t != nil {
		role = string(t.Role)
		title = t.Title
		state = string(t.State)
	}

	return &TaskConfirmation{
		TaskID:    taskID,
		TaskTitle: title,
		Role:      role,
		Content:   content,
		State:     state,
	}
}

// ConfirmTask handles human confirmation for a task in pending_confirmation state.
// approved=true → transition to producing; approved=false → back to suspended.
func (a *App) ConfirmTask(taskID string, approved bool, feedback string) string {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil {
		return "engine not ready"
	}

	t, err := eng.Store.GetTask(taskID)
	if err != nil || t == nil {
		return "task not found"
	}

	eng.Whiteboard.ClearConfirmation(taskID)

	if approved {
		if err := eng.Store.TransitionState(taskID, team_engine.TaskStateProducing, "human-approved", feedback); err != nil {
			return err.Error()
		}
		pod.Log("confirm", "task %s approved → producing", taskID)
	} else {
		if err := eng.Store.TransitionState(taskID, team_engine.TaskStateSuspended, "human-rejected", feedback); err != nil {
			return err.Error()
		}
		if feedback != "" {
			msg := team_engine.NewMessage(taskID, "human", "确认被拒绝。反馈: "+feedback, "")
			eng.Whiteboard.WriteMessage(taskID, msg)
		}
		pod.Log("confirm", "task %s rejected → suspended", taskID)
	}

	runtime.EventsEmit(a.ctx, "task-event", team_engine.TaskEvent{
		Type:     team_engine.EventStateChanged,
		TaskID:   taskID,
		NewState: string(t.State),
	})
	return ""
}

// GetConfirmationsForMaster returns all pending confirmations for tasks under a master task.
func (a *App) GetConfirmationsForMaster(masterTaskID string) []TaskConfirmation {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil {
		return nil
	}

	tasks, _ := eng.Store.ListTasksByMasterTask(masterTaskID)
	var result []TaskConfirmation
	for _, t := range tasks {
		if eng.Whiteboard.HasConfirmation(t.ID) {
			content, _ := eng.Whiteboard.ReadConfirmation(t.ID)
			if content != "" {
				result = append(result, TaskConfirmation{
					TaskID:    t.ID,
					TaskTitle: t.Title,
					Role:      string(t.Role),
					Content:   content,
					State:     string(t.State),
				})
			}
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func readDialogue(workDir, taskID string) []pod.AgentDialogueJSON {
	tasksDir := filepath.Join(workDir, ".whale", "team_tasks")
	taskDir := filepath.Join(tasksDir, taskID)
	logsDir := filepath.Join(tasksDir, "logs", "tasks", taskID)

	roleName := "worker"
	if data, err := os.ReadFile(filepath.Join(taskDir, "meta.json")); err == nil {
		var meta struct{ Role string `json:"role"` }
		if json.Unmarshal(data, &meta) == nil && meta.Role != "" { roleName = meta.Role }
	}

	var dialogue []pod.AgentDialogueJSON
	if input, err := os.ReadFile(filepath.Join(taskDir, "input.md")); err == nil && len(input) > 0 {
		dialogue = append(dialogue, pod.AgentDialogueJSON{Role: "input", Content: string(input)})
	}
	for round := 1; round <= 99; round++ {
		wc, _ := os.ReadFile(filepath.Join(logsDir, fmt.Sprintf("worker_%03d.md", round)))
		vc, _ := os.ReadFile(filepath.Join(logsDir, fmt.Sprintf("verifier_%03d.md", round)))
		if len(wc) > 0 { dialogue = append(dialogue, pod.AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", roleName, round), Content: string(wc)}) }
		if len(vc) > 0 { dialogue = append(dialogue, pod.AgentDialogueJSON{Role: fmt.Sprintf("审查 (round %d)", round), Content: string(vc)}) }
		if len(wc) == 0 && len(vc) == 0 { break }
	}
	return dialogue
}

func readLeaderPlan(workDir string) []pod.AgentDialogueJSON {
	leaderDir := filepath.Join(workDir, ".whale", "team_tasks", "logs", "leader")
	var dialogue []pod.AgentDialogueJSON
	for round := 1; round <= 9; round++ {
		for _, prefix := range []string{"decompose", "review"} {
			data, _ := os.ReadFile(filepath.Join(leaderDir, fmt.Sprintf("%s_%03d.md", prefix, round)))
			if len(data) > 0 {
				label := "📋 目标分解"; if prefix == "review" { label = "📋 执行审查" }
				dialogue = append(dialogue, pod.AgentDialogueJSON{Role: fmt.Sprintf("%s (round %d)", label, round), Content: string(data)})
			}
		}
	}
	return dialogue
}

func readChat(workDir, taskID string) []pod.ChatMessageJSON {
	msgDir := filepath.Join(workDir, ".whale", "team_tasks", taskID, "messages")
	entries, _ := os.ReadDir(msgDir)
	// Sort by name (timestamp prefix)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var msgs []pod.ChatMessageJSON
	var lastHumanTime int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") { continue }
		content, _ := os.ReadFile(filepath.Join(msgDir, e.Name()))
		from := "human"
		if strings.Contains(e.Name(), "_agent") {
			from = "agent"
		}
		tsStr := strings.TrimSuffix(strings.Split(e.Name(), "_")[0], ".md")
		var ts int64
		fmt.Sscanf(tsStr, "%d", &ts)
		msg := pod.ChatMessageJSON{Time: tsStr, From: from, Content: string(content)}
		if from == "agent" && lastHumanTime > 0 && ts > lastHumanTime {
			msg.DurationMs = (ts - lastHumanTime) / 1_000_000 // ns → ms
		}
		if from == "human" {
			lastHumanTime = ts
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func updateMetaState(workDir, taskID, state string) {
	metaPath := filepath.Join(workDir, ".whale", "team_tasks", taskID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil { return }
	var meta map[string]interface{}
	if json.Unmarshal(data, &meta) != nil { return }
	meta["state"] = state
	newData, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, newData, 0644)
}
