package mesh

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// lostWakeupConn blocks its first write until the write deadline, as gonet's
// UDPConn.WriteTo does when the writable notification fires before it registers.
type lostWakeupConn struct {
	ConnPacketConn
	mu       sync.Mutex
	deadline time.Time
	writes   int
}

func (c *lostWakeupConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *lostWakeupConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	first, deadline := c.writes == 1, c.deadline
	c.mu.Unlock()
	if !first {
		return len(b), nil
	}
	if deadline.IsZero() {
		select {} // the unbounded wait the product must never reach
	}
	time.Sleep(time.Until(deadline))
	return 0, os.ErrDeadlineExceeded
}

func shortWriteStall(t *testing.T) {
	previous := writeStall
	writeStall = 100 * time.Millisecond
	t.Cleanup(func() { writeStall = previous })
}

func TestStallBoundedWriteRetriesALostWakeup(t *testing.T) {
	shortWriteStall(t)
	fake := &lostWakeupConn{}
	conn := newStallBoundedConn(fake)
	done := make(chan error, 1)
	go func() { _, err := conn.Write([]byte("datagram")); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write after a lost wakeup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write stayed blocked past the stall bound")
	}
	if fake.writes != 2 {
		t.Fatalf("writes = %d, want the stalled one and its retry", fake.writes)
	}
}

func TestStallBoundedWriteKeepsTheCallersEarlierDeadline(t *testing.T) {
	shortWriteStall(t)
	fake := &lostWakeupConn{}
	conn := newStallBoundedConn(fake)
	if err := conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("datagram")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write error = %v, want the caller's deadline", err)
	}
	if fake.writes != 1 {
		t.Fatalf("writes = %d, a caller deadline must not be retried", fake.writes)
	}
}
