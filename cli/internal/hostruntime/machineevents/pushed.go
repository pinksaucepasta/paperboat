package machineevents

import "time"

// PushHealthLifetime bounds trust in a worker's last report when the worker dies.
const PushHealthLifetime = 2 * time.Minute

// NewPushed makes local subscriptions without a control-plane waiter. The worker
// owns the only server stream; its local receivers call Push instead of Start.
func NewPushed() *Hub {
	return &Hub{fallback: DefaultFallbackInterval, subs: make(map[Topic][]*Subscription), known: make(map[Topic]int64), changed: make(chan struct{}), recovered: make(chan struct{}, 1)}
}

// Push forwards topic wakes and stream health. Healthy heartbeats renew the
// lease without waking consumers. Recovery wakes every consumer and the audit
// uploader, including after a worker restart or an expired heartbeat lease.
func (h *Hub) Push(topics []Topic, healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	wasHealthy := h.healthy && !h.lastPush.IsZero() && time.Since(h.lastPush) < PushHealthLifetime
	h.healthy, h.lastPush = healthy, time.Now()
	if healthy && !wasHealthy {
		select {
		case h.recovered <- struct{}{}:
		default:
		}
		for _, subs := range h.subs {
			for _, sub := range subs {
				sub.notify()
			}
		}
	}
	for _, topic := range topics {
		for _, sub := range h.subs[topic] {
			sub.notify()
		}
	}
}

// Generations snapshots topic cursors and the serial of cursor-free invalidations
// atomically, so local delivery neither loses recovery wakes nor repeats topic wakes.
func (h *Hub) Generations() (map[Topic]int64, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	known := make(map[Topic]int64, len(h.known))
	for topic, generation := range h.known {
		known[topic] = generation
	}
	return known, h.invalidations
}
