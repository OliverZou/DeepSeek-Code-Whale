package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
					Step   string   `json:"step"`
					Status string   `json:"status"`
					Files  []string `json:"files"`
				} `json:"plan"`
			}
			if err := json.Unmarshal([]byte(tc.Input), &input); err != nil {
				continue
			}
			changed := sessionFilesChanged(msgs)
			pending := 0
			var untouched []string
			for _, s := range input.Plan {
				if s.Status != "completed" {
					pending++
				}
				// A pending step that declared target files but none of them
				// have been touched yet is genuinely incomplete.
				if s.Status != "completed" && len(s.Files) > 0 {
					allUntouched := true
					for _, f := range s.Files {
						if changed[normalizeChangedPath(f)] {
							allUntouched = false
							break
						}
					}
					if allUntouched {
						untouched = append(untouched, strings.Join(s.Files, ", "))
					}
				}
			}
			if pending > 0 {
				msg := fmt.Sprintf("Plan: %d/%d steps done, %d pending.", len(input.Plan)-pending, len(input.Plan), pending)
				if len(untouched) > 0 {
					msg += fmt.Sprintf(" Untouched target file(s): %s.", strings.Join(untouched, "; "))
				}
				if len(changed) > 0 {
					msg += fmt.Sprintf(" %d file(s) modified this session.", len(changed))
				}
				return msg + " Update progress with update_plan."
			}
			msg := fmt.Sprintf("All %d plan steps completed.", len(input.Plan))
			if len(changed) > 0 {
				msg += fmt.Sprintf(" %d file(s) modified.", len(changed))
			}
			return msg
		}
	}
	return ""
}

// normalizeChangedPath lowercases and normalizes slashes in a file path so
// plan step target files compare reliably against recorded mutations.
func normalizeChangedPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
}

// sessionFilesChanged returns the set of normalized file paths modified by
// edit/write/multi_edit/ast_edit/ast_patch calls across the session.
func sessionFilesChanged(msgs []core.Message) map[string]bool {
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
				seen[normalizeChangedPath(input.FilePath)] = true
			}
		}
	}
	return seen
}

// hasPendingPlanSteps reports whether the most recent update_plan still has
// steps whose status is not "completed".
func (a *Agent) hasPendingPlanSteps(ctx context.Context, sessionID string) bool {
	msgs, err := a.store.List(ctx, sessionID)
	if err != nil {
		return false
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
					Status string `json:"status"`
				} `json:"plan"`
			}
			if err := json.Unmarshal([]byte(tc.Input), &input); err != nil {
				return false
			}
			for _, s := range input.Plan {
				if s.Status != "completed" {
					return true
				}
			}
			return false
		}
	}
	return false
}
