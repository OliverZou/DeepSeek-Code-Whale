package app

import (
	"fmt"
	"os"

	"github.com/usewhale/whale/internal/agent"
	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/lsp"
	whalemcp "github.com/usewhale/whale/internal/mcp"
	"github.com/usewhale/whale/internal/plugins"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/tools"
	"strings"
)

func initAppTools(cfg Config, start StartOptions, workspaceRoot string) (appToolInit, error) {
	toolset, err := tools.NewToolset(workspaceRoot)
	if err != nil {
		return appToolInit{}, fmt.Errorf("init tools failed: %w", err)
	}
	toolset.SetWorktreeContext(start.Worktree.Path, start.Worktree.OriginalWorkspace)
	toolset.SetForegroundShellWait(cfg.ShellForegroundWaitDefaultMS, cfg.ShellForegroundWaitMaxMS)
	toolset.SetExecBoundaryPolicy(policy.RulePolicy{
		Default:       cfg.PermissionDefault,
		Rules:         append([]policy.PermissionRule(nil), cfg.PermissionRules...),
		WorkspaceRoot: workspaceRoot,
		WorktreeRoot:  start.Worktree.Path,
	})
	toolset.SetSkillDisabled(cfg.SkillsDisabled)
	mcpConfigPath := strings.TrimSpace(cfg.MCPConfigPath)
	if mcpConfigPath == "" {
		mcpConfigPath = whalemcp.DefaultConfigPath(cfg.DataDir)
	}
	mcpConfig, err := whalemcp.LoadConfig(mcpConfigPath)
	if err != nil {
		return appToolInit{}, fmt.Errorf("load mcp config: %w", err)
	}
	pluginManager := plugins.NewManager(plugins.Context{DataDir: cfg.DataDir, WorkspaceRoot: workspaceRoot}, cfg.Plugins)
	pluginOutcome := pluginManager.Outcome()
	mergePluginMCPServers(&mcpConfig, pluginOutcome.MCPServers)
	mcpManager := whalemcp.NewManager(mcpConfig, workspaceRoot)
	pluginTools := pluginOutcome.Tools
	toolset.SetExtraSkills(pluginOutcome.Skills)
	var lspMgr *lsp.Manager
	// LSP is off by default; enable via /lsp on or [lsp] enabled = true.
	if cfg.LSPEnabled {
		lspConfigPath := lsp.DefaultConfigPath(cfg.DataDir)
		lspCfg, err := lsp.LoadLSPConfig(lspConfigPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "whale: lsp: failed to load config from %s: %v (LSP disabled)\n", lspConfigPath, err)
		} else if len(lspCfg.Servers) > 0 {
			lspMgr = lsp.NewManager(lspCfg, workspaceRoot)
			toolset.SetLSPManager(lspMgr)
			lspMgr.Warmup()
		}
	}
	baseTools := append([]core.Tool{}, toolset.Tools()...)
	// Inline leader（/team 缺省）：当前主会话直接成为 Leader，需要 team 编排工
	// 具（team_run_plan/team_status/team_feedback/…）——主 agent 常驻它们，
	// 用户可随时插话/打断；显式 --subagent 模式仍走后台 subagent leader。
	baseTools = append(baseTools, toolset.TeamEngineTools()...)
	baseToolRegistry, err := core.NewToolRegistryChecked(baseTools)
	if err != nil {
		return appToolInit{}, fmt.Errorf("init base tool registry failed: %w", err)
	}
	// Team engine tools (team_run/team_status/team_feedback/…) belong to the
	// subagent registry: a Leader child agent selects them by name from its .md
	// tools (or the engine's OrchestrationToolNames) and drives the team state
	// machine from its session. Parent (main agent) tools stay unchanged.
	subagentTools := append([]core.Tool{}, toolset.TeamEngineTools()...)
	subagentTools = append(subagentTools, pluginTools...)
	subagentToolRegistry, err := core.NewToolRegistryChecked(subagentTools)
	if err != nil {
		return appToolInit{}, fmt.Errorf("init subagent tool registry failed: %w", err)
	}
	hooks, hookSources, hookLoadErr := agent.LoadHooks(workspaceRoot, cfg.DataDir)
	if hookLoadErr != nil {
		return appToolInit{}, fmt.Errorf("load hooks failed: %w", hookLoadErr)
	}
	hookStates, err := LoadHookStates(cfg.DataDir, workspaceRoot)
	if err != nil {
		return appToolInit{}, fmt.Errorf("load hook state failed: %w", err)
	}
	allHooks := append([]agent.ResolvedHook{}, hooks...)
	allHooks = append(allHooks, pluginOutcome.CommandHooks...)
	hookRunner := agent.NewHookRunnerWithState(allHooks, workspaceRoot, hookStates)
	hookRunner.AddHandlers(pluginOutcome.HookHandlers...)
	return appToolInit{
		toolset:              toolset,
		mcpManager:           mcpManager,
		lspManager:           lspMgr,
		pluginManager:        pluginManager,
		pluginTools:          pluginTools,
		pluginAgents:         pluginOutcome.Agents,
		baseTools:            baseTools,
		baseToolRegistry:     baseToolRegistry,
		subagentToolRegistry: subagentToolRegistry,
		hooks:                hooks,
		hookStates:           hookStates,
		hookRunner:           hookRunner,
		hookSources:          hookSources,
	}, nil
}

func mergePluginMCPServers(cfg *whalemcp.Config, servers map[string]whalemcp.ServerConfig) {
	if cfg == nil || len(servers) == 0 {
		return
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]whalemcp.ServerConfig{}
	}
	for name, srv := range servers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		srv.Name = name
		cfg.Servers[name] = srv
	}
}
