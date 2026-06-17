package app

import (
	"context"
	"os"
	"path/filepath"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/bridge"
	"github.com/usewhale/whale/internal/plugins"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/team_engine"
	teampglog "github.com/usewhale/whale/internal/team_engine/log"
)

func New(ctx context.Context, cfg Config, start StartOptions) (*App, error) {
	workspaceRoot, _ := os.Getwd()
	var workflowOverlay workflowConfigOverlay
	if !cfg.ConfigLoaded {
		workflowOverlay = workflowConfigOverlayFromInput(cfg)
	}
	cfg, err := loadNewConfig(cfg, workspaceRoot)
	if err != nil {
		return nil, err
	}
	sessionInit, err := initAppSession(cfg, start, workspaceRoot)
	if err != nil {
		return nil, err
	}
	toolInit, err := initAppTools(cfg, start, workspaceRoot)
	if err != nil {
		return nil, err
	}
	sessionInit, err = completeAppSessionState(sessionInit, start, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var appRef *App
	runtimeInit, err := initAppRuntime(cfg, sessionInit, toolInit, workspaceRoot, start.Worktree.Path, func() string {
		if appRef != nil {
			return appRef.sessionID
		}
		return sessionInit.sessionID
	}, func(req policy.ApprovalRequest) policy.ApprovalDecision {
		if appRef == nil {
			return policy.ApprovalAllow
		}
		appRef.approvalMu.Lock()
		defer appRef.approvalMu.Unlock()
		if appRef.autoAcceptPermissions {
			return policy.ApprovalAllow
		}
		return appRef.approvalFn(req)
	})
	if err != nil {
		return nil, err
	}
	cfg = runtimeInit.cfg

	app := &App{
		ctx:                   ctx,
		sessionsDir:           sessionInit.sessionsDir,
		workspaceRoot:         workspaceRoot,
		branch:                sessionInit.branch,
		msgStore:              sessionInit.msgStore,
		toolRegistry:          runtimeInit.toolRegistry,
		baseToolRegistry:      toolInit.baseToolRegistry,
		subagentToolRegistry:  toolInit.subagentToolRegistry,
		toolset:               toolInit.toolset,
		baseTools:             append([]core.Tool{}, toolInit.baseTools...),
		taskTools:             append([]core.Tool{}, runtimeInit.taskTools...),
		goalTools:             append([]core.Tool{}, runtimeInit.goalTools...),
		workflowTools:         append([]core.Tool{}, runtimeInit.workflowTools...),
		hooks:                 toolInit.hooks,
		hookStates:            toolInit.hookStates,
		hookRunner:            toolInit.hookRunner,
		hookSources:           toolInit.hookSources,
		currentMode:           sessionInit.mode,
		sessionID:             sessionInit.sessionID,
		permissionPolicy:      policy.RulePolicy{Default: cfg.PermissionDefault, Rules: append([]policy.PermissionRule{}, cfg.PermissionRules...), WorkspaceRoot: workspaceRoot, WorktreeRoot: start.Worktree.Path},
		autoAcceptPermissions: cfg.AutoAcceptPermissions,
		budgetWarningUSD:      cfg.BudgetWarningUSD,
		cfg:                   cfg,
		model:                 runtimeInit.model,
		reasoningEffort:       runtimeInit.effort,
		thinkingEnabled:       runtimeInit.thinking,
		contextWindow:         runtimeInit.contextWindow,
		mcpManager:            toolInit.mcpManager,
		pluginManager:         toolInit.pluginManager,
		pluginTools:           append([]core.Tool{}, toolInit.pluginTools...),
		pluginAgents:          append([]plugins.AgentDefinition{}, toolInit.pluginAgents...),
		workflowManager:       runtimeInit.workflowManager,
		workflowRunner:        runtimeInit.workflowRunner,
		workflowConfigOverlay: workflowOverlay,
		worktree:              start.Worktree,
		apiKey:                runtimeInit.apiKey,
		approvalFn:            defaultApprovalFunc(start.ApprovalFunc),
		userInput:             defaultUserInputFunc(start.UserInputFunc),
	}
	appRef = app

	// Initialize team-engine lifecycle logger (no-op without -tags teamlog).
	team_engine.SetLogger(teampglog.NewTeamLog(workspaceRoot))

	// Register with the external whale-dashboard process if it's running.
	// The heartbeat loop also retries registration if the dashboard starts later.
	app.dashboardClient = bridge.NewClient(workspaceRoot)
	app.dashboardClient.StartHeartbeat()
	// Enable auto-resume: dashboard → heartbeat → toolset picks up pending resume.
	app.toolset.SetDashboardClient(app.dashboardClient)
	// When dashboard sends a resume command, auto-execute directly.
	app.dashboardClient.OnResume = func(masterTaskID string) {
		app.toolset.AutoExecuteMasterTask(masterTaskID)
	}
	app.dashboardClient.OnCancel = func(masterTaskID string) {
		app.toolset.CancelAutoExecute()
	}

	// Clean up tasks left in transient states from a previous crash/exit.
	team_engine.CleanupInterruptedTasks(filepath.Join(workspaceRoot, ".whale", "team_engine.db"))

	return app, nil
}
