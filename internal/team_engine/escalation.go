package team_engine

import (
	"fmt"
	"sync"
	"time"
)

// =============================================================================
// Escalation System
//
// "Engine 遇到风险过高、需求模糊、成本超支等场景时，应 suspend 当前流程，
//  向用户请示。"
//
// The EscalationManager provides:
//   - Blocking escalate: Engine suspends and waits for user decision
//   - Non-blocking resolve: User (or agent) provides decision via CLI/API
//   - Timeout: If user doesn't respond, pipeline aborts
//   - File-based audit trail: All escalations written to board.md
// =============================================================================

// EscalationDecision is the user's response to an escalation.
type EscalationDecision string

const (
	// EscalationContinue — proceed with execution.
	EscalationContinue EscalationDecision = "continue"
	// EscalationRetry — retry the batch with adjustments.
	EscalationRetry EscalationDecision = "retry"
	// EscalationAbort — abort the entire pipeline.
	EscalationAbort EscalationDecision = "abort"
	// EscalationModify — modify the plan and continue.
	EscalationModify EscalationDecision = "modify"
)

const (
	// escalationTimeout is how long the engine waits for a user decision.
	escalationTimeout = 30 * time.Minute
)

// EscalationRequest captures why an escalation was triggered.
type EscalationRequest struct {
	BatchID    string `json:"batch_id"`
	BatchLabel string `json:"batch_label,omitempty"`
	Reason     string `json:"reason"`
	Feedback   string `json:"feedback,omitempty"`
	Cycle      int    `json:"cycle"`
	CreatedAt  string `json:"created_at"`
	TimeoutMin int    `json:"timeout_min"`
}

// EscalationManager manages pending escalations and their resolution.
type EscalationManager struct {
	mu      sync.Mutex
	pending map[string]chan EscalationDecision
}

// NewEscalationManager creates a new escalation manager.
func NewEscalationManager() *EscalationManager {
	return &EscalationManager{
		pending: make(map[string]chan EscalationDecision),
	}
}

// Escalate creates a pending escalation and blocks until resolved.
// Returns the user's decision.
func (em *EscalationManager) Escalate(req EscalationRequest) (EscalationDecision, error) {
	key := req.BatchID
	ch := make(chan EscalationDecision, 1)

	em.mu.Lock()
	em.pending[key] = ch
	em.mu.Unlock()

	defer func() {
		em.mu.Lock()
		delete(em.pending, key)
		em.mu.Unlock()
	}()

	// Block with timeout.
	select {
	case decision := <-ch:
		return decision, nil
	case <-time.After(escalationTimeout):
		return EscalationAbort, fmt.Errorf("escalation timeout after %v for batch %s", escalationTimeout, key)
	}
}

// Resolve resolves a pending escalation with the given decision.
// Returns an error if there's no pending escalation for this batch.
func (em *EscalationManager) Resolve(batchID string, decision EscalationDecision) error {
	em.mu.Lock()
	ch, ok := em.pending[batchID]
	em.mu.Unlock()

	if !ok {
		return fmt.Errorf("no pending escalation for batch %q", batchID)
	}

	select {
	case ch <- decision:
		return nil
	default:
		return fmt.Errorf("escalation for batch %q already resolved", batchID)
	}
}

// HasPending reports whether a batch has a pending escalation.
func (em *EscalationManager) HasPending(batchID string) bool {
	em.mu.Lock()
	defer em.mu.Unlock()
	_, ok := em.pending[batchID]
	return ok
}

// ListPending returns all batch IDs with pending escalations.
func (em *EscalationManager) ListPending() []string {
	em.mu.Lock()
	defer em.mu.Unlock()
	var ids []string
	for id := range em.pending {
		ids = append(ids, id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// AgentChannel extension for escalation resolution
// ---------------------------------------------------------------------------

// ResolveEscalation implements the "resolve" operation on AgentChannel.
// Users and agents can resolve a pending escalation with the same interface.
func (e *TeamEngine) ResolveEscalation(batchID string, decision EscalationDecision) error {
	if e.Escalation == nil {
		return fmt.Errorf("escalation manager not initialized")
	}

	return e.Escalation.Resolve(batchID, decision)
}
