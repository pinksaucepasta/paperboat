package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// carrierTerminal retains the local byte stream while a staged remote carrier
// takes over. Ordinary loss still returns to today's CLI reconnect path.
type carrierTerminal struct {
	*ReconnectingConn
	outputMu             sync.Mutex
	delivered            uint64
	produced             uint64
	boundaries           []carrierOutputBoundary
	sequenceSink         func(int)
	readMu               sync.Mutex
	writeMu              sync.Mutex
	moveMu               sync.Mutex
	mu                   sync.Mutex
	helper               *helperTerminalConn
	session              *native.Session
	application          peerApplication
	target               *resolver.TerminalTarget
	now                  func() time.Time
	sequence             *atomic.Int64
	prepared             *preparedTerminalMove
	observer             atomic.Pointer[func(bool)]
	completedCompression TerminalCompressionTelemetry
}

type preparedTerminalMove struct {
	session *native.Session
	helper  *helperTerminalConn
	target  *resolver.TerminalTarget
	result  chan error
	taken   bool // guarded by carrierTerminal.mu
}

func newCarrierTerminal(ctx context.Context, helper *helperTerminalConn, session *native.Session, application peerApplication, target *resolver.TerminalTarget, now func() time.Time, sequence *atomic.Int64, sink func(int)) (*carrierTerminal, error) {
	c := &carrierTerminal{helper: helper, session: session, application: application, target: target, now: now, sequence: sequence, sequenceSink: sink}
	c.ReconnectingConn = NewObservedReconnectingConn(ctx, &carrierTerminalSource{helperTerminalConn: helper, owner: c}, 1, 0, c.attachPrepared, nil, nil, TelemetryContext{}, func(options *reconnectingOptions) {
		options.plannedMovesOnly = true
		options.observer = func(event ReconnectEvent) {
			if observer := c.observer.Load(); observer != nil {
				if event == ReconnectStalled {
					(*observer)(true)
				}
				if event == ReconnectResumed || event == ReconnectRecovered {
					(*observer)(false)
				}
			}
		}
	})
	if err := session.OnCarrierMove(c.move); err != nil {
		c.ReconnectingConn.Close()
		return nil, err
	}
	return c, nil
}

func (c *carrierTerminal) Close() error {
	c.cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ReconnectingConn.Close()
}

func (c *carrierTerminal) ObserveLinkState(observer func(bool)) { c.observer.Store(&observer) }
func (c *carrierTerminal) Write(data []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ReconnectingConn.Write(data)
}
func (c *carrierTerminal) Resize(rows, columns uint16) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ReconnectingConn.Resize(rows, columns)
}
func (c *carrierTerminal) TerminalRuntimeVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.helper.TerminalRuntimeVersion()
}
func (c *carrierTerminal) TerminalCompressionTelemetry() TerminalCompressionTelemetry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return addTerminalCompression(c.completedCompression, c.helper.TerminalCompressionTelemetry())
}

func (c *carrierTerminal) move(ctx context.Context, offer native.CarrierOffer) error {
	if !c.moveMu.TryLock() {
		return native.ErrInvalid
	}
	defer c.moveMu.Unlock()
	c.mu.Lock()
	old, oldHelper := c.session, c.helper
	target := *c.target
	c.mu.Unlock()
	// Finish handshake and application authentication while the old terminal
	// continues serving. Any refusal here leaves its input/output untouched.
	next, err := old.DialCarrier(ctx, offer)
	if err != nil {
		return err
	}
	group := &cliNativeStreamGroup{session: next, target: &target, application: c.application, consumer: "terminal", now: c.now}
	message, err := authenticateNativeStreamGroup(ctx, group, &target, "native")
	if err != nil {
		next.Close()
		return err
	}
	var helper *helperTerminalConn
	move := &preparedTerminalMove{session: next, target: &target, result: make(chan error, 1)}
	defer func() {
		// attachPrepared owns the move once it took it; otherwise release it.
		c.mu.Lock()
		owned := move.taken
		if c.prepared == move {
			c.prepared = nil
		}
		c.mu.Unlock()
		if !owned {
			if helper != nil {
				helper.Close()
			}
			message.Close()
			next.Close()
		}
	}()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := target.InputQueue.WaitEmpty(ctx); err != nil {
		return err
	}
	// Attach on the new carrier before cutting the old one: the old reader keeps
	// delivering output until the host hands the attachment over (dadape direct2d
	// stalled 4.85 s on an attach made after the cut; inbound1d lost the terminal
	// when the new path failed after the cut). Magicsock likewise keeps its best
	// path until a new one has answered (~/ref/tailscale/wgengine/magicsock/endpoint.go:2005).
	target.AfterSequence = int(c.sequence.Load())
	target.RestartIfNotRunning = false
	target.ReplayHistory = false
	conn, err := c.application.helper(ctx, message, &target)
	// A failed initializer returns a nil *helperTerminalConn through Conn.
	helper, isHelper := conn.(*helperTerminalConn)
	if !isHelper && conn != nil {
		conn.Close()
	}
	if err == nil && helper == nil {
		err = ErrPeerTerminalInvalid
	}
	if err == nil {
		err = next.OnCarrierMove(c.move)
	}
	if err != nil {
		return err
	}
	move.helper = helper
	c.mu.Lock()
	c.prepared = move
	c.mu.Unlock()
	// Mark redial before stopping the old reader. Close is suppressed during a
	// move so it cannot enqueue a detach or close the QUIC offer/ACK connection.
	// An old link that is already ending reaches attachPrepared on its own.
	c.redial.Store(true)
	if err := oldHelper.closeForCarrierMove(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	select {
	case err := <-move.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *carrierTerminal) attachPrepared(ctx context.Context) (Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	move := c.prepared
	c.prepared = nil
	if move == nil {
		return nil, StopReconnect(ErrTransportLost)
	}
	// supervise joined the old reader before entering this function, so every
	// byte through this cursor is already queued locally; the replacement replays
	// from the cursor at its attach and skips what the old link delivered since
	// (ET keeps the reader sequence through recovery, ~/ref/EternalTerminal/src/base/Connection.cpp:119).
	move.taken = true
	move.helper.skipThrough = uint64(max(c.sequence.Load(), 0))
	c.completedCompression = addTerminalCompression(c.completedCompression, c.helper.TerminalCompressionTelemetry())
	c.helper = move.helper
	c.session = move.session
	c.target = move.target
	move.result <- nil
	return &carrierTerminalSource{helperTerminalConn: move.helper, owner: c}, nil
}

func addTerminalCompression(a, b TerminalCompressionTelemetry) TerminalCompressionTelemetry {
	return TerminalCompressionTelemetry{RawFrames: a.RawFrames + b.RawFrames, ZstdFrames: a.ZstdFrames + b.ZstdFrames, DecodedBytes: a.DecodedBytes + b.DecodedBytes, EncodedBytes: a.EncodedBytes + b.EncodedBytes, DecodeNanos: a.DecodeNanos + b.DecodeNanos, DecodeFailures: a.DecodeFailures + b.DecodeFailures}
}
