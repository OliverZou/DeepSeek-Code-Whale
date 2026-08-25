package daemon

import (
	"sync"
)

// SSEEvent 是写入 SSE 连接的单条消息，由 EventHub 广播给所有订阅者。
// 前端通过 SSE 的 event: 字段（channel）与 data: 字段（JSON 载荷）区分渲染。
type SSEEvent struct {
	// Name 对应 SSE 的 "event:" 字段，取值为 Channel 的字符串形式（agent/team/dag）。
	Name string
	// Data 对应 SSE 的 "data:" 字段，为 Event 的 JSON 编码结果。
	Data []byte
}

// subscriber 是 EventHub 的一个活跃 SSE 连接。
// done 在 Close 或 cancel 时被关闭，用于唤醒订阅方的 Select 循环。
// once 保证 done 只被关闭一次：Close() 与 cancel() 都经由 closeDone() 关闭它，
// 避免服务 Shutdown 关闭 hub 后，连接结束时再次 close 造成 panic。
type subscriber struct {
	ch   chan SSEEvent
	done chan struct{}
	once sync.Once
	wh   func() <-chan struct{} // 兼容：返回 done 的只读视图
}

// closeDone 通过 once 关闭 done，保证 Close() 与 cancel() 互不重复关闭。
func (s *subscriber) closeDone() {
	s.once.Do(func() { close(s.done) })
}

// EventHub 并发安全地管理多个 SSE 连接，向所有订阅者广播事件。
// 订阅者阻塞在自身 channel 上；广播为无阻塞写入，慢消费者会被跳过
// （丢弃最旧事件），避免单个慢连接拖垮整个服务。
type EventHub struct {
	mu        sync.RWMutex
	subs      map[*subscriber]struct{}
	closed    bool
	bufferLen int
}

// NewEventHub 创建一个 EventHub。bufferLen 为每个订阅者的积压缓冲大小。
func NewEventHub(bufferLen int) *EventHub {
	if bufferLen <= 0 {
		bufferLen = 64
	}
	return &EventHub{
		subs:      make(map[*subscriber]struct{}),
		bufferLen: bufferLen,
	}
}

// Subscribe 注册一个新连接，返回其接收通道与取消函数。
// 调用方必须在连接结束时调用 cancel，否则连接会持续占用资源。
func (h *EventHub) Subscribe() (<-chan SSEEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		// 服务已关闭：返回一个立即关闭的通道，避免死锁。
		ch := make(chan SSEEvent)
		close(ch)
		return ch, func() {}
	}

	done := make(chan struct{})
	sub := &subscriber{ch: make(chan SSEEvent, h.bufferLen), done: done}
	sub.wh = func() <-chan struct{} { return done }
	h.subs[sub] = struct{}{}

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, sub)
		sub.closeDone()
	}
	return sub.ch, cancel
}

// Broadcast 将事件写入所有订阅者。无阻塞：若某订阅者缓冲区已满，
// 丢弃最旧事件以腾出空间（慢消费者降级为最新事件优先）。
func (h *EventHub) Broadcast(ev SSEEvent) {
	h.mu.RLock()
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.RUnlock()

	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			// 缓冲区已满：丢弃最旧事件，再尝试放入最新事件。
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// Close 关闭所有订阅者连接，释放资源。幂等。
func (h *EventHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.ch) // 关闭事件流通道，订阅方读取时收到 ok==false 从而退出
		s.closeDone()
	}
}

// SubscriberCount 返回当前活跃订阅者数量（用于 healthz 观测）。
func (h *EventHub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
