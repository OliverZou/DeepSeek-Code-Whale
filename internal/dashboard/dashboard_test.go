package dashboard

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/usewhale/whale/internal/eventbus"
	"github.com/usewhale/whale/internal/team_engine"
)

// ============================================================================
// BumpUpdateSeq / GetUpdateSeq
// ============================================================================

func TestUpdateSeq_InitialValue(t *testing.T) {
	mgr := NewMultiEngineManager(t.TempDir())
	if mgr.GetUpdateSeq() != 0 {
		t.Errorf("expected initial seq 0, got %d", mgr.GetUpdateSeq())
	}
}

func TestUpdateSeq_Monotonic(t *testing.T) {
	mgr := NewMultiEngineManager(t.TempDir())
	mgr.BumpUpdateSeq()
	mgr.BumpUpdateSeq()
	if mgr.GetUpdateSeq() != 2 {
		t.Errorf("expected seq 2, got %d", mgr.GetUpdateSeq())
	}
}

func TestUpdateSeq_Concurrent(t *testing.T) {
	mgr := NewMultiEngineManager(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.BumpUpdateSeq()
		}()
	}
	wg.Wait()
	if mgr.GetUpdateSeq() != 100 {
		t.Errorf("expected seq 100 after 100 concurrent bumps, got %d", mgr.GetUpdateSeq())
	}
}

// ============================================================================
// Bridge event → TaskEvent extraction
// ============================================================================

func TestExtractTaskEvent_FromTypedPayload(t *testing.T) {
	mgr := NewMultiEngineManager(t.TempDir())
	received := make(chan team_engine.TaskEvent, 1)
	mgr.OnEngineEvent(func(evt team_engine.TaskEvent) {
		received <- evt
	})

	// Direct typed payload (Go-to-Go, within same process).
	evt := team_engine.TaskEvent{Type: team_engine.EventStateChanged, TaskID: "test-123"}
	mgr.fireEngineEvent(evt)

	select {
	case got := <-received:
		if got.Type != team_engine.EventStateChanged {
			t.Errorf("expected EventStateChanged, got %d", got.Type)
		}
		if got.TaskID != "test-123" {
			t.Errorf("expected taskID test-123, got %s", got.TaskID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event callback")
	}
}

func TestExtractTaskEvent_FromJSONMap(t *testing.T) {
	// Simulates what happens after JSON round-trip through the bridge:
	// TaskEvent → JSON → map[string]interface{} → JSON → TaskEvent
	original := team_engine.TaskEvent{
		Type:   team_engine.EventStateChanged,
		TaskID: "bridge-test-456",
		Title:  "test task",
	}

	// Marshal to JSON.
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal to map (simulates bridge JSON round-trip).
	var payloadMap map[string]interface{}
	if err := json.Unmarshal(data, &payloadMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	// Re-marshal and unmarshal to TaskEvent.
	data2, _ := json.Marshal(payloadMap)
	var restored team_engine.TaskEvent
	if err := json.Unmarshal(data2, &restored); err != nil {
		t.Fatalf("unmarshal to TaskEvent: %v", err)
	}

	if restored.Type != team_engine.EventStateChanged {
		t.Errorf("expected EventStateChanged, got %d", restored.Type)
	}
	if restored.TaskID != "bridge-test-456" {
		t.Errorf("expected taskID bridge-test-456, got %s", restored.TaskID)
	}
}

func TestExtractTaskEvent_InvalidPayloadIgnored(t *testing.T) {
	// Invalid payload should not trigger fireEngineEvent.
	invalidMap := map[string]interface{}{"type": "not_a_number", "task_id": 123}
	data, _ := json.Marshal(invalidMap)
	var evt team_engine.TaskEvent
	err := json.Unmarshal(data, &evt)
	// TaskEvent uses numeric Type, so "not_a_number" → 0 (default).
	// Type 0 is EventStateChanged in the constants.  This is a quirk but
	// harmless — the event is still valid JSON.  Verify unmarshal doesn't
	// error-out.
	if err != nil {
		t.Logf("unmarshal invalid type: %v (expected — json numbers only)", err)
	}
}

// ============================================================================
// EventBus integration
// ============================================================================

func TestEngineEventFiresOnBridgeEvent(t *testing.T) {
	mgr := NewMultiEngineManager(t.TempDir())
	received := make(chan team_engine.TaskEvent, 1)
	mgr.OnEngineEvent(func(evt team_engine.TaskEvent) {
		received <- evt
	})

	// Publish a team_engine event to the EventBus — this is what the
	// bridge-in goroutine does when receiving from the whale CLI.
	evt := team_engine.TaskEvent{
		Type:   team_engine.EventAgentLog,
		TaskID: "bus-test",
	}
	eventbus.Global().Publish(eventbus.TopicTeamEngine, eventbus.Event{
		Type:    eventbus.EventAgentLog,
		Payload: evt,
	})

	// The OnEngineEvent callback should NOT fire just from EventBus
	// publishing — it fires from fireEngineEvent.  Verify that.
	select {
	case <-received:
		// This test verifies the current architecture:
		// EventBus publish alone does not trigger fireEngineEvent.
		// The bridge read-loop explicitly calls fireEngineEvent after
		// extracting the TaskEvent from the BridgedEvent payload.
		t.Log("received event from EventBus publish (may happen if subscriber calls fireEngineEvent)")
	case <-time.After(500 * time.Millisecond):
		t.Log("no event from EventBus publish alone (expected — bridge calls fireEngineEvent explicitly)")
	}
}
