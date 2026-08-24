package team_engine

import (
	"strings"
	"testing"
)

func saveTestMemory(t *testing.T, store *FileTaskStore, role AgentRole, key, content string) {
	t.Helper()
	mem := &MemoryEntry{
		ID:        "id-" + key,
		AgentRole: string(role),
		Key:       key,
		Content:   content,
		CreatedAt: "2026-08-20T00:00:00Z",
	}
	if err := store.SaveMemory(mem); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
}

// TestGetMemories_ExactKeyMatch locks the fix against filename-prefix matching:
// a lookup for key "game" must not also return "game_logic".
func TestGetMemories_ExactKeyMatch(t *testing.T) {
	store, err := NewFileTaskStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	saveTestMemory(t, store, RoleDeveloper, "game", "lesson A")
	saveTestMemory(t, store, RoleDeveloper, "game_logic", "lesson B")

	got, err := store.GetMemories(RoleDeveloper, "game")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 memory for key 'game', got %d", len(got))
	}
	if got[0].Key != "game" {
		t.Fatalf("expected Key 'game', got %q", got[0].Key)
	}
}

// TestBuildMemoryContext_CapsAtThree locks the injection cap: even with 5
// recent lessons for the role, the worker context carries at most 3.
func TestBuildMemoryContext_CapsAtThree(t *testing.T) {
	store, err := NewFileTaskStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		saveTestMemory(t, store, RoleDeveloper, "k", strings.Repeat("x", i+1))
	}

	eng := &TeamEngine{Store: store}
	ctx, err := eng.BuildMemoryContext(RoleDeveloper, "nomatch-key")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(ctx, "\n- "); got > 3 {
		t.Fatalf("expected at most 3 injected memories, got %d in:\n%s", got, ctx)
	}
	if ctx == "" {
		t.Fatal("expected non-empty memory context for a role with history")
	}
}
