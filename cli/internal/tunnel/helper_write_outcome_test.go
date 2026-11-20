package tunnel

import (
	"errors"
	"io"
	"testing"
)

// A write that completed stays a success when the link ended before its caller looked: a Resize answered just before
// stream_end must not report EOF.
func TestHelperWriteOutcomeKeepsACompletedWriteWhenTheLinkHasEnded(t *testing.T) {
	c := &helperTerminalConn{done: make(chan struct{})}
	close(c.done)
	for range 200 { // a select with both cases ready picks one at random
		result := make(chan error, 1)
		result <- nil
		if err := c.writeOutcome(result); err != nil {
			t.Fatalf("a completed write reported %v after the link ended", err)
		}
	}
	if err := c.writeOutcome(make(chan error, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("a write that never ran reported %v, want io.EOF", err)
	}
}
