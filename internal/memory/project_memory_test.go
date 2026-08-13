package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadProjectMemoryByPriority(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o600); err != nil {
		t.Fatalf("write agents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude"), 0o600); err != nil {
		t.Fatalf("write claude: %v", err)
	}
	pm, ok := ReadProjectMemory(dir, []string{"AGENTS.md", "CLAUDE.md"}, 8000)
	if !ok {
		t.Fatal("expected memory file found")
	}
	if !strings.HasSuffix(pm.Path, "AGENTS.md") || pm.Content != "agents" {
		t.Fatalf("unexpected memory: %+v", pm)
	}
}

func TestReadProjectMemoryTruncates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("1234567890"), 0o600); err != nil {
		t.Fatalf("write agents: %v", err)
	}
	pm, ok := ReadProjectMemory(dir, []string{"AGENTS.md"}, 5)
	if !ok {
		t.Fatal("expected memory file found")
	}
	if !pm.Truncated || !strings.Contains(pm.Content, "truncated") {
		t.Fatalf("expected truncation, got %+v", pm)
	}
}

func TestReadSavedMemory(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".whale", "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "auth-pattern.md"), []byte("use JWT"), 0o600); err != nil {
		t.Fatalf("write auth-pattern: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "build-commands.md"), []byte("go build ./..."), 0o600); err != nil {
		t.Fatalf("write build-commands: %v", err)
	}
	pm, ok := ReadSavedMemory(dir, 8000)
	if !ok {
		t.Fatal("expected saved memory found")
	}
	if !strings.Contains(pm.Content, "## auth-pattern") || !strings.Contains(pm.Content, "use JWT") {
		t.Fatalf("unexpected content: %q", pm.Content)
	}
	if !strings.Contains(pm.Content, "## build-commands") {
		t.Fatalf("expected both files, got %q", pm.Content)
	}
}

func TestReadSavedMemoryEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadSavedMemory(dir, 8000); ok {
		t.Fatal("expected no saved memory when .whale/memory missing")
	}
}
