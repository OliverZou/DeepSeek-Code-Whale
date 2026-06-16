// Package eventbus provides a lightweight in-process pub/sub event bus backed
// by Go channels.  It is designed for single-machine, same-process-tree
// communication between the Whale CLI, TeamEngine, and Dashboard.
//
// Cross-process bridging:
//
//	Use BridgeRead() / BridgeWrite() to connect two Bus instances in different
//	processes (e.g. dashboard ↔ whale CLI).  Events published with Publish()
//	are forwarded to BridgeRead(); events received from the peer process should
//	be injected via BridgeWrite(), which publishes them locally without
//	re-forwarding, preventing infinite loops.
//
// Topics use dotted-namespace convention:
//
//	team_engine.state_changed
//	team_engine.task_done
//	team_engine.leader_log
//	team_engine.agent_log
//	workspace.registered
//	workspace.engine_ready
//	workspace.heartbeat
//	dashboard.command
//
// Publish is non-blocking: slow consumers skip messages rather than blocking
// the producer.  Subscribers get a buffered channel; consumers that fall
// behind lose the oldest events (ring-buffer semantics via select/default).
package eventbus

import (
	"fmt"
	"log"
	"sync"
)

// logBridge logs bridge-related messages.  Kept minimal to avoid spam.
func logBridge(format string, args ...interface{}) {
	log.Printf("eventbus: "+format, args...)
}

// Event is the universal event envelope.  The Type field matches the topic
// suffix (e.g. "state_changed"); Payload is the event-specific data.
type Event struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

// BridgedEvent wraps a topic and Event for cross-process serialization.
type BridgedEvent struct {
	Topic string `json:"topic"`
	Event Event  `json:"event"`
}

// Bus is a topic-based pub/sub event bus.
type Bus struct {
	mu    sync.RWMutex
	subs  map[string][]chan Event
	async bool // when true, Publish never blocks

	// Cross-process bridge: events published with Publish() land here
	// for forwarding to the peer process.
	bridgeOut chan BridgedEvent

	// Cross-process bridge: events injected from the peer process land
	// here and are published locally without re-forwarding.
	bridgeIn chan BridgedEvent

	// bridgeDrainOnce ensures the bridgeIn drain goroutine is started
	// exactly once.
	bridgeDrainOnce sync.Once
}

// New creates an EventBus.  async=true means Publish is non-blocking
// (slow consumers are skipped).
func New() *Bus {
	return &Bus{
		subs:  make(map[string][]chan Event),
		async: true,
	}
}

// Subscribe returns a channel that receives events published to the given
// topic.  bufSize controls the channel buffer (64 is a reasonable default).
// Call Unsubscribe with the returned channel to stop receiving.
func (b *Bus) Subscribe(topic string, bufSize int) <-chan Event {
	if bufSize <= 0 {
		bufSize = 64
	}
	ch := make(chan Event, bufSize)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscription channel from a topic.
// Safe to call multiple times.
func (b *Bus) Unsubscribe(topic string, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subs[topic]
	for i, s := range subs {
		if s == ch {
			b.subs[topic] = append(subs[:i], subs[i+1:]...)
			close(s)
			return
		}
	}
}

// Publish sends an event to all subscribers of the given topic and forwards
// it to any cross-process bridge reader (BridgeRead).  In async mode
// (default), it never blocks — slow consumers are skipped.
func (b *Bus) Publish(topic string, ev Event) {
	b.publishToSubs(topic, ev)

	// Forward to bridge for cross-process delivery.
	b.mu.RLock()
	bo := b.bridgeOut
	b.mu.RUnlock()
	if bo != nil {
		be := BridgedEvent{Topic: topic, Event: ev}
		select {
		case bo <- be:
		default:
			// Bridge consumer is slow — drop.
			logBridge("Publish: bridgeOut full, dropped topic=%s type=%s", topic, ev.Type)
		}
	}
}

// PublishLocal publishes an event WITHOUT forwarding it to the bridge.
// Use this when injecting events received from another process to avoid
// infinite bridge loops.
func (b *Bus) PublishLocal(topic string, ev Event) {
	b.publishToSubs(topic, ev)
}

// publishToSubs delivers an event to all subscribers of a topic.
func (b *Bus) publishToSubs(topic string, ev Event) {
	b.mu.RLock()
	subs := make([]chan Event, len(b.subs[topic]))
	copy(subs, b.subs[topic])
	b.mu.RUnlock()

	for _, ch := range subs {
		if b.async {
			select {
			case ch <- ev:
			default:
				// Slow consumer — drop the event to avoid blocking publisher.
			}
		} else {
			ch <- ev
		}
	}
}

// EnableBridge initialises the cross-process bridge channels and starts a
// background goroutine that reads from bridgeIn and publishes events locally
// (without re-forwarding).  Returns:
//
//	readCh  — bridge out: events published in THIS process (send to peer)
//	writeCh — bridge in:  events from the peer process (caller writes here)
//
// Call EnableBridge once per Bus instance.
func (b *Bus) EnableBridge() (readCh <-chan BridgedEvent, writeCh chan<- BridgedEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bridgeOut == nil {
		b.bridgeOut = make(chan BridgedEvent, 128)
	}
	if b.bridgeIn == nil {
		b.bridgeIn = make(chan BridgedEvent, 128)
	}

	// Drain: inject events from the peer process into local subscribers.
	b.bridgeDrainOnce.Do(func() {
		go func() {
			logBridge("bridge drain started")
			for be := range b.bridgeIn {
				logBridge("bridge inject: topic=%s type=%s", be.Topic, be.Event.Type)
				b.PublishLocal(be.Topic, be.Event)
			}
		}()
	})

	return b.bridgeOut, b.bridgeIn
}

// PublishSimple is a convenience wrapper that builds an Event from a type
// string and payload.
func (b *Bus) PublishSimple(topic, eventType string, payload interface{}) {
	b.Publish(topic, Event{Type: eventType, Payload: payload})
}

// NumSubscribers returns the number of subscribers for a topic (useful for
// debug logging).
func (b *Bus) NumSubscribers(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[topic])
}

// Topics returns a copy of the current topic list.
func (b *Bus) Topics() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	topics := make([]string, 0, len(b.subs))
	for t := range b.subs {
		topics = append(topics, t)
	}
	return topics
}

// String returns a debug representation.
func (b *Bus) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s := "EventBus topics:\n"
	for t, subs := range b.subs {
		s += fmt.Sprintf("  %s: %d subscribers\n", t, len(subs))
	}
	s += fmt.Sprintf("  bridgeOut: %v\n", b.bridgeOut != nil)
	s += fmt.Sprintf("  bridgeIn: %v\n", b.bridgeIn != nil)
	return s
}

// ---- Pre-defined topic constants and event types ----

const (
	// TopicTeamEngine is the root topic for all TeamEngine events.
	TopicTeamEngine = "team_engine"

	// TopicWorkspace is the root topic for workspace lifecycle events.
	TopicWorkspace = "workspace"

	// TopicDashboard carries commands from Dashboard to Whale CLI.
	TopicDashboard = "dashboard"

	// --- TeamEngine event types (Payload is team_engine.TaskEvent) ---
	EventStateChanged   = "state_changed"
	EventWorkerOutput   = "worker_output"
	EventVerifierResult = "verifier_result"
	EventTaskDone       = "task_done"
	EventLeaderLog      = "leader_log"
	EventAgentLog       = "agent_log"
	EventSyncMasterTasks = "sync_master_tasks"
	EventSyncSubtasks    = "sync_subtasks"

	// --- Workspace event types ---
	EventWSRegistered  = "registered"
	EventWSEngineReady = "engine_ready" // DB file created, payload: workspace path
	EventWSHeartbeat   = "heartbeat"

	// --- Dashboard command event types ---
	EventCmdResume       = "resume"
	EventCmdCancelMaster = "cancel_master"
)

// ---- Global bus ----

var global = New()

// Global returns the process-wide event bus singleton.
func Global() *Bus { return global }

// SetGlobal replaces the global bus (for testing).
func SetGlobal(b *Bus) { global = b }

// EnableGlobalBridge is a convenience wrapper: calls EnableBridge on the
// global Bus singleton.
func EnableGlobalBridge() (readCh <-chan BridgedEvent, writeCh chan<- BridgedEvent) {
	return global.EnableBridge()
}
