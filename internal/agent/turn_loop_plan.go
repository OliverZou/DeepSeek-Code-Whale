package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/usewhale/whale/internal/core"
)

// checkIncompletePlan scans the session history for the most recent update_plan
// call and returns a summary with plan progress and file change counts. Returns
// "" if no plan exists.
func (a *Agent) checkIncompletePlan(ctx context.Context, sessionID string) string {
	msgs, err := a.store.List(ctx, sessionID)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != core.RoleAssistant {
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			if tc.Name != "update_plan" {
				continue
			}
			var input struct {
				Plan []struct {
					Step   string `json:"step"`
					Status string `json:"status"`
				} `json:"plan"`
			}
			if err := json.Unmarshal([]byte(tc.Input), &input); err != nil {
				continue
			}
			pending := 0
			for _, s := range input.Plan {
				if s.Status != "completed" {
					pending++
				}
			}
			filesChanged := countSessionFilesChanged(msgs)
			if pending > 0 {
				msg := fmt.Sprintf("Plan: %d/%d steps done, %d pending.", len(input.Plan)-pending, len(input.Plan), pending)
				if filesChanged > 0 {
					msg += fmt.Sprintf(" %d file(s) modified this session.", filesChanged)
				}
				return msg + " Update progress with update_plan."
			}
			msg := fmt.Sprintf("All %d plan steps completed.", len(input.Plan))
			if filesChanged > 0 {
				msg += fmt.Sprintf(" %d file(s) modified.", filesChanged)
			}
			return msg
		}
	}
	return ""
}

// countSessionFilesChanged counts unique files modified by edit/write/multi_edit
// calls across all messages in the session.
func countSessionFilesChanged(msgs []core.Message) int {
	seen := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role != core.RoleAssistant {
			continue
		}
		for _, tc := range msg.ToolCalls {
			switch tc.Name {
			case "edit", "multi_edit", "write", "ast_edit", "ast_patch":
			default:
				continue
			}
			var input struct {
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal([]byte(tc.Input), &input); err != nil {
				continue
			}
			if input.FilePath != "" {
				seen[input.FilePath] = true
			}
		}
	}
	return len(seen)
}
