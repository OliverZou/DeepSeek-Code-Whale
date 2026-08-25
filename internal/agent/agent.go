package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/defaults"
	"github.com/usewhale/whale/internal/llm"
	llmretry "github.com/usewhale/whale/internal/llm/retry"
	"github.com/usewhale/whale/internal/memory"
	"github.com/usewhale/whale/internal/policy"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/skills"
	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/telemetry"
)

var ErrSessionBusy = errors.New("session is currently processing another request")
var ErrBudgetExceeded = errors.New("session budget exhausted")

type AgentEventType string

const (
	AgentEventTypeAssistantDelta         AgentEventType = "assistant_delta"
	AgentEventTypeReasoningDelta         AgentEventType = "reasoning_delta"
	AgentEventTypeToolArgsDelta          AgentEventType = "tool_args_delta"
	AgentEventTypeToolArgsRepaired       AgentEventType = "tool_args_repaired"
	AgentEventTypeToolCallBlocked        AgentEventType = "tool_call_blocked"
	AgentEventTypeToolModeBlocked        AgentEventType = "tool_mode_blocked"
	AgentEventTypeToolApprovalRequired   AgentEventType = "tool_approval_required"
	AgentEventTypeToolApprovalGranted    AgentEventType = "tool_approval_granted"
	AgentEventTypeToolCallScavenged      AgentEventType = "tool_call_scavenged"
	AgentEventTypeToolPolicyDecision     AgentEventType = "tool_policy_decision"
	AgentEventTypeToolCall               AgentEventType = "tool_call"
	AgentEventTypeToolResult             AgentEventType = "tool_result"
	AgentEventTypeUserInputRequired      AgentEventType = "user_input_required"
	AgentEventTypeUserInputSubmitted     AgentEventType = "user_input_submitted"
	AgentEventTypeUserInputCancelled     AgentEventType = "user_input_cancelled"
	AgentEventTypePlanDelta              AgentEventType = "plan_delta"
	AgentEventTypePlanCompleted          AgentEventType = "plan_completed"
	AgentEventTypePlanUpdate             AgentEventType = "plan_update"
	AgentEventTypePlanStepBlocked        AgentEventType = "plan_step_blocked"
	AgentEventTypeProviderRetryScheduled AgentEventType = "provider_retry_scheduled"
	AgentEventTypeResponseReset          AgentEventType = "response_reset"
	AgentEventTypeLeakedToolCallScrubbed AgentEventType = "leaked_tool_call_scrubbed"
	AgentEventTypeToolRecoveryScheduled  AgentEventType = "tool_recovery_scheduled"
	AgentEventTypeToolRecoveryAttempt    AgentEventType = "tool_recovery_attempt"
	AgentEventTypeToolRecoveryExhausted  AgentEventType = "tool_recovery_exhausted"
	AgentEventTypeReplanRequiredSet      AgentEventType = "replan_required_set"
	AgentEventTypeContextCompacted       AgentEventType = "context_compacted"
	AgentEventTypePrefixDrift            AgentEventType = "prefix_drift"
	AgentEventTypePrefixCacheMetrics     AgentEventType = "prefix_cache_metrics"
	AgentEventTypeUsage                  AgentEventType = "usage"
	AgentEventTypeBudgetWarning          AgentEventType = "budget_warning"
	AgentEventTypeTurnVerification       AgentEventType = "turn_verification"

	// Verify-feedback loop events (Feature A).
	AgentEventTypeVerifyFixStarted      AgentEventType = "verify_fix_started"
	AgentEventTypeVerifyFixRoundStart   AgentEventType = "verify_fix_round_start"
	AgentEventTypeVerifyFixRoundResult  AgentEventType = "verify_fix_round_result"
	AgentEventTypeVerifyFixPassed       AgentEventType = "verify_fix_passed"
	AgentEventTypeVerifyFixFailed       AgentEventType = "verify_fix_failed"
	AgentEventTypeVerifyFixSkipped      AgentEventType = "verify_fix_skipped"
	AgentEventTypeTurnCancelled         AgentEventType = "turn_cancelled"
	AgentEventTypeForcedSummaryStarted  AgentEventType = "forced_summary_started"
	AgentEventTypeForcedSummaryDone     AgentEventType = "forced_summary_done"
	AgentEventTypeForcedSummaryFailed   AgentEventType = "forced_summary_failed"
	AgentEventTypeHookStarted           AgentEventType = "hook_started"
	AgentEventTypeHookBlocked           AgentEventType = "hook_blocked"
	AgentEventTypeHookWarned            AgentEventType = "hook_warned"
	AgentEventTypeHookFailed            AgentEventType = "hook_failed"
	AgentEventTypeHookCompleted         AgentEventType = "hook_completed"
	AgentEventTypeParallelReasonStarted AgentEventType = "parallel_reason_started"
	AgentEventTypeParallelReasonDone    AgentEventType = "parallel_reason_completed"
	AgentEventTypeSubagentStarted       AgentEventType = "subagent_started"
	AgentEventTypeTaskProgress          AgentEventType = "task_progress"
	AgentEventTypeSubagentDone          AgentEventType = "subagent_completed"
	AgentEventTypeClassifierReview      AgentEventType = "classifier_review"
	AgentEventTypeDone                  AgentEventType = "done"
	AgentEventTypeError                 AgentEventType = "error"
)

type ToolArgsProgress struct {
	ToolCallIndex int
	ToolName      string
	ArgsChars     int
	ReadyCount    int
}

type ToolArgsRepair struct {
	ToolCallIndex int
	ToolName      string
}

type ToolCallBlocked struct {
	ToolCallID string
	ToolName   string
	ReasonCode string
}

type ToolApprovalRequired struct {
	ToolCallID string
	ToolName   string
	Reason     string
	Code       string
	Key        string
	Keys       []string
	Summary    string
	Scope      string
	Metadata   map[string]any
}

type ToolApprovalGranted struct {
	SessionID  string
	ToolCallID string
	ToolName   string
	Key        string
	Keys       []string
}

type ToolCallScavenged struct {
	Count int
}

type ToolPolicyDecision struct {
	ToolCallID    string
	ToolName      string
	Allow         bool
	NeedsApproval bool
	Reason        string
	Code          string
	Phase         string
	MatchedRule   string
}

type VerifyFixInfo struct {
	Round    int      // current round number (1-based)
	MaxRound int      // configured max rounds
	Passed   bool     // true when verification passes
	Findings []string // finding summaries (one per P0/P1 finding)
	Skipped  bool     // true when skipped (flaky, context, force summary)
	Reason   string   // skip reason or "" if not skipped
}

type AgentEvent struct {
	Type             AgentEventType
	Content          string
	ReasoningDelta   string
	ToolArgs         *ToolArgsProgress
	ToolArgsRepair   *ToolArgsRepair
	ToolBlocked      *ToolCallBlocked
	Approval         *ToolApprovalRequired
	ApprovalGrant    *ToolApprovalGranted
	Scavenged        *ToolCallScavenged
	Policy           *ToolPolicyDecision
	Recovery         *ToolRecoveryInfo
	ProviderRetry    *llmretry.Info
	Compact          *CompactInfo
	PrefixDrift      *PrefixDriftInfo
	CacheMetrics     *PrefixCacheMetricsInfo
	Usage            *UsageInfo
	Budget           *BudgetWarningInfo
	Hook             *HookEventInfo
	Task             *TaskActivityInfo
	PlanUpdate       *PlanUpdateInfo
	Classifier       *ClassifierReviewEvent
	ToolCall         *core.ToolCall
	UserInputReq     *core.UserInputRequest
	UserInputResp    *core.UserInputResponse
	Result           *core.ToolResult
	Message          *core.Message
	TurnVerification *string
	VerifyFix        *VerifyFixInfo
	Err              error
}

type PlanUpdateStep struct {
	Step   string   `json:"step"`
	Status string   `json:"status"`
	Files  []string `json:"files,omitempty"`
}

type PlanUpdateInfo struct {
	Explanation string           `json:"explanation,omitempty"`
	Plan        []PlanUpdateStep `json:"plan"`
}

type TaskActivityInfo struct {
	ToolCallID       string
	ToolName         string
	Role             string
	Model            string
	Count            int
	Summary          string
	Status           string
	DurationMS       int64
	Metadata         map[string]any
	ProgressMessages []core.SubagentStep
}

type BudgetWarningInfo struct {
	CapUSD      float64
	SpentUSD    float64
	Percent     int
	TurnCostUSD float64
}

type UserInputRequest struct {
	SessionID string
	ToolCall  core.ToolCall
	Questions []core.UserInputQuestion
}

type UserInputFunc func(req UserInputRequest) (core.UserInputResponse, bool)

type HookEventInfo struct {
	ID         string
	Name       string
	Event      HookEvent
	Source     string
	Command    string
	Decision   HookDecision
	ExitCode   int
	Message    string
	DurationMS int64
	Truncated  bool
}

type CompactInfo struct {
	Compacted      bool
	Auto           bool
	MessagesBefore int
	MessagesAfter  int
	BeforeEstimate int
	AfterEstimate  int
}

type PrefixDriftInfo struct {
	Expected string
	Actual   string
}

type PrefixCacheMetricsInfo struct {
	Model             string
	PrefixFingerprint string
	CacheShape        *telemetry.CacheShape
	PromptTokens      int
	CachedTokens      int
	CacheHitRatio     float64
}

type UsageInfo struct {
	Model string
	Usage llm.Usage
}

type ToolRecoveryInfo struct {
	ToolCallID     string
	ToolName       string
	FailureClass   string
	Action         string
	Attempt        int
	MaxAttempts    int
	Reason         string
	Executed       bool
	ReplanInjected bool
}

type Agent struct {
	provider               llm.Provider
	store                  store.MessageStore
	tools                  *core.ToolRegistry
	toolRefresh            func(context.Context) error
	storm                  stormConfig
	repairer               *toolCallRepair
	policy                 policy.ToolPolicy
	approve                policy.ApprovalFunc
	userInput              UserInputFunc
	approvalCache          *policy.SessionApprovalCache
	mode                   session.Mode
	autoCompact            bool
	compactThresh          float64
	contextWindow          int
	recovery               RecoveryPolicy
	hooks                  *HookRunner
	classifier             *Classifier
	projectMemoryEnabled   bool
	projectMemoryMaxChars  int
	projectMemoryFileOrder []string
	workspaceRoot          string
	worktreeRoot           string
	originalWorkspace      string
	disabledSkills         []string
	extraSkills            []*skills.Skill
	extraSystemBlocks      []string
	dynamicSystemBlocks    []func(RunOptions) string
	sessionRuntime         *memory.SessionRuntime
	sessionsDir            string
	childAgent             bool
	budgetWarningUSD       float64
	usageLogPath           string
	toolResultArchiveDir   string
	lifecycleCtx           context.Context
	lifecycleCancel        context.CancelFunc
	budgetWarned80         sync.Map
	maxToolIters           int
	maxToolCalls           int
	maxTurns               int
	maxParallelSubagents   int
	active                 sync.Map

	// Turn-level state (P1/P2 discipline). Reset by resetTurnState at the
	// start of every user turn. Agent is per-session; no sync needed.
	filesReadThisTurn   map[string]bool // P1: read-before-edit gate tracking
	dirtySinceVerify    bool            // P2: debounce flag for auto-verify (build/lint)
	dirtySinceTurnTest  bool            // P2: turn-level test+review debounce
	sourceFilesThisTurn map[string]bool // P2: source files mutated this turn (for test reminder)
	testFilesThisTurn   map[string]bool // P2: test files mutated this turn (for test reminder)

	// Agent-level configuration (set once, read-only after construction).
	verifyCommands        []string      // P2: post-edit auto-verify commands (build+lint)
	verifyTimeout         time.Duration // P2: auto-verify timeout
	verifyReviewThreshold int           // P2: diff review prompt threshold
	testCommands          []string      // P2: post-edit test commands
	testTimeout           time.Duration // P2: test commands timeout
	reviewAgentEnabled    bool          // P2: third-party review agent switch
	reviewModel           string        // P2: review agent model (e.g. deepseek-v4-pro)
	reviewAPIKey          string        // P2: review agent API key
	reviewBaseURL         string        // P2: review agent API base URL
	reviewClient          *http.Client  // P2: review agent HTTP client (lazy init)
	reviewClientOnce      sync.Once     // P2: guards reviewClient initialization
	gateReadBeforeEdit    bool          // P1: configurable gate switch

	// System-level write allowlist (team-engine ownership boundary). When
	// non-empty, mutation tools may only write to these normalized absolute
	// paths (the task's declared outputs) plus the exempt dirs below. Any other
	// path is rejected BEFORE the tool runs — this is an internal invariant,
	// not a prompt hint. Empty = disabled (no restriction).
	writeAllowlist  map[string]bool
	writeExemptDirs []string // normalized absolute dirs always writable (temp/artifacts)

	// Turn-level state for review agent.
	lastUserInput     string // P2: last user message text, for review agent context
	lastAssistantText string // P4: last assistant reasoning text, for root cause cross-check

	// Verify-feedback loop state (Feature A). Reset per turn.
	verifyLoopConfig      VerifyLoopConfig // set once at construction, read-only
	mutationsFromSubagent map[string]bool  // file paths mutated by subagents (skip auto-fix)
	verifyFixRound        int              // current verify-fix round (0 = not in loop)
	verifyFixIteration    bool             // true when current main-loop iteration is a fix attempt
	prevRoundFindings     map[string]bool  // fingerprint of previous round's findings (flaky detection)
}

// VerifyLoopConfig controls the verify-feedback loop (Feature A of
// the agent-verify-feedback-loop design). Verification runs after the
// model finishes its work but before the turn is declared Done, giving
// the model a chance to fix issues in the same turn.
type VerifyLoopConfig struct {
	// Enabled toggles the verify-feedback loop. Default false — opt in.
	Enabled bool
	// MaxRounds caps verify-fix iterations. Default 1, max 3.
	MaxRounds int
	// SelfCheck enables the Pre-Done self-check nudge (Feature B).
	SelfCheck bool
	// PerRoundTimeout is the max time for one verification round (test + review agent).
	// Default 120s.
	PerRoundTimeout time.Duration
	// TotalTimeout is the max time for the entire verify loop across all rounds.
	// Default 300s.
	TotalTimeout time.Duration
	// DegradeOnFailure skips remaining rounds on review agent failure.
	DegradeOnFailure bool
	// IgnoreFlakyFindings skips repeated findings across consecutive rounds.
	IgnoreFlakyFindings bool
}

// DefaultVerifyLoopConfig returns the default configuration: verify-feedback
// loop is disabled (opt-in), max 1 round, self-check enabled.
func DefaultVerifyLoopConfig() VerifyLoopConfig {
	return VerifyLoopConfig{
		Enabled:             false,
		MaxRounds:           1,
		SelfCheck:           true,
		PerRoundTimeout:     120 * time.Second,
		TotalTimeout:        300 * time.Second,
		DegradeOnFailure:    true,
		IgnoreFlakyFindings: true,
	}
}

// runTurnLevelVerification runs test and review agent once per turn,
// after the LLM has finished responding. Results are persisted as a
// tool message in the session history so the model sees them next turn.
func (a *Agent) runTurnLevelVerification(ctx context.Context, sessionID string, emit func(AgentEvent) bool) MergedVerification {
	type turnVerifyOut struct{ label, text string }
	ch := make(chan turnVerifyOut, 2)

	go func() {
		if r := a.runAutoTest(ctx); r != "" {
			ch <- turnVerifyOut{"test", r}
		} else {
			ch <- turnVerifyOut{}
		}
	}()
	go func() {
		if a.reviewAgentEnabled {
			if diffText := a.collectTurnDiffText(sessionID); diffText != "" {
				if r := a.runReviewAgent(ctx, diffText, a.lastUserInput); r != "" {
					ch <- turnVerifyOut{"review", r}
					return
				}
			}
		}
		ch <- turnVerifyOut{}
	}()

	var testText, reviewText string
	for i := 0; i < 2; i++ {
		select {
		case out := <-ch:
			switch out.label {
			case "test":
				testText = out.text
			case "review":
				reviewText = out.text
			}
		case <-ctx.Done():
			return MergedVerification{}
		}
	}

	mv := mergeVerificationResults("", testText, reviewText, a.mutationsFromSubagent)
	return mv
}

// buildSelfCheckNudge returns a message appended to the assistant's reply
// before the turn finalizes, reminding the model to self-review its output.
func (a *Agent) buildSelfCheckNudge() string {
	if a.mode == session.ModePlan {
		return "Before finalizing the plan, verify: (1) all user requirements addressed? (2) each step is concrete and actionable? (3) dependencies between steps noted?"
	}
	return "Before finalizing, verify: (1) all parts of the user's request addressed? (2) tests pass? (3) output complete and correct?"
}

// collectTurnDiffText gathers diff text from all mutation tool results
// in the current turn's session history.
func (a *Agent) collectTurnDiffText(sessionID string) string {
	msgs, err := a.store.List(context.Background(), sessionID)
	if err != nil {
		return ""
	}
	var parts []string
	for _, msg := range msgs {
		if msg.Role != core.RoleTool {
			continue
		}
		for _, tr := range msg.ToolResults {
			if !isMutationTool(tr.Name) || tr.Outcome != core.OutcomeSuccess {
				continue
			}
			if tr.Metadata == nil {
				continue
			}
			kind, _ := tr.Metadata["kind"].(string)
			if kind != "file_diff" {
				continue
			}
			files, ok := tr.Metadata["files"].([]map[string]any)
			if !ok {
				continue
			}
			for _, fm := range files {
				diff, _ := fm["unified_diff"].(string)
				if diff != "" {
					parts = append(parts, diff)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// resetTurnState clears turn-level discipline state (except lastUserInput,
// which is set separately from the user message). Called from the turn loop;
// not safe for concurrent use across sessions (Agent is per-session).
func (a *Agent) resetTurnState() {
	if a.filesReadThisTurn == nil {
		a.filesReadThisTurn = make(map[string]bool)
	} else {
		clear(a.filesReadThisTurn)
	}
	a.dirtySinceVerify = false
	a.dirtySinceTurnTest = false
	if a.sourceFilesThisTurn == nil {
		a.sourceFilesThisTurn = make(map[string]bool)
	} else {
		clear(a.sourceFilesThisTurn)
	}
	a.testFilesThisTurn = make(map[string]bool)
	a.lastAssistantText = ""
	if a.mutationsFromSubagent == nil {
		a.mutationsFromSubagent = make(map[string]bool)
	} else {
		clear(a.mutationsFromSubagent)
	}
	a.verifyFixRound = 0
	a.verifyFixIteration = false
	a.prevRoundFindings = make(map[string]bool)
}

type activeTurnState struct {
	mu      sync.Mutex
	pending []core.Message
}

func (s *activeTurnState) appendPending(messages []core.Message) {
	if s == nil || len(messages) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, messages...)
}

func (s *activeTurnState) drainPending() []core.Message {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := append([]core.Message(nil), s.pending...)
	s.pending = nil
	return out
}

func (s *activeTurnState) hasPending() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

const defaultMaxParallelSubagentCap = 128

var runtimeNumCPU = runtime.NumCPU

func defaultMaxParallelSubagents() int {
	n := runtimeNumCPU() * 2
	if n < 2 {
		return 2
	}
	if n > defaultMaxParallelSubagentCap {
		return defaultMaxParallelSubagentCap
	}
	return n
}

func NewAgent(provider llm.Provider, store store.MessageStore, tools []core.Tool) *Agent {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &Agent{
		provider:               provider,
		store:                  store,
		tools:                  core.NewToolRegistry(tools),
		storm:                  defaultStormConfig(),
		repairer:               newToolCallRepair(defaultStormConfig()),
		policy:                 policy.DefaultToolPolicy{},
		approvalCache:          policy.NewSessionApprovalCache(),
		mode:                   session.ModeAgent,
		compactThresh:          defaults.DefaultAgentCompactThreshold,
		contextWindow:          defaults.DefaultContextWindow,
		recovery:               DefaultRecoveryPolicy(),
		hooks:                  NewHookRunner(nil, ""),
		classifier:             NewClassifier(DefaultClassifierConfig()),
		projectMemoryEnabled:   true,
		projectMemoryMaxChars:  defaults.DefaultMemoryMaxChars,
		projectMemoryFileOrder: defaults.DefaultMemoryFileOrder(),
		sessionRuntime:         memory.NewSessionRuntime(""),
		usageLogPath:           telemetry.DefaultUsageLogDir(),
		toolResultArchiveDir:   defaultToolResultArchiveDir(telemetry.DefaultUsageLogDir()),
		lifecycleCtx:           lifecycleCtx,
		lifecycleCancel:        lifecycleCancel,
		maxToolIters:           0, // 0 = unlimited: the interactive main agent is bounded by user cancellation, compaction, and the storm loop-guard (see maxConsecutiveStormRounds) — not by a round count. Subagents override via WithMaxToolIters.
		maxParallelSubagents:   defaultMaxParallelSubagents(),
	}
}

func NewAgentWithRegistry(provider llm.Provider, store store.MessageStore, tools *core.ToolRegistry, opts ...AgentOption) *Agent {
	if tools == nil {
		tools = core.NewToolRegistry(nil)
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	a := &Agent{
		provider:               provider,
		store:                  store,
		tools:                  tools,
		storm:                  defaultStormConfig(),
		repairer:               newToolCallRepair(defaultStormConfig()),
		policy:                 policy.DefaultToolPolicy{},
		approvalCache:          policy.NewSessionApprovalCache(),
		mode:                   session.ModeAgent,
		compactThresh:          defaults.DefaultAgentCompactThreshold,
		contextWindow:          defaults.DefaultContextWindow,
		recovery:               DefaultRecoveryPolicy(),
		hooks:                  NewHookRunner(nil, ""),
		classifier:             NewClassifier(DefaultClassifierConfig()),
		projectMemoryEnabled:   true,
		projectMemoryMaxChars:  defaults.DefaultMemoryMaxChars,
		projectMemoryFileOrder: defaults.DefaultMemoryFileOrder(),
		sessionRuntime:         memory.NewSessionRuntime(""),
		usageLogPath:           telemetry.DefaultUsageLogDir(),
		toolResultArchiveDir:   defaultToolResultArchiveDir(telemetry.DefaultUsageLogDir()),
		lifecycleCtx:           lifecycleCtx,
		lifecycleCancel:        lifecycleCancel,
		maxToolIters:           0, // 0 = unlimited: the interactive main agent is bounded by user cancellation, compaction, and the storm loop-guard (see maxConsecutiveStormRounds) — not by a round count. Subagents override via WithMaxToolIters.
		maxParallelSubagents:   defaultMaxParallelSubagents(),
		filesReadThisTurn:      make(map[string]bool),
		sourceFilesThisTurn:    make(map[string]bool),
		testFilesThisTurn:      make(map[string]bool),
		mutationsFromSubagent:  make(map[string]bool),
		prevRoundFindings:      make(map[string]bool),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

func (a *Agent) Close() {
	if a == nil || a.lifecycleCancel == nil {
		return
	}
	a.lifecycleCancel()
}

func (a *Agent) cleanupLoopContext() context.Context {
	if a == nil || a.lifecycleCtx == nil {
		return context.Background()
	}
	return a.lifecycleCtx
}

// isToolResultReadTool reports whether the tool name is one of the read-only
// filesystem tools that may be auto-allowed for tool-results paths.
func isToolResultReadTool(name string) bool {
	switch name {
	case "read_file", "grep", "search_files":
		return true
	default:
		return false
	}
}

// isToolResultReadPath reports whether the given tool call targets a path
// inside the current session's persisted tool-results archive directory.
func (a *Agent) isToolResultReadPath(sessionID string, call core.ToolCall) bool {
	if a == nil || strings.TrimSpace(a.toolResultArchiveDir) == "" {
		return false
	}
	dir := core.ToolResultArchiveSessionDir(a.toolResultArchiveDir, sessionID)
	return policy.ToolResultReadPath(dir, call)
}

type AgentOption func(*Agent)

func WithToolPolicy(policy policy.ToolPolicy) AgentOption {
	return func(a *Agent) {
		if policy != nil {
			a.policy = policy
		}
	}
}

func WithToolRefresh(fn func(context.Context) error) AgentOption {
	return func(a *Agent) {
		a.toolRefresh = fn
	}
}

func WithApprovalFunc(fn policy.ApprovalFunc) AgentOption {
	return func(a *Agent) {
		a.approve = fn
	}
}

func WithUserInputFunc(fn UserInputFunc) AgentOption {
	return func(a *Agent) {
		a.userInput = fn
	}
}

func WithSessionMode(mode session.Mode) AgentOption {
	return func(a *Agent) {
		a.mode = mode
	}
}

func WithSessionsDir(sessionsDir string) AgentOption {
	return func(a *Agent) {
		a.sessionsDir = strings.TrimSpace(sessionsDir)
		a.sessionRuntime = memory.NewSessionRuntime(sessionsDir)
	}
}

func WithAutoCompact(enabled bool, threshold float64, contextWindow int) AgentOption {
	return func(a *Agent) {
		a.autoCompact = enabled
		if threshold > 0 && threshold < 1 {
			a.compactThresh = threshold
		}
		if contextWindow > 0 {
			a.contextWindow = contextWindow
		}
	}
}

func WithBudgetWarningUSD(capUSD float64) AgentOption {
	return func(a *Agent) {
		if capUSD > 0 {
			a.budgetWarningUSD = capUSD
		} else {
			a.budgetWarningUSD = 0
		}
	}
}

func WithUsageLogPath(path string) AgentOption {
	return func(a *Agent) {
		a.usageLogPath = strings.TrimSpace(path)
		a.toolResultArchiveDir = defaultToolResultArchiveDir(a.usageLogPath)
	}
}

func defaultToolResultArchiveDir(usageLogPath string) string {
	usageLogPath = strings.TrimSpace(usageLogPath)
	if usageLogPath == "" {
		usageLogPath = telemetry.DefaultUsageLogDir()
	}
	return filepath.Join(filepath.Dir(filepath.Clean(usageLogPath)), "tool-results")
}

func WithRecoveryPolicy(r RecoveryPolicy) AgentOption {
	return func(a *Agent) {
		a.recovery = r
	}
}

func WithHooks(hooks []ResolvedHook, workspaceRoot string) AgentOption {
	return func(a *Agent) {
		a.hooks = NewHookRunner(hooks, workspaceRoot)
	}
}

func WithHookRunner(runner *HookRunner) AgentOption {
	return func(a *Agent) {
		if runner != nil {
			a.hooks = runner
		}
	}
}

func WithHookHandlers(handlers ...HookHandler) AgentOption {
	return func(a *Agent) {
		if a.hooks == nil {
			a.hooks = NewHookRunner(nil, "")
		}
		a.hooks.AddHandlers(handlers...)
	}
}

func WithHookExecutors(promptExecutor, agentExecutor HookExecutor) AgentOption {
	return func(a *Agent) {
		if a.hooks == nil {
			a.hooks = NewHookRunner(nil, "")
		}
		a.hooks.SetExecutors(promptExecutor, agentExecutor)
	}
}

func WithProjectMemory(enabled bool, maxChars int, fileOrder []string, workspaceRoot string) AgentOption {
	return func(a *Agent) {
		a.projectMemoryEnabled = enabled
		if maxChars > 0 {
			a.projectMemoryMaxChars = maxChars
		}
		if len(fileOrder) > 0 {
			a.projectMemoryFileOrder = fileOrder
		}
		a.workspaceRoot = strings.TrimSpace(workspaceRoot)
	}
}

func WithWorktreeContext(worktreeRoot, originalWorkspace string) AgentOption {
	return func(a *Agent) {
		a.worktreeRoot = strings.TrimSpace(worktreeRoot)
		a.originalWorkspace = strings.TrimSpace(originalWorkspace)
	}
}

func WithDisabledSkills(names []string) AgentOption {
	return func(a *Agent) {
		a.disabledSkills = append([]string(nil), names...)
	}
}

func WithExtraSkills(extra []*skills.Skill) AgentOption {
	return func(a *Agent) {
		a.extraSkills = append([]*skills.Skill(nil), extra...)
	}
}

func WithExtraSystemBlocks(blocks ...string) AgentOption {
	return func(a *Agent) {
		a.extraSystemBlocks = append([]string(nil), blocks...)
	}
}

// WithChildAgentMode marks the agent as a child (subagent) session. Child
// agents skip the interactive-only system prompt blocks — mode switching,
// mode contract, delegation policy, request_user_input guidance, and the
// skills index — none of which apply to a bounded, tool-scoped worker.
// These blocks are re-sent every round, so skipping them also cuts a fixed
// ~1k token/round cost on long tool loops.
func WithChildAgentMode() AgentOption {
	return func(a *Agent) {
		a.childAgent = true
	}
}

func WithDynamicSystemBlocks(blocks ...func() string) AgentOption {
	return func(a *Agent) {
		a.dynamicSystemBlocks = make([]func(RunOptions) string, 0, len(blocks))
		for _, block := range blocks {
			render := block
			a.dynamicSystemBlocks = append(a.dynamicSystemBlocks, func(RunOptions) string {
				if render == nil {
					return ""
				}
				return render()
			})
		}
	}
}

func WithDynamicSystemBlocksForTurn(blocks ...func(RunOptions) string) AgentOption {
	return func(a *Agent) {
		a.dynamicSystemBlocks = append([]func(RunOptions) string(nil), blocks...)
	}
}

func WithMaxToolIters(maxIters int) AgentOption {
	return func(a *Agent) {
		if maxIters > 0 {
			a.maxToolIters = maxIters
		}
	}
}

func WithMaxToolCalls(maxCalls int) AgentOption {
	return func(a *Agent) {
		if maxCalls > 0 {
			a.maxToolCalls = maxCalls
		}
	}
}

func WithMaxTurns(maxTurns int) AgentOption {
	return func(a *Agent) {
		if maxTurns > 0 {
			a.maxTurns = maxTurns
		}
	}
}

func WithMaxParallelSubagents(maxParallel int) AgentOption {
	return func(a *Agent) {
		if maxParallel > 0 {
			a.maxParallelSubagents = maxParallel
		}
	}
}

// WithClassifierConfig sets the auto-review classifier configuration.
func WithClassifierConfig(cfg ClassifierConfig) AgentOption {
	return func(a *Agent) {
		a.classifier = NewClassifier(cfg)
	}
}

// WithVerifyLoopConfig sets the verify-feedback loop configuration.
func WithVerifyLoopConfig(cfg VerifyLoopConfig) AgentOption {
	return func(a *Agent) {
		if cfg.MaxRounds < 1 {
			cfg.MaxRounds = 1
		}
		if cfg.MaxRounds > 3 {
			cfg.MaxRounds = 3
		}
		a.verifyLoopConfig = cfg
	}
}

type VerifyConfig struct {
	Commands        []string
	Timeout         time.Duration
	ReviewThreshold int
	TestCommands    []string
	TestTimeout     time.Duration
	ReviewAgent     bool
	ReviewModel     string
	ReviewAPIKey    string
	ReviewBaseURL   string
}

func WithVerifyConfig(cfg VerifyConfig) AgentOption {
	return func(a *Agent) {
		a.verifyCommands = cfg.Commands
		a.verifyTimeout = cfg.Timeout
		a.verifyReviewThreshold = cfg.ReviewThreshold
		a.testCommands = cfg.TestCommands
		a.testTimeout = cfg.TestTimeout
		a.reviewAgentEnabled = cfg.ReviewAgent
		a.reviewModel = cfg.ReviewModel
		a.reviewAPIKey = cfg.ReviewAPIKey
		a.reviewBaseURL = cfg.ReviewBaseURL
	}
}

// WithGateConfig sets the P1 read-before-edit gate configuration.
// Defaults to true; set to false to disable.
func WithGateConfig(readBeforeEdit bool) AgentOption {
	return func(a *Agent) {
		a.gateReadBeforeEdit = readBeforeEdit
	}
}

// WithWriteAllowlist seeds the system-level write allowlist. Non-empty means
// mutation tools may only write to these normalized absolute paths (plus any
// dirs seeded via WithWriteExemptDirs). Empty = restriction disabled. This is
// an internal invariant enforced in the tool-dispatch layer, not a prompt hint.
func WithWriteAllowlist(files []string, exemptDirs ...string) AgentOption {
	return func(a *Agent) {
		if len(files) == 0 {
			a.writeAllowlist = nil
			a.writeExemptDirs = nil
			return
		}
		a.writeAllowlist = make(map[string]bool, len(files))
		for _, f := range files {
			if strings.TrimSpace(f) == "" {
				continue
			}
			a.writeAllowlist[normalizeAllowlistPath(f, a.workspaceRoot)] = true
		}
		for _, d := range exemptDirs {
			if strings.TrimSpace(d) == "" {
				continue
			}
			a.writeExemptDirs = append(a.writeExemptDirs, normalizeAllowlistPath(d, a.workspaceRoot))
		}
	}
}

// WithReadFiles seeds the P1 read-before-edit file tracker with files
// that were already read by a parent context (e.g. a parent agent).
func WithReadFiles(files map[string]bool) AgentOption {
	return func(a *Agent) {
		if a.filesReadThisTurn == nil {
			a.filesReadThisTurn = make(map[string]bool)
		}
		for f := range files {
			a.filesReadThisTurn[f] = true
		}
	}
}

const defaultVerifyTimeout = 30 * time.Second
const defaultTestTimeout = 60 * time.Second
const defaultVerifyReviewThreshold = 20
const maxVerifyOutputBytes = 4096

// runAutoVerify executes configured verification commands after file mutations.
// Uses os/exec directly (no approval flow) with timeout and output truncation.
// Auto-detected commands are skipped when their config files were modified
// this turn. Commands that depend on modified config files are
// skipped individually (see cmdDependsOnDirtyConfig).
func (a *Agent) runAutoVerify(ctx context.Context) string {
	return a.runCommands(ctx, a.resolveVerifyCommands(), a.verifyTimeout, defaultVerifyTimeout)
}

func (a *Agent) runAutoTest(ctx context.Context) string {
	return a.runCommands(ctx, a.resolveTestCommands(), a.testTimeout, defaultTestTimeout)
}

func (a *Agent) runCommands(ctx context.Context, commands []string, timeout, defaultTimeout time.Duration) string {
	if len(commands) == 0 {
		return ""
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}

	var results []string
	for _, cmd := range commands {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		out, err := a.execVerifyCommand(ctx, cmd)
		cancel()
		if err != nil {
			detail := strings.TrimSpace(out)
			if detail != "" {
				results = append(results, fmt.Sprintf("$ %s\n[error: %s]\n%s", cmd, err.Error(), truncateVerifyOutput(detail, maxVerifyOutputBytes)))
			} else {
				results = append(results, fmt.Sprintf("$ %s\n[error: %s]", cmd, err.Error()))
			}
		} else {
			results = append(results, fmt.Sprintf("$ %s\n%s", cmd, truncateVerifyOutput(out, maxVerifyOutputBytes)))
		}
	}
	return strings.Join(results, "\n\n")
}

func (a *Agent) execVerifyCommand(ctx context.Context, command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/d", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = a.workspaceRoot
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func truncateVerifyOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return strings.TrimRight(s, " \t\r\n")
	}
	return strings.TrimRight(s[:maxBytes], " \t\r\n") + "\n... (output truncated)"
}

// resolveVerifyCommands returns configured verify commands, or auto-detected
// commands if none are configured. Commands that depend on a config file
// modified this turn are skipped individually.
func (a *Agent) resolveVerifyCommands() []string {
	if len(a.verifyCommands) > 0 {
		return a.verifyCommands
	}
	if a.workspaceRoot == "" {
		return nil
	}
	cmds := autoDetectVerifyCommands(a.workspaceRoot)
	if cmds == nil {
		return nil
	}
	dirty := a.dirtyConfigFiles()
	var safe []string
	for _, cmd := range cmds {
		if !cmdDependsOnDirtyConfig(cmd, dirty) {
			safe = append(safe, cmd)
		}
	}
	return safe
}

func (a *Agent) resolveTestCommands() []string {
	if len(a.testCommands) > 0 {
		return a.testCommands
	}
	if strings.TrimSpace(a.workspaceRoot) == "" {
		return nil
	}
	return autoDetectTestCommands(a.workspaceRoot)
}

// dirtyConfigFiles returns which auto-detection config files were modified this turn.
func (a *Agent) dirtyConfigFiles() map[string]bool {
	configFiles := []string{"package.json", "Makefile", "go.mod", "pyproject.toml",
		"pytest.ini", "Cargo.toml"}
	dirty := make(map[string]bool)
	for _, f := range configFiles {
		if a.filesReadThisTurn[normalizeWorkspacePath(f, a.workspaceRoot)] {
			dirty[f] = true
		}
	}
	return dirty
}

// cmdDependsOnDirtyConfig returns true when cmd uses a config file that was modified.
func cmdDependsOnDirtyConfig(cmd string, dirty map[string]bool) bool {
	if dirty["go.mod"] && (strings.Contains(cmd, "go build") || strings.Contains(cmd, "go vet") || strings.Contains(cmd, "go test")) {
		return true
	}
	if dirty["package.json"] && strings.Contains(cmd, "npm run") {
		return true
	}
	if dirty["Cargo.toml"] && strings.Contains(cmd, "cargo") {
		return true
	}
	if dirty["pyproject.toml"] && strings.Contains(cmd, "ruff") {
		return true
	}
	if dirty["Makefile"] && strings.Contains(cmd, "make ") {
		return true
	}
	return false
}

func autoDetectTestCommands(workspaceRoot string) []string {
	if fileExists(filepath.Join(workspaceRoot, "go.mod")) {
		return []string{"go test ./... -count=1"}
	}
	pkgJSON := filepath.Join(workspaceRoot, "package.json")
	if fileExists(pkgJSON) {
		if hasNPMScript(pkgJSON, "test") {
			return []string{"npm test"}
		}
	}
	if fileExists(filepath.Join(workspaceRoot, "pyproject.toml")) || fileExists(filepath.Join(workspaceRoot, "pytest.ini")) {
		return []string{"pytest -x -q"}
	}
	if fileExists(filepath.Join(workspaceRoot, "Cargo.toml")) {
		return []string{"cargo test"}
	}
	if fileExists(filepath.Join(workspaceRoot, "Makefile")) {
		if hasMakeTarget(workspaceRoot, "test") {
			return []string{"make test"}
		}
	}
	return nil
}

const reviewAgentMaxTokens = 4096
const reviewAgentDefaultTimeout = 30 * time.Second

func (a *Agent) runReviewAgent(ctx context.Context, diffText, userRequest string) string {
	if !a.reviewAgentEnabled || strings.TrimSpace(diffText) == "" {
		return ""
	}
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(a.reviewAPIKey)
	}
	if apiKey == "" {
		return ""
	}
	baseURL := strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(a.reviewBaseURL)
	}
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	model := strings.TrimSpace(a.reviewModel)
	if model == "" {
		model = defaults.DefaultModel
	}

	timeout := reviewAgentDefaultTimeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	userPrompt := buildReviewerPrompt(userRequest, diffText)
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": reviewerSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"max_tokens":  reviewAgentMaxTokens,
		"temperature": 0,
		"stream":      false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("[review agent error: %s]", err.Error())
	}

	url := baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("[review agent error: %s]", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	a.reviewClientOnce.Do(func() {
		a.reviewClient = &http.Client{
			Timeout: timeout + 5*time.Second,
		}
	})

	resp, err := a.reviewClient.Do(req)
	if err != nil {
		return fmt.Sprintf("[review agent error: %s]", err.Error())
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("[review agent error: %s]", err.Error())
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("[review agent error: API returned %d]", resp.StatusCode)
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return fmt.Sprintf("[review agent error: %s]", err.Error())
	}
	if len(chatResp.Choices) == 0 {
		return "[review agent error: empty response]"
	}

	content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	if content == "" {
		return ""
	}
	return content
}

func autoDetectVerifyCommands(workspaceRoot string) []string {
	if fileExists(filepath.Join(workspaceRoot, "go.mod")) {
		return []string{"go build ./...", "go vet ./..."}
	}
	pkgJSON := filepath.Join(workspaceRoot, "package.json")
	if fileExists(pkgJSON) {
		var cmds []string
		if hasNPMScript(pkgJSON, "build") {
			cmds = append(cmds, "npm run build")
		}
		if hasNPMScript(pkgJSON, "lint") {
			cmds = append(cmds, "npm run lint")
		}
		if len(cmds) > 0 {
			return cmds
		}
	}
	if fileExists(filepath.Join(workspaceRoot, "Cargo.toml")) {
		return []string{"cargo check"}
	}
	if fileExists(filepath.Join(workspaceRoot, "pyproject.toml")) {
		return []string{"ruff check ."}
	}
	if fileExists(filepath.Join(workspaceRoot, "pom.xml")) {
		return []string{"mvn compile -q"}
	}
	if fileExists(filepath.Join(workspaceRoot, "build.gradle")) || fileExists(filepath.Join(workspaceRoot, "build.gradle.kts")) {
		return []string{"gradle build -q"}
	}
	if fileExists(filepath.Join(workspaceRoot, "Makefile")) {
		var cmds []string
		if hasMakeTarget(workspaceRoot, "check") {
			cmds = append(cmds, "make check")
		}
		if hasMakeTarget(workspaceRoot, "lint") {
			cmds = append(cmds, "make lint")
		}
		if len(cmds) > 0 {
			return cmds
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasNPMScript(pkgJSON, script string) bool {
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	_, ok := pkg.Scripts[script]
	return ok
}

func hasMakeTarget(workspaceRoot, target string) bool {
	makefilePath := filepath.Join(workspaceRoot, "Makefile")
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, target+":") || strings.HasPrefix(trimmed, target+" :") {
			return true
		}
	}
	return false
}

// Classifier returns the auto-review classifier for runtime toggle.
func (a *Agent) Classifier() *Classifier {
	if a == nil {
		return nil
	}
	return a.classifier
}

func (a *Agent) RunSession(ctx context.Context, sessionID, input string) (core.Message, error) {
	events, err := a.RunStream(ctx, sessionID, input)
	if err != nil {
		return core.Message{}, err
	}
	var final core.Message
	cancelled := false
	for ev := range events {
		if ev.Type == AgentEventTypeError && ev.Err != nil {
			return core.Message{}, ev.Err
		}
		if ev.Type == AgentEventTypeTurnCancelled {
			cancelled = true
		}
		if ev.Type == AgentEventTypeDone && ev.Message != nil {
			final = *ev.Message
		}
	}
	if final.ID == "" {
		if cancelled {
			if err := ctx.Err(); err != nil {
				return core.Message{}, err
			}
			return core.Message{}, context.Canceled
		}
		return core.Message{}, errors.New("agent finished without final message")
	}
	return final, nil
}

func (a *Agent) RunStream(ctx context.Context, sessionID, input string) (<-chan AgentEvent, error) {
	return a.RunStreamWithOptions(ctx, sessionID, input, false)
}
