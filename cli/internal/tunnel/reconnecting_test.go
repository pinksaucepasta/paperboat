package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/telemetry"
)

type tunnelEventSink struct{ events []telemetry.Event }

func (s *tunnelEventSink) Record(e telemetry.Event) { s.events = append(s.events, e) }

type closeTrackingConn struct {
	*reconnectTestConn
	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error { c.closed.Store(true); return nil }

type reconnectTestConn struct {
	*bytes.Reader
	code   int
	err    error
	writes bytes.Buffer
}

type compressionTestConn struct {
	*reconnectTestConn
	compression TerminalCompressionTelemetry
}

func (c *compressionTestConn) TerminalCompressionTelemetry() TerminalCompressionTelemetry {
	return c.compression
}

func (c *reconnectTestConn) Write(p []byte) (int, error) { return c.writes.Write(p) }
func (*reconnectTestConn) Close() error                  { return nil }
func (*reconnectTestConn) Resize(uint16, uint16) error   { return nil }
func (c *reconnectTestConn) Wait() (int, error)          { return c.code, c.err }

func TestReconnectingConnReattachesWithoutReplayingInput(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader([]byte("before")), err: ErrTransportLost}
	second := &reconnectTestConn{Reader: bytes.NewReader([]byte("after")), code: 7}
	reconnects := 0
	c := NewReconnectingConn(context.Background(), first, 1, 0, func(context.Context) (Conn, error) { reconnects++; return second, nil })
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	code, err := c.Wait()
	if err != nil || code != 7 || string(got) != "beforeafter" || reconnects != 1 {
		t.Fatalf("code=%d err=%v output=%q reconnects=%d", code, err, got, reconnects)
	}
	if first.writes.Len() != 0 || second.writes.Len() != 0 {
		t.Fatal("reconnect replayed terminal input")
	}
}

func TestReconnectingConnRecordsReconnectAndLifetime(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	second := &reconnectTestConn{Reader: bytes.NewReader(nil)}
	sink := &tunnelEventSink{}
	n := int64(0)
	now := func() time.Time { n++; return time.Unix(0, n*int64(time.Millisecond)) }
	c := NewObservedReconnectingConn(context.Background(), first, 1, 0, func(context.Context) (Conn, error) { return second, nil }, sink, now, TelemetryContext{MachineID: "prj_1", EnvironmentID: "env_1"})
	_, _ = io.ReadAll(c)
	_, _ = c.Wait()
	if len(sink.events) != 3 {
		t.Fatalf("events = %+v", sink.events)
	}
	if sink.events[0].Name != "terminal.reconnect" || sink.events[0].Outcome != "success" {
		t.Fatalf("reconnect event = %+v", sink.events[0])
	}
	if sink.events[0].MachineID != "prj_1" || sink.events[0].EnvironmentID != "env_1" || sink.events[2].MachineID != "prj_1" || sink.events[2].EnvironmentID != "env_1" {
		t.Fatalf("missing correlation: %+v", sink.events)
	}
	if sink.events[1].Name != "terminal.output" || sink.events[1].Outcome != "success" {
		t.Fatalf("output event = %+v", sink.events[1])
	}
	if sink.events[2].Name != "terminal.lifetime" || sink.events[2].Outcome != "success" {
		t.Fatalf("lifetime event = %+v", sink.events[2])
	}
}

func TestReconnectingConnRecordsOutputPerformance(t *testing.T) {
	sink := &tunnelEventSink{}
	c := &ReconnectingConn{telemetry: sink, now: time.Now}
	c.ObserveLocalWrite(6, 12*time.Millisecond)
	c.maxQueueChunks.Store(4)
	c.recordOutputPerformance("success")
	if len(sink.events) != 3 {
		t.Fatalf("events = %+v", sink.events)
	}
	output := sink.events[0]
	if output.Name != "terminal.output" || output.SizeBytes != 6 || output.LatencyMS != 12 || output.Count != 4 {
		t.Fatalf("output event = %+v", output)
	}
	stage := sink.events[1]
	if stage.Name != "terminal.stage" || stage.Stage != "socket_read_to_stdout" || stage.SizeBytes != 6 || stage.LatencyMS != 12 || stage.Count != 1 {
		t.Fatalf("output stage = %+v", stage)
	}
	if queue := sink.events[2]; queue.Stage != "socket_read_to_queue" || queue.SizeBytes != 6 || queue.Count != 1 {
		t.Fatalf("queue stage = %+v", queue)
	}
}

func TestReconnectingConnRecordsCompressionWithoutContentOrIdentity(t *testing.T) {
	conn := &compressionTestConn{reconnectTestConn: &reconnectTestConn{Reader: bytes.NewReader(nil)}, compression: TerminalCompressionTelemetry{RawFrames: 2, ZstdFrames: 3, DecodedBytes: 4096, EncodedBytes: 1024, DecodeNanos: 9000, DecodeFailures: 1}}
	sink := &tunnelEventSink{}
	c := NewObservedReconnectingConn(context.Background(), conn, 0, 0, nil, sink, time.Now, TelemetryContext{MachineID: "prj_private", EnvironmentID: "env_private"})
	_, _ = io.ReadAll(c)
	_, _ = c.Wait()
	compressionEvents := 0
	for _, event := range sink.events {
		if event.Name != "terminal.compression" {
			continue
		}
		compressionEvents++
		if event.MachineID != "" || event.EnvironmentID != "" || event.SessionID != "" || event.RequestID != "" {
			t.Fatalf("compression event contains identity: %+v", event)
		}
	}
	if compressionEvents != 6 {
		t.Fatalf("compression events=%d all=%+v", compressionEvents, sink.events)
	}
}

func TestCompressionTelemetryCountersSaturate(t *testing.T) {
	if got := telemetryInt64(math.MaxUint64); got != math.MaxInt64 {
		t.Fatalf("counter=%d", got)
	}
	if got := telemetryInt64SaturatingAdd(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("sum=%d", got)
	}
}

func TestReconnectingConnRecordsInputAndEchoStages(t *testing.T) {
	sink := &tunnelEventSink{}
	c := &ReconnectingConn{telemetry: sink, now: time.Now}
	c.ObserveInputWrite(4, 70*time.Millisecond)
	c.ObserveLocalWrite(4, 3*time.Millisecond)
	c.recordOutputPerformance("success")
	if len(sink.events) != 6 {
		t.Fatalf("events = %+v", sink.events)
	}
	if input := sink.events[1]; input.Name != "terminal.stage" || input.Stage != "stdin_to_socket" || input.SizeBytes != 4 || input.LatencyMS != 70 || input.Count != 1 {
		t.Fatalf("input stage = %+v", input)
	}
	if stall := sink.events[4]; stall.Stage != "flow_control_stall" || stall.LatencyMS != 70 || stall.Count != 1 {
		t.Fatalf("flow-control stage = %+v", stall)
	}
	if echo := sink.events[5]; echo.Name != "terminal.stage" || echo.Stage != "input_to_next_output" || echo.SizeBytes != 0 || echo.Count != 1 {
		t.Fatalf("echo stage = %+v", echo)
	}
}

func TestReconnectingConnBoundsFailures(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	c := NewReconnectingConn(context.Background(), first, 1, time.Millisecond, func(context.Context) (Conn, error) { return nil, errors.New("still offline") })
	_, _ = io.ReadAll(c)
	code, err := c.Wait()
	if err == nil || code != 1 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestReconnectingConnStopsOnAuthoritativeFailure(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	attempts := 0
	c := NewReconnectingConn(context.Background(), first, 6, 0, func(context.Context) (Conn, error) {
		attempts++
		return nil, StopReconnect(errors.New("access revoked"))
	})
	_, err := c.Wait()
	if err == nil || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestReconnectingConnDoesNotRetryTerminalFailure(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: errors.New("remote command failed")}
	reconnects := 0
	c := NewReconnectingConn(context.Background(), first, 2, 0, func(context.Context) (Conn, error) {
		reconnects++
		return nil, errors.New("should not reconnect")
	})
	_, err := c.Wait()
	if err == nil || err.Error() != "remote command failed" || reconnects != 0 {
		t.Fatalf("err=%v reconnects=%d", err, reconnects)
	}
}

func TestReconnectingConnRejectsLateReconnectAfterClose(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	replacement := &closeTrackingConn{reconnectTestConn: &reconnectTestConn{Reader: bytes.NewReader(nil)}}
	started := make(chan struct{})
	release := make(chan struct{})
	c := NewReconnectingConn(context.Background(), first, 1, 0, func(context.Context) (Conn, error) {
		close(started)
		<-release
		return replacement, nil
	})
	<-started
	_ = c.Close()
	close(release)
	done := make(chan struct{})
	go func() { _, _ = c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait hung after a late reconnect")
	}
	if !replacement.closed.Load() {
		t.Fatal("late reconnect result was not closed")
	}
}

type writeRaceConn struct{ wait chan error }

func (*writeRaceConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *writeRaceConn) Write([]byte) (int, error) {
	return 0, ErrTransportLost
}
func (c *writeRaceConn) Close() error {
	select {
	case c.wait <- nil:
	default:
	}
	return nil
}
func (c *writeRaceConn) MarkTransportLost(error) {
	select {
	case c.wait <- ErrTransportLost:
	default:
	}
}
func (*writeRaceConn) Resize(uint16, uint16) error { return nil }
func (c *writeRaceConn) Wait() (int, error)        { return 1, <-c.wait }

func TestReconnectingConnCoordinatesWriteFailureWithReconnect(t *testing.T) {
	first := &writeRaceConn{wait: make(chan error, 1)}
	second := &reconnectTestConn{Reader: bytes.NewReader(nil)}
	c := NewReconnectingConn(context.Background(), first, 1, 0, func(context.Context) (Conn, error) { return second, nil })
	if _, err := c.Write([]byte("uncertain")); !errors.Is(err, ErrWriteUncertain) {
		t.Fatalf("err=%v", err)
	}
	if _, err := c.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if second.writes.String() != "next" {
		t.Fatalf("replayed writes=%q", second.writes.String())
	}
	_ = c.Close()
}

func TestTerminalRecoveryBudgetDoesNotRestartAfterSuccessfulReattach(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader([]byte("first")), err: ErrTransportLost}
	calls := 0
	connection := NewReconnectingConn(t.Context(), first, 1, 0, func(context.Context) (Conn, error) {
		calls++
		if calls > 2 {
			return &reconnectTestConn{Reader: bytes.NewReader(nil)}, nil
		}
		return &reconnectTestConn{Reader: bytes.NewReader([]byte("recovered")), err: ErrTransportLost}, nil
	})
	defer connection.Close()
	_, _ = io.ReadAll(connection)
	_, err := connection.Wait()
	if calls != 1 || !errors.Is(err, ErrTransportLost) {
		t.Fatalf("recovery budget restarted: calls=%d err=%v", calls, err)
	}
}

type blockingReconnectConn struct {
	*reconnectTestConn
	stop chan struct{}
	once atomic.Bool
}

func (c *blockingReconnectConn) Read(p []byte) (int, error) { <-c.stop; return 0, io.EOF }
func (c *blockingReconnectConn) Wait() (int, error)         { <-c.stop; return 0, nil }
func (c *blockingReconnectConn) Close() error {
	if c.once.CompareAndSwap(false, true) {
		close(c.stop)
	}
	return nil
}

// A shared terminal whose role changed redials for fresh authorization; a plain close of the old
// connection reports a clean end, so without Redial the joined terminal would just exit.
func TestReconnectingConnRedialReplacesTheConnection(t *testing.T) {
	first := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	second := &reconnectTestConn{Reader: bytes.NewReader([]byte("fresh")), code: 3}
	var reconnects atomic.Int32
	c := NewReconnectingConn(context.Background(), first, 2, 0, func(context.Context) (Conn, error) {
		reconnects.Add(1)
		return second, nil
	})
	c.Redial()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	code, err := c.Wait()
	if err != nil || code != 3 || string(got) != "fresh" || reconnects.Load() != 1 {
		t.Fatalf("code=%d err=%v output=%q reconnects=%d", code, err, got, reconnects.Load())
	}
}

// F3-15: keys typed while the terminal redials are neither dropped nor doubled: the write waits for the new link
// and is sent on it once.
func TestReconnectingConnKeepsInputTypedDuringRedial(t *testing.T) {
	lost := make(chan struct{})
	first := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	second := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	redialing, release := make(chan struct{}), make(chan struct{})
	c := NewReconnectingConn(t.Context(), &lostOnStop{first, lost}, 1, 0, func(context.Context) (Conn, error) {
		close(redialing)
		<-release
		return second, nil
	})
	defer c.Close()
	close(lost)
	<-redialing
	written := make(chan error, 1)
	go func() { _, err := c.Write([]byte("keys")); written <- err }()
	select {
	case err := <-written:
		t.Fatalf("write returned during the redial: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if got := second.writes.String(); got != "keys" || first.writes.Len() != 0 {
		t.Fatalf("new link got %q, old link got %d bytes", got, first.writes.Len())
	}
}

type lostOnStop struct {
	*blockingReconnectConn
	lost chan struct{}
}

func (c *lostOnStop) Wait() (int, error) { <-c.lost; return 1, ErrTransportLost }

// notSentConn fails its first write before sending anything, as the daemon IPC does when the daemon restarts.
type notSentConn struct {
	*blockingReconnectConn
	lost chan struct{}
	once atomic.Bool
}

func (c *notSentConn) Write([]byte) (int, error) {
	return 0, errors.Join(ErrInputNotSent, io.ErrClosedPipe)
}
func (c *notSentConn) MarkTransportLost(error) {
	if c.once.CompareAndSwap(false, true) {
		close(c.lost)
	}
}
func (c *notSentConn) Wait() (int, error) { <-c.lost; return 1, ErrTransportLost }

// F3-16 (N12): keys typed while the local daemon restarts were not sent, so they go out once on the new link and
// the terminal stays open instead of ending.
func TestReconnectingConnResendsInputThatWasNotSent(t *testing.T) {
	first := &notSentConn{blockingReconnectConn: &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}, lost: make(chan struct{})}
	second := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	c := NewReconnectingConn(t.Context(), first, 2, 0, func(context.Context) (Conn, error) { return second, nil })
	defer c.Close()
	if n, err := c.Write([]byte("keys")); err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := second.writes.String(); got != "keys" {
		t.Fatalf("new link got %q", got)
	}
}

// F3-18: the reason is known when ReconnectStarted fires, so the notice can say why the terminal reconnects.
func TestReconnectingConnSaysWhyItReconnects(t *testing.T) {
	for _, tc := range []struct {
		lost error
		want string
	}{
		{errors.Join(ErrTransportLost, ErrLocalDaemonLost), "Paperboat on this computer restarted"},
		{ErrTransportLost, "the connection to the machine was lost"},
	} {
		first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: tc.lost}
		second := &reconnectTestConn{Reader: bytes.NewReader(nil)}
		started := make(chan struct{}, 1)
		c := NewObservedReconnectingConn(t.Context(), first, 1, 0, func(context.Context) (Conn, error) { return second, nil }, nil, nil, TelemetryContext{},
			WithReconnectObserver(func(event ReconnectEvent) {
				if event == ReconnectStarted {
					started <- struct{}{}
				}
			}))
		<-started
		if reason := c.ReconnectReason(); reason != tc.want {
			t.Fatalf("reason=%q, want %q", reason, tc.want)
		}
		_ = c.Close()
	}
	if reason := reconnectReason(ErrTransportLost, true); reason != "you asked to reconnect" {
		t.Fatalf("requested reason=%q", reason)
	}
}

// A held `pb connect` died about 2 min into a peer's update activation (lanei-8p, .7→.8): the six-attempt
// budget was shared by every loss, and never refilled after a recovery. A loss after a healthy link gets a fresh one.
func TestReconnectingConnRefillsBudgetAfterRecovery(t *testing.T) {
	previous := reattachRefillAfter
	reattachRefillAfter = 0 // each reattached link counts as having stayed up
	defer func() { reattachRefillAfter = previous }()
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	second := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	last := &reconnectTestConn{Reader: bytes.NewReader([]byte("ok")), code: 0}
	next := []Conn{second, last}
	c := NewReconnectingConn(context.Background(), first, 1, 0, func(context.Context) (Conn, error) {
		conn := next[0]
		next = next[1:]
		return conn, nil
	})
	got, _ := io.ReadAll(c)
	if code, err := c.Wait(); err != nil || code != 0 || string(got) != "ok" {
		t.Fatalf("code=%d err=%v output=%q", code, err, got)
	}
	if ReattachAttempts(0) != 0 || ReattachAttempts(6) != 600 {
		t.Fatalf("ReattachAttempts = %d, %d", ReattachAttempts(0), ReattachAttempts(6))
	}
}

type stalledReconnectConn struct{ *blockingReconnectConn }

func (c stalledReconnectConn) ObserveLinkState(observer func(bool)) { go observer(true) }

// A carrier restart on the machine left pb connect on a dead, silent link for ~2 min until TCP gave up
// (ltc-s1→ltc-s2, 126 s). A stalled link now dials alongside it, retrying a failed dial, and moves to the fresh link.
func TestReconnectingConnStalledLinkMovesToAFreshLink(t *testing.T) {
	first := stalledReconnectConn{&blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}}
	second := &reconnectTestConn{Reader: bytes.NewReader([]byte("fresh")), code: 3}
	var reconnects atomic.Int32
	c := NewReconnectingConn(context.Background(), first, 3, time.Millisecond, func(context.Context) (Conn, error) {
		if reconnects.Add(1) == 1 {
			return nil, errors.New("carrier still starting")
		}
		return second, nil
	})
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	code, err := c.Wait()
	if err != nil || code != 3 || string(got) != "fresh" || reconnects.Load() != 2 {
		t.Fatalf("code=%d err=%v output=%q reconnects=%d", code, err, got, reconnects.Load())
	}
}

// F3-15 N13 (pbvm2, 2026-11-05): access to the terminal was revoked while its link was silent. The stalled-link
// probe was refused for good, stopped probing, and kept the dead link: the terminal said nothing for about
// 150 s until the transport gave up. A permanent refusal while stalled ends the terminal at once with it.
func TestReconnectingConnStalledLinkEndsAtOnceWhenTheReattachIsRefused(t *testing.T) {
	first := stalledReconnectConn{&blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}}
	revoked := errors.New("access to this terminal was revoked")
	var reconnects atomic.Int32
	c := NewReconnectingConn(context.Background(), first, 600, time.Millisecond, func(context.Context) (Conn, error) {
		reconnects.Add(1)
		return nil, Revoked(revoked)
	})
	defer c.Close()
	ended := make(chan error, 1)
	go func() { _, err := c.Wait(); ended <- err }()
	select {
	case err := <-ended:
		if !errors.Is(err, revoked) {
			t.Fatalf("terminal ended with %v, want the refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("terminal still waiting on its silent link after a permanent refusal (%d dials)", reconnects.Load())
	}
	if n := reconnects.Load(); n > 2 {
		t.Fatalf("a permanent refusal was retried %d times", n)
	}
}

// handoverConn is a stalled link whose host hands the terminal to a newer attachment: the attachment's arrival
// sends the old link a last message (clearing its stall), then the host ends it marked as handed over.
type handoverConn struct {
	*blockingReconnectConn
	observer atomic.Pointer[func(bool)]
	end      atomic.Pointer[error] // how the link ended; nil is the shell's exit
}

func newHandoverConn() *handoverConn {
	return &handoverConn{blockingReconnectConn: &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}}
}

func (c *handoverConn) ObserveLinkState(observer func(bool)) {
	c.observer.Store(&observer)
	go observer(true)
}

func (c *handoverConn) Wait() (int, error) {
	<-c.stop
	if end := c.end.Load(); end != nil {
		return 1, *end
	}
	return 0, nil
}

// heard clears the stall: the host's last message reached the old link.
func (c *handoverConn) heard() {
	if observer := c.observer.Load(); observer != nil {
		(*observer)(false)
	}
}

func (c *handoverConn) handOver() {
	c.heard()
	err := ErrHandedOver
	c.end.Store(&err)
	_ = c.Close()
}

// exit is the shell exiting after the stall flapped back.
func (c *handoverConn) exit() {
	c.heard()
	_ = c.Close()
}

// F3-15 N4/N13 (pbvm2 -> pbvm3, dev-ab41be003, 2026-11-05): after a 45 s drop and a 60 s black hole the terminal
// printed nothing for 120 s and 150 s. The stalled-link probe reattached once the network was back
// (13:40:08.484, 13:43:16.359), but the host's handover to it cleared the old link's stall, so the probe discarded
// its fresh link (probe_discarded) and the host then ended the old one: no link was left and nothing redialed.
// A probe that has attached supersedes the old link on the host; it is adopted, as ET and mosh let the newest
// client connection win.
func TestReconnectingConnAdoptsTheProbeWhenTheHostHandsTheTerminalToIt(t *testing.T) {
	old := newHandoverConn()
	fresh := &reconnectTestConn{Reader: bytes.NewReader([]byte("resumed")), code: 0}
	c := NewReconnectingConn(context.Background(), old, 600, time.Millisecond, func(context.Context) (Conn, error) {
		old.handOver() // the host takes the terminal from the old link as this attachment completes
		return fresh, nil
	})
	defer c.Close()
	got := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(c); got <- b }()
	select {
	case b := <-got:
		if string(b) != "resumed" {
			t.Fatalf("output after the handover = %q, want the fresh link's", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal left without a link after its probe reattached")
	}
}

// Windows CI run 42963879264 (075936b86): the old link's handover-cleared end reached the supervisor before the probe
// stored its fresh link, so the terminal ended with no output and no link (the silent terminal of a41f3d241). The
// probe here returns only after the terminal ended or the supervisor waits for it, forcing that interleaving.
func TestReconnectingConnAdoptsTheProbeWhenTheOldLinkEndsBeforeTheProbeStoresIt(t *testing.T) {
	waiting, ended := make(chan struct{}), make(chan struct{})
	var once sync.Once
	probeWait := func(o *reconnectingOptions) { o.probeWait = func() { once.Do(func() { close(waiting) }) } }
	old := newHandoverConn()
	fresh := &reconnectTestConn{Reader: bytes.NewReader([]byte("resumed")), code: 0}
	c := NewObservedReconnectingConn(context.Background(), old, 600, time.Millisecond, func(ctx context.Context) (Conn, error) {
		old.handOver()
		select {
		case <-ended:
		case <-waiting:
		case <-ctx.Done():
		}
		return fresh, nil
	}, nil, nil, TelemetryContext{}, probeWait)
	defer c.Close()
	got := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(c); close(ended); got <- b }()
	select {
	case b := <-got:
		if string(b) != "resumed" {
			t.Fatalf("output after the handover = %q, want the fresh link's", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal left without a link after its probe reattached")
	}
}

// Review of 841635f63: a stall flapped back, then the user typed exit while the probe was still dialing; the clean end
// waited on the probe, whose terminal dial has no watchdog, so exit hung as long as the dial (unbounded in resolve or
// QUIC), and 5c24bc0f2 still held it up to 1 s. A shell exit is not a handover: it returns at once. The probe's dial
// here returns only when its context is cancelled.
func TestReconnectingConnCleanExitDoesNotWaitOutAProbeStillDialing(t *testing.T) {
	old := newHandoverConn()
	dialing := make(chan struct{})
	var once sync.Once
	c := NewReconnectingConn(context.Background(), old, 600, time.Millisecond, func(ctx context.Context) (Conn, error) {
		once.Do(func() { close(dialing) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer c.Close()
	select {
	case <-dialing:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled link was never probed")
	}
	ended := make(chan error, 1)
	exited := time.Now()
	old.exit()
	go func() { _, err := c.Wait(); ended <- err }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("clean exit ended with %v", err)
		}
		if took := time.Since(exited); took >= 100*time.Millisecond {
			t.Fatalf("clean exit took %v while a probe was dialing", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("clean exit is waiting for a probe that is still dialing")
	}
}

// 5c24bc0f2 cancelled the probe 1 s after the old link ended, so a handover over a slow link (high RTT) ended the user's
// terminal. The host now marks a handover end, and the supervisor waits for the probe however long its attempt takes.
func TestReconnectingConnAdoptsASlowProbeAfterAHandoverEnd(t *testing.T) {
	old := newHandoverConn()
	fresh := &reconnectTestConn{Reader: bytes.NewReader([]byte("resumed")), code: 0}
	var dials atomic.Int32
	c := NewReconnectingConn(context.Background(), old, 600, time.Millisecond, func(ctx context.Context) (Conn, error) {
		if dials.Add(1) > 1 {
			return nil, errors.New("redialed instead of adopting the probe")
		}
		old.handOver()
		select {
		case <-time.After(5 * time.Second): // the attach reply crossing a slow link
			return fresh, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	defer c.Close()
	got := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(c); got <- b }()
	select {
	case b := <-got:
		if string(b) != "resumed" || dials.Load() != 1 {
			t.Fatalf("output after a slow handover = %q after %d dials, want the probe's link", b, dials.Load())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("terminal left without a link after a slow handover")
	}
}

// A handover end with no probe of ours left to adopt is a lost link: the terminal redials instead of ending.
func TestReconnectingConnRedialsAHandoverEndWithoutAProbe(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), code: 1, err: ErrHandedOver}
	second := &reconnectTestConn{Reader: bytes.NewReader([]byte("redialed")), code: 0}
	c := NewReconnectingConn(context.Background(), first, 3, time.Millisecond, func(context.Context) (Conn, error) { return second, nil })
	defer c.Close()
	got, _ := io.ReadAll(c)
	if code, err := c.Wait(); err != nil || code != 0 || string(got) != "redialed" {
		t.Fatalf("code=%d err=%v output=%q", code, err, got)
	}
}

// A target ready after several refusals must be retried promptly instead of
// sitting in a 30 s exponential-backoff sleep after its update has finished.
func TestReconnectingConnRetriesReadyTargetPromptly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	second := &reconnectTestConn{Reader: bytes.NewReader([]byte("recovered"))}
	attempts := 0
	c := NewReconnectingConn(ctx, first, 5, 10*time.Millisecond, func(context.Context) (Conn, error) {
		attempts++
		if attempts < 4 {
			return nil, ErrTransportLost
		}
		return second, nil
	})
	defer c.Close()
	got, err := io.ReadAll(c)
	_, waitErr := c.Wait()
	if err != nil || waitErr != nil || string(got) != "recovered" {
		t.Fatalf("ready target was left waiting: attempts=%d output=%q read=%v wait=%v", attempts, got, err, waitErr)
	}
}

func TestReconnectingConnRedialsImmediatelyThenBacksOffFailures(t *testing.T) {
	first := &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost}
	attempts := make(chan time.Time, 2)
	c := NewReconnectingConn(t.Context(), first, 2, time.Second, func(context.Context) (Conn, error) {
		attempts <- time.Now()
		return nil, errors.New("offline")
	})
	defer c.Close()
	select {
	case <-attempts:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("first redial waited for backoff")
	}
	select {
	case <-attempts:
		t.Fatal("repeated failure did not back off")
	case <-time.After(700 * time.Millisecond):
	}
	select {
	case <-attempts:
	case <-time.After(time.Second):
		t.Fatal("retry did not run")
	}
}

func TestReconnectingConnDoesNotBackOffANewLossAfterShortRecovery(t *testing.T) {
	lost := func() Conn { return &reconnectTestConn{Reader: bytes.NewReader(nil), err: ErrTransportLost} }
	attempts := make(chan struct{}, 2)
	n := 0
	c := NewReconnectingConn(t.Context(), lost(), 2, time.Second, func(context.Context) (Conn, error) {
		n++
		attempts <- struct{}{}
		if n == 1 {
			return lost(), nil
		}
		return &reconnectTestConn{Reader: bytes.NewReader(nil)}, nil
	})
	defer c.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-attempts:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("a successful dial made the next loss wait for failure backoff")
		}
	}
	if _, err := c.Wait(); err != nil {
		t.Fatal(err)
	}
}

// lostLinkConn is a stalled link that, once closed, reports an unexpected transport loss, as a QUIC link does when
// its idle timeout fires after the network returns.
type lostLinkConn struct{ stalledReconnectConn }

func (c lostLinkConn) Wait() (int, error) { <-c.stop; return 0, ErrTransportLost }

// F3-15 N4 (45 s network drop) regression: pb wraps every dial error that is not retryable in StopReconnect, and
// the local-daemon dial fails that way while the network is down. Only a revocation may end a terminal whose link
// is silent; any other refusal during the outage keeps the link, and the terminal resumes when it is lost.
func TestReconnectingConnStalledLinkSurvivesANonRevocationRefusalDuringAnOutage(t *testing.T) {
	first := lostLinkConn{stalledReconnectConn{&blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}}}
	second := &reconnectTestConn{Reader: bytes.NewReader([]byte("resumed")), code: 5}
	var reconnects atomic.Int32
	probed := make(chan struct{})
	c := NewReconnectingConn(context.Background(), first, 3, time.Millisecond, func(context.Context) (Conn, error) {
		if reconnects.Add(1) == 1 {
			close(probed)
			return nil, StopReconnect(errors.New("the stream to the machine did not open within 10 s"))
		}
		return second, nil
	})
	defer c.Close()
	<-probed
	time.Sleep(50 * time.Millisecond) // the probe has returned; a bad probe would have closed the link by now
	_ = first.Close()                 // the outage ends and the transport gives up the silent link
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal("terminal ended during the outage:", err)
	}
	code, err := c.Wait()
	if err != nil || code != 5 || string(got) != "resumed" || reconnects.Load() != 2 {
		t.Fatalf("code=%d err=%v output=%q reconnects=%d", code, err, got, reconnects.Load())
	}
}

// The probe of a stalled link can connect just after the supervisor replaced that link itself. Such a probe is closed,
// not adopted when the replacement ends: adopted, it brought an ended terminal back and replayed its output
// ("resumedresumed", Cross-platform run 42737697330 on macOS).
func TestReconnectingConnDropsAProbeMadeForAnEarlierLink(t *testing.T) {
	first := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	stale := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	c := NewReconnectingConn(context.Background(), first, 2, 0, func(context.Context) (Conn, error) {
		return nil, errors.New("must not redial")
	})
	defer c.Close()
	c.probed.Store(&probedLink{stalled: &reconnectTestConn{}, conn: stale}) // made for a link already replaced
	_ = first.Close()                                                       // the current link ends cleanly
	ended := make(chan int, 1)
	go func() { code, _ := c.Wait(); ended <- code }()
	select {
	case code := <-ended:
		if code != 0 || !stale.once.Load() {
			t.Fatalf("code=%d stale probe closed=%t", code, stale.once.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal moved to a probe made for an earlier link instead of ending")
	}
}

// A probe can store its link while the supervisor is already redialing the same ended link. Once the redial has a
// link, the probe's is closed at once: the terminal never holds two.
func TestReconnectingConnClosesAProbeThatStoredDuringTheSupervisorsRedial(t *testing.T) {
	first := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	second := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	stale := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	dialing, release := make(chan struct{}), make(chan struct{})
	c := NewReconnectingConn(context.Background(), first, 2, 0, func(context.Context) (Conn, error) {
		close(dialing)
		<-release
		return second, nil
	})
	defer c.Close()
	c.Redial() // the supervisor leaves the first link and redials
	<-dialing
	c.probed.Store(&probedLink{stalled: first, conn: stale}) // the probe stores its link in that window
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !stale.once.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the probe's link stayed open beside the supervisor's")
		}
		time.Sleep(time.Millisecond)
	}
	if second.once.Load() {
		t.Fatal("the supervisor's own link was closed")
	}
}

// Close ends a stored probe link too, and the supervisor does not adopt one after Close.
func TestReconnectingConnCloseEndsAStoredProbe(t *testing.T) {
	first := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	stale := &blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}
	c := NewReconnectingConn(context.Background(), first, 2, 0, func(context.Context) (Conn, error) {
		return nil, errors.New("must not redial")
	})
	c.probed.Store(&probedLink{stalled: first, conn: stale})
	_ = c.Close()
	ended := make(chan struct{})
	go func() { _, _ = c.Wait(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor adopted a probe after Close and never ended")
	}
	if !stale.once.Load() {
		t.Fatal("Close left the stored probe link open")
	}
}

// F3-15 N4 (1 run in 16): the daemon tore down both silent links at once and each reported a clean end (code 0, no error),
// which ended the terminal as if the shell had exited, 46 s into a 45 s outage. A shell exit is heard before it is
// reported, and hearing the host clears the stall; a clean end of a link still flagged silent is a local teardown, so the
// terminal redials.
func TestReconnectingConnStalledLinkThatEndsCleanlyIsRedialedNotEnded(t *testing.T) {
	first := stalledReconnectConn{&blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}}
	var down atomic.Bool
	down.Store(true)
	var reconnects atomic.Int32
	c := NewReconnectingConn(context.Background(), first, 600, time.Millisecond, func(context.Context) (Conn, error) {
		reconnects.Add(1)
		if down.Load() {
			return nil, errors.New("network down")
		}
		return &reconnectTestConn{Reader: bytes.NewReader([]byte("resumed")), code: 5}, nil // one per dial: the probe and the supervisor may both dial
	})
	defer c.Close()
	for reconnects.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	down.Store(false) // the network is back
	_ = first.Close() // the daemon tears the silent link down: Wait reports a clean end
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal("terminal ended:", err)
	}
	code, err := c.Wait()
	if err != nil || code != 5 || string(got) != "resumed" {
		t.Fatalf("code=%d err=%v output=%q: a clean end of a silent link ended the terminal", code, err, got)
	}
}
