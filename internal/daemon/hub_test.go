package daemon

import (
	"testing"
	"time"
)

func TestEventHubSubscribeBroadcast(t *testing.T) {
	h := NewEventHub(8)
	defer h.Close()
	ch, cancel := h.Subscribe()
	defer cancel()

	if h.SubscriberCount() != 1 {
		t.Fatalf("subscriber count = %d, want 1", h.SubscriberCount())
	}

	want := SSEEvent{Name: "agent", Data: []byte(`{"channel":"agent"}`)}
	h.Broadcast(want)

	select {
	case got := <-ch:
		if got.Name != want.Name || string(got.Data) != string(want.Data) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast event")
	}
}

func TestEventHubSlowConsumerDropsOldest(t *testing.T) {
	h := NewEventHub(1) // 最小缓冲，触发丢弃
	defer h.Close()
	ch, cancel := h.Subscribe()
	defer cancel()

	first := SSEEvent{Name: "team", Data: []byte("1")}
	second := SSEEvent{Name: "team", Data: []byte("2")}

	h.Broadcast(first)
	h.Broadcast(second)

	// 缓冲容量 1，第二次广播时最先事件被丢弃，仅保留最新事件。
	select {
	case got := <-ch:
		if string(got.Data) != "2" {
			t.Fatalf("got data %q, want %q (oldest should be dropped)", got.Data, "2")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	// 之后不再有数据。
	select {
	case got := <-ch:
		t.Fatalf("unexpected extra event: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventHubSubscribeAfterClose(t *testing.T) {
	h := NewEventHub(8)
	h.Close()

	ch, cancel := h.Subscribe()
	defer cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after hub close")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closed channel")
	}
}

func TestEventHubCloseIdempotent(t *testing.T) {
	h := NewEventHub(8)
	ch, cancel := h.Subscribe()
	defer cancel()

	h.Close()
	h.Close() // 第二次不 panic

	if h.SubscriberCount() != 0 {
		t.Fatalf("subscriber count = %d, want 0 after close", h.SubscriberCount())
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after hub close")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closed channel")
	}
}

func TestEventHubDefaultBufferLen(t *testing.T) {
	h := NewEventHub(0) // 无效值回退到默认 64
	defer h.Close()
	if h.bufferLen != 64 {
		t.Fatalf("bufferLen = %d, want 64", h.bufferLen)
	}
}
