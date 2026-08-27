package team_engine

import (
	"sync"
	"testing"
)

// TestDefaultRunEngineRegistration covers the single-engine-per-run contract:
// the session engine registers once, tool paths reuse it (same pointer),
// and Close auto-unregisters so a later call falls back to a fresh engine.
func TestDefaultRunEngineRegistration(t *testing.T) {
	if DefaultRunEngine() != nil {
		t.Fatal("default run engine must start unregistered")
	}
	defer ClearDefaultRunEngine()

	eng, err := New(":memory:", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	SetDefaultRunEngine(eng)
	if got := DefaultRunEngine(); got != eng {
		t.Fatal("registered engine must be returned as-is (single instance)")
	}

	// Close must unregister the engine so tool paths never reuse a closed one.
	if err := eng.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if DefaultRunEngine() != nil {
		t.Fatal("closed engine must be unregistered")
	}
}

// TestDefaultRunEngineConcurrentRegistration makes the registry safe under
// concurrent tool calls (team_run_plan / team_run firing in parallel).
func TestDefaultRunEngineConcurrentRegistration(t *testing.T) {
	eng, err := New(":memory:", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			SetDefaultRunEngine(eng)
			_ = DefaultRunEngine()
			ClearDefaultRunEngine()
			SetDefaultRunEngine(eng)
		}()
	}
	wg.Wait()
	if DefaultRunEngine() != eng {
		t.Fatal("final registration must be the same engine")
	}
	ClearDefaultRunEngine()
}
