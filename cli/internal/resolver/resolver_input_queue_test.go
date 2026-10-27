package resolver

import (
	"errors"
	"testing"
	"time"
)

// N11: typing faster than the host answers backs up instead of failing; nothing is lost or reordered, and a
// terminal that ends stops the wait.
func TestInputQueueWaitsForRoomInsteadOfFailing(t *testing.T) {
	q := NewTerminalInputQueue(2)
	done := make(chan struct{})
	for _, key := range []string{"a", "b"} {
		if _, err := q.EnqueueWait(done, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan uint64, 1)
	go func() {
		sequence, err := q.EnqueueWait(done, []byte("c"))
		if err != nil {
			t.Error(err)
		}
		got <- sequence
	}()
	select {
	case <-got:
		t.Fatal("a full queue took more input before the host answered")
	case <-time.After(50 * time.Millisecond):
	}
	q.Complete(1)
	select {
	case sequence := <-got:
		if sequence != 3 {
			t.Fatalf("the waiting key got sequence %d, want 3", sequence)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an answer did not make room")
	}
	failed := make(chan error, 1)
	go func() { _, err := q.EnqueueWait(done, []byte("d")); failed <- err }()
	close(done)
	select {
	case err := <-failed:
		if !errors.Is(err, ErrTerminalInputQueueFull) {
			t.Fatalf("an ended terminal returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an ended terminal kept waiting")
	}
}
