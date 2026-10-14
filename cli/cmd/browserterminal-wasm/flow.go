package main

import (
	"errors"
	"sync"
)

const terminalWriteQueueBytes = (1 << 20) + 4096
const terminalWriteQueueDepth = 32
const terminalACKBytes = 64 << 10

type terminalWrite struct {
	kind     byte
	payloads [][]byte
	bytes    int
}

// Bytes remain reserved through the active write, so a slow socket cannot
// grow memory beyond the queue's budget. Admission never waits on the socket.
type terminalWriterQueue struct {
	mu        sync.Mutex
	items     chan terminalWrite
	bytes     int
	closed    bool
	ackLatest uint64
	ackSent   uint64
	ackNotify chan struct{}
}

func newTerminalWriterQueue() *terminalWriterQueue {
	return &terminalWriterQueue{items: make(chan terminalWrite, terminalWriteQueueDepth), ackNotify: make(chan struct{}, 1)}
}

func (q *terminalWriterQueue) enqueue(item terminalWrite) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || item.bytes > terminalWriteQueueBytes-q.bytes {
		return errors.New("terminal write queue is closed or full")
	}
	select {
	case q.items <- item:
		q.bytes += item.bytes
		return nil
	default:
		return errors.New("terminal write queue is full")
	}
}

func (q *terminalWriterQueue) release(item terminalWrite) {
	q.mu.Lock()
	q.bytes -= item.bytes
	q.mu.Unlock()
}

func (q *terminalWriterQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	for {
		select {
		case item := <-q.items:
			q.release(item)
		default:
			return
		}
	}
}

func (q *terminalWriterQueue) ack(sequence uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errors.New("terminal write queue is closed")
	}
	if sequence > q.ackLatest {
		q.ackLatest = sequence
	}
	if q.ackLatest > q.ackSent {
		select {
		case q.ackNotify <- struct{}{}:
		default:
		}
	}
	return nil
}

func (q *terminalWriterQueue) pendingACK() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ackLatest <= q.ackSent {
		return 0
	}
	return q.ackLatest
}

func (q *terminalWriterQueue) sentACK(sequence uint64) {
	q.mu.Lock()
	q.ackSent = sequence
	q.mu.Unlock()
}

func (q *terminalWriterQueue) urgentACK() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ackLatest-q.ackSent >= terminalACKBytes
}

const terminalBroadcastQueueBytes = 512 << 10

// Charge metadata together with ciphertext so tiny valid records cannot hit an
// unrelated record limit before the byte budget. The fixed channel's 8192 slice
// slots add 192KiB on wasm (24-byte Go slice headers).
const terminalBroadcastRecordMetadataBytes = 64
const terminalBroadcastQueueDepth = terminalBroadcastQueueBytes / terminalBroadcastRecordMetadataBytes

// The reservation survives dequeue while the consumer waits for its epoch key.
// Both transport backlog and pending authentication share one byte budget.
type terminalReceiveQueue struct {
	mu     sync.Mutex
	items  chan []byte
	bytes  int
	closed bool
}

func newTerminalReceiveQueue() *terminalReceiveQueue {
	return &terminalReceiveQueue{items: make(chan []byte, terminalBroadcastQueueDepth)}
}

func (q *terminalReceiveQueue) copyAndEnqueue(size int, copyRecord func([]byte) bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || size < 1 || size > terminalBroadcastQueueBytes-terminalBroadcastRecordMetadataBytes-q.bytes || len(q.items) == cap(q.items) {
		return errors.New("terminal output receive queue is closed or full")
	}
	data := make([]byte, size)
	if !copyRecord(data) {
		return errors.New("terminal output could not be copied")
	}
	q.items <- data // Capacity checked under the sole producer lock; never waits.
	q.bytes += size + terminalBroadcastRecordMetadataBytes
	return nil
}

func (q *terminalReceiveQueue) release(data []byte) {
	q.mu.Lock()
	q.bytes -= len(data) + terminalBroadcastRecordMetadataBytes
	q.mu.Unlock()
}

func (q *terminalReceiveQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	for {
		select {
		case data := <-q.items:
			q.release(data)
		default:
			return
		}
	}
}
