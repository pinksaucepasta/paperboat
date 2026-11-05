package resolver

import (
	"errors"
	"testing"
)

func TestTerminalInputQueueBoundsAndCompletion(t *testing.T) {
	queue := NewTerminalInputQueue(1)
	first, err := queue.Enqueue([]byte("one"))
	if err != nil || first != 1 {
		t.Fatalf("first=%d err=%v", first, err)
	}
	if _, err := queue.Enqueue([]byte("two")); !errors.Is(err, ErrTerminalInputQueueFull) {
		t.Fatalf("overflow err=%v", err)
	}
	queue.Complete(first)
	second, err := queue.Enqueue([]byte("two"))
	if err != nil || second != 2 {
		t.Fatalf("second=%d err=%v", second, err)
	}
}

// A reattach delivers each keystroke exactly once and never blocks later input: what the host has
// passed is decided (not sent again), its last sequence and everything after it go again in order.
func TestTerminalInputQueueReconcileResendsWhatTheHostHasNotAnswered(t *testing.T) {
	queue := NewTerminalInputQueue(8)
	for _, data := range []string{"one", "two", "three", "four"} {
		if _, err := queue.Enqueue([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	decided, resend, _ := queue.Reconcile(2)
	if len(decided) != 1 || decided[0].Sequence != 1 {
		t.Fatalf("decided=%#v", decided)
	}
	if len(resend) != 3 || resend[0].Sequence != 2 || resend[1].Sequence != 3 || resend[2].Sequence != 4 || string(resend[2].Data) != "four" {
		t.Fatalf("resend=%#v", resend)
	}
	next, err := queue.Enqueue([]byte("five"))
	if err != nil || next != 5 {
		t.Fatalf("input after a reattach: next=%d err=%v", next, err)
	}
	for _, s := range []uint64{2, 3, 4, 5} {
		queue.Complete(s)
	}
	if decided, resend, unconfirmed := queue.Reconcile(5); len(decided)+len(resend)+len(unconfirmed) != 0 {
		t.Fatalf("answered input came back: %#v %#v", decided, resend)
	}
}

// A host that does not know the numbering (a new attachment) cannot match unanswered input; it is
// unconfirmed (never resent, the user is told) and numbering continues from the host's sequence, so the next keystroke is contiguous.
func TestTerminalInputQueueReconcileFollowsANewAttachment(t *testing.T) {
	queue := NewTerminalInputQueue(8)
	for range 3 {
		if _, err := queue.Enqueue([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	queue.Complete(1)
	decided, resend, unconfirmed := queue.Reconcile(0)
	if len(decided) != 0 || len(resend) != 0 || len(unconfirmed) != 2 {
		t.Fatalf("decided=%#v resend=%#v unconfirmed=%#v", decided, resend, unconfirmed)
	}
	if next, err := queue.Enqueue([]byte("y")); err != nil || next != 1 {
		t.Fatalf("next=%d err=%v", next, err)
	}
}
