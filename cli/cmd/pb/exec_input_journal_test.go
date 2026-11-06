package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// hostStdin is the execution's stdin: it takes a chunk at its byte offset, drops what it already received and refuses
// a gap, as execprocess.Execution.WriteAt does.
type hostStdin struct {
	mu       sync.Mutex
	received []byte
}

func (h *hostStdin) writeAt(offset uint64, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if offset > uint64(len(h.received)) {
		return errors.New("input gap")
	}
	skip := uint64(len(h.received)) - offset
	if skip < uint64(len(data)) {
		h.received = append(h.received, data[skip:]...)
	}
	return nil
}

// cutConn is an exec connection that starts writing at stdin offset base and loses the connection after delivering
// deliver bytes: later writes are accepted by the transport (no error yet) but never arrive, then fail.
type cutConn struct {
	host    *hostStdin
	offset  uint64
	deliver int
	failAt  int // the write that returns an error (0 = never)
	writes  int
	eof     bool
}

func (c *cutConn) Write(p []byte) (int, error) {
	c.writes++
	if c.failAt != 0 && c.writes >= c.failAt {
		return 0, io.ErrClosedPipe
	}
	take := len(p)
	if c.deliver >= 0 {
		take = min(len(p), c.deliver)
		c.deliver -= take
	}
	if take > 0 {
		if err := c.host.writeAt(c.offset, p[:take]); err != nil {
			return 0, err
		}
	}
	c.offset += uint64(len(p))
	return len(p), nil
}
func (c *cutConn) Read([]byte) (int, error)        { return 0, io.EOF }
func (c *cutConn) Close() error                    { return nil }
func (c *cutConn) Resize(uint16, uint16) error     { return nil }
func (c *cutConn) Wait() (int, error)              { return 0, nil }
func (c *cutConn) Events() <-chan tunnel.ExecEvent { return nil }
func (c *cutConn) Cancel() error                   { return nil }
func (c *cutConn) Signal(string) error             { return nil }
func (c *cutConn) CloseWrite() error               { c.eof = true; return nil }
func (c *cutConn) Detach() error                   { return nil }

// pbvm4 update check: exec_stdin output did not equal its input. The old forwarder dropped the chunk a cut
// connection failed on, and bytes the transport had accepted but never delivered; the journal resends both.
func TestExecStdinSurvivesAConnectionCutMidStream(t *testing.T) {
	host := &hostStdin{}
	// The first connection delivers the first 6 bytes, "accepts" the next chunk without delivering it, then fails.
	first := &cutConn{host: host, deliver: 6, failAt: 3}
	ref := newExecConnectionRef(first)
	ctx := context.Background()
	for _, chunk := range []string{"hello ", "wor", "ld"} {
		connection, err := ref.SendInput(ctx, []byte(chunk))
		if err != nil {
			ref.Clear(connection) // as forwardExecInput does; the input stays journaled
		}
	}
	// The reattach: the replacement starts at the journal's offset and gets the whole window.
	second := &cutConn{host: host, offset: ref.BeginReattach(), deliver: -1}
	if err := ref.Set(second); err != nil {
		t.Fatal(err)
	}
	if connection, err := ref.SendInput(ctx, []byte("!")); err != nil || connection != second {
		t.Fatalf("input after the reattach: %v", err)
	}
	if err := ref.CloseInput(); err != nil || !second.eof {
		t.Fatalf("EOF not sent on the replacement: %v %v", err, second.eof)
	}
	if got := string(host.received); got != "hello world!" {
		t.Fatalf("the execution received %q, want %q", got, "hello world!")
	}
}
