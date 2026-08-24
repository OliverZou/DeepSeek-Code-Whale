package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/llm"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/store"
)

// TestContinueSubagentAppendsTurn verifies seam #1: ContinueSubagent reuses the
// same sessionID instead of minting a new one, appends a turn to the existing
// JSONL transcript, and never creates a second .meta.json.
func TestContinueSubagentAppendsTurn(t *testing.T) {
	dir := t.TempDir()
	msgStore, err := store.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	parent := core.NewToolRegistry([]core.Tool{testTool{name: "read_file", readOnly: true, capabilities: []string{CapabilityWorkspaceRead}}})
	factory := func(_ string, _ int) (llm.Provider, error) {
		return providerFunc(func(_ context.Context, _ []core.Message, _ []core.Tool) <-chan llm.ProviderEvent {
			out := make(chan llm.ProviderEvent, 1)
			go func() {
				defer close(out)
				out <- llm.ProviderEvent{
					Type: llm.EventComplete,
					Response: &llm.ProviderResponse{
						Content: "done",
						Usage:   llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
					},
				}
			}()
			return out
		}), nil
	}
	r := NewRunner(RunnerConfig{
		ProviderFactory: factory,
		ParentTools:     parent,
		MessageStore:    msgStore,
		SessionsDir:     dir,
		ParentSessionID: "parent-session",
	})

	first, err := r.SpawnSubagent(context.Background(), SpawnSubagentRequest{Task: "first task", Role: "review"})
	if err != nil {
		t.Fatalf("SpawnSubagent: %v", err)
	}
	if first.SessionID == "" {
		t.Fatalf("SpawnSubagent returned empty session id")
	}

	before, err := msgStore.List(context.Background(), first.SessionID)
	if err != nil {
		t.Fatalf("List before: %v", err)
	}
	if len(before) == 0 {
		t.Fatalf("expected a non-empty transcript after spawn")
	}

	metaPath := filepath.Join(dir, first.SessionID+".meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("meta file missing after spawn: %v", err)
	}

	second, err := r.ContinueSubagent(context.Background(), SpawnSubagentRequest{Task: "second task", Role: "review"}, first.SessionID)
	if err != nil {
		t.Fatalf("ContinueSubagent: %v", err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("ContinueSubagent must reuse session: got %q want %q", second.SessionID, first.SessionID)
	}
	if second.Report == "" {
		t.Fatalf("expected a report from the continued turn")
	}

	after, err := msgStore.List(context.Background(), first.SessionID)
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("expected transcript to grow after ContinueSubagent: before=%d after=%d", len(before), len(after))
	}

	// ContinueSubagent must not mint a second .meta.json — the original is
	// patched in place and ends completed.
	meta, err := session.LoadSessionMeta(dir, first.SessionID)
	if err != nil {
		t.Fatalf("LoadSessionMeta: %v", err)
	}
	if meta.Status != "completed" {
		t.Fatalf("meta status = %q, want completed", meta.Status)
	}
}
