package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/store"
)

// extractPlanProgress scans the message store for the most recent update_plan
// call and returns the parsed plan state. Returns nil if no plan found.
func extractPlanProgress(ctx context.Context, msgStore store.MessageStore, sessionID string) *planProgressState {
	if msgStore == nil {
		return nil
	}
	msgs, err := msgStore.List(ctx, sessionID)
	if err != nil {
		return nil
	}
	// Scan in reverse for the most recent update_plan call.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != core.RoleAssistant {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.Name != "update_plan" {
				continue
			}
			var input struct {
				Explanation string          `json:"explanation"`
				Plan        []planStepState `json:"plan"`
			}
			if err := json.Unmarshal([]byte(tc.Input), &input); err != nil {
				continue
			}
			if len(input.Plan) == 0 {
				continue
			}
			completed := 0
			for _, s := range input.Plan {
				if s.Status == "completed" {
					completed++
				}
			}
			return &planProgressState{
				Steps:     input.Plan,
				Completed: completed,
				Total:     len(input.Plan),
			}
		}
	}
	return nil
}

type planStepState struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type planProgressState struct {
	Steps     []planStepState
	Completed int
	Total     int
}

// renderPlanProgress formats the current plan state for system prompt injection.
func renderPlanProgress(state *planProgressState) string {
	if state == nil || state.Total == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Active Plan (%d/%d completed)\n\n", state.Completed, state.Total)
	for _, s := range state.Steps {
		switch s.Status {
		case "completed":
			fmt.Fprintf(&b, "[x] %s\n", strings.TrimSpace(s.Step))
		case "in_progress":
			fmt.Fprintf(&b, "[~] %s  ← current\n", strings.TrimSpace(s.Step))
		default:
			fmt.Fprintf(&b, "[ ] %s\n", strings.TrimSpace(s.Step))
		}
	}
	// Find the next pending step.
	for _, s := range state.Steps {
		if s.Status == "pending" {
			b.WriteString("\n")
			fmt.Fprintf(&b, "Next: %s\n", strings.TrimSpace(s.Step))
			break
		}
	}
	// Check if any step is in_progress.
	hasActive := false
	for _, s := range state.Steps {
		if s.Status == "in_progress" {
			hasActive = true
			break
		}
	}
	if state.Completed < state.Total {
		if !hasActive {
			b.WriteString("\nNo step is in progress. Pick the next pending step and mark it in_progress with update_plan before starting work.\n")
		}
		b.WriteString("Follow the plan above. Focus on the current step. ")
		b.WriteString("When done, mark it completed with update_plan, then move to the next. ")
		b.WriteString("Do not work on steps out of order unless the user asks.")
	}
	return strings.TrimSpace(b.String())
}
