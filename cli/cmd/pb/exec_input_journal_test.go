package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
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

// A second blip while the journal is resent is a lost transport, not the end of the command: the reattach goes round
// again from the same journal, bounded by the same attempts.
func TestExecReattachRetriesWhenTheReplacementDropsDuringTheStdinResend(t *testing.T) {
	host := &hostStdin{}
	first := &cutConn{host: host, deliver: 3, failAt: 2}
	ref := newExecConnectionRef(first)
	for _, chunk := range []string{"abc", "def"} {
		if connection, err := ref.SendInput(context.Background(), []byte(chunk)); err != nil {
			ref.Clear(connection)
		}
	}
	var request tunnel.ExecRequest
	var dialed []*cutConn
	dial := func(context.Context, api.ExecDescriptor) (tunnel.ExecConn, error) {
		connection := &cutConn{host: host, offset: request.InputOffset, deliver: -1}
		if len(dialed) == 0 {
			connection.failAt = 1 // the replacement drops on its first write
		}
		dialed = append(dialed, connection)
		return connection, nil
	}
	descriptor := func(context.Context) (api.ExecDescriptor, error) { return api.ExecDescriptor{}, nil }
	recovery := reattach.New(reattach.Policy{Attempts: 3, Retryable: retryableExecReattachment})
	var replacement tunnel.ExecConn
	err := recovery.Run(context.Background(), func(ctx context.Context) error {
		var attemptErr error
		replacement, attemptErr = reattachExecConnection(ctx, ref, &request, descriptor, dial)
		return attemptErr
	})
	if err != nil || len(dialed) != 2 || replacement != dialed[1] {
		t.Fatalf("reattach: err=%v dials=%d", err, len(dialed))
	}
	if got := string(host.received); got != "abcdef" {
		t.Fatalf("the execution received %q, want %q", got, "abcdef")
	}
}

// Stdin no connection has accepted yet stays in the journal even past the window: trimming it would make the
// replacement start after bytes the execution never received.
func TestExecStdinJournalNeverTrimsBytesNoConnectionAccepted(t *testing.T) {
	host := &hostStdin{}
	ref := newExecConnectionRef(&cutConn{host: host, failAt: 1}) // every write fails
	data := make([]byte, execInputWindow+1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if connection, err := ref.SendInput(context.Background(), data); err == nil {
		t.Fatal("the write on the dead connection did not fail")
	} else {
		ref.Clear(connection)
	}
	replacement := &cutConn{host: host, offset: ref.BeginReattach(), deliver: -1}
	if replacement.offset != 0 {
		t.Fatalf("the replacement starts at stdin offset %d, bytes before it were never delivered", replacement.offset)
	}
	if err := ref.Set(replacement); err != nil {
		t.Fatal(err)
	}
	if string(host.received) != string(data) {
		t.Fatalf("the execution received %d bytes, want %d", len(host.received), len(data))
	}
}

// A daemon that accepts the socket and never answers cannot hang a reattach attempt: it times out as a retryable
// peer-stream failure and the bounded recovery tries again.
func TestExecReattachAttemptTimesOutAndIsRetried(t *testing.T) {
	old := execReattachAttemptTimeout
	execReattachAttemptTimeout = 50 * time.Millisecond
	defer func() { execReattachAttemptTimeout = old }()
	ref := newExecConnectionRef(&cutConn{host: &hostStdin{}})
	var request tunnel.ExecRequest
	descriptor := func(context.Context) (api.ExecDescriptor, error) { return api.ExecDescriptor{}, nil }
	silent := func(ctx context.Context, _ api.ExecDescriptor) (tunnel.ExecConn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	_, err := reattachExecConnection(context.Background(), ref, &request, descriptor, silent)
	if err == nil || !retryableExecReattachment(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("a silent daemon: err=%v after %v", err, time.Since(start))
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = reattachExecConnection(canceled, ref, &request, descriptor, silent); retryableExecReattachment(err) {
		t.Fatalf("a canceled command was retried: %v", err)
	}
}

// A permanent answer that arrives as the attempt timer fires is the answer: it is not turned into a retryable timeout.
func TestExecReattachKeepsAPermanentErrorThatRacesTheAttemptTimer(t *testing.T) {
	old := execReattachAttemptTimeout
	execReattachAttemptTimeout = 20 * time.Millisecond
	defer func() { execReattachAttemptTimeout = old }()
	ref := newExecConnectionRef(&cutConn{host: &hostStdin{}})
	var request tunnel.ExecRequest
	missing := &api.APIError{Code: "user_machine_not_found"}
	descriptor := func(ctx context.Context) (api.ExecDescriptor, error) {
		<-ctx.Done()
		return api.ExecDescriptor{}, missing
	}
	dial := func(context.Context, api.ExecDescriptor) (tunnel.ExecConn, error) {
		return nil, errors.New("not dialed")
	}
	_, err := reattachExecConnection(context.Background(), ref, &request, descriptor, dial)
	if !errors.Is(err, missing) || retryableExecReattachment(err) {
		t.Fatalf("the permanent error became %v (retryable %v)", err, retryableExecReattachment(err))
	}
}

// stallConn accepts the connection and then never completes a write until it is aborted.
type stallConn struct {
	cutConn
	aborted chan struct{}
	once    sync.Once     // Detach is idempotent, as helperExecConn's closeOnce makes it
	writing chan struct{} // optional: signalled when a write starts blocking
}

func (c *stallConn) Write([]byte) (int, error) {
	select {
	case c.writing <- struct{}{}:
	default:
	}
	<-c.aborted
	return 0, io.ErrClosedPipe
}
func (c *stallConn) Detach() error { c.once.Do(func() { close(c.aborted) }); return nil }

// A replacement that connects and then stalls while the stdin journal is resent is cut at the attempt deadline and
// retried, instead of hanging the reattach until Ctrl-C.
func TestExecReattachBoundsTheStdinResend(t *testing.T) {
	old := execReattachAttemptTimeout
	execReattachAttemptTimeout = 50 * time.Millisecond
	defer func() { execReattachAttemptTimeout = old }()
	host := &hostStdin{}
	ref := newExecConnectionRef(&cutConn{host: host, failAt: 1})
	if connection, err := ref.SendInput(context.Background(), []byte("stdin")); err != nil {
		ref.Clear(connection)
	}
	var request tunnel.ExecRequest
	descriptor := func(context.Context) (api.ExecDescriptor, error) { return api.ExecDescriptor{}, nil }
	dial := func(context.Context, api.ExecDescriptor) (tunnel.ExecConn, error) {
		return &stallConn{cutConn: cutConn{host: host}, aborted: make(chan struct{})}, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := reattachExecConnection(context.Background(), ref, &request, descriptor, dial)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !retryableExecReattachment(err) {
			t.Fatalf("a stalled resend: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled stdin resend hung the reattach")
	}
}

// A stdin write blocked on the lost connection's full flow-control window holds the journal lock. The recovery loop
// aborts that connection before BeginReattach, which unblocks the write (helperExecConn.Detach closes the session:
// TestHelperExecDetachUnblocksAStdinWriteOnAFullWindow), so the reattach is never held by it.
func TestExecReattachIsNotHeldByAStdinWriteOnAFullWindow(t *testing.T) {
	stalled := &stallConn{cutConn: cutConn{host: &hostStdin{}}, aborted: make(chan struct{}), writing: make(chan struct{}, 1)}
	ref := newExecConnectionRef(stalled)
	sent := make(chan error, 1)
	go func() { _, err := ref.SendInput(context.Background(), []byte("stdin")); sent <- err }()
	<-stalled.writing
	ref.Clear(stalled) // the recovery loop's order: clear, abort, then BeginReattach
	_ = abortExecAttachment(stalled)
	begun := make(chan uint64, 1)
	go func() { begun <- ref.BeginReattach() }()
	select {
	case offset := <-begun:
		if offset != 0 {
			t.Fatalf("the reattach starts at stdin offset %d, want 0 (nothing was accepted)", offset)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stdin write blocked on a full window held the reattach")
	}
	if err := <-sent; err == nil {
		t.Fatal("the write on the aborted connection reported success")
	}
}

// Attempts alone did not bound a recovery: 40 attempts each hanging to their own timeout took 10 minutes, while the
// backoff comment promised about half a minute. One recovery now ends at execReattachWindow, as a spent budget.
func TestExecRecoveryEndsAtTheReattachWindow(t *testing.T) {
	oldAttempt, oldWindow := execReattachAttemptTimeout, execReattachWindow
	execReattachAttemptTimeout, execReattachWindow = 100*time.Millisecond, 300*time.Millisecond
	defer func() { execReattachAttemptTimeout, execReattachWindow = oldAttempt, oldWindow }()
	ref := newExecConnectionRef(&cutConn{host: &hostStdin{}})
	var request tunnel.ExecRequest
	descriptor := func(context.Context) (api.ExecDescriptor, error) { return api.ExecDescriptor{}, nil }
	silent := func(ctx context.Context, _ api.ExecDescriptor) (tunnel.ExecConn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	recovery := reattach.New(reattach.Policy{Attempts: execReattachAttempts, Retryable: retryableExecReattachment})
	start := time.Now()
	_, err := recoverExecConnection(context.Background(), recovery, func(ctx context.Context) (tunnel.ExecConn, error) {
		return reattachExecConnection(ctx, ref, &request, descriptor, silent)
	})
	// Unbounded, 40 attempts of 100 ms take 4 s.
	if elapsed := time.Since(start); !errors.Is(err, reattach.ErrExhausted) || elapsed > 2*time.Second {
		t.Fatalf("recovery: err=%v after %v", err, elapsed)
	}
	if recovery.Remaining() == 0 {
		t.Fatal("the window did not end the recovery; the attempt budget did")
	}
}

// gateConn holds its first write until release, and records how much stdin the host had when EOF arrived.
type gateConn struct {
	cutConn
	writing   chan struct{}
	release   chan struct{}
	eofAtSize chan int
}

func (c *gateConn) Write(p []byte) (int, error) {
	if c.writes == 0 {
		close(c.writing)
		<-c.release
	}
	return c.cutConn.Write(p)
}
func (c *gateConn) CloseWrite() error {
	c.host.mu.Lock()
	c.eofAtSize <- len(c.host.received)
	c.host.mu.Unlock()
	return nil
}

// Stdin EOF that raced a reattach's journal resend reached the new connection before the resent tail: the execution
// saw EOF early and lost the tail. CloseInput now waits for the resend.
func TestExecStdinEOFWaitsForTheReattachResend(t *testing.T) {
	host := &hostStdin{}
	ref := newExecConnectionRef(&cutConn{host: host, failAt: 1})
	if connection, err := ref.SendInput(context.Background(), []byte("tail")); err != nil {
		ref.Clear(connection)
	}
	second := &gateConn{cutConn: cutConn{host: host, offset: ref.BeginReattach(), deliver: -1},
		writing: make(chan struct{}), release: make(chan struct{}), eofAtSize: make(chan int, 2)}
	set := make(chan error, 1)
	go func() { set <- ref.Set(second) }()
	<-second.writing
	closed := make(chan error, 1)
	go func() { closed <- ref.CloseInput() }()
	time.Sleep(50 * time.Millisecond) // let an unserialized CloseInput reach the new connection first
	close(second.release)
	if err := <-set; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if size := <-second.eofAtSize; size != len("tail") {
		t.Fatalf("EOF reached the execution after %d stdin bytes, want %d", size, len("tail"))
	}
}
