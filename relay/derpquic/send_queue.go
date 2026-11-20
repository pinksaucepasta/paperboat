package derpquic

import "sync"

// sendQueueBytes is how much a peer's carrier may have waiting at the relay: about 60 ms at the carrier ceiling (carrierBytesPerSecond).
// A BBR sender keeps up to two bandwidth-delay products in flight and its excess waits here. A queue of 64 packets (about 6 ms at
// 100 Mbit/s) made the relay drop 4-12% of the datagrams of a jittery 250 ms path; with 4096 the drops fell to 0.1-0.6%. Tailscale's DERP
// keeps only 32 packets (derpserver.go defaultPerClientSendQueueDepth) because its senders are TCP and feel the back-pressure; these
// datagrams only ever see a drop. Over the budget a datagram is dropped, as before.
const sendQueueBytes = carrierBytesPerSecond / 16

// receiveDepth is how many packets a relay client buffers for its reader; a full buffer drops, like the relay's queue.
const receiveDepth = 4096

// sendQueue is a peer's outbound queue, bounded in bytes. It grows as it fills and lets go of its memory when it empties,
// so an idle peer costs nothing (a channel of the same depth would be allocated for every connection up front).
type sendQueue struct {
	mu    sync.Mutex
	items []queued
	head  int
	bytes int
	wake  chan struct{} // holds a token while messages wait
}

func newSendQueue() *sendQueue { return &sendQueue{wake: make(chan struct{}, 1)} }

// push appends m unless that would take the queue over sendQueueBytes.
func (q *sendQueue) push(m queued) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.bytes+len(m.data) > sendQueueBytes {
		return false
	}
	q.items = append(q.items, m)
	q.bytes += len(m.data)
	q.signal()
	return true
}

// pop removes the oldest message. It passes the wake token on while more are waiting, so a reader that handles one message per wake
// still sees control frames and shutdown between them.
func (q *sendQueue) pop() (queued, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.head == len(q.items) {
		return queued{}, false
	}
	m := q.items[q.head]
	q.items[q.head] = queued{}
	q.head++
	q.bytes -= len(m.data)
	switch {
	case q.head == len(q.items):
		q.head = 0
		if cap(q.items) > 256 {
			q.items = nil
		} else {
			q.items = q.items[:0]
		}
	case q.head >= 1024 && 2*q.head >= len(q.items):
		n := copy(q.items, q.items[q.head:])
		clear(q.items[n:])
		q.items, q.head = q.items[:n], 0
		q.signal()
	default:
		q.signal()
	}
	return m, true
}

func (q *sendQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
