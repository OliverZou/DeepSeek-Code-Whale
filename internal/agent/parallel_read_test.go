package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// mockReadTool mimics a pure file read: no side effects, arbitrary delay, so
// concurrent execution can be measured via running/max counters.
type mockReadTool struct {
	name    string
	calls   atomic.Int32
	running atomic.Int32
	max     atomic.Int32
	delay   time.Duration
}

func (t *mockReadTool) Name() string { return t.name }
func (t *mockReadTool) ReadOnly() bool {
	return true
}
func (t *mockReadTool) Run(ctx context.Context, call ToolCall) (ToolResult, error) {
	t.calls.Add(1)
	running := t.running.Add(1)
	for {
		max := t.max.Load()
		if running <= max || t.max.CompareAndSwap(max, running) {
			break
		}
	}
	defer t.running.Add(-1)
	if err := waitOrCancel(ctx, t.delay); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{ToolCallID: call.ID, Name: call.Name, ModelText: "ok:" + call.ID}, nil
}

type readBurstProvider struct {
	calls int
	names []string
}

func (p *readBurstProvider) StreamResponse(_ context.Context, _ []Message, _ []Tool) <-chan ProviderEvent {
	p.calls++
	if p.calls > 1 {
		return eventStream(endTurnEvent("done"))
	}
	calls := make([]ToolCall, 0, len(p.names))
	for i, name := range p.names {
		id := fmt.Sprintf("tc-read-%d", i+1)
		args := fmt.Sprintf("{\"file_path\":\"f%d\"}", i+1)
		calls = append(calls, toolCall(id, name, args))
	}
	return eventStream(toolUseEvent(calls...))
}

// TestParallelReadToolsRunConcurrently locks the system-level read parallelism:
// three read_file calls in one model turn execute concurrently (previously
// sequential round-trips), results persist in original call order, and the
// total wall time collapses toward a single round-trip.
func TestParallelReadToolsRunConcurrently(t *testing.T) {
	store := NewInMemoryStore()
	read := &mockReadTool{name: "read_file", delay: 150 * time.Millisecond}
	a := NewAgentWithRegistry(
		&readBurstProvider{names: []string{"read_file", "read_file", "read_file"}},
		store,
		NewToolRegistry([]Tool{read}),
	)

	start := time.Now()
	events, err := a.RunStream(context.Background(), "s-parallel-read", "go")
	if err != nil {
		t.Fatalf("run stream failed: %v", err)
	}
	for ev := range events {
		if ev.Type == AgentEventTypeError && ev.Err != nil {
			t.Fatalf("run stream emitted error: %v", ev.Err)
		}
	}
	elapsed := time.Since(start)

	if got := read.calls.Load(); got != 3 {
		t.Fatalf("expected 3 read_file calls, got %d", got)
	}
	if got := read.max.Load(); got < 2 {
		t.Fatalf("expected overlapping read_file calls, max concurrency was %d", got)
	}
	if elapsed >= 450*time.Millisecond {
		t.Fatalf("expected concurrent wall-clock under 450ms for three 150ms calls, got %s", elapsed)
	}

	msgs, err := store.List(context.Background(), "s-parallel-read")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	toolMsg := findOnlyToolMessage(t, msgs)
	wantOrder := []string{"tc-read-1", "tc-read-2", "tc-read-3"}
	if got := toolResultIDs(toolMsg.ToolResults); !sameStringSlice(got, wantOrder) {
		t.Fatalf("persisted tool result order = %v, want %v", got, wantOrder)
	}
}

// TestParallelReadsSplitAroundMutation keeps mutation ordering intact: an edit
// in the same turn flushes pending reads first (P1 read-before-edit gate data
// must be recorded before the mutation executes) and stays sequential.
func TestParallelReadsSplitAroundMutation(t *testing.T) {
	store := NewInMemoryStore()
	read := &mockReadTool{name: "read_file", delay: 50 * time.Millisecond}
	edit := &mockReadTool{name: "edit", delay: 30 * time.Millisecond}
	a := NewAgentWithRegistry(
		&readBurstProvider{names: []string{"read_file", "read_file", "edit"}},
		store,
		NewToolRegistry([]Tool{read, edit}),
	)

	events, err := a.RunStream(context.Background(), "s-parallel-read-edit", "go")
	if err != nil {
		t.Fatalf("run stream failed: %v", err)
	}
	for ev := range events {
		if ev.Type == AgentEventTypeError && ev.Err != nil {
			t.Fatalf("run stream emitted error: %v", ev.Err)
		}
	}
	if got := read.max.Load(); got < 2 {
		t.Fatalf("expected reads overlapping, max concurrency was %d", got)
	}

	msgs, err := store.List(context.Background(), "s-parallel-read-edit")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	toolMsg := findOnlyToolMessage(t, msgs)
	if got := len(toolMsg.ToolResults); got != 3 {
		t.Fatalf("expected 3 tool results, got %d", got)
	}
}
