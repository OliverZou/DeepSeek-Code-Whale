package team_engine

import (
	"context"
	"testing"
)

// TestRunDecomposerUsesLiteSpawner verifies the decompose path goes through
// the no-tools lite spawner when one is wired — a full subagent spawn
// resolves a planner agent definition with workspace tools, which tempted
// the model into exploratory tool calls that burned the one-shot budget and
// force-summarized the turn before the plan JSON was emitted (v13: first
// decompose draw auto-interrupted → retry → plan drifted to 6 serial
// batches).
func TestRunDecomposerUsesLiteSpawner(t *testing.T) {
	var fullCalled, liteCalled bool
	full := NewFuncSpawner(func(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
		fullCalled = true
		return SubagentResponse{Output: `[{"title":"t","description":"d","output":"f.js","role":"r","batch_id":"1","depends_on_batch":[]}]`, Success: true, SpawnerType: "adapter"}, nil
	})
	lite := NewFuncSpawner(func(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
		liteCalled = true
		return SubagentResponse{Output: `[{"title":"t","description":"d","output":"f.js","role":"r","batch_id":"1","depends_on_batch":[]}]`, Success: true}, nil
	})
	ar := NewRunner(full)
	ar.SetLiteSpawner(lite)

	res := ar.RunDecomposer("decompose this", t.TempDir(), 30*1000*1000*1000)
	if res == nil || !res.Success {
		t.Fatalf("decompose failed: %+v", res)
	}
	if liteCalled && fullCalled {
		t.Fatal("both spawners called — lite must short-circuit")
	}
	if !liteCalled {
		t.Fatalf("lite spawner not called (full=%v) — decompose must use the lite path", fullCalled)
	}
	if fullCalled {
		t.Fatal("full spawner called — decompose must NOT use the tooled subagent path")
	}
}
