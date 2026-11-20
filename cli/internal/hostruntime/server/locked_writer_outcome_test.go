package server

import (
	"errors"
	"io"
	"testing"
)

// A write that succeeded stays a success when the writer stopped before its caller looked: the peer closing right
// after reading its answer (the control plane's create call does) must not make the answer count as undelivered.
func TestLockedWriterOutcomeKeepsASuccessfulWriteWhenTheWriterHasStopped(t *testing.T) {
	w := &lockedWriter{done: make(chan struct{})}
	close(w.done)
	for range 200 { // a select with both cases ready picks one at random
		result := make(chan error, 1)
		result <- nil
		if err := w.outcome(result); err != nil {
			t.Fatalf("a delivered write reported %v after the writer stopped", err)
		}
	}
	if err := w.outcome(make(chan error, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("a write that never ran reported %v, want io.ErrClosedPipe", err)
	}
}
