package memory

import (
	"strings"
	"testing"

	"github.com/usewhale/whale/internal/core"
)

// TestAppendTruncatesOversizedToolCallInput locks the core token-saving fix:
// a tool_call whose input embeds a large payload (e.g. `write` with the full
// file content) must be truncated before it enters the history log, so it is
// not replayed to the model on every subsequent turn. This was the root of the
// "token explosion" where a 383-line write produced a ~159 KB tool_call input
// that got re-sent ~17 times (~2M tokens of pure history replay).
func TestAppendTruncatesOversizedToolCallInput(t *testing.T) {
	log := NewAppendOnlyLog()
	bigInput := strings.Repeat("function game(){}\n", 5000) // > 24 KB
	log.Append(core.Message{
		Role: core.RoleAssistant,
		ToolCalls: []core.ToolCall{
			{ID: "tc-1", Name: "write", Input: `{"file_path":"game.js","content":"` + bigInput + `"}`},
		},
	})

	entries := log.Entries()
	if len(entries) != 1 || len(entries[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 message with 1 tool_call, got %+v", entries)
	}
	truncated := entries[0].ToolCalls[0].Input
	if len(truncated) >= len(bigInput) {
		t.Fatalf("tool_call input not truncated: %d bytes kept (original ~%d)", len(truncated), len(bigInput))
	}
	if !strings.Contains(truncated, "[tool_call input truncated") {
		t.Fatalf("truncation marker missing in: %.120s", truncated)
	}
	// The head/tail must be preserved (file_path stays visible).
	if !strings.Contains(truncated, `"file_path":"game.js"`) {
		t.Fatalf("leading context lost: %.160s", truncated)
	}
}

// TestAppendLeavesSmallToolCallUntouched guards that ordinary small tool calls
// are byte-for-byte preserved — the truncation must not alter normal tool use.
func TestAppendLeavesSmallToolCallUntouched(t *testing.T) {
	log := NewAppendOnlyLog()
	smallInput := `{"file_path":"game.js","search":"a","replace":"b"}`
	log.Append(core.Message{
		Role: core.RoleAssistant,
		ToolCalls: []core.ToolCall{
			{ID: "tc-1", Name: "edit", Input: smallInput},
		},
	})
	got := log.Entries()[0].ToolCalls[0].Input
	if got != smallInput {
		t.Fatalf("small tool_call input changed: %q", got)
	}
}
