package tasks

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/usewhale/whale/internal/agent"
	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/defaults"
	"github.com/usewhale/whale/internal/llm"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/worktree"
)

type SpawnSubagentRequest struct {
	Task         string          `json:"task"`
	Role         string          `json:"role,omitempty"`
	Team         string          `json:"team,omitempty"` // team name, recorded into session meta for picker rendering
	Agent        AgentDefinition `json:"agent,omitempty"`
	Model        string          `json:"model,omitempty"`
	MaxToolIters int             `json:"max_tool_iters,omitempty"`
	MaxToolCalls int             `json:"max_tool_calls,omitempty"`
	Tools        []string        `json:"tools,omitempty"`
	OutputSchema map[string]any  `json:"output_schema,omitempty"`
	// ReportCap overrides the runner's default report cap for the final
	// report: >0 caps at that many chars (full text is saved to the session
	// dir when exceeded), <0 disables the cap entirely (full report always
	// returned in-place), 0 uses the runner default.
	ReportCap int `json:"report_cap,omitempty"`
	// WriteAllowlist is the task's declared output files (relative to the
	// spawn workdir). When non-empty, the spawned subagent may only write these
	// paths (+ WriteExemptDirs) — a system-level internal invariant that
	// prevents overwriting another task's deliverable.
	WriteAllowlist []string `json:"-"`
	// WriteExemptDirs are dirs always writable regardless of the allowlist
	// (e.g. system temp dir for verification scripts).
	WriteExemptDirs   []string        `json:"-"`
	ParentToolCallID  string          `json:"-"`
	WorkflowRunID     string          `json:"-"`
	WorkflowName      string          `json:"-"`
	WorkflowPhase     string          `json:"-"`
	WorkflowTaskID    string          `json:"-"`
	WorkflowTaskLabel string          `json:"-"`
	ParentReadFiles   map[string]bool `json:"-"`
}

type SpawnSubagentResponse struct {
	SessionID         string `json:"session_id"`
	Role              string `json:"role"`
	Model             string `json:"model"`
	PermissionProfile string `json:"permission_profile"`
	Status            string `json:"status"`
	// Report is the subagent's full final assistant message — the primary
	// payload the parent agent must read. Carried as a first-class field (not
	// folded into Summary) so the renderer surfaces it as the tool result body
	// verbatim. Bounded by the runner's report cap; see Truncated.
	Report string `json:"report,omitempty"`
	// Summary is a one-line preview of Report for UI/progress/session lists.
	// It is NOT a substitute for Report and must never be the only thing the
	// parent agent receives.
	Summary          string   `json:"summary"`
	StructuredResult any      `json:"structured_result,omitempty"`
	Error            string   `json:"error,omitempty"`
	Truncated        bool     `json:"truncated"`
	ReportFile       string   `json:"report_file,omitempty"`
	ToolCalls        []string `json:"tool_calls,omitempty"`
	RequestedTools   []string `json:"requested_tools,omitempty"`
	ResolvedTools    []string `json:"resolved_tools,omitempty"`
	ToolMode         string   `json:"tool_mode,omitempty"`
	// SystemPrompt is the assembled extra system-prompt content injected for
	// this subagent (agent definition, workflow context, output schema, skills,
	// memory, hooks). Surfaced so the parent can audit exactly what the child
	// saw beyond the task prompt — it is not fed back to the model.
	SystemPrompt   string         `json:"system_prompt,omitempty"`
	Usage          llm.Usage      `json:"usage,omitempty"`
	SubagentBudget SubagentBudget `json:"subagent_budget,omitempty"`
	DurationMS     int64          `json:"duration_ms"`
	CompletedAt    string         `json:"completed_at"`
}

type SubagentBudget struct {
	SpawnCount  int    `json:"spawn_count"`
	TotalTokens int    `json:"total_tokens"`
	Hint        string `json:"hint,omitempty"`
}

type SpawnSubagentError struct {
	SessionID string
	Code      string
	Message   string
	Err       error
}

func (e *SpawnSubagentError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "subagent failed"
}

func (e *SpawnSubagentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (r *Runner) SpawnSubagent(ctx context.Context, req SpawnSubagentRequest) (SpawnSubagentResponse, error) {
	return r.SpawnSubagentWithProgress(ctx, req, nil)
}

func (r *Runner) AllowedSubagentTools(req SpawnSubagentRequest) ([]string, error) {
	cfg, err := ResolveAgentRuntimeConfigWithLibrary(req, RunnerDefaults{
		Model:        r.defaultModel,
		MaxToolIters: r.defaultMaxToolIters,
	}, r.agentDefinitions)
	if err != nil {
		return nil, err
	}
	return AllowedAgentToolNamesForMCPServers(r.parentTools, cfg.ToolSelectors, cfg.PermissionProfile, cfg.MCPServers, cfg.DisallowedTools)
}

func (r *Runner) SubagentStatus(sessionID string) (session.SessionMeta, error) {
	if strings.TrimSpace(sessionID) == "" {
		return session.SessionMeta{}, errors.New("session_id is required")
	}
	if strings.TrimSpace(r.sessionsDir) == "" {
		return session.SessionMeta{}, errors.New("sessions directory is not configured")
	}
	return session.LoadSessionMeta(r.sessionsDir, sessionID)
}

func (r *Runner) CancelBackgroundSubagent(sessionID string) (session.SessionMeta, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return session.SessionMeta{}, false, errors.New("session_id is required")
	}
	r.backgroundMu.Lock()
	cancel := r.backgroundCancels[strings.TrimSpace(sessionID)]
	if cancel != nil {
		delete(r.backgroundCancels, strings.TrimSpace(sessionID))
	}
	r.backgroundMu.Unlock()
	if cancel == nil {
		meta, err := r.SubagentStatus(sessionID)
		return meta, false, err
	}
	cancel()
	meta, err := session.PatchSessionMeta(r.sessionsDir, sessionID, session.SessionMetaPatch{
		Status:      "cancelling",
		CompletedAt: time.Now().UTC(),
	})
	return meta, true, err
}

func childSessionMode(permissionMode string) session.Mode {
	switch strings.TrimSpace(permissionMode) {
	case AgentPermissionAsk, AgentPermissionAuto, AgentPermissionTrusted:
		return session.ModeAgent
	default:
		return session.ModeAsk
	}
}

func childToolPolicy(parent policy.ToolPolicy, permissionMode, workspaceRoot string, toolSelectors []string) policy.ToolPolicy {
	base := childBasePolicy(parent, workspaceRoot)
	switch strings.TrimSpace(permissionMode) {
	case AgentPermissionAsk:
		return childToolApprovalPolicy{Base: base, ToolSelectors: toolSelectors}
	default:
		return base
	}
}

func childBasePolicy(parent policy.ToolPolicy, workspaceRoot string) policy.ToolPolicy {
	if parent == nil {
		return policy.RulePolicy{Default: policy.PermissionAllow, Rules: policy.DefaultRules(), WorkspaceRoot: workspaceRoot}
	}
	switch typed := parent.(type) {
	case policy.RulePolicy:
		typed.WorkspaceRoot = workspaceRoot
		typed.WorktreeRoot = ""
		return typed
	case *policy.RulePolicy:
		if typed == nil {
			return policy.RulePolicy{Default: policy.PermissionAllow, Rules: policy.DefaultRules(), WorkspaceRoot: workspaceRoot}
		}
		clone := *typed
		clone.WorkspaceRoot = workspaceRoot
		clone.WorktreeRoot = ""
		return clone
	default:
		return parent
	}
}

func childFallbackApproval(permissionMode string) policy.ApprovalDecision {
	if strings.TrimSpace(permissionMode) == AgentPermissionAsk {
		return policy.ApprovalDeny
	}
	return policy.ApprovalAllow
}

func (r *Runner) SpawnSubagentWithProgress(ctx context.Context, req SpawnSubagentRequest, progress func(core.ToolProgress)) (SpawnSubagentResponse, error) {
	return r.runSubagent(ctx, req, progress, r.childSessionID(req.ParentToolCallID), true)
}

// ContinueSubagent appends a new turn to an existing subagent session. It runs
// the same spawn pipeline (definition resolution, workspace, tools, provider,
// agent loop) but reuses the supplied sessionID instead of minting a new child
// session, and skips the initial saveSubagentMeta — so the existing .meta.json
// and JSONL transcript are extended in place rather than replaced. This is the
// "prompt an existing member session" primitive used by the team engine's
// prompt/fork/summarize surface.
func (r *Runner) ContinueSubagent(ctx context.Context, req SpawnSubagentRequest, sessionID string) (SpawnSubagentResponse, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SpawnSubagentResponse{}, errors.New("session_id is required")
	}
	return r.runSubagent(ctx, req, nil, sessionID, false)
}

func (r *Runner) runSubagent(ctx context.Context, req SpawnSubagentRequest, progress func(core.ToolProgress), sessionID string, createMeta bool) (SpawnSubagentResponse, error) {
	task := strings.TrimSpace(req.Task)
	if task == "" {
		return SpawnSubagentResponse{}, errors.New("task is required")
	}
	if r.providerFactory == nil && r.providerFactoryWithOptions == nil {
		return SpawnSubagentResponse{}, errors.New("provider factory is not configured")
	}
	cfg, err := ResolveAgentRuntimeConfigWithLibrary(req, RunnerDefaults{
		Model:        r.defaultModel,
		MaxToolIters: r.defaultMaxToolIters,
	}, r.agentDefinitions)
	if err != nil {
		return SpawnSubagentResponse{}, err
	}
	role := cfg.Definition.Name
	model := cfg.Model
	maxToolIters := cfg.MaxToolIters
	maxToolCalls := cfg.MaxToolCalls
	reportCap := req.ReportCap
	if reportCap == 0 {
		reportCap = r.summaryMaxChars
	}
	workspace, err := r.resolveSubagentWorkspace(cfg, sessionID, role)
	if err != nil {
		return SpawnSubagentResponse{}, err
	}
	parentTools := r.parentTools
	if workspace.WorktreeRoot != "" {
		workspaceTools, err := r.toolsForWorkspace(workspace)
		if err != nil {
			return SpawnSubagentResponse{}, err
		}
		parentTools, err = mergeWorkspaceAndParentTools(parentTools, workspaceTools)
		if err != nil {
			return SpawnSubagentResponse{}, err
		}
	}
	childTools, err := BuildAgentRegistryForMCPServers(parentTools, cfg.ToolSelectors, cfg.PermissionProfile, cfg.MCPServers, cfg.DisallowedTools)
	if err != nil {
		return SpawnSubagentResponse{}, err
	}
	resolvedToolNames := toolNames(childTools.Tools())
	toolMode := agentToolMode(cfg.ToolSelectors, resolvedToolNames, cfg.PermissionProfile)
	var structuredCapture *structuredOutputCapture
	if len(req.OutputSchema) > 0 {
		structuredCapture = &structuredOutputCapture{}
		tools := childTools.Tools()
		tools = append(tools, structuredOutputTool{schema: req.OutputSchema, capture: structuredCapture})
		childTools, err = core.NewToolRegistryChecked(tools)
		if err != nil {
			return SpawnSubagentResponse{}, err
		}
	}
	provider, err := r.newProvider(model, 0, cfg.Effort)
	if err != nil {
		return SpawnSubagentResponse{}, err
	}
	childStore := r.messageStore
	if childStore == nil {
		childStore = store.NewInMemoryStore()
	}
	start := time.Now()
	if createMeta {
		parentSessionID := r.currentParentSessionID()
		r.saveSubagentMeta(sessionID, session.SessionMeta{
			Kind:               "subagent",
			ParentSessionID:    parentSessionID,
			Team:               req.Team,
			Role:               role,
			Model:              model,
			Task:               task,
			Status:             "running",
			Workspace:          workspace.WorkspaceRoot,
			WorktreeName:       workspace.WorktreeName,
			WorktreePath:       workspace.WorktreeRoot,
			WorktreeBranch:     workspace.WorktreeBranch,
			OriginalWorkspace:  workspace.OriginalWorkspace,
			OriginalBranch:     workspace.OriginalBranch,
			OriginalHeadCommit: workspace.OriginalHeadCommit,
			StartedAt:          start.UTC(),
		})
	}
	extraBlocks := []string{agentDefinitionSystemBlock(cfg.Definition, cfg.ToolSelectors, resolvedToolNames, toolMode)}
	if workflowBlock := workflowContextSystemBlock(req); workflowBlock != "" {
		extraBlocks = append(extraBlocks, workflowBlock)
	}
	if schemaBlock := outputSchemaSystemBlock(req.OutputSchema); schemaBlock != "" {
		extraBlocks = append(extraBlocks, schemaBlock)
	}
	if skillBlock := preloadedSkillsSystemBlock(workspace.WorkspaceRoot, cfg.Skills, r.skillsDisabled, r.extraSkills); skillBlock != "" {
		extraBlocks = append(extraBlocks, skillBlock)
	}
	if memoryBlock := agentMemorySystemBlock(workspace.WorkspaceRoot, role, cfg.Memory); memoryBlock != "" {
		extraBlocks = append(extraBlocks, memoryBlock)
	}
	prompt := task
	if cfg.InitialPrompt != "" {
		prompt = cfg.InitialPrompt + "\n\n" + task
	}
	promptHookExecutor := r.hookModelExecutor(model, cfg.Effort, "prompt")
	agentHookExecutor := r.hookModelExecutor(model, cfg.Effort, "agent")
	if hookBlock, err := runSubagentStartHooks(ctx, cfg.Hooks, sessionID, workspace.WorkspaceRoot, role, model, cfg.PermissionProfile, prompt, promptHookExecutor, agentHookExecutor); err != nil {
		r.discardSubagentWorktree(workspace)
		r.patchSubagentMeta(sessionID, session.SessionMeta{Status: "failed", Error: err.Error(), CompletedAt: time.Now().UTC()})
		return SpawnSubagentResponse{}, &SpawnSubagentError{SessionID: sessionID, Code: "subagent_start_hook_blocked", Message: err.Error(), Err: err}
	} else if hookBlock != "" {
		extraBlocks = append(extraBlocks, hookBlock)
	}
	systemPrompt := strings.Join(extraBlocks, "\n\n---\n\n")
	newChild := func(registry *core.ToolRegistry, maxIters int) *agent.Agent {
		return agent.NewAgentWithRegistry(provider, childStore, registry,
			agent.WithChildAgentMode(),
			agent.WithSessionMode(childSessionMode(cfg.PermissionProfile)),
			agent.WithToolPolicy(childToolPolicy(r.parentPolicy, cfg.PermissionProfile, workspace.WorkspaceRoot, cfg.ToolSelectors)),
			// The child registry is capability-restricted, but policy decisions
			// still flow through the parent approval path so workspace/user
			// permission rules remain effective inside subagents.
			agent.WithApprovalFunc(func(approvalReq policy.ApprovalRequest) policy.ApprovalDecision {
				if r.approvalFunc == nil {
					return childFallbackApproval(cfg.PermissionProfile)
				}
				approvalReq.Metadata = workflowApprovalMetadata(approvalReq.Metadata, req)
				return r.approvalFunc(approvalReq)
			}),
			agent.WithSessionsDir(r.sessionsDir),
			agent.WithAutoCompact(r.autoCompact, r.autoCompactThreshold, r.contextWindowForModel(model)),
			agent.WithCompactSummaryContext(r.compactSummaryContext),
			agent.WithProjectMemory(r.memoryEnabled, r.memoryMaxChars, r.memoryFileOrder, workspace.WorkspaceRoot),
			agent.WithWorktreeContext(workspace.WorktreeRoot, workspace.OriginalWorkspace),
			// System-level write boundary: when the request carries declared
			// outputs, the child may only write them (+ exempt dirs). This is an
			// internal invariant enforced at tool dispatch, not a prompt hint,
			// and prevents a worker overwriting another task's deliverable.
			agent.WithWriteAllowlist(req.WriteAllowlist, req.WriteExemptDirs...),
			agent.WithDisabledSkills(r.skillsDisabled),
			agent.WithExtraSkills(r.extraSkills),
			agent.WithUsageLogPath(r.usageLogPath),
			agent.WithHooks(cfg.Hooks, workspace.WorkspaceRoot),
			agent.WithHookExecutors(promptHookExecutor, agentHookExecutor),
			agent.WithMaxToolIters(maxIters),
			agent.WithMaxToolCalls(maxToolCalls),
			agent.WithMaxTurns(cfg.MaxTurns),
			agent.WithExtraSystemBlocks(extraBlocks...),
			agent.WithVerifyConfig(agent.VerifyConfig{
				Commands:     r.verifyCommands,
				Timeout:      r.verifyTimeout,
				TestCommands: r.testCommands,
				TestTimeout:  r.testTimeout,
			}),
			agent.WithGateConfig(r.gateReadBeforeEdit),
		)
	}
	runChild := func(runCtx context.Context, progress func(core.ToolProgress)) (SpawnSubagentResponse, error) {
		child := newChild(childTools, maxToolIters)
		events, err := child.RunStreamWithTurnOptions(runCtx, sessionID, prompt, agent.RunOptions{
			AssistantPrefix:  cfg.Generation.AssistantPrefix,
			PrefixCompletion: cfg.Generation.PrefixCompletion,
		})
		if err != nil {
			r.discardSubagentWorktree(workspace)
			r.patchSubagentMeta(sessionID, session.SessionMeta{Status: "failed", Error: err.Error(), CompletedAt: time.Now().UTC()})
			return SpawnSubagentResponse{}, &SpawnSubagentError{SessionID: sessionID, Code: "spawn_subagent_failed", Message: err.Error(), Err: err}
		}
		var report string
		var reportFile string
		var usage llm.Usage
		var truncated bool
		var toolCalls []string
		childActions := map[string]childToolAction{}
		progressCount := 0
		var progressMessages []core.SubagentStep
		fail := func(code string, err error) (SpawnSubagentResponse, error) {
			r.discardSubagentWorktree(workspace)
			msg := "subagent failed"
			if code == "cancelled" {
				msg = "turn cancelled"
			}
			if err != nil {
				msg = err.Error()
			}
			r.patchSubagentMeta(sessionID, session.SessionMeta{Status: code, Error: msg, CompletedAt: time.Now().UTC()})
			return SpawnSubagentResponse{}, &SpawnSubagentError{SessionID: sessionID, Code: code, Message: msg, Err: err}
		}
		drainEvents := func(events <-chan agent.AgentEvent) (string, error) {
			for {
				var ev agent.AgentEvent
				var ok bool
				select {
				case ev, ok = <-events:
					if !ok {
						// Child closed its event stream: normal completion.
						return "", nil
					}
				case <-runCtx.Done():
					// The child's stream hung without closing (e.g. a provider
					// that returns no data and ignores ctx). Stop waiting so the
					// parent turn can unwind instead of deadlocking here ("关不
					// 掉"); the abandoned child goroutine is left to exit on its
					// own if its provider ever unblocks.
					return "cancelled", runCtx.Err()
				}
				switch ev.Type {
				case agent.AgentEventTypeToolCall:
					if ev.ToolCall != nil {
						internalStructuredOutput := ev.ToolCall.Name == structuredOutputToolName
						if !internalStructuredOutput {
							toolCalls = append(toolCalls, ev.ToolCall.Name)
						}
						action := summarizeChildToolCall(*ev.ToolCall)
						childActions[ev.ToolCall.ID] = action
						if internalStructuredOutput {
							continue
						}
						progressCount++
						step := core.SubagentStep{
							ToolName: ev.ToolCall.Name,
							Status:   "running",
							Summary:  action.Running,
						}
						progressMessages = append(progressMessages, step)
						emitSubagentProgressWithSteps(progress, role, model, progressCount, "running", action.Running, progressMessages, map[string]any{
							"child_session_id": sessionID,
							"child_tool":       ev.ToolCall.Name,
						})
					}
				case agent.AgentEventTypeToolResult:
					if ev.Result != nil {
						if ev.Result.Name == structuredOutputToolName {
							continue
						}
						progressCount++
						status := "running"
						if ev.Result.IsError() {
							status = "tool_failed"
						}
						action := childActions[ev.Result.ToolCallID]
						summary := summarizeChildToolResult(*ev.Result, action)
						step := core.SubagentStep{
							ToolName: ev.Result.Name,
							Status:   status,
							Summary:  summary,
						}
						progressMessages = append(progressMessages, step)
						emitSubagentProgressWithSteps(progress, role, model, progressCount, status, summary, progressMessages, map[string]any{
							"child_session_id": sessionID,
							"child_tool":       ev.Result.Name,
						})
					}
				case agent.AgentEventTypeDone:
					if ev.Message != nil {
						fullText := strings.TrimSpace(ev.Message.Text)
						report, reportFile, truncated = saveSubagentReport(r.sessionsDir, sessionID, fullText, reportCap)
						progressSummary := subagentSummaryLine(report)
						if progressSummary == "" {
							progressSummary = "child completed"
						}
						progressMessages = append(progressMessages, core.SubagentStep{
							ToolName: "subagent",
							Status:   "completed",
							Summary:  progressSummary,
						})
						emitSubagentProgressWithSteps(progress, role, model, progressCount, "completed", progressSummary, progressMessages, map[string]any{
							"child_session_id": sessionID,
							"truncated":        truncated,
						})
					}
				case agent.AgentEventTypeUsage:
					if ev.Usage != nil {
						usage = addUsage(usage, ev.Usage.Usage)
					}
				case agent.AgentEventTypeError:
					if ev.Err != nil {
						return "failed", ev.Err
					}
					return "failed", errors.New("subagent failed")
				case agent.AgentEventTypeTurnCancelled:
					return "cancelled", ctx.Err()
				default:
					if status, eventSummary, metadata, ok := summarizeChildAgentEvent(ev); ok {
						progressCount++
						if metadata == nil {
							metadata = map[string]any{}
						}
						metadata["child_session_id"] = sessionID
						step := core.SubagentStep{
							ToolName: "agent_event",
							Status:   status,
							Summary:  eventSummary,
						}
						progressMessages = append(progressMessages, step)
						emitSubagentProgressWithSteps(progress, role, model, progressCount, status, eventSummary, progressMessages, metadata)
					}
					// Other child events are intentionally drained here. The parent only
					// exposes stable subagent lifecycle/progress updates, not every
					// internal child-agent stream event.
				}
				if err := runCtx.Err(); err != nil {
					return "cancelled", err
				}
			}
		}
		if code, err := drainEvents(events); err != nil {
			return fail(code, err)
		}
		if err := runCtx.Err(); err != nil {
			return fail("cancelled", err)
		}
		if len(req.OutputSchema) > 0 {
			if _, ok := structuredCapture.get(); !ok {
				repairPrompt := structuredOutputRepairPrompt(structuredCapture.error(), report)
				repairTools, err := core.NewToolRegistryChecked([]core.Tool{structuredOutputTool{schema: req.OutputSchema, capture: structuredCapture}})
				if err != nil {
					return fail("structured_output_missing", err)
				}
				repairChild := newChild(repairTools, 1)
				repairEvents, err := repairChild.RunStreamWithOptions(runCtx, sessionID, repairPrompt, true)
				if err != nil {
					return fail("structured_output_missing", err)
				}
				if code, err := drainEvents(repairEvents); err != nil {
					return fail(code, err)
				}
			}
		}
		completedAt := time.Now().UTC()
		var structuredResult any
		if len(req.OutputSchema) > 0 {
			value, ok := structuredCapture.get()
			if !ok {
				code := "structured_output_missing"
				msg := "subagent finished without calling structured_output"
				if lastErr := structuredCapture.error(); lastErr != "" {
					code = "structured_output_invalid"
					msg = "subagent did not submit valid structured output: " + lastErr
				}
				err := errors.New(msg)
				r.discardSubagentWorktree(workspace)
				r.patchSubagentMeta(sessionID, session.SessionMeta{Status: "failed", Error: err.Error(), CompletedAt: completedAt})
				return SpawnSubagentResponse{}, &SpawnSubagentError{SessionID: sessionID, Code: code, Message: err.Error(), Err: err}
			}
			structuredResult = value
			if strings.TrimSpace(report) == "" {
				if b, err := core.MarshalToolJSON(value); err == nil {
					fullText := string(b)
					report, reportFile, truncated = saveSubagentReport(r.sessionsDir, sessionID, fullText, reportCap)
				}
			}
		}
		summary := subagentSummaryLine(report)
		if err := runSubagentStopHooks(runCtx, cfg.Hooks, sessionID, workspace.WorkspaceRoot, role, model, cfg.PermissionProfile, report, promptHookExecutor, agentHookExecutor); err != nil {
			r.discardSubagentWorktree(workspace)
			r.patchSubagentMeta(sessionID, session.SessionMeta{Status: "failed", Error: err.Error(), CompletedAt: completedAt})
			return SpawnSubagentResponse{}, &SpawnSubagentError{SessionID: sessionID, Code: "subagent_stop_hook_failed", Message: err.Error(), Err: err}
		}
		r.patchSubagentMeta(sessionID, session.SessionMeta{Status: "completed", Report: report, Summary: summary, CompletedAt: completedAt})
		if workspace.WorktreeRoot != "" {
			if err := worktree.Merge(workspace.OriginalWorkspace, workspace.WorktreeName); err == nil {
				_, _ = worktree.Remove(workspace.OriginalWorkspace, workspace.WorktreeName, false)
			} else {
				r.patchSubagentMeta(sessionID, session.SessionMeta{Error: "worktree merge failed, kept for manual handling: " + err.Error()})
			}
		}
		return SpawnSubagentResponse{
			SessionID:         sessionID,
			Role:              role,
			Model:             model,
			PermissionProfile: cfg.PermissionProfile,
			Status:            "completed",
			Report:            report,
			Summary:           summary,
			StructuredResult:  structuredResult,
			Truncated:         truncated,
			ReportFile:        reportFile,
			ToolCalls:         toolCalls,
			RequestedTools:    cloneStrings(cfg.ToolSelectors),
			ResolvedTools:     cloneStrings(resolvedToolNames),
			ToolMode:          toolMode,
			SystemPrompt:      systemPrompt,
			Usage:             usage,
			DurationMS:        time.Since(start).Milliseconds(),
			CompletedAt:       completedAt.Format(time.RFC3339),
		}, nil
	}
	if cfg.Definition.Background {
		bgCtx, cancel := context.WithCancel(context.Background())
		r.registerBackgroundSubagent(sessionID, cancel)
		emitSubagentProgress(progress, role, model, 0, "background_started", "background subagent launched", map[string]any{
			"child_session_id": sessionID,
			"background":       true,
		})
		go func() {
			defer r.unregisterBackgroundSubagent(sessionID)
			_, _ = runChild(bgCtx, nil)
		}()
		res := SpawnSubagentResponse{
			SessionID:         sessionID,
			Role:              role,
			Model:             model,
			PermissionProfile: cfg.PermissionProfile,
			Status:            "running",
			Summary:           "background subagent launched",
			RequestedTools:    cloneStrings(cfg.ToolSelectors),
			ResolvedTools:     cloneStrings(resolvedToolNames),
			ToolMode:          toolMode,
			DurationMS:        time.Since(start).Milliseconds(),
		}
		res.SubagentBudget = r.recordSubagentBudget(llm.Usage{})
		return res, nil
	}
	res, err := runChild(ctx, progress)
	if err != nil {
		return res, err
	}
	res.SubagentBudget = r.recordSubagentBudget(res.Usage)
	return res, nil
}

func (r *Runner) recordSubagentBudget(usage llm.Usage) SubagentBudget {
	if r == nil {
		return SubagentBudget{}
	}
	tokens := usage.TotalTokens
	if tokens <= 0 {
		tokens = usage.PromptTokens + usage.CompletionTokens
	}
	r.subagentBudgetMu.Lock()
	defer r.subagentBudgetMu.Unlock()
	r.subagentBudget.SpawnCount++
	r.subagentBudget.TotalTokens += tokens
	out := r.subagentBudget
	out.Hint = subagentBudgetHint(out.SpawnCount, out.TotalTokens)
	return out
}

func subagentBudgetHint(spawnCount, totalTokens int) string {
	if spawnCount > 4 || totalTokens >= 50_000 {
		return "Subagent budget is high for this session. Prefer using current-context tools or combining remaining child tasks; each fresh subagent may pay a prefix-cache miss and a full child loop."
	}
	if spawnCount > 1 {
		return "Subagent budget is warming up. Prefer direct tools for small follow-ups; each fresh subagent may pay a prefix-cache miss and a full child loop."
	}
	return ""
}

func workflowContextSystemBlock(req SpawnSubagentRequest) string {
	name := strings.TrimSpace(req.WorkflowName)
	runID := strings.TrimSpace(req.WorkflowRunID)
	phase := strings.TrimSpace(req.WorkflowPhase)
	label := strings.TrimSpace(req.WorkflowTaskLabel)
	if name == "" && runID == "" && phase == "" && label == "" {
		return ""
	}
	lines := []string{"Workflow context:"}
	if name != "" {
		lines = append(lines, "- workflow: "+name)
	}
	if runID != "" {
		lines = append(lines, "- run: "+runID)
	}
	if phase != "" {
		lines = append(lines, "- phase: "+phase)
	}
	if label != "" {
		lines = append(lines, "- task: "+label)
	}
	lines = append(lines, "Mention this workflow context in user-facing tool approval rationale or progress summaries when relevant.")
	return strings.Join(lines, "\n")
}

func workflowApprovalMetadata(existing map[string]any, req SpawnSubagentRequest) map[string]any {
	if strings.TrimSpace(req.WorkflowName) == "" && strings.TrimSpace(req.WorkflowRunID) == "" && strings.TrimSpace(req.WorkflowPhase) == "" && strings.TrimSpace(req.WorkflowTaskLabel) == "" {
		return existing
	}
	out := map[string]any{}
	for k, v := range existing {
		out[k] = v
	}
	if req.WorkflowRunID != "" {
		out["workflow_run_id"] = req.WorkflowRunID
	}
	if req.WorkflowName != "" {
		out["workflow_name"] = req.WorkflowName
	}
	if req.WorkflowPhase != "" {
		out["workflow_phase"] = req.WorkflowPhase
	}
	if req.WorkflowTaskID != "" {
		out["workflow_task_id"] = req.WorkflowTaskID
	}
	if req.WorkflowTaskLabel != "" {
		out["workflow_task_label"] = req.WorkflowTaskLabel
	}
	return out
}

func (r *Runner) resolveSubagentWorkspace(cfg AgentRuntimeConfig, sessionID, role string) (ToolWorkspace, error) {
	root := strings.TrimSpace(r.workspaceRoot)
	if root == "" {
		root = "."
	}
	workspace := ToolWorkspace{WorkspaceRoot: root}
	if cfg.Isolation != AgentIsolationWorktree {
		return workspace, nil
	}
	name := isolatedWorktreeName(role, sessionID)
	sess, err := worktree.Start(root, name)
	if err != nil {
		return ToolWorkspace{}, fmt.Errorf("create isolated subagent worktree: %w", err)
	}
	workspace.WorkspaceRoot = sess.Path
	workspace.WorktreeRoot = sess.Path
	workspace.OriginalWorkspace = sess.OriginalWorkspace
	workspace.WorktreeName = sess.Name
	workspace.WorktreeBranch = sess.Branch
	workspace.OriginalBranch = sess.OriginalBranch
	workspace.OriginalHeadCommit = sess.OriginalHeadCommit
	return workspace, nil
}

// discardSubagentWorktree removes a failed/cancelled subagent's isolated
// worktree and branch, discarding its partial changes. Best-effort: cleanup
// failure never blocks the subagent result.
func (r *Runner) discardSubagentWorktree(ws ToolWorkspace) {
	if ws.WorktreeRoot == "" || ws.WorktreeName == "" {
		return
	}
	_, _ = worktree.Remove(ws.OriginalWorkspace, ws.WorktreeName, true)
}

func (r *Runner) toolsForWorkspace(workspace ToolWorkspace) (*core.ToolRegistry, error) {
	factory := r.workspaceTools
	if factory == nil {
		factory = defaultWorkspaceTools
	}
	return factory(workspace)
}

func mergeWorkspaceAndParentTools(parent, workspace *core.ToolRegistry) (*core.ToolRegistry, error) {
	if workspace == nil {
		if parent == nil {
			return core.NewToolRegistryChecked(nil)
		}
		return parent.Snapshot(), nil
	}
	workspaceTools := workspace.Tools()
	workspaceNames := map[string]bool{}
	for _, tool := range workspaceTools {
		if tool != nil && strings.TrimSpace(tool.Name()) != "" {
			workspaceNames[tool.Name()] = true
		}
	}
	merged := append([]core.Tool{}, workspaceTools...)
	if parent != nil {
		for _, tool := range parent.Tools() {
			if tool == nil || workspaceNames[tool.Name()] {
				continue
			}
			merged = append(merged, tool)
		}
	}
	return core.NewToolRegistryChecked(merged)
}

func isolatedWorktreeName(role, sessionID string) string {
	role = safeSessionPart(role)
	if role == "" {
		role = "agent"
	}
	sum := sha1.Sum([]byte(sessionID))
	suffix := hex.EncodeToString(sum[:])[:10]
	name := "agent-" + role + "-" + suffix
	if len(name) > 64 {
		keep := 64 - len("agent--") - len(suffix)
		if keep < 1 {
			keep = 1
		}
		if len(role) > keep {
			role = strings.Trim(role[:keep], "-_.")
			if role == "" {
				role = "agent"
			}
		}
		name = "agent-" + role + "-" + suffix
	}
	return name
}

func (r *Runner) contextWindowForModel(model string) int {
	return defaults.ContextWindowForModel(model)
}

// childIDCounter disambiguates concurrent UnixNano samples. On coarse-tick
// platforms (Windows), two spawns landing on the same clock tick would return
// the identical nanosecond value, colliding two subagent sessions into one.
var childIDCounter atomic.Uint64

func (r *Runner) childSessionID(parentToolCallID string) string {
	childID := safeSessionPart(parentToolCallID)
	if childID == "" {
		childID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), childIDCounter.Add(1))
	}
	parentID := safeSessionPart(r.currentParentSessionID())
	if parentID == "" {
		return "subagent-" + childID
	}
	return parentID + "--subagent-" + childID
}

func (r *Runner) currentParentSessionID() string {
	if r != nil && r.parentSessionIDFunc != nil {
		if id := strings.TrimSpace(r.parentSessionIDFunc()); id != "" {
			return id
		}
	}
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.parentSessionID)
}

func safeSessionPart(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, v)
	out = strings.Trim(out, "-_.")
	if len(out) > 96 {
		out = out[:96]
	}
	return out
}

func (r *Runner) saveSubagentMeta(sessionID string, meta session.SessionMeta) {
	if strings.TrimSpace(r.sessionsDir) == "" || strings.TrimSpace(sessionID) == "" {
		return
	}
	_ = session.SaveSessionMeta(r.sessionsDir, sessionID, meta)
}

func (r *Runner) patchSubagentMeta(sessionID string, meta session.SessionMeta) {
	if strings.TrimSpace(r.sessionsDir) == "" || strings.TrimSpace(sessionID) == "" {
		return
	}
	_, _ = session.PatchSessionMeta(r.sessionsDir, sessionID, session.SessionMetaPatchFromMeta(meta))
}

func (r *Runner) registerBackgroundSubagent(sessionID string, cancel context.CancelFunc) {
	if strings.TrimSpace(sessionID) == "" || cancel == nil {
		return
	}
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	if r.backgroundCancels == nil {
		r.backgroundCancels = map[string]context.CancelFunc{}
	}
	r.backgroundCancels[strings.TrimSpace(sessionID)] = cancel
}

func (r *Runner) unregisterBackgroundSubagent(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	delete(r.backgroundCancels, strings.TrimSpace(sessionID))
}

// saveSubagentReport saves the full report to a file when it exceeds the cap.
// Returns a short preview + file link, the absolute file path, and true.
// Falls back to plain truncation when the session directory is unavailable.
func saveSubagentReport(sessionsDir, sessionID, fullText string, cap int) (report, reportFile string, truncated bool) {
	if cap <= 0 || len(fullText) <= cap {
		return fullText, "", false
	}
	// Save full report to session directory.
	if sessionsDir != "" && sessionID != "" {
		dir := filepath.Join(sessionsDir, sessionID)
		filePath := filepath.Join(dir, "report.md")
		if err := os.MkdirAll(dir, 0700); err == nil {
			if err := os.WriteFile(filePath, []byte(fullText), 0600); err == nil {
				absPath, _ := filepath.Abs(filePath)
				if absPath == "" {
					absPath = filePath
				}
				preview := reportPreview(fullText)
				link := fmt.Sprintf("file:///%s", filepath.ToSlash(absPath))
				report = fmt.Sprintf("%s\n\n---\nReport too long (%d chars, cap %d). Full report: %s",
					preview, len(fullText), cap, link)
				return report, absPath, true
			}
		}
	}
	// Fallback: plain truncation when session dir is unavailable.
	report, _ = truncateString(fullText, cap)
	return report, "", true
}

// reportPreview returns the first few lines of a report for preview.
func reportPreview(text string) string {
	lines := strings.Split(text, "\n")
	limit := 3
	if len(lines) < limit {
		limit = len(lines)
	}
	return strings.Join(lines[:limit], "\n")
}
