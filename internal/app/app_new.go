package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/bridge"
	"github.com/usewhale/whale/internal/plugins"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/team_engine"
	teampglog "github.com/usewhale/whale/internal/team_engine/log"
)

func New(ctx context.Context, cfg Config, start StartOptions) (*App, error) {
	workspaceRoot, _ := os.Getwd()

	// Diagnostic: write timing to engine.log so we can pinpoint hangs.
	diagLog := func(step string) {
		logDir := filepath.Join(workspaceRoot, ".whale", "team_tasks", "logs")
		os.MkdirAll(logDir, 0755)
		f, err := os.OpenFile(filepath.Join(logDir, "engine.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			fmt.Fprintf(f, "[%s] app.New: %s pid=%d\n", time.Now().Format(time.RFC3339), step, os.Getpid())
			f.Close()
		}
	}
	diagLog("START")
	var workflowOverlay workflowConfigOverlay
	if !cfg.ConfigLoaded {
		workflowOverlay = workflowConfigOverlayFromInput(cfg)
	}
	cfg, err := loadNewConfig(cfg, workspaceRoot)
	diagLog("loadNewConfig done")
	if err != nil {
		return nil, err
	}
	sessionInit, err := initAppSession(cfg, start, workspaceRoot)
	diagLog("initAppSession done")
	if err != nil {
		return nil, err
	}
	toolInit, err := initAppTools(cfg, start, workspaceRoot)
	diagLog("initAppTools done")
	if err != nil {
		return nil, err
	}
	sessionInit, err = completeAppSessionState(sessionInit, start, workspaceRoot)
	diagLog("completeAppSessionState done")
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
	diagLog("initAppRuntime done")
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

		// Dashboard registration 闁?only for the main CLI process.
		// Subprocesses (whale exec --persist / whale exec subprocesses) skip
		// registration to avoid a cascade: each subprocess registering
		// triggers SyncDashboardState 闁?new engine 闁?another subprocess...
		if os.Getenv("WHALE_NO_DASHBOARD") == "" {

	// Initialize team-engine lifecycle logger (no-op without -tags teamlog).
	team_engine.SetLogger(teampglog.NewTeamLog(workspaceRoot))

	// Register with the external whale-dashboard process if it's running.
	// The heartbeat loop also retries registration if the dashboard starts later.
	app.dashboardClient = bridge.NewClient(workspaceRoot)
	// Set callbacks BEFORE StartHeartbeat to avoid race.
	app.toolset.SetDashboardClient(app.dashboardClient)
	app.dashboardClient.OnResume = func(masterTaskID string) {
		app.toolset.AutoExecuteTaskSession(masterTaskID)
	}
	app.dashboardClient.OnCancel = func(masterTaskID string) {
		app.toolset.CancelAutoExecute()
	}
	app.dashboardClient.OnRunTask = func(taskID string) {
		app.toolset.RunSingleTask(taskID)
	}
	// Use a channel to trigger initial sync on every WS connect.
	// More reliable than a callback closure.
	syncCh := make(chan struct{}, 1)
	app.dashboardClient.SyncCh = syncCh
	app.dashboardClient.StartHeartbeat()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				team_engine.Log("sync", "sync goroutine panic: %v", r)
			}
		}()
		team_engine.Log("sync", "sync goroutine started, waiting for connect...")
		for range syncCh {
			team_engine.Log("sync", "sync goroutine received connect signal")
			app.toolset.SyncDashboardState(app.dashboardClient, workspaceRoot)
		}
	}()
}

	// Clean up tasks left in transient states from a previous crash/exit.
	team_engine.CleanupInterruptedTasks(filepath.Join(workspaceRoot, ".whale", "team_tasks"))

	return app, nil
}
