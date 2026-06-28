package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/mcp"
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
	podDataDir   string
	teamsDir     string
	expertsDir   string
	sessionsDir  string
	sessionStore *store.JSONLStore
	engine       *team_engine.TeamEngine
	mu           sync.Mutex
	running      bool

	cachedAPIKey    string
	agentDefCache   map[string]tasks.AgentDefinition
	agentDefCacheMu sync.RWMutex
	expertRegistry  *team_engine.ExpertRegistry

	abortMu     sync.Mutex
	abortCancels map[string]context.CancelFunc

	mcpManager *mcp.Manager
}

func NewApp() *App {
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	teamsDir := filepath.Join(exeDir, "teams")
	expertsDir := filepath.Join(exeDir, "experts")
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if fi, err := os.Stat(filepath.Join(dir, "bin", "teams")); err == nil && fi.IsDir() {
				binDir := filepath.Join(dir, "bin")
				teamsDir = filepath.Join(binDir, "teams")
				expertsDir = filepath.Join(binDir, "experts")
				exeDir = binDir
				break
			}
		}
	}
	return &App{workDir: exeDir, teamsDir: teamsDir, expertsDir: expertsDir}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Initialize global sessions store (CLI-compatible)
	dataDir := store.DefaultDataDir()
	a.podDataDir = filepath.Join(dataDir, "pod")
	os.MkdirAll(a.podDataDir, 0755)
	a.sessionsDir = store.DefaultSessionsDir(dataDir)
	sessStore, err := store.NewJSONLStore(a.sessionsDir)
	if err != nil {
		pod.Log("startup", "session store: %v", err)
	} else {
		a.sessionStore = sessStore
	}

	// Cache API key (avoids repeated disk reads on every LLM call).
	a.cachedAPIKey = resolveAPIKey()

	// Cache agent definitions (avoids repeated filesystem scans).
	a.cacheAgentDefs()

	if reg, err := team_engine.LoadAllExperts(a.expertsDir); err != nil {
		pod.Log("startup", "load experts: %v", err)
	} else {
		a.expertRegistry = reg
		pod.Log("startup", "loaded %d expert files", len(reg.Files()))
	}

	pod.Log("startup", "whale-pod workDir=%s teamsDir=%s expertsDir=%s sessionsDir=%s", a.workDir, a.teamsDir, a.expertsDir, a.sessionsDir)
	a.openEngine()
	a.initMCP()
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
	data, err := os.ReadFile(filepath.Join(a.podDataDir, "summoned.json"))
	if err != nil {
		return []pod.SummonedItemJSON{}
	}
	var items []pod.SummonedItemJSON
	if err := json.Unmarshal(data, &items); err != nil {
		pod.Log("summoned", "unmarshal error: %v", err)
		return []pod.SummonedItemJSON{}
	}
	return items
}

func (a *App) SaveSummonedItems(items []pod.SummonedItemJSON) {
	data, err := json.Marshal(items)
	if err != nil {
		pod.Log("summoned", "marshal error: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(a.podDataDir, "summoned.json"), data, 0644); err != nil {
		pod.Log("summoned", "write error: %v", err)
	}
}

// agentNameResolver implements team_engine.AgentInfoProvider by scanning
// the agents directory and mapping agent file names to their role titles.
type agentNameResolver struct {
	roleMap      map[string]string
	capMap       map[string]string
	outputSpecMap map[string]string
}

func (r *agentNameResolver) AgentRole(name string) string         { return r.roleMap[name] }
func (r *agentNameResolver) AgentDesc(name string) string          { return "" }
func (r *agentNameResolver) AgentCapabilities(name string) string  { return r.capMap[name] }
func (r *agentNameResolver) AgentOutputSpec(name string) string    { return r.outputSpecMap[name] }

// cacheAgentDefs scans the agents directory once and caches all definitions in memory.
// This avoids repeated filesystem scans on every API call.
func (a *App) cacheAgentDefs() {
	agentsDir := filepath.Join(filepath.Dir(a.teamsDir), "agents")
	m := make(map[string]tasks.AgentDefinition)

	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		pod.Log("agents", "cacheAgentDefs read %s: %v", agentsDir, err)
		return
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
				pod.Log("agents", "read %s: %v", path, err)
				continue
			}
			def, ok, _ := tasks.ParseMarkdownAgentDefinition(string(data), se.Name(), "")
			if !ok {
				continue
			}
			m[def.Name] = def
		}
	}

	a.agentDefCacheMu.Lock()
	a.agentDefCache = m
	a.agentDefCacheMu.Unlock()
	pod.Log("agents", "cached %d agent definitions", len(m))
}

// loadAgentNameResolver builds a resolver from the cached agent definitions.
func (a *App) loadAgentNameResolver() *agentNameResolver {
	a.agentDefCacheMu.RLock()
	cache := a.agentDefCache
	a.agentDefCacheMu.RUnlock()

	roleMap := make(map[string]string, len(cache))
	capMap := make(map[string]string, len(cache))
	outputSpecMap := make(map[string]string, len(cache))
	for name, def := range cache {
		roleMap[name] = def.Role
		capMap[name] = team_engine.ExtractSection(def.Prompt, "核心能力")
		outputSpecMap[name] = team_engine.ExtractSection(def.Prompt, "输出规范")
	}
	if a.expertRegistry != nil {
		for _, ef := range a.expertRegistry.Files() {
			for _, exp := range ef.Experts {
				if exp.Agent != "" {
					if exp.Name != "" {
						roleMap[exp.Agent] = exp.Name
					}
				}
			}
		}
	}
	return &agentNameResolver{roleMap: roleMap, capMap: capMap, outputSpecMap: outputSpecMap}
}

// loadAgentSystemPrompt returns the system prompt for a given agent identifier.
// Agent format: "" or "whale:" = Whale, "expert:name" = expert, "team:name" = team.
func (a *App) loadAgentSystemPrompt(agentID string) string {
	defaultPrompt := "你是 Whale Pod，一个强大的 AI 编程助手。回复时遵循以下规则：\n\n" +
		"1. 简洁直接地回答问题\n" +
		"2. 如果需要执行具体操作（创建文件、修改代码、运行命令等），用清晰的编号列表描述每个操作步骤\n" +
		"3. 在列出操作后，明确询问用户\"需要我执行以上操作吗？\"，等待用户确认后再行动\n" +
		"4. 如果只是建议或讨论，只需给出方案说明，不需要列出操作步骤"

	if agentID == "" || agentID == "whale:" {
		return defaultPrompt
	}

	parts := strings.SplitN(agentID, ":", 2)
	if len(parts) != 2 {
		return defaultPrompt
	}
	agentType, agentName := parts[0], parts[1]

	switch agentType {
	case "expert":
		return a.loadExpertPrompt(agentName)
	case "team":
		return a.loadTeamLeaderPrompt(agentName)
	}
	return defaultPrompt
}

// loadExpertPrompt loads an expert agent's markdown definition and returns
// a system prompt based on its role and personality. Uses cached definitions.
func (a *App) loadExpertPrompt(agentName string) string {
	a.agentDefCacheMu.RLock()
	def, ok := a.agentDefCache[agentName]
	a.agentDefCacheMu.RUnlock()

	if !ok {
		pod.Log("agents", "expert '%s' not found in cache", agentName)
		return "你是 Whale Pod 的专家助手。请基于你的专业知识回答用户的问题。"
	}

	role := def.Role
	if role == "" {
		role = def.Name
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("你是 %s，%s。\n\n%s", role, def.Description, def.Prompt))
	if def.WhenToUse != "" {
		sb.WriteString(fmt.Sprintf("\n\n你的专业范围：%s", def.WhenToUse))
	}
	sb.WriteString("\n\n决策规则：\n")
	sb.WriteString("- 如果用户的问题不在你的专业范围内，你作为通用助理直接回答，不要输出 ACTION\n")
	sb.WriteString(fmt.Sprintf("- 如果用户的问题在你的专业范围内且需要执行具体操作，在回复末尾输出 <!-- ACTION:{\"mode\":\"agent\",\"role\":\"%s\",\"goal\":\"任务目标\"} -->\n", role))
	sb.WriteString("ACTION 注释必须放在回复最末尾，用户不会看到这段内容。\n")
	return sb.String()
}

// loadTeamLeaderPrompt loads a team's leader config and returns
// a system prompt for the team leader.
func (a *App) loadTeamLeaderPrompt(teamName string) string {
	tc, err := team_engine.FindTeam(a.teamsDir, teamName)
	if err != nil {
		pod.Log("agent", "team '%s' not found: %v", teamName, err)
		return fmt.Sprintf("你是 %s 团队的 Leader。请协调团队成员完成用户的编程任务。", teamName)
	}
	resolver := a.loadAgentNameResolver()
	tc.ResolveRoles(resolver, a.expertRegistry)

	role := tc.Leader.Role
	prompt := tc.Leader.Prompt
	if prompt == "" {
		prompt = tc.Leader.Description
	}
	// Try team-local agent MD first
	if localPrompt, ok := tc.LoadTeamAgentPrompt(role); ok {
		pod.Log("agent", "using team-local agent for leader: %s", role)
		return localPrompt
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("你是 %s。%s", role, prompt))
	if len(tc.Leader.Rules) > 0 {
		sb.WriteString("\n\n团队规则：\n")
		for i, r := range tc.Leader.Rules {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r))
		}
	}
	if len(tc.Roles) > 0 {
		sb.WriteString("\n团队成员：")
		var names []string
		for _, r := range tc.Roles {
			if label := tc.RoleDisplayName(r); label != "" {
				names = append(names, label)
			} else {
				names = append(names, r)
			}
		}
		sb.WriteString(strings.Join(names, "、"))

		sb.WriteString("\n\n## 团队成员能力清单\n\n")
		sb.WriteString("| 专家 | 角色 | 核心能力 | 输出规范 |\n")
		sb.WriteString("|------|------|---------|---------|\n")
		for _, r := range tc.Roles {
			title := tc.RoleDisplayName(r)
			agentName := tc.RoleAgentName(r)
			capabilities := tc.RoleCapabilities[r]
			outputSpec := tc.RoleOutputSpecs[r]
			if capabilities == "" {
				capabilities = "—"
			}
			if outputSpec == "" {
				outputSpec = "—"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", agentName, title, capabilities, outputSpec))
		}
	}
	if len(tc.Capabilities) > 0 {
		sb.WriteString("\n\n团队能力范围：\n")
		for _, c := range tc.Capabilities {
			sb.WriteString(fmt.Sprintf("- %s\n", c))
		}
	}

	sb.WriteString("\n\n## 协作铁律\n\n")
	sb.WriteString("1. 你是编排者，不是执行者——禁止自己写代码、写文档、做专业分析\n")
	sb.WriteString("2. 每个专业产出必须由对应角色输出后再采信，你只做编排与汇编\n")
	sb.WriteString("3. 未完成前序任务不可跳到后续任务\n")
	sb.WriteString("4. 验证不通过的任务必须回退重做，不可跳过\n")
	sb.WriteString("5. 禁止自己代写任何团队成员的专业产出\n")

	sb.WriteString("\n决策规则：\n")
	sb.WriteString("- 如果用户的问题不在团队能力范围内，你作为通用助理直接回答，不要输出 ACTION\n")
	sb.WriteString("- 如果用户的问题在能力范围内，且只需要单一角色处理，在回复末尾输出 <!-- ACTION:{\"mode\":\"agent\",\"role\":\"角色名\",\"goal\":\"任务目标\"} -->\n")
	sb.WriteString("- 如果用户的问题在能力范围内，且需要多角色协作，在回复末尾输出 <!-- ACTION:{\"mode\":\"team\",\"goal\":\"任务目标\"} -->\n")
	sb.WriteString("ACTION 注释必须放在回复最末尾，用户不会看到这段内容。\n")
	return sb.String()
}

func (a *App) ListTeamDetails() []pod.TeamDetailJSON {
	teamsDir := a.teamsDir
	entries, err := os.ReadDir(teamsDir)
	if err != nil {
		pod.Log("teams", "ListTeamDetails read %s: %v", teamsDir, err)
		return []pod.TeamDetailJSON{}
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
		tc.ResolveRoles(resolver, a.expertRegistry)
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
		return []pod.AgentInfoJSON{}
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

		defer func() {
			if r := recover(); r != nil {
				pod.Log("task", "panic in StartTask: %v", r)
			}
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()

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
		sessionID := newSessionID()
		now := time.Now()
		if err := session.SaveSessionMeta(a.sessionsDir, sessionID, session.SessionMeta{
			Title: goal, Workspace: taskWorkDir,
			Agent: "team:" + teamName, Status: "active", StartedAt: now, UpdatedAt: now,
		}); err != nil {
			pod.Log("task", "save session meta: %v", err)
		}
		session.EnsureSessionFile(a.sessionsDir, sessionID)

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

	go func() {
		a.mu.Lock()
		if a.running { a.mu.Unlock(); return }
		a.running = true
		a.mu.Unlock()

		defer func() {
			if r := recover(); r != nil {
				pod.Log("task", "panic in StartExpertTask: %v", r)
			}
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()

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
		sessionID := newSessionID()
		now := time.Now()
		if err := session.SaveSessionMeta(a.sessionsDir, sessionID, session.SessionMeta{
			Title: goal, Workspace: taskWorkDir,
			Agent: "expert:" + agentName, Status: "active", StartedAt: now, UpdatedAt: now,
		}); err != nil {
			pod.Log("task", "save session meta: %v", err)
		}
		session.EnsureSessionFile(a.sessionsDir, sessionID)

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

// StartTaskInSession starts a team task within an existing session.
func (a *App) StartTaskInSession(sessionID, goal, teamName, workDir string) string {
	if strings.TrimSpace(goal) == "" || strings.TrimSpace(sessionID) == "" {
		return "goal 和 sessionID 不能为空"
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				pod.Log("task", "panic in StartTaskInSession: %v", r)
			}
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()

		a.mu.Lock()
		if a.running { a.mu.Unlock(); return }
		a.running = true
		a.mu.Unlock()

		a.mu.Lock()
		eng := a.engine
		a.mu.Unlock()
		if eng == nil { a.openEngine(); a.mu.Lock(); eng = a.engine; a.mu.Unlock() }
		if eng == nil { return }

		taskWorkDir := workDir
		if taskWorkDir == "" {
			meta, _ := session.LoadSessionMeta(a.sessionsDir, sessionID)
			if meta.Workspace != "" {
				taskWorkDir = meta.Workspace
			} else {
				taskWorkDir = filepath.Join(a.workDir, "chats")
				os.MkdirAll(taskWorkDir, 0755)
			}
		}

		if teamName != "" {
			tc, err := team_engine.FindTeam(a.teamsDir, teamName)
			if err == nil { eng.SetTeam(tc) }
		}

		session.EnsureSessionFile(a.sessionsDir, sessionID)

		mt, err := eng.CreateMasterTask(goal, taskWorkDir, sessionID)
		if err != nil { pod.Log("task", "create master: %v", err); return }

		ctx := context.Background()
		batches, err := eng.PlanAndRun(ctx, goal, taskWorkDir, mt.ID)
		pod.Log("task", "done: batches=%d err=%v", len(batches), err)
		eng.CompleteMasterTask(mt.ID)
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}()

	return sessionID
}

// StartExpertTaskInSession starts a single-agent task within an existing session.
func (a *App) StartExpertTaskInSession(sessionID, goal, agentName, workDir string) string {
	if strings.TrimSpace(goal) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(agentName) == "" {
		return "参数不能为空"
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				pod.Log("task", "panic in StartExpertTaskInSession: %v", r)
			}
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()

		a.mu.Lock()
		if a.running { a.mu.Unlock(); return }
		a.running = true
		a.mu.Unlock()

		a.mu.Lock()
		eng := a.engine
		a.mu.Unlock()
		if eng == nil { a.openEngine(); a.mu.Lock(); eng = a.engine; a.mu.Unlock() }
		if eng == nil { return }

		taskWorkDir := workDir
		if taskWorkDir == "" {
			meta, _ := session.LoadSessionMeta(a.sessionsDir, sessionID)
			if meta.Workspace != "" {
				taskWorkDir = meta.Workspace
			} else {
				taskWorkDir = filepath.Join(a.workDir, "chats")
				os.MkdirAll(taskWorkDir, 0755)
			}
		}

		tc := &team_engine.TeamConfig{
			Label: agentName,
			Leader: team_engine.TeamLeaderConfig{
				Role:        agentName,
				Description: "Single expert agent",
			},
			Roles: []string{agentName},
		}
		eng.SetTeam(tc)

		session.EnsureSessionFile(a.sessionsDir, sessionID)

		mt, err := eng.CreateMasterTask(goal, taskWorkDir, sessionID)
		if err != nil { pod.Log("task", "create master: %v", err); return }

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

	return sessionID
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
	sessionID := newSessionID()

	now := time.Now()
	if err := session.SaveSessionMeta(a.sessionsDir, sessionID, session.SessionMeta{
		Title:     goal,
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

// ActionInfo represents a parsed ACTION directive from an AI reply.
type ActionInfo struct {
	Mode string `json:"mode"` // "agent" | "team"
	Role string `json:"role,omitempty"`
	Goal string `json:"goal"`
}

// DirectChatResult is returned from DirectChat as JSON.
type DirectChatResult struct {
	Reply       string      `json:"reply"`
	Thinking    string      `json:"thinking"`
	DurationMs  int64       `json:"durationMs"`
	NeedsAction bool        `json:"needsAction"`
	ActionType  string      `json:"actionType,omitempty"` // "plan" or "agent"
	Action      *ActionInfo `json:"action,omitempty"`
}

// parseAction extracts an ACTION directive from the reply, removes it,
// and returns the cleaned reply and the parsed action (if any).
func parseAction(reply string) (string, *ActionInfo) {
	re := regexp.MustCompile(`<!-- ACTION:(\{.*?\}) -->`)
	matches := re.FindStringSubmatch(reply)
	if len(matches) < 2 {
		return reply, nil
	}
	cleaned := strings.TrimSpace(re.ReplaceAllString(reply, ""))
	var action ActionInfo
	if err := json.Unmarshal([]byte(matches[1]), &action); err != nil {
		pod.Log("action", "parse ACTION JSON: %v", err)
		return cleaned, nil
	}
	pod.Log("action", "detected: mode=%s role=%s goal=%s", action.Mode, action.Role, action.Goal)
	return cleaned, &action
}

// DirectChat sends a user message and returns the AI reply with thinking info.
// deepThink: use deepseek-reasoner model with thinking chain.
func (a *App) DirectChat(taskID, message string, deepThink bool) string {
	if taskID == "" || strings.TrimSpace(message) == "" {
		return ""
	}

	// Try session store for any session that has a JSONL file on disk.
	sessionFile := filepath.Join(a.sessionsDir, taskID+".jsonl")
	if _, statErr := os.Stat(sessionFile); statErr == nil && a.sessionStore != nil {
		return a.directChatSession(taskID, message, deepThink)
	}

	// Legacy team_task fallback
	return a.directChatLegacy(taskID, message, deepThink)
}

// isPodSession checks if a task ID is a pod session (stored in ~/.whale/sessions/).
func isPodSession(id string) bool {
	return strings.HasPrefix(id, "pod-")
}

func newSessionID() string {
	u, err := uuid.NewV7()
	if err != nil {
		return time.Now().Format("20060102-150405")
	}
	return u.String()
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
		if m.Role == core.RoleAssistant || m.Role == core.RoleTool {
			from = "agent"
		}
		history = append(history, chatMsg{From: from, Content: core.MessagePlainText(m)})
	}

	// Load agent-specific system prompt
	meta, _ := session.LoadSessionMeta(a.sessionsDir, sessionID)
	agentPrompt := a.loadAgentSystemPrompt(meta.Agent)

	// Call LLM
	start := time.Now()
	aiReply, thinking := a.callLLM(history, deepThink, agentPrompt)
	duration := time.Since(start).Milliseconds()

	// Parse ACTION directive from reply
	aiReply, action := parseAction(aiReply)

	// Store AI reply (cleaned, without ACTION comment)
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
		session.UpdateSessionMeta(a.sessionsDir, sessionID, func(m *session.SessionMeta) {
			m.TurnCount++
		})
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}

	needsAction, actionType := detectIntent(aiReply)
	result, _ := json.Marshal(DirectChatResult{
		Reply: aiReply, Thinking: thinking, DurationMs: duration,
		NeedsAction: needsAction, ActionType: actionType, Action: action,
	})
	return string(result)
}

// StreamChatChunk is emitted per SSE chunk to the frontend.
type StreamChatChunk struct {
	SessionID string `json:"sessionId"`
	Content   string `json:"content"`
	Thinking  string `json:"thinking"`
	Done      bool   `json:"done"`
	Error     string `json:"error,omitempty"`
}

// AbortChat cancels an ongoing streaming chat for the given session.
func (a *App) AbortChat(sessionID string) {
	a.abortMu.Lock()
	cancel, ok := a.abortCancels[sessionID]
	if ok {
		cancel()
		delete(a.abortCancels, sessionID)
	}
	a.abortMu.Unlock()
}

// StreamChat starts a streaming chat and pushes chunks via Wails events.
// It runs the LLM call in a goroutine and returns immediately.
// Frontend should listen to "chat-chunk" events.
func (a *App) StreamChat(sessionID, message string, deepThink bool) string {
	if sessionID == "" || strings.TrimSpace(message) == "" {
		return ""
	}

	// Always use streaming path — create JSONL session if needed
	if a.sessionStore == nil {
		resultJSON := a.directChatLegacy(sessionID, message, deepThink)
		var result DirectChatResult
		if json.Unmarshal([]byte(resultJSON), &result) == nil {
			if result.Thinking != "" {
				runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
					SessionID: sessionID, Thinking: result.Thinking, Done: false,
				})
			}
			if result.Reply != "" {
				runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
					SessionID: sessionID, Content: result.Reply, Done: false,
				})
			}
			runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
				SessionID: sessionID, Done: true,
			})
		}
		return resultJSON
	}

	// Cancel any existing stream for this session
	a.AbortChat(sessionID)

	// Write user message
	_, err := a.sessionStore.Create(context.Background(), core.Message{
		SessionID: sessionID,
		Role:      core.RoleUser,
		Text:      message,
	})
	if err != nil {
		pod.Log("chat", "write user msg: %v", err)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				pod.Log("chat", "panic in StreamChat goroutine: %v", r)
				runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
					SessionID: sessionID, Done: true,
					Error: fmt.Sprintf("内部错误: %v", r),
				})
			}
		}()
		// Read full conversation history
		msgs, _ := a.sessionStore.List(context.Background(), sessionID)
		var history []chatMsg
		for _, m := range msgs {
			from := "human"
			if m.Role == core.RoleAssistant || m.Role == core.RoleTool {
				from = "agent"
			}
			history = append(history, chatMsg{From: from, Content: core.MessagePlainText(m)})
		}

		// Load agent-specific system prompt
		meta, _ := session.LoadSessionMeta(a.sessionsDir, sessionID)
		agentPrompt := a.loadAgentSystemPrompt(meta.Agent)

		// Create cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		a.abortMu.Lock()
		if a.abortCancels == nil {
			a.abortCancels = make(map[string]context.CancelFunc)
		}
		a.abortCancels[sessionID] = cancel
		a.abortMu.Unlock()

		defer func() {
			a.abortMu.Lock()
			delete(a.abortCancels, sessionID)
			a.abortMu.Unlock()
			cancel()
		}()

		start := time.Now()
		fullReply, fullThinking, streamErr := a.callLLMStream(ctx, history, deepThink, agentPrompt, sessionID)
		duration := time.Since(start).Milliseconds()

		// Check if cancelled
		select {
		case <-ctx.Done():
			// Store partial reply if any
			if fullReply != "" || fullThinking != "" {
				a.sessionStore.Create(context.Background(), core.Message{
					SessionID:  sessionID,
					Role:       core.RoleAssistant,
					Text:       fullReply + "\n\n*[已停止]*",
					Reasoning:  fullThinking,
					DurationMs: duration,
				})
				session.UpdateSessionMeta(a.sessionsDir, sessionID, func(m *session.SessionMeta) {
					m.TurnCount++
				})
			}
			runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
				SessionID: sessionID, Done: true, Error: "cancelled",
			})
			runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
			return
		default:
		}

		// Emit done event
		if streamErr != "" {
			runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
				SessionID: sessionID, Done: true, Error: streamErr,
			})
			// Store error as AI reply so user sees it
			a.sessionStore.Create(context.Background(), core.Message{
				SessionID:  sessionID,
				Role:       core.RoleAssistant,
				Text:       streamErr,
				DurationMs: duration,
			})
		} else if fullReply != "" {
			// Parse ACTION directive from reply
			cleanedReply, action := parseAction(fullReply)

			// Store complete AI reply (cleaned, without ACTION comment)
			a.sessionStore.Create(context.Background(), core.Message{
				SessionID:  sessionID,
				Role:       core.RoleAssistant,
				Text:       cleanedReply,
				Reasoning:  fullThinking,
				DurationMs: duration,
			})
			session.UpdateSessionMeta(a.sessionsDir, sessionID, func(m *session.SessionMeta) {
				m.TurnCount++
			})

			// If action detected, emit it so frontend can trigger task
			if action != nil {
				runtime.EventsEmit(a.ctx, "chat-action", map[string]interface{}{
					"sessionId": sessionID,
					"mode":      action.Mode,
					"role":      action.Role,
					"goal":      action.Goal,
				})
			}
		}

		runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
			SessionID: sessionID, Done: true,
		})
		runtime.EventsEmit(a.ctx, "update", a.GetMasterTasks())
	}()

	return ""
}

// callLLMStream makes a streaming HTTP request to DeepSeek API and emits
// "chat-chunk" events for each SSE data line.
func (a *App) callLLMStream(ctx context.Context, messages []chatMsg, deepThink bool, systemPrompt, sessionID string) (reply string, thinking string, errStr string) {
	apiKey := a.cachedAPIKey
	if apiKey == "" {
		return "", "", "未找到 DeepSeek API Key。请设置 DEEPSEEK_API_KEY 环境变量或在设置中配置"
	}

	model := "deepseek-chat"
	if deepThink {
		model = "deepseek-reasoner"
	}

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
		"stream":      true,
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepseek.com/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return "", "", fmt.Sprintf("请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return reply, thinking, "" // cancelled
		}
		return "", "", fmt.Sprintf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var errResp struct {
			Error struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(bodyBytes, &errResp) == nil && errResp.Error.Message != "" {
			return "", "", fmt.Sprintf("API 错误 (%d): %s", resp.StatusCode, errResp.Error.Message)
		}
		return "", "", fmt.Sprintf("API 错误 (%d): %s", resp.StatusCode, safePrefix(string(bodyBytes), 300))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer for long lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var fullReply, fullThinking strings.Builder

	for scanner.Scan() {
		// Check cancellation
		select {
		case <-ctx.Done():
			return fullReply.String(), fullThinking.String(), ""
		default:
		}

		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		contentChunk := ""
		thinkingChunk := ""
		if len(chunk.Choices) > 0 {
			contentChunk = chunk.Choices[0].Delta.Content
			thinkingChunk = chunk.Choices[0].Delta.ReasoningContent
		}

		if contentChunk != "" {
			fullReply.WriteString(contentChunk)
		}
		if thinkingChunk != "" {
			fullThinking.WriteString(thinkingChunk)
		}

		if contentChunk != "" || thinkingChunk != "" {
			runtime.EventsEmit(a.ctx, "chat-chunk", StreamChatChunk{
				SessionID: sessionID,
				Content:   contentChunk,
				Thinking:  thinkingChunk,
				Done:      false,
			})
		}
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return fullReply.String(), fullThinking.String(), ""
		}
		return fullReply.String(), fullThinking.String(), fmt.Sprintf("流式读取错误: %v", err)
	}

	return fullReply.String(), fullThinking.String(), ""
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
	aiReply, thinking := a.callLLM(history, deepThink, a.loadAgentSystemPrompt(""))
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
	if err := os.MkdirAll(msgDir, 0755); err != nil {
		pod.Log("chat", "mkdir %s: %v", msgDir, err)
		return
	}
	msgFile := filepath.Join(msgDir, fmt.Sprintf("%d_%s.md", time.Now().UnixNano(), from))
	if err := os.WriteFile(msgFile, []byte(content), 0644); err != nil {
		pod.Log("chat", "write message %s: %v", msgFile, err)
	}
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

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// SettingsData holds all user-configurable settings.
type SettingsData struct {
	APIKey      string `json:"apiKey"`
	Model       string `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int    `json:"maxTokens"`
	Theme       string `json:"theme"` // "dark" | "light" | "system"
}

// GetSettings reads the current settings from disk.
func (a *App) GetSettings() SettingsData {
	dataDir := store.DefaultDataDir()
	settingsPath := filepath.Join(dataDir, "settings.json")
	credPath := filepath.Join(dataDir, "credentials.json")

	s := SettingsData{
		Model:       "deepseek-chat",
		Temperature: 0.7,
		MaxTokens:   4096,
		Theme:       "dark",
	}

	// Read credentials
	if data, err := os.ReadFile(credPath); err == nil {
		var creds struct {
			DeepSeekAPIKey string `json:"deepseek_api_key"`
		}
		if json.Unmarshal(data, &creds) == nil {
			s.APIKey = creds.DeepSeekAPIKey
		}
	}

	// Read settings (overrides defaults)
	if data, err := os.ReadFile(settingsPath); err == nil {
		var stored SettingsData
		if json.Unmarshal(data, &stored) == nil {
			if stored.Model != "" {
				s.Model = stored.Model
			}
			if stored.Temperature > 0 {
				s.Temperature = stored.Temperature
			}
			if stored.MaxTokens > 0 {
				s.MaxTokens = stored.MaxTokens
			}
			if stored.Theme != "" {
				s.Theme = stored.Theme
			}
			// Don't override API key from settings.json (keep credentials.json as source of truth)
		}
	}

	return s
}

// SaveSettings writes settings to disk and applies them immediately.
func (a *App) SaveSettings(s SettingsData) string {
	dataDir := store.DefaultDataDir()
	settingsPath := filepath.Join(dataDir, "settings.json")
	credPath := filepath.Join(dataDir, "credentials.json")

	// Validate
	if s.Temperature < 0 || s.Temperature > 2 {
		return "Temperature 必须在 0-2 之间"
	}
	if s.MaxTokens < 100 || s.MaxTokens > 32000 {
		return "MaxTokens 必须在 100-32000 之间"
	}
	validThemes := map[string]bool{"dark": true, "light": true, "system": true}
	if !validThemes[s.Theme] {
		return "主题必须是 dark、light 或 system"
	}

	// Save API key to credentials.json
	if s.APIKey != "" {
		credData, _ := json.MarshalIndent(map[string]string{
			"deepseek_api_key": s.APIKey,
		}, "", "  ")
		if err := os.WriteFile(credPath, credData, 0600); err != nil {
			return fmt.Sprintf("保存 API Key 失败: %v", err)
		}
		// Update cached key
		a.cachedAPIKey = s.APIKey
	}

	// Save settings (without API key for security)
	settingsData, _ := json.MarshalIndent(SettingsData{
		Model:       s.Model,
		Temperature: s.Temperature,
		MaxTokens:   s.MaxTokens,
		Theme:       s.Theme,
	}, "", "  ")
	if err := os.WriteFile(settingsPath, settingsData, 0644); err != nil {
		return fmt.Sprintf("保存设置失败: %v", err)
	}

	pod.Log("settings", "saved model=%s temp=%.1f tokens=%d theme=%s", s.Model, s.Temperature, s.MaxTokens, s.Theme)
	runtime.EventsEmit(a.ctx, "settings-updated", s)
	return ""
}

// TestConnection tests the DeepSeek API connection with the given key.
func (a *App) TestConnection(apiKey string) string {
	if strings.TrimSpace(apiKey) == "" {
		return "API Key 不能为空"
	}

	body := map[string]interface{}{
		"model":       "deepseek-chat",
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens":  1,
		"temperature": 0,
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.deepseek.com/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Sprintf("请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Sprintf("连接失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		return "" // success
	}
	if resp.StatusCode == 401 {
		return "API Key 无效 (401 Unauthorized)"
	}

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return fmt.Sprintf("API 返回 %d: %s", resp.StatusCode, safePrefix(string(bodyBytes), 200))
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

func (a *App) callLLM(messages []chatMsg, deepThink bool, systemPrompt string) (reply string, thinking string) {
	apiKey := a.cachedAPIKey
	if apiKey == "" {
		return "未找到 DeepSeek API Key。请设置 DEEPSEEK_API_KEY 环境变量或在 ~/.whale/credentials.json 中配置", ""
	}

	model := "deepseek-chat"
	if deepThink {
		model = "deepseek-reasoner"
	}

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

	// Limit response body to 1 MB to prevent memory exhaustion.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("读取响应失败: %v", err), ""
	}

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
		pod.Log("llm", "unmarshal response (len=%d): %v — first 200 chars: %s",
			len(respBody), err, safePrefix(string(respBody), 200))
		return fmt.Sprintf("解析响应失败: %v", err), ""
	}
	if result.Error.Message != "" {
		return fmt.Sprintf("API 错误: %s", result.Error.Message), ""
	}
	if len(result.Choices) == 0 {
		return "未收到回复", ""
	}
	return result.Choices[0].Message.Content, result.Choices[0].Message.ReasoningContent
}

// safePrefix returns up to n characters of s, safe for logging.
func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// GetMasterTasks returns sessions from ~/.whale/sessions/.
// All conversations (direct chat, expert, team) are sessions.
// Team engine tasks are linked via SessionID.
func (a *App) GetMasterTasks() []pod.MasterTaskJSON {
	result := make([]pod.MasterTaskJSON, 0)

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

				createdAt := s.Meta.StartedAt
				if createdAt.IsZero() {
					createdAt = s.ModTime
				}
				result = append(result, pod.MasterTaskJSON{
					ID: s.ID, Goal: goal,
					Agent: s.Meta.Agent,
					SessionPath: filepath.Join(a.sessionsDir, s.ID + ".jsonl"),
					WorkspacePath: wp, WorkspaceLabel: filepath.Base(wp),
					Status: status, CreatedAt: createdAt.Format(time.RFC3339),
					TaskCount: taskCount, DoneCount: doneCount,
					WorkspaceOnline: true,
				})
			}
		}
	}

	return result
}

// ListSessionsByAgent returns sessions filtered by agent, with pagination.
func (a *App) ListSessionsByAgent(agent string, offset, limit int) []pod.MasterTaskJSON {
	result := make([]pod.MasterTaskJSON, 0)
	if a.sessionStore == nil {
		return result
	}
	sessions, err := session.ListSessions(a.sessionsDir, offset+limit)
	if err != nil {
		return result
	}
	count := 0
	for _, s := range sessions {
		if s.Meta.Kind == "subagent" {
			continue
		}
		sa := s.Meta.Agent
		if agent == "" && sa != "" {
			continue
		}
		if agent != "" && sa != agent {
			continue
		}
		count++
		if count <= offset {
			continue
		}
		goal := s.Meta.Title
		if goal == "" {
			goal = s.Conversation
		}
		wp := s.Meta.Workspace
		status := "done"
		if s.Meta.Status == "active" {
			status = "running"
		}
		createdAt := s.Meta.StartedAt
		if createdAt.IsZero() {
			createdAt = s.ModTime
		}
		result = append(result, pod.MasterTaskJSON{
			ID: s.ID, Goal: goal, Agent: sa,
			SessionPath: filepath.Join(a.sessionsDir, s.ID + ".jsonl"),
			WorkspacePath: wp, WorkspaceLabel: filepath.Base(wp),
			Status: status, CreatedAt: createdAt.Format(time.RFC3339),
		})
		if len(result) >= limit {
			break
		}
	}
	return result
}

// GetSubtasksBySession returns subtasks for all master tasks linked to a session.
func (a *App) GetSubtasksBySession(sessionID string) []pod.SubtaskJSON {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil || eng.Store == nil { return []pod.SubtaskJSON{} }

	mts, _ := eng.Store.ListMasterTasksBySession(sessionID)
	if len(mts) == 0 { return []pod.SubtaskJSON{} }

	var allTasks []*team_engine.Task
	for _, mt := range mts {
		tasks, _ := eng.Store.ListTasksByMasterTask(mt.ID)
		allTasks = append(allTasks, tasks...)
	}
	if len(allTasks) == 0 { return []pod.SubtaskJSON{} }

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
	roots := make([]pod.SubtaskJSON, 0)
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
	if eng == nil || eng.Store == nil { return []pod.SubtaskJSON{} }

	tasks, _ := eng.Store.ListTasksByMasterTask(mtID)
	if len(tasks) == 0 { return []pod.SubtaskJSON{} }

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
	roots := make([]pod.SubtaskJSON, 0)
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
	if err := os.MkdirAll(msgDir, 0755); err != nil {
		pod.Log("feedback", "mkdir %s: %v", msgDir, err)
		return err.Error()
	}
	msgFile := filepath.Join(msgDir, fmt.Sprintf("%d.md", time.Now().UnixNano()))
	if err := os.WriteFile(msgFile, []byte(message), 0644); err != nil {
		pod.Log("feedback", "write %s: %v", msgFile, err)
		return err.Error()
	}
	return ""
}

// RunSubtask resets a task to pending for re-execution.
func (a *App) RunSubtask(taskID string) string { updateMetaState(a.workDir, taskID, "pending"); return "" }

// CancelSubtask suspends a task.
func (a *App) CancelSubtask(taskID string) string { updateMetaState(a.workDir, taskID, "suspended"); return "" }

// ApplyOutput copies a task's out/ directory to the given target directory.
// The user explicitly triggers this to adopt agent-produced files into their workspace.
func (a *App) ApplyOutput(taskID, targetDir string) string {
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil {
		return "引擎未启动"
	}
	if err := eng.ApplyOutput(taskID, targetDir); err != nil {
		return err.Error()
	}
	return ""
}

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

// DeleteAllSessions deletes all sessions (pod + task) for the specified agent.
func (a *App) DeleteAllSessions(agent string) string {
	sessions, err := session.ListSessions(a.sessionsDir, 0)
	if err != nil {
		return err.Error()
	}
	for _, s := range sessions {
		if s.Meta.Kind == "subagent" {
			continue
		}
		if agent == "" && s.Meta.Agent != "" {
			continue
		}
		if agent != "" && s.Meta.Agent != agent {
			continue
		}
		a.DeleteSession(s.ID)
	}
	return ""
}

// ClearEmptySessions deletes sessions that have no chat messages for the specified agent.
func (a *App) ClearEmptySessions(agent string) string {
	sessions, err := session.ListSessions(a.sessionsDir, 0)
	if err != nil {
		return err.Error()
	}
	for _, s := range sessions {
		if s.Meta.Kind == "subagent" {
			continue
		}
		if agent == "" && s.Meta.Agent != "" {
			continue
		}
		if agent != "" && s.Meta.Agent != agent {
			continue
		}
		// Check if session has any messages
		if a.sessionStore != nil {
			msgs, err := a.sessionStore.List(context.Background(), s.ID)
			if err == nil && len(msgs) == 0 {
				a.DeleteSession(s.ID)
			}
		}
	}
	return ""
}

// ExecuteActionResult is returned by ExecuteAction.
type ExecuteActionResult struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
}

// ExecuteAction executes a user-approved action (create file / run command).
// actionType: "create_file" | "run_command"
// payload: JSON with {path, content} for files or {command, workDir} for commands.
func (a *App) ExecuteAction(sessionID, actionType, payloadJSON string) ExecuteActionResult {
	if sessionID == "" {
		return ExecuteActionResult{Success: false, Error: "sessionID 不能为空"}
	}

	// Determine workspace directory from session
	workspace := a.workDir
	if meta, err := session.LoadSessionMeta(a.sessionsDir, sessionID); err == nil && meta.Workspace != "" {
		workspace = meta.Workspace
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return ExecuteActionResult{Success: false, Error: fmt.Sprintf("无效的 payload: %v", err)}
	}

	switch actionType {
	case "create_file":
		path, _ := payload["path"].(string)
		content, _ := payload["content"].(string)
		if path == "" {
			return ExecuteActionResult{Success: false, Error: "文件路径不能为空"}
		}

		// Ensure path is relative to workspace
		fullPath := filepath.Join(workspace, path)
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return ExecuteActionResult{Success: false, Error: fmt.Sprintf("创建目录失败: %v", err)}
		}

		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return ExecuteActionResult{Success: false, Error: fmt.Sprintf("写入文件失败: %v", err)}
		}

		pod.Log("action", "created file: %s (%d bytes)", fullPath, len(content))
		return ExecuteActionResult{Success: true, Output: fmt.Sprintf("文件已创建: %s", path)}

	case "run_command":
		command, _ := payload["command"].(string)
		cmdWorkDir, _ := payload["workDir"].(string)
		if command == "" {
			return ExecuteActionResult{Success: false, Error: "命令不能为空"}
		}
		if cmdWorkDir == "" {
			cmdWorkDir = workspace
		} else if !filepath.IsAbs(cmdWorkDir) {
			cmdWorkDir = filepath.Join(workspace, cmdWorkDir)
		}

		cmd := exec.Command("cmd", "/C", command)
		cmd.Dir = cmdWorkDir
		outBytes, err := cmd.CombinedOutput()
		output := string(outBytes)
		if err != nil {
			return ExecuteActionResult{
				Success: true,
				Output:  output,
				Error:   err.Error(),
			}
		}

		pod.Log("action", "ran command in %s: %s", cmdWorkDir, command)
		return ExecuteActionResult{Success: true, Output: output}

	default:
		return ExecuteActionResult{Success: false, Error: fmt.Sprintf("不支持的操作类型: %s", actionType)}
	}
}

// ReadFileContent reads a file's content for diff/confirmation preview.
func (a *App) ReadFileContent(sessionID, relativePath string) string {
	workspace := a.workDir
	if meta, err := session.LoadSessionMeta(a.sessionsDir, sessionID); err == nil && meta.Workspace != "" {
		workspace = meta.Workspace
	}
	fullPath := filepath.Join(workspace, relativePath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ""
	}
	return string(data)
}

// RenameMasterTask renames a master task's goal/title.
func (a *App) RenameMasterTask(taskID, newGoal string) string {
	if strings.TrimSpace(newGoal) == "" {
		return "goal 不能为空"
	}

	// Try session meta for any session that has a JSONL file on disk.
	sessionFile := filepath.Join(a.sessionsDir, taskID+".jsonl")
	if _, statErr := os.Stat(sessionFile); statErr == nil {
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
	// Try JSONL session store first (covers both pod-* and UUID-style sessions).
	if a.sessionStore != nil {
		msgs, err := a.sessionStore.List(context.Background(), taskID)
		if err == nil && len(msgs) > 0 {
			result := make([]pod.ChatMessageJSON, 0, len(msgs))
			for _, m := range msgs {
				from := "human"
				if m.Role == core.RoleAssistant || m.Role == core.RoleTool {
					from = "agent"
				}
				result = append(result, pod.ChatMessageJSON{
					Time:       m.CreatedAt.Format(time.RFC3339),
					From:       from,
					Content:    core.MessagePlainText(m),
					Thinking:   m.Reasoning,
					DurationMs: m.DurationMs,
				})
			}
			return result
		}
		// If an error occurred, or no messages, fall through to check manifest existence.
		// If the session file exists on disk (even with zero messages), stop here.
		sessionFile := filepath.Join(a.sessionsDir, taskID+".jsonl")
		if _, statErr := os.Stat(sessionFile); statErr == nil {
			return []pod.ChatMessageJSON{}
		}
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
		return []TeamChatMessage{}
	}

	messages, err := eng.Whiteboard.ReadChatMessages(masterTaskID)
	if err != nil {
		return []TeamChatMessage{}
	}

	result := make([]TeamChatMessage, 0, len(messages))
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
		return []TaskConfirmation{}
	}

	tasks, _ := eng.Store.ListTasksByMasterTask(masterTaskID)
	result := make([]TaskConfirmation, 0, len(tasks))
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

	dialogue := make([]pod.AgentDialogueJSON, 0)
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
	dialogue := make([]pod.AgentDialogueJSON, 0)
	for round := 1; round <= 99; round++ {
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	msgs := make([]pod.ChatMessageJSON, 0, len(entries))
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
	newData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		pod.Log("task", "marshal meta %s: %v", metaPath, err)
		return
	}
	if err := os.WriteFile(metaPath, newData, 0644); err != nil {
		pod.Log("task", "write meta %s: %v", metaPath, err)
	}
}

// ---------------------------------------------------------------------------
// MCP Server Management
// ---------------------------------------------------------------------------

type MCPServerInfo struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Disabled  bool     `json:"disabled"`
	Connected bool     `json:"connected"`
	Tools     int      `json:"tools"`
	ToolNames []string `json:"toolNames"`
	Command   string   `json:"command,omitempty"`
	URL       string   `json:"url,omitempty"`
	Type      string   `json:"type,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func (a *App) initMCP() {
	mcpConfigPath := filepath.Join(a.workDir, ".whale", "mcp.json")
	cfg, err := mcp.LoadConfig(mcpConfigPath)
	if err != nil {
		pod.Log("mcp", "load config: %v", err)
		return
	}
	a.mcpManager = mcp.NewManager(cfg, a.workDir)
	go func() {
		a.mcpManager.InitializeWithEvents(a.ctx, func(ev mcp.StartupEvent) {
			if ev.Complete {
				pod.Log("mcp", "all servers initialized")
			} else {
				pod.Log("mcp", "%s: %s (tools=%d)", ev.State.Name, ev.State.Status, ev.State.Tools)
			}
		})
	}()
}

func (a *App) ListMCPServers() []MCPServerInfo {
	if a.mcpManager == nil {
		return make([]MCPServerInfo, 0)
	}
	states := a.mcpManager.States()
	out := make([]MCPServerInfo, 0, len(states))
	for _, st := range states {
		srv := a.mcpManager.GetServerConfig(st.Name)
		info := MCPServerInfo{
			Name:      st.Name,
			Status:    st.Status,
			Disabled:  st.Disabled,
			Connected: st.Connected,
			Tools:     st.Tools,
			ToolNames: st.ToolNames,
			Error:     st.Error,
		}
		if srv != nil {
			info.Command = srv.Command
			info.URL = srv.URL
			info.Type = srv.Type
		}
		out = append(out, info)
	}
	return out
}

func (a *App) SetMCPServerEnabled(name string, enabled bool) string {
	if a.mcpManager == nil {
		return "MCP manager not initialized"
	}
	if enabled {
		if err := a.mcpManager.EnableServer(a.ctx, name); err != nil {
			return err.Error()
		}
	} else {
		if err := a.mcpManager.DisableServer(name); err != nil {
			return err.Error()
		}
	}
	return ""
}
