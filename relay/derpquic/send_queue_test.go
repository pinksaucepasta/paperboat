package derpquic

import (
	"testing"
)

func message(n int, size int) queued {
	return queued{packet: packet{data: make([]byte, size)}, service: n%2 == 0}
}

func TestSendQueueIsFIFOAndBoundedInBytes(t *testing.T) {
	q := newSendQueue()
	const size = 1000
	pushed := 0
	for q.push(message(pushed, size)) {
		pushed++
	}
	if pushed*size > sendQueueBytes || (pushed+1)*size <= sendQueueBytes {
		t.Fatalf("queue took %d messages of %d bytes, want it full at %d bytes", pushed, size, sendQueueBytes)
	}
	for i := 0; i < pushed; i++ {
		m, ok := q.pop()
		if !ok || m.service != (i%2 == 0) {
			t.Fatalf("message %d came back out of order or missing (ok=%v)", i, ok)
		}
	}
	if _, ok := q.pop(); ok {
		t.Fatal("empty queue returned a message")
	}
	if !q.push(message(0, size)) {
		t.Fatal("an emptied queue refused a message")
	}
}

func TestSendQueueWakesUntilEmptyAndReleasesMemory(t *testing.T) {
	q := newSendQueue()
	const count = 3000
	for i := 0; i < count; i++ {
		if !q.push(message(i, 100)) {
			t.Fatalf("message %d refused", i)
		}
	}
	for i := 0; i < count; i++ {
		select {
		case <-q.wake:
		default:
			t.Fatalf("no wake token with %d messages left", count-i)
		}
		m, ok := q.pop()
		if !ok || m.service != (i%2 == 0) {
			t.Fatalf("message %d came back out of order (ok=%v)", i, ok)
		}
	}
	if _, ok := q.pop(); ok {
		t.Fatal("empty queue returned a message")
	}
	if q.items != nil || q.bytes != 0 {
		t.Fatalf("emptied queue keeps %d slots and %d bytes", cap(q.items), q.bytes)
	}
}
