package app

import (
	"context"
	"testing"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/session"
	"github.com/usewhale/whale/internal/store"
)

// TestSessionOpsFork verifies seam #2's Fork primitive: it clones the source
// transcript verbatim into a new .jsonl and records a new .meta.json with
// Kind=="fork" and the correct ParentSessionID.
func TestSessionOpsFork(t *testing.T) {
	dir := t.TempDir()
	msgStore, err := store.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	src := "src-session"
	for _, text := range []string{"hello", "world"} {
		if _, err := msgStore.Create(context.Background(), core.Message{
			SessionID: src,
			Role:      core.RoleUser,
			Text:      text,
		}); err != nil {
			t.Fatalf("seed Create: %v", err)
		}
	}

	rt := NewTeamRuntime(nil, nil, dir, msgStore)
	next, err := rt.Fork(context.Background(), src)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if next == "" || next == src {
		t.Fatalf("Fork returned invalid new session id %q", next)
	}

	srcMsgs, err := msgStore.List(context.Background(), src)
	if err != nil {
		t.Fatalf("List src: %v", err)
	}
	nextMsgs, err := msgStore.List(context.Background(), next)
	if err != nil {
		t.Fatalf("List next: %v", err)
	}
	if len(nextMsgs) != len(srcMsgs) {
		t.Fatalf("fork message count = %d, want %d", len(nextMsgs), len(srcMsgs))
	}
	for i := range srcMsgs {
		if nextMsgs[i].Text != srcMsgs[i].Text {
			t.Fatalf("fork message[%d] text = %q, want %q", i, nextMsgs[i].Text, srcMsgs[i].Text)
		}
		if nextMsgs[i].SessionID != next {
			t.Fatalf("fork message[%d] session = %q, want %q", i, nextMsgs[i].SessionID, next)
		}
	}

	meta, err := session.LoadSessionMeta(dir, next)
	if err != nil {
		t.Fatalf("LoadSessionMeta: %v", err)
	}
	if meta.Kind != "fork" {
		t.Fatalf("fork meta Kind = %q, want fork", meta.Kind)
	}
	if meta.ParentSessionID != src {
		t.Fatalf("fork meta ParentSessionID = %q, want %q", meta.ParentSessionID, src)
	}
}
