package team_engine

import "strings"

// truncateStr trims s and caps it at maxLen characters, appending an ellipsis
// when truncated. Shared by the verifier and related output-formatting paths.
func truncateStr(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
