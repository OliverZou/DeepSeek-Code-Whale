package team_engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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
		if name != "node" || len(args) != 1 || args[0] != "--test" {
			t.Fatalf("got %q %v, want node --test", name, args)
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
		dir := t.TempDir()
		ok, _ := runMechanicalVerify(dir, dir, nil)
		if !ok {
			t.Fatal("expected pass when no automated tests detected")
		}
	})

	t.Run("failing declared node test -> fail with detail", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("node not available")
		}
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "test"), 0755)
		os.WriteFile(filepath.Join(dir, "test", "fail.test.js"), []byte("process.exit(1)\n"), 0644)
		ok, detail := runMechanicalVerify(dir, dir, []string{"test/fail.test.js"})
		if ok {
			t.Fatal("expected fail for a failing declared test")
		}
		if detail == "" {
			t.Fatal("expected non-empty failure detail")
		}
	})

	t.Run("passing declared node test -> pass", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("node not available")
		}
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "ok.test.js"), []byte("require('node:test');\n"), 0644)
		ok, _ := runMechanicalVerify(dir, dir, []string{"ok.test.js"})
		if !ok {
			t.Fatal("expected pass for a passing declared test")
		}
	})

	t.Run("undeclared failing test ignored -> pass", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("node not available")
		}
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "test"), 0755)
		os.WriteFile(filepath.Join(dir, "test", "fail.test.js"), []byte("process.exit(1)\n"), 0644)
		// 任务只声明了 index.html：别的任务的测试文件不能构成本任务的客观门
		// （跨任务误伤会制造注定失败的 retry 循环）。
		ok, _ := runMechanicalVerify(dir, dir, []string{"index.html"})
		if !ok {
			t.Fatal("expected pass when the only tests belong to other tasks")
		}
	})
}

func TestMechanicalTargets(t *testing.T) {
	t.Run("standalone module deliverable", func(t *testing.T) {
		work := t.TempDir()
		out := t.TempDir()
		os.MkdirAll(filepath.Join(out, "go2048"), 0755)
		os.WriteFile(filepath.Join(out, "go2048", "go.mod"), []byte("module go2048\n"), 0644)
		// The workspace root is also a module (monorepo) — it must NOT be a
		// target, or the repo's own pre-existing failures would gate the
		// deliverable.
		os.WriteFile(filepath.Join(work, "go.mod"), []byte("module root\n"), 0644)

		targets := mechanicalTargets(work, out)
		if len(targets) != 1 || targets[0] != filepath.Join(work, "go2048") {
			t.Fatalf("targets = %v, want [%s]", targets, filepath.Join(work, "go2048"))
		}
	})

	t.Run("root module deliverable", func(t *testing.T) {
		work := t.TempDir()
		out := t.TempDir()
		os.WriteFile(filepath.Join(out, "go.mod"), []byte("module x\n"), 0644)
		targets := mechanicalTargets(work, out)
		if len(targets) != 1 || targets[0] != work {
			t.Fatalf("targets = %v, want [%s]", targets, work)
		}
	})

	t.Run("no standalone module falls back to workdir", func(t *testing.T) {
		work := t.TempDir()
		out := t.TempDir()
		os.WriteFile(filepath.Join(out, "script.py"), []byte("# x"), 0644)
		targets := mechanicalTargets(work, out)
		if len(targets) != 1 || targets[0] != work {
			t.Fatalf("targets = %v, want [%s]", targets, work)
		}
	})

	t.Run("hidden dirs are skipped", func(t *testing.T) {
		work := t.TempDir()
		out := t.TempDir()
		os.MkdirAll(filepath.Join(out, ".whale", "go2048"), 0755)
		os.WriteFile(filepath.Join(out, ".whale", "go2048", "go.mod"), []byte("module x\n"), 0644)
		targets := mechanicalTargets(work, out)
		if len(targets) != 1 || targets[0] != work {
			t.Fatalf("targets = %v, want [%s]", targets, work)
		}
	})
}

// TestRunMechanicalVerify_ModuleScoped 验证目标收敛到交付 module：仓库根的
// 既有失败不得违背独立 module 交付物的验证；没有独立 module 时回退到
// workdir（改动现有文件任务的回归验证保持现状）。
func TestRunMechanicalVerify_ModuleScoped(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}

	t.Run("root pre-existing failures do not gate standalone module", func(t *testing.T) {
		work := t.TempDir()
		out := t.TempDir()

		// Workspace root: a Go module with a failing test, standing in for the
		// whale repo's pre-existing Windows failures.
		os.WriteFile(filepath.Join(work, "go.mod"), []byte("module rootmod\n"), 0644)
		os.WriteFile(filepath.Join(work, "fail_test.go"), []byte("package rootmod\nimport \"testing\"\nfunc TestBoom(t *testing.T) { t.Fatal(\"pre-existing failure\") }\n"), 0644)

		// Worker deliverable: a healthy standalone module (mirrored in workdir
		// as propagation would leave it).
		for _, d := range []struct{ dir, mod string }{{work, "work"}, {out, "out"}} {
			mod := filepath.Join(d.dir, "go2048")
			os.MkdirAll(mod, 0755)
			os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module go2048\n"), 0644)
			os.WriteFile(filepath.Join(mod, "ok_test.go"), []byte("package go2048\nimport \"testing\"\nfunc TestOK(t *testing.T) {}\n"), 0644)
		}

		ok, detail := runMechanicalVerify(work, out, nil)
		if !ok {
			t.Fatalf("standalone module deliverable must pass regardless of root failures:\n%s", detail)
		}
	})

	t.Run("no module deliverable verifies workdir itself", func(t *testing.T) {
		work := t.TempDir()
		out := t.TempDir()
		os.WriteFile(filepath.Join(work, "go.mod"), []byte("module workmod\n"), 0644)
		os.WriteFile(filepath.Join(work, "fail_test.go"), []byte("package workmod\nimport \"testing\"\nfunc TestBoom(t *testing.T) { t.Fatal(\"break\") }\n"), 0644)
		ok, _ := runMechanicalVerify(work, out, nil)
		if ok {
			t.Fatal("expected fail when the deliverable IS the root module and it fails")
		}
	})
}

func TestTruncateMechanicalDetail(t *testing.T) {
	t.Run("short output unchanged", func(t *testing.T) {
		got := truncateMechanicalDetail("ok")
		if got != "ok" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("long output truncated with marker", func(t *testing.T) {
		long := strings.Repeat("x", 20000)
		got := truncateMechanicalDetail(long)
		if len(got) >= len(long) {
			t.Fatalf("expected truncation, got %d bytes", len(got))
		}
		if !strings.Contains(got, "output truncated") {
			t.Fatalf("missing truncation marker: %q", got[len(got)-40:])
		}
	})

	t.Run("utf-8 boundary preserved", func(t *testing.T) {
		// 9999 ASCII bytes + a 3-byte rune straddling the 8192 cut.
		long := strings.Repeat("a", 8191) + "中"
		got := truncateMechanicalDetail(long)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid utf-8 after truncation: %q", got[len(got)-20:])
		}
	})
}

func TestMechanicalTargets_EmptyWorkdir(t *testing.T) {
	// 空 workdir 是任务配置 bug：mechanicalTargets 不得回退到 "."（= 进程 cwd，
	// 冒烟时是仓库根——会把仓库自带的失败算进交付物）。返回 nil = 不测任何目录。
	if got := mechanicalTargets("", t.TempDir()); got != nil {
		t.Fatalf("empty workdir must yield no targets, got %v", got)
	}
}

func TestTaskWorkdirPersistsAcrossReload(t *testing.T) {
	// 重启（rebuildIndex）后 task.Workdir 不得丢失：传播/机械验证/absOutputPath
	// 都依赖它，丢失会让路径回落 cwd。
	store, err := NewFileTaskStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	task := NewTask("t1", "t", "d", RoleDeveloper, "", 3, "D:/ws/2048", nil, "b", "m")
	if err := store.InsertTask(task); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	// 模拟重启：同一个 baseDir 重新扫描。
	store2, err := NewFileTaskStore(store.baseDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	got, err := store2.GetTask("t1")
	if err != nil || got == nil {
		t.Fatalf("get task after reload: %v", err)
	}
	if got.Workdir != "D:/ws/2048" {
		t.Fatalf("workdir lost across reload: %q", got.Workdir)
	}
}
