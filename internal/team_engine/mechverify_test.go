package team_engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDetectTestCommand(t *testing.T) {
	t.Run("go module", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0644)
		name, args := detectTestCommand(dir)
		if name != "go" || len(args) != 2 || args[0] != "test" || args[1] != "./..." {
			t.Fatalf("got %q %v, want go test ./...", name, args)
		}
	})

	t.Run("node test dir", func(t *testing.T) {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "test"), 0755)
		os.WriteFile(filepath.Join(dir, "test", "game.test.js"), []byte("// x"), 0644)
		name, args := detectTestCommand(dir)
		if name != "node" || len(args) != 2 || args[0] != "--test" || args[1] != "test" {
			t.Fatalf("got %q %v, want node --test test", name, args)
		}
	})

	t.Run("node root test file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "a.test.js"), []byte("// x"), 0644)
		name, args := detectTestCommand(dir)
		if name != "node" || len(args) != 1 || args[0] != "--test" {
			t.Fatalf("got %q %v, want node --test", name, args)
		}
	})

	t.Run("npm real test", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"node --test"}}`), 0644)
		name, args := detectTestCommand(dir)
		if name != "npm" || len(args) != 1 || args[0] != "test" {
			t.Fatalf("got %q %v, want npm test", name, args)
		}
	})

	t.Run("npm noop test skipped", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`), 0644)
		name, _ := detectTestCommand(dir)
		if name != "" {
			t.Fatalf("got %q, want empty (noop test skipped)", name)
		}
	})

	t.Run("no tests", func(t *testing.T) {
		name, _ := detectTestCommand(t.TempDir())
		if name != "" {
			t.Fatalf("got %q, want empty", name)
		}
	})
}

func TestRunMechanicalVerify(t *testing.T) {
	t.Run("no tests -> pass", func(t *testing.T) {
		ok, _ := runMechanicalVerify(t.TempDir())
		if !ok {
			t.Fatal("expected pass when no automated tests detected")
		}
	})

	t.Run("failing node test -> fail with detail", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("node not available")
		}
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "test"), 0755)
		os.WriteFile(filepath.Join(dir, "test", "fail.test.js"), []byte("process.exit(1)\n"), 0644)
		ok, detail := runMechanicalVerify(dir)
		if ok {
			t.Fatal("expected fail for a failing test")
		}
		if detail == "" {
			t.Fatal("expected non-empty failure detail")
		}
	})
}
