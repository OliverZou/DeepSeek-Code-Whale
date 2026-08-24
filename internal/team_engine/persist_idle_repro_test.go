package team_engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPersistIdleExit reproduces the verifier persistent-session idle exit.
// It spawns a real `whale exec --persist` child, sends prompt #1, reads EOT,
// idles for the given duration, then sends prompt #2 and reports whether the
// child survived and what its stderr said.
//
// Skipped by default: it needs a real whale binary + API key and takes minutes.
// Run explicitly with: WHALE_PERSIST_IDLE=1 WHALE_BIN=<path> go test -run TestPersistIdleExit -v -timeout 20m
func TestPersistIdleExit(t *testing.T) {
	if os.Getenv("WHALE_PERSIST_IDLE") == "" {
		t.Skip("set WHALE_PERSIST_IDLE=1 to run the idle-exit reproduction")
	}
	bin := os.Getenv("WHALE_BIN")
	if bin == "" {
		bin = "whale"
	}
	idle := 6 * time.Minute
	if v := os.Getenv("WHALE_PERSIST_IDLE_SEC"); v != "" {
		var sec int
		fmt.Sscanf(v, "%d", &sec)
		idle = time.Duration(sec) * time.Second
	}

	workdir := t.TempDir()
	cmd := exec.Command(bin, "exec", "--dangerously-skip-permissions", "--timeout-sec", "300", "--persist")
	cmd.Dir = workdir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("child pid=%d", cmd.Process.Pid)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	send := func(prompt string) (output string, ok bool) {
		if _, err := io.WriteString(stdin, prompt+"\n__WHALE_EOP__\n"); err != nil {
			t.Logf("write prompt: %v", err)
			return "", false
		}
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "__WHALE_EOT__" {
				return strings.Join(lines, "\n"), true
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n"), false
	}

	// Round 1.
	out1, ok1 := send("Reply with exactly: OK")
	t.Logf("round1 ok=%v output=%q", ok1, truncate(out1))

	// Start a waiter goroutine BEFORE idling so an idle-time exit is caught
	// immediately with its exit code and stderr, instead of only being
	// inferred later from a failed round 2.
	idleStart := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	t.Logf("idling for %s ...", idle)
	select {
	case werr := <-done:
		dt := time.Since(idleStart)
		code := 0
		if ee, isExit := werr.(*exec.ExitError); isExit {
			code = ee.ExitCode()
		}
		t.Logf("CHILD EXITED DURING IDLE after %.1fs (err=%v exit=%d)", dt.Seconds(), werr, code)
		t.Logf("=== stderr at exit:\n%s", stderr.String())
	case <-time.After(idle):
		t.Logf("idle complete — child survived %s", idle)
	}

	// Round 2 (only meaningful if the child is still alive).
	out2, ok2 := send("Reply with exactly: STILL_ALIVE")
	t.Logf("round2 ok=%v output=%q", ok2, truncate(out2))

	// Final wait (with timeout) and report exit code.
	select {
	case werr := <-done:
		if werr == nil {
			t.Logf("child exited: exit code 0")
		} else if ee, isExit := werr.(*exec.ExitError); isExit {
			t.Logf("child exited: exit code %d", ee.ExitCode())
		} else {
			t.Logf("child exited: %v", werr)
		}
	case <-time.After(10 * time.Second):
		t.Logf("child still running after 10s wait; killing")
		cmd.Process.Kill()
	}

	t.Logf("=== FINAL stderr:\n%s", stderr.String())
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
