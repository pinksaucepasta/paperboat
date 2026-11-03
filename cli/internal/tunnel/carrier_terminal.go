package tunnel

import (
	"context"
	"errors"
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
	ctx     context.Context
	session *native.Session
	message *nativeMessageConnection
	target  *resolver.TerminalTarget
	result  chan error
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
	committed := false
	defer func() {
		if !committed {
			message.Close()
			next.Close()
		}
	}()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := target.InputQueue.WaitEmpty(ctx); err != nil {
		return err
	}
	move := &preparedTerminalMove{ctx: ctx, session: next, message: message, target: &target, result: make(chan error, 1)}
	c.mu.Lock()
	c.prepared = move
	c.mu.Unlock()
	// Mark redial before stopping the old reader. Close is suppressed during a
	// move so it cannot enqueue a detach or close the QUIC offer/ACK connection.
	c.redial.Store(true)
	if err := oldHelper.closeForCarrierMove(); err != nil {
		return err
	}
	select {
	case err := <-move.result:
		committed = err == nil
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *carrierTerminal) attachPrepared(ctx context.Context) (Conn, error) {
	c.mu.Lock()
	move := c.prepared
	c.prepared = nil
	c.mu.Unlock()
	if move == nil {
		return nil, StopReconnect(ErrTransportLost)
	}
	// ET keeps the reader/writer sequence through recovery (~/ref/EternalTerminal/src/base/Connection.cpp:119).
	// supervise joined the old reader before entering this function. Its output
	// queue survives the move, and the cursor now includes every byte in it.
	move.target.AfterSequence = int(c.sequence.Load())
	move.target.RestartIfNotRunning = false
	move.target.ReplayHistory = false
	conn, err := c.application.helper(move.ctx, move.message, move.target)
	if err == nil {
		helper, ok := conn.(*helperTerminalConn)
		if !ok || helper == nil {
			err = ErrPeerTerminalInvalid
		} else {
			err = move.session.OnCarrierMove(c.move)
			if err == nil {
				c.mu.Lock()
				c.completedCompression = addTerminalCompression(c.completedCompression, c.helper.TerminalCompressionTelemetry())
				c.helper = helper
				c.session = move.session
				c.target = move.target
				c.mu.Unlock()
			}
		}
	}
	if err != nil {
		// A failed initializer returns a nil *helperTerminalConn through Conn.
		// Preserve the initialization error without calling Close on that pointer.
		if helper, ok := conn.(*helperTerminalConn); ok {
			if helper != nil {
				helper.Close()
			}
		} else if conn != nil {
			conn.Close()
		}
		move.message.Close()
		move.session.Close()
	}
	move.result <- err
	if err != nil {
		return nil, StopReconnect(errors.Join(ErrTransportLost, err))
	}
	return &carrierTerminalSource{helperTerminalConn: conn.(*helperTerminalConn), owner: c}, nil
}

func addTerminalCompression(a, b TerminalCompressionTelemetry) TerminalCompressionTelemetry {
	return TerminalCompressionTelemetry{RawFrames: a.RawFrames + b.RawFrames, ZstdFrames: a.ZstdFrames + b.ZstdFrames, DecodedBytes: a.DecodedBytes + b.DecodedBytes, EncodedBytes: a.EncodedBytes + b.EncodedBytes, DecodeNanos: a.DecodeNanos + b.DecodeNanos, DecodeFailures: a.DecodeFailures + b.DecodeFailures}
}
