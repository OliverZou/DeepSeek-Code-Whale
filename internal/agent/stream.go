package agent

import (
	"context"
	"path/filepath"
	"regexp"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/llm"
	"github.com/usewhale/whale/internal/memory"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/telemetry"
)

type preparedToolDispatch struct {
	Index             int
	Call              core.ToolCall
	PreHookContext    string
	GrantOnSuccess    bool
	GrantKey          string
	GrantKeys         []string
	ExternalReadRoots []string
}

type toolDispatchOutcome struct {
	Prepared         preparedToolDispatch
	Result           core.ToolResult
	OK               bool
	PrimarySucceeded bool
}

func (a *Agent) streamAndHandle(ctx context.Context, sessionID string, history []core.Message, rt *memory.RuntimeState, events chan<- AgentEvent, toolPolicy policy.ToolPolicy, tools *core.ToolRegistry, remainingToolCalls int, autoDenyCounts map[string]int, opts RunOptions) (core.Message, *core.Message, llm.Usage, string, *telemetry.CacheShape, bool, int, error) {
	assistant, lastUsage, lastModel, cacheShape, err := a.collectAssistantStream(ctx, sessionID, rt, events, tools, opts)
	if err != nil {
		return core.Message{}, nil, llm.Usage{}, "", nil, false, 0, err
	}

	dispatchCalls, blocked, err := a.prepareToolDispatches(ctx, sessionID, lastModel, assistant, events, tools)
	if err != nil {
		return core.Message{}, nil, llm.Usage{}, "", nil, false, 0, err
	}
	attemptedToolCalls := len(dispatchCalls)
	if remainingToolCalls > 0 && len(dispatchCalls) > remainingToolCalls {
		allowed := append([]core.ToolCall(nil), dispatchCalls[:remainingToolCalls]...)
		for _, call := range dispatchCalls[remainingToolCalls:] {
			blocked = append(blocked, toolCallCapBlockedResult(call))
		}
		dispatchCalls = allowed
	}
	if len(dispatchCalls) == 0 {
		if len(blocked) == 0 {
			return assistant, nil, lastUsage, lastModel, cacheShape, false, attemptedToolCalls, nil
		}
		toolMsg, abortTurn, err := a.dispatchToolCalls(ctx, streamDispatchContext{
			SessionID:      sessionID,
			Assistant:      assistant,
			Model:          lastModel,
			Policy:         toolPolicy,
			Tools:          tools,
			Events:         events,
			AutoDenyCounts: autoDenyCounts,
			History:        history,
		}, nil, blocked)
		if err != nil {
			return core.Message{}, nil, llm.Usage{}, "", nil, false, attemptedToolCalls, err
		}
		return assistant, toolMsg, lastUsage, lastModel, cacheShape, abortTurn, attemptedToolCalls, nil
	}
	toolMsg, abortTurn, err := a.dispatchToolCalls(ctx, streamDispatchContext{
		SessionID:      sessionID,
		Assistant:      assistant,
		Model:          lastModel,
		Policy:         toolPolicy,
		Tools:          tools,
		Events:         events,
		AutoDenyCounts: autoDenyCounts,
		History:        history,
	}, dispatchCalls, blocked)
	if err != nil {
		return core.Message{}, nil, llm.Usage{}, "", nil, false, attemptedToolCalls, err
	}
	return assistant, toolMsg, lastUsage, lastModel, cacheShape, abortTurn, attemptedToolCalls, nil
}

func toolCallCapBlockedResult(call core.ToolCall) core.ToolResult {
	content, err := core.MarshalToolEnvelope(core.ToolEnvelope{
		OK:      false,
		Success: false,
		Code:    "tool_call_cap_reached",
		Error:   "tool call cap reached",
		Message: "tool call cap reached",
	})
	if err != nil {
		content = `{"success":false,"error":"tool call cap reached","code":"tool_call_cap_reached"}`
	}
	return core.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		ModelText:  content,
		Code:       "tool_call_cap_reached",
		Metadata: map[string]any{
			"blocked_reason_code": "tool_call_cap_reached",
		},
	}
}

func (a *Agent) flushPendingParallelSubagents(ctx context.Context, sessionID, assistantMessageID, model string, pending []preparedToolDispatch, events chan<- AgentEvent, results *[]core.ToolResult, tools *core.ToolRegistry) error {
	ready := make([]readyParallelSubagentCall, 0, len(pending))
	for _, prepared := range pending {
		ready = append(ready, readyParallelSubagentCall{Index: prepared.Index, Call: prepared.Call})
	}
	groups := eligibleReadyParallelSubagentGroups(ready)

	var outcomes []toolDispatchOutcome
	if len(groups) != 1 || groups[0].Start != pending[0].Index || len(groups[0].Calls) != len(pending) {
		for _, prepared := range pending {
			finalRes, ok, primarySucceeded := a.dispatchWithRecovery(ctx, sessionID, assistantMessageID, model, prepared.Call, prepared.ExternalReadRoots, events, tools)
			if err := ctx.Err(); err != nil {
				return err
			}
			outcomes = append(outcomes, toolDispatchOutcome{
				Prepared:         prepared,
				Result:           finalRes,
				OK:               ok,
				PrimarySucceeded: primarySucceeded,
			})
		}
	} else {
		var err error
		outcomes, err = a.dispatchParallelSubagentsWithRecovery(ctx, sessionID, assistantMessageID, model, pending, events, tools)
		if err != nil {
			return err
		}
	}
	for _, outcome := range outcomes {
		if !outcome.OK {
			continue
		}
		if !a.appendDispatchedToolResult(ctx, sessionID, outcome.Prepared, outcome.Result, outcome.PrimarySucceeded, events, results, true) {
			return ctx.Err()
		}
	}
	return nil
}

func (a *Agent) flushPendingParallelReasoning(ctx context.Context, sessionID, assistantMessageID, model string, pending []preparedToolDispatch, events chan<- AgentEvent, results *[]core.ToolResult, tools *core.ToolRegistry) error {
	outcomes, err := a.dispatchParallelToolCallsWithRecovery(ctx, sessionID, assistantMessageID, model, pending, events, tools, maxParallelReasonToolCalls)
	if err != nil {
		return err
	}
	for _, outcome := range outcomes {
		if !outcome.OK {
			continue
		}
		if !a.appendDispatchedToolResult(ctx, sessionID, outcome.Prepared, outcome.Result, outcome.PrimarySucceeded, events, results, true) {
			return ctx.Err()
		}
	}
	return nil
}

func (a *Agent) dispatchParallelSubagentsWithRecovery(ctx context.Context, sessionID, assistantMessageID, model string, pending []preparedToolDispatch, events chan<- AgentEvent, tools *core.ToolRegistry) ([]toolDispatchOutcome, error) {
	limit := a.maxParallelSubagents
	if limit <= 0 {
		limit = defaultMaxParallelSubagents()
	}
	return a.dispatchParallelToolCallsWithRecovery(ctx, sessionID, assistantMessageID, model, pending, events, tools, limit)
}

func (a *Agent) dispatchParallelToolCallsWithRecovery(ctx context.Context, sessionID, assistantMessageID, model string, pending []preparedToolDispatch, events chan<- AgentEvent, tools *core.ToolRegistry, limit int) ([]toolDispatchOutcome, error) {
	if len(pending) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1
	}
	if limit > len(pending) {
		limit = len(pending)
	}
	outcomeCh := make(chan toolDispatchOutcome, len(pending))
	workCh := make(chan preparedToolDispatch)
	for i := 0; i < limit; i++ {
		go func() {
			for prepared := range workCh {
				finalRes, ok, primarySucceeded := a.dispatchWithRecovery(ctx, sessionID, assistantMessageID, model, prepared.Call, prepared.ExternalReadRoots, events, tools)
				// outcomeCh is buffered for every pending call so a worker that
				// already started can report without blocking after parent
				// cancellation. The actual stop still depends on
				// dispatchWithRecovery and the tool honoring ctx.
				outcomeCh <- toolDispatchOutcome{
					Prepared:         prepared,
					Result:           finalRes,
					OK:               ok,
					PrimarySucceeded: primarySucceeded,
				}
			}
		}()
	}
	go func() {
		defer close(workCh)
		for _, prepared := range pending {
			select {
			case workCh <- prepared:
			case <-ctx.Done():
				return
			}
		}
	}()

	outcomes := make([]toolDispatchOutcome, 0, len(pending))
	for range pending {
		select {
		case outcome := <-outcomeCh:
			outcomes = append(outcomes, outcome)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	sort.SliceStable(outcomes, func(i, j int) bool {
		return outcomes[i].Prepared.Index < outcomes[j].Prepared.Index
	})
	return outcomes, nil
}

// extractAnalysisFiles parses the model's <analysis> block to find
// file names mentioned in the root_cause field.
func extractAnalysisFiles(assistantText string) map[string]bool {
	const openTag = "<analysis>"
	const closeTag = "</analysis>"
	start := strings.Index(assistantText, openTag)
	if start == -1 {
		return nil
	}
	end := strings.Index(assistantText[start:], closeTag)
	if end == -1 {
		return nil
	}
	block := assistantText[start+len(openTag) : start+end]
	files := make(map[string]bool)
	// Match file-like patterns: *.go, *.rs, *.py, etc.
	re := regexp.MustCompile(`[\w./-]+\.(go|rs|py|ts|js|tsx|jsx|toml|json|yaml|yml|mod|sum)`)
	for _, m := range re.FindAllString(block, -1) {
		base := filepath.Base(m)
		if base != "." && base != ".." {
			files[strings.ToLower(base)] = true
		}
	}
	return files
}

// extractDiffFiles extracts the set of files touched in a tool result's diff.
func extractDiffFiles(res core.ToolResult) map[string]bool {
	if res.Metadata == nil {
		return nil
	}
	kind, _ := res.Metadata["kind"].(string)
	if kind != "file_diff" {
		return nil
	}
	files, ok := res.Metadata["files"].([]map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]bool)
	for _, fm := range files {
		if f, ok := fm["file"].(string); ok && f != "" {
			result[strings.ToLower(filepath.Base(f))] = true
		}
	}
	return result
}

// analysisDiffMismatch checks whether files mentioned in the model's root
// cause analysis overlap with files actually changed in the diff. Returns
// a warning message if there is no overlap.
func analysisDiffMismatch(assistantText string, res core.ToolResult) string {
	analysisFiles := extractAnalysisFiles(assistantText)
	if len(analysisFiles) == 0 {
		return ""
	}
	diffFiles := extractDiffFiles(res)
	if len(diffFiles) == 0 {
		return ""
	}
	// Check for any overlap
	for f := range analysisFiles {
		if diffFiles[f] {
			return "" // match found
		}
	}
	// No overlap - construct warning
	var af, df []string
	for f := range analysisFiles {
		af = append(af, f)
	}
	for f := range diffFiles {
		df = append(df, f)
	}
	sort.Strings(af)
	sort.Strings(df)
	return fmt.Sprintf("\n\n⚠ Analysis-diff mismatch: your analysis mentions %s, but your changes are in %s. Does the change still address the root cause?", strings.Join(af, ", "), strings.Join(df, ", "))
}


func (a *Agent) appendDispatchedToolResult(ctx context.Context, sessionID string, prepared preparedToolDispatch, finalRes core.ToolResult, primarySucceeded bool, events chan<- AgentEvent, results *[]core.ToolResult, requireEventDelivery bool) bool {
	emit := func(ev AgentEvent) bool {
		return sendAgentEvent(ctx, events, ev)
	}
	call := prepared.Call
	if prepared.GrantOnSuccess && primarySucceeded {
		if !a.grantApprovals(ctx, sessionID, call, prepared.GrantKey, prepared.GrantKeys, events) && requireEventDelivery {
			return false
		}
	}
	// Recovery wrappers and other bypass producers arrive unclassified
	// (raw envelope in ModelText, no Outcome); finalize before hooks so
	// PostToolUse payloads see the same result shape that is persisted.
	finalRes = core.FinalizeToolResultChannels(finalRes)
	if prepared.PreHookContext != "" {
		addHookContextToToolResult(&finalRes, prepared.PreHookContext)
	}
	// P1: record read_file calls for the read-before-edit gate
	if call.Name == "read_file" && primarySucceeded {
		a.recordFileRead(call)
	}
	// P1: successful mutation also counts as having "read" the file,
	// so a subsequent edit in the same turn is not blocked.
	if isMutationTool(call.Name) && primarySucceeded {
		a.recordFileRead(call)
	}
	// P2: mark dirty on successful mutation for auto-verify debounce
	if isMutationTool(call.Name) && primarySucceeded {
		a.dirtySinceVerify = true
		a.dirtySinceTurnTest = true
	}
	// P2: track source/test files for test reminder
	if isMutationTool(call.Name) && primarySucceeded {
		trackMutatedFiles(finalRes, a.sourceFilesThisTurn, a.testFilesThisTurn)
	}
	// P2: diff self-review prompt after every successful mutation
	if isMutationTool(call.Name) && primarySucceeded {
		threshold := a.verifyReviewThreshold
		if threshold == 0 {
			threshold = defaultVerifyReviewThreshold
		}
		if additions, deletions := diffCountsFromResult(finalRes); additions+deletions > threshold {
			finalRes.ModelText += fmt.Sprintf(
				"\n\n--- Change summary ---\n%d additions, %d deletions. Verify: (1) every change traces to the user's request, (2) no unrelated refactoring, (3) no missing error handling for new paths.",
				additions, deletions,
			)
			// P4: cross-check root cause analysis against actual diff
			if mismatch := analysisDiffMismatch(a.lastAssistantText, finalRes); mismatch != "" {
				finalRes.ModelText += mismatch
			}
		} else if additions+deletions > 0 {
			finalRes.ModelText += fmt.Sprintf(
				"\n\n--- Self-review ---\n%d additions, %d deletions. Confirm every change traces to the user's request.",
				additions, deletions,
			)
			// P4: cross-check root cause analysis against actual diff
			if mismatch := analysisDiffMismatch(a.lastAssistantText, finalRes); mismatch != "" {
				finalRes.ModelText += mismatch
			}
		}
	}
	// Parallel spawn_subagent batches run post hooks only after the whole batch
	// returns, in original tool-call order, so stored tool results and events
	// stay deterministic even when the underlying subagents finish out of order.
	if !a.hooks.Empty() {
		var toolArgs any
		_ = json.Unmarshal([]byte(call.Input), &toolArgs)
		payload := NewPostToolUsePayload(sessionID, call, toolArgs, finalRes)
		payload.CWD = a.workspaceRoot
		report := a.hooks.RunHookWithObserver(ctx, payload, a.hookRunObserver(ctx, events))
		if strings.TrimSpace(report.AdditionalContext) != "" {
			addHookContextToToolResult(&finalRes, report.AdditionalContext)
		}
	}
	*results = append(*results, finalRes)
	r := finalRes
	if taskEvent, ok := taskCompletedEvent(finalRes); ok {
		if !emit(taskEvent) && requireEventDelivery {
			return false
		}
	}
	if !emit(AgentEvent{Type: AgentEventTypeToolResult, Result: &r}) && requireEventDelivery {
		return false
	}
	return true
}

func (a *Agent) bestEffortUpdateAssistant(msg core.Message) {
	// Preserve the terminal assistant state even if the caller's turn context
	// was canceled. This is diagnostic persistence; failure must not mask the
	// original provider error.
	_ = a.store.Update(context.Background(), msg)
}

// addHookContextToToolResult injects hook context into both channels of a
// result: the structured Metadata (which phase 2's plain-text renderer will
// read) and the model-visible text via addHookContextToToolContent (the
// sanctioned pre-persistence ModelText mutation).
func addHookContextToToolResult(res *core.ToolResult, hookContext string) {
	hookContext = strings.TrimSpace(hookContext)
	if hookContext == "" {
		return
	}
	// Unclassified bypass producers carry a raw envelope: derive the
	// channels first so the injection lands on the rendered text.
	if res.Outcome == "" {
		*res = core.FinalizeToolResultChannels(*res)
	}
	if res.Metadata == nil {
		res.Metadata = map[string]any{}
	}
	res.Metadata["hook_context"] = appendHookContextValue(res.Metadata["hook_context"], hookContext)
	res.ModelText = addHookContextToToolContent(res.ModelText, hookContext)
}

func addHookContextToToolContent(content, hookContext string) string {
	hookContext = strings.TrimSpace(hookContext)
	if hookContext == "" {
		return content
	}
	if env, ok := core.ParseToolEnvelope(content); ok {
		if env.Metadata == nil {
			env.Metadata = map[string]any{}
		}
		env.Metadata["hook_context"] = appendHookContextValue(env.Metadata["hook_context"], hookContext)
		if encoded, err := core.MarshalToolEnvelope(env); err == nil {
			return encoded
		}
	}
	return strings.TrimSpace(content + "\n" + hookContext)
}

func appendHookContextValue(existing any, hookContext string) any {
	switch v := existing.(type) {
	case nil:
		return []string{hookContext}
	case string:
		if strings.TrimSpace(v) == "" {
			return []string{hookContext}
		}
		return []string{v, hookContext}
	case []string:
		return append(v, hookContext)
	case []any:
		return append(v, hookContext)
	default:
		return []any{v, hookContext}
	}
}

func modeBlockedDetails(mode session.Mode) (code, message, summary string, data map[string]any) {
	return modeBlockedDetailsForCall(mode, core.ToolCall{})
}

func modeBlockedDetailsForCall(mode session.Mode, call core.ToolCall) (code, message, summary string, data map[string]any) {
	switch mode {
	case session.ModeAsk:
		if call.Name == "shell_run" {
			return "ask_mode_blocked",
				"shell command not confirmed read-only in ask mode",
				"Ask mode blocked this shell command; do not retry the same shell operation with another shell command in this mode.",
				map[string]any{
					"current_mode":    "ask",
					"tool":            call.Name,
					"action":          "do_not_retry_same_command",
					"retryable":       false,
					"suggested_modes": []string{"/agent", "/plan", "Shift+Tab"},
				}
		}
		return "ask_mode_blocked",
			"tool unavailable in ask mode",
			"Ask mode blocked this tool call; do not retry the same call in this mode.",
			map[string]any{
				"current_mode":    "ask",
				"tool":            call.Name,
				"action":          "do_not_retry_same_call",
				"retryable":       false,
				"suggested_modes": []string{"/agent", "/plan", "Shift+Tab"},
			}
	case session.ModePlan:
		if call.Name == "update_plan" {
			return "plan_mode_blocked",
				"update_plan is a TODO/checklist tool and is not allowed in Plan mode",
				"update_plan only tracks implementation checklist progress after a plan has been accepted. In Plan mode, do not retry update_plan; when the plan is decision-complete, write it as your final reply in plain Markdown instead.",
				map[string]any{
					"current_mode": "plan",
					"tool":         call.Name,
					"action":       "write_plan_as_final_reply",
					"retryable":    false,
				}
		}
		if call.Name == "shell_run" {
			return "plan_mode_blocked",
				"shell command not confirmed read-only in plan mode",
				"Plan mode blocked this shell command; do not retry the same shell operation with another shell command in this mode. If the user asked for this action, add it to the plan as a step instead of performing it; do not suggest switching modes. Continue with allowed read-only tools, or write the final plan as your reply in plain Markdown.",
				map[string]any{
					"current_mode": "plan",
					"tool":         call.Name,
					"action":       "do_not_retry_same_command",
					"retryable":    false,
				}
		}
		return "plan_mode_blocked",
			"tool unavailable in plan mode",
			"Plan mode blocked this tool call; do not retry the same call in this mode. If the user asked for this action, add it to the plan as a step instead of performing it; do not suggest switching modes. Continue with allowed read-only tools, or write the final plan as your reply in plain Markdown.",
			map[string]any{
				"current_mode": "plan",
				"tool":         call.Name,
				"action":       "do_not_retry_same_call",
				"retryable":    false,
			}
	default:
		return "mode_blocked",
			"tool unavailable in current mode",
			"Current mode blocked this tool call; do not retry the same call in this mode.",
			map[string]any{
				"tool":      call.Name,
				"action":    "do_not_retry_same_call",
				"retryable": false,
			}
	}
}

func policyDenialEnvelope(d policy.PolicyDecision) string {
	code := strings.TrimSpace(d.Code)
	if code == "" {
		code = "policy_denied"
	}
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		reason = "policy denied tool call"
	}
	content, err := core.MarshalToolEnvelope(core.ToolEnvelope{
		OK:      false,
		Success: false,
		Code:    code,
		Error:   reason,
		Message: reason,
		Summary: "This tool call was blocked automatically by policy. Do not retry the same tool call in this turn; continue with allowed alternatives or explain the block.",
		Data: map[string]any{
			"action":    "do_not_retry_same_call",
			"retryable": false,
		},
	})
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":%q,"code":%q}`, reason, code)
	}
	return content
}

func autoDenyMetadata(counts map[string]int, code, tool string) map[string]any {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "policy_denied"
	}
	tool = strings.TrimSpace(tool)
	key := code + "\x00" + tool
	count := 1
	if counts != nil {
		counts[key]++
		count = counts[key]
	}
	metadata := map[string]any{
		"auto_denied":         true,
		"ui_visibility":       "audit",
		"blocked_reason_code": code,
		"policy_code":         code,
	}
	if count > 1 {
		metadata["auto_deny_repeat_count"] = count
		metadata["auto_deny_notice"] = repeatedAutoDenyNotice(code, tool)
	}
	return metadata
}

func repeatedAutoDenyNotice(code, tool string) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		tool = "tool"
	}
	switch strings.TrimSpace(code) {
	case "plan_mode_blocked":
		return fmt.Sprintf("Repeated %s attempts were blocked because plan mode only allows safe read-only actions. The model was told not to retry this tool.", tool)
	case "ask_mode_blocked":
		return fmt.Sprintf("Repeated %s attempts were blocked because ask mode only allows read-only actions. The model was told not to retry this tool.", tool)
	case "read_only_turn_denied":
		return fmt.Sprintf("Repeated %s attempts were blocked because this turn is read-only. The model was told not to retry this tool.", tool)
	default:
		return fmt.Sprintf("Repeated %s attempts were blocked by policy. The model was told not to retry this tool.", tool)
	}
}

func addPolicyApprovalMetadata(metadata map[string]any, decision policy.PolicyDecision) map[string]any {
	if metadata == nil {
		metadata = map[string]any{}
	}
	if strings.TrimSpace(decision.MatchedRule) != "" {
		metadata["matched_rule"] = decision.MatchedRule
	}
	if strings.TrimSpace(decision.Permission) != "" {
		metadata["permission_kind"] = decision.Permission
	}
	if strings.TrimSpace(decision.Pattern) != "" {
		metadata["permission_target"] = decision.Pattern
	}
	return metadata
}
