package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
)

type ReconnectFunc func(context.Context) (Conn, error)

type stopReconnectError struct{ error }

func (e stopReconnectError) Unwrap() error { return e.error }

// StopReconnect marks a reconnect failure as authoritative and permanent.
func StopReconnect(err error) error {
	if err == nil {
		return nil
	}
	return stopReconnectError{error: err}
}

type TelemetryContext struct {
	MachineID     string
	EnvironmentID string
}

var ErrWriteUncertain = errors.New("terminal write outcome is uncertain")
// ReattachAttempts is the per-loss budget of a held terminal: 2+4+8+16 s, then 30 s steps, about
// 10 minutes, so a peer's update activation does not end the terminal. 0 keeps reconnect off.
func ReattachAttempts(dialRetries int) int { return min(dialRetries, 1) * 24 }

var ErrTransportLost = errors.New("terminal transport lost")

// ErrLocalDaemonLost: the link to the Paperboat daemon on this computer broke (it restarted or stopped).
var ErrLocalDaemonLost = fmt.Errorf("local Paperboat daemon connection lost: %w", ErrTransportLost)

// ErrInputNotSent: a write failed before any of it left this process, so it is safe to send again on the next link.
var ErrInputNotSent = fmt.Errorf("terminal input was not sent: %w", ErrTransportLost)

const terminalOutputBatchWindow = time.Duration(0)

type ReconnectingOption func(*reconnectingOptions)

// ReconnectEvent carries only lifecycle state, never transport error details.
type ReconnectEvent string

const (
	ReconnectStarted   ReconnectEvent = "reconnecting"
	ReconnectRecovered ReconnectEvent = "recovered"
	ReconnectFailed    ReconnectEvent = "failed"
	// ReconnectStalled: the machine has not been heard from for a while; the link is kept and resumes on its own
	// with the input typed meanwhile. ReconnectResumed: it is heard again.
	ReconnectStalled ReconnectEvent = "stalled"
	ReconnectResumed ReconnectEvent = "resumed"
)

type reconnectingOptions struct {
	outputQueueChunks int
	outputBatchWindow time.Duration
	observer          func(ReconnectEvent)
}

// ReconnectReason says, in words for the user, why the terminal is reconnecting or last reconnected.
func (c *ReconnectingConn) ReconnectReason() string {
	reason, _ := c.reason.Load().(string)
	return reason
}

func reconnectReason(err error, requested bool) string {
	switch {
	case requested:
		return "you asked to reconnect"
	case errors.Is(err, ErrLocalDaemonLost):
		return "Paperboat on this computer restarted"
	}
	return "the connection to the machine was lost"
}

func WithReconnectObserver(observer func(ReconnectEvent)) ReconnectingOption {
	return func(options *reconnectingOptions) { options.observer = observer }
}

func WithReconnectingOutput(queueChunks int, batchWindow time.Duration) ReconnectingOption {
	return func(options *reconnectingOptions) {
		if queueChunks > 0 {
			options.outputQueueChunks = queueChunks
		}
		if batchWindow >= 0 {
			options.outputBatchWindow = batchWindow
		}
	}
}

type transportFailureMarker interface {
	MarkTransportLost(error)
}

const flowControlStallThreshold = 50 * time.Millisecond

// ReconnectingConn supervises unexpected transport loss while one session
// loop retains ownership of stdin. Failed writes are never replayed.
type ReconnectingConn struct {
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.RWMutex
	current           Conn
	available         chan struct{}
	redial            atomic.Bool
	probed            atomic.Pointer[Conn] // a fresh link dialed while the current one was stalled
	probing           atomic.Bool
	reconnect         ReconnectFunc
	maxRetries        int
	delay             time.Duration
	out               chan []byte
	done              chan reconnectResult
	closed            chan struct{}
	closeOnce         sync.Once
	pending           []byte
	telemetry         telemetry.Sink
	started           time.Time
	now               func() time.Time
	correlation       TelemetryContext
	outputBytes       atomic.Int64
	outputFrames      atomic.Int64
	inputBytes        atomic.Int64
	inputFrames       atomic.Int64
	lastInputAt       atomic.Int64
	echoSamples       atomic.Int64
	maxQueueChunks    atomic.Int64
	maxQueueDelayMS   atomic.Int64
	maxWriteLatencyMS atomic.Int64
	maxInputLatencyMS atomic.Int64
	flowControlStalls atomic.Int64
	maxFlowStallMS    atomic.Int64
	maxEchoLatencyMS  atomic.Int64
	outputBatchWindow time.Duration
	observer          func(ReconnectEvent)
	reason            atomic.Value // string: why the current or last reconnect happened
}

func (c *ReconnectingConn) record(name, outcome string, started time.Time) {
	if c.telemetry == nil {
		return
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	ended := now()
	e := telemetry.Event{Name: name, At: ended, MachineID: c.correlation.MachineID, EnvironmentID: c.correlation.EnvironmentID, Outcome: outcome, LatencyMS: ended.Sub(started).Milliseconds()}
	if e.Validate() == nil {
		c.telemetry.Record(e)
	}
}

type reconnectResult struct {
	code int
	err  error
}

func NewReconnectingConn(ctx context.Context, initial Conn, maxRetries int, delay time.Duration, reconnect ReconnectFunc) *ReconnectingConn {
	return NewObservedReconnectingConn(ctx, initial, maxRetries, delay, reconnect, nil, nil, TelemetryContext{})
}

func NewObservedReconnectingConn(ctx context.Context, initial Conn, maxRetries int, delay time.Duration, reconnect ReconnectFunc, sink telemetry.Sink, now func() time.Time, correlation TelemetryContext, opts ...ReconnectingOption) *ReconnectingConn {
	available := make(chan struct{})
	close(available)
	if now == nil {
		now = time.Now
	}
	options := reconnectingOptions{outputQueueChunks: terminalOutputQueueChunks, outputBatchWindow: terminalOutputBatchWindow}
	for _, option := range opts {
		option(&options)
	}
	runCtx, cancel := context.WithCancel(ctx)
	c := &ReconnectingConn{ctx: runCtx, cancel: cancel, current: initial, available: available, reconnect: reconnect, maxRetries: maxRetries, delay: delay, out: make(chan []byte, options.outputQueueChunks), done: make(chan reconnectResult, 1), closed: make(chan struct{}), telemetry: sink, now: now, started: now(), correlation: correlation, outputBatchWindow: options.outputBatchWindow, observer: options.observer}
	go c.supervise(initial)
	return c
}

func (c *ReconnectingConn) supervise(conn Conn) {
	defer close(c.out)
	lifetimeOutcome := "failure"
	defer func() {
		c.recordOutputPerformance(lifetimeOutcome)
		c.record("terminal.lifetime", lifetimeOutcome, c.started)
	}()
	for {
		if observed, ok := conn.(LinkStateObserver); ok {
			stalledConn := conn
			observed.ObserveLinkState(func(stalled bool) {
				if stalled {
					c.notify(ReconnectStalled)
					go c.probeStalled(stalledConn)
				} else {
					c.notify(ReconnectResumed)
				}
			})
		}
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					b := append([]byte(nil), buf[:n]...)
					queuedAt := time.Now()
					select {
					case c.out <- b:
						updateAtomicMax(&c.maxQueueChunks, int64(len(c.out)))
						updateAtomicMax(&c.maxQueueDelayMS, time.Since(queuedAt).Milliseconds())
					case <-c.closed:
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		code, err := conn.Wait()
		_ = conn.Close()
		<-readDone
		c.recordCompression(conn)
		if next := c.probed.Swap(nil); next != nil {
			c.redial.Store(false)
			c.setUnavailable(conn)
			c.reason.Store("no reply from the machine")
			conn = *next
			c.setCurrent(conn)
			c.notify(ReconnectRecovered)
			continue
		}
		requested := c.redial.Swap(false) && c.reconnect != nil && c.maxRetries > 0
		if requested {
			err = ErrTransportLost
		}
		if err == nil || !errors.Is(err, ErrTransportLost) || c.reconnect == nil || c.maxRetries <= 0 {
			if err == nil {
				lifetimeOutcome = "success"
			}
			c.done <- reconnectResult{code, err}
			return
		}
		c.setUnavailable(conn)
		c.reason.Store(reconnectReason(err, requested))
		c.notify(ReconnectStarted)
		// Each loss gets the full budget, like EternalTerminal's per-disconnect pollReconnect loop
		// (~/ref/EternalTerminal/src/base/ClientConnection.cpp:210): a held terminal outlives many peer restarts.
		recovery := reattach.New(reattach.Policy{Attempts: c.maxRetries, Backoff: reattach.Backoff(c.delay, 30*time.Second), Retryable: func(err error) bool { var stopped stopReconnectError; return !errors.As(err, &stopped) }})
		recoveryErr := recovery.Run(c.ctx, func(dialCtx context.Context) error {
			attemptStarted := c.now()
			next, dialErr := c.reconnect(dialCtx)
			if dialErr != nil {
				c.record("terminal.reconnect", "failure", attemptStarted)
				err = errors.Join(err, dialErr)
				return dialErr
			}
			if next == nil {
				dialErr = errors.New("reconnect returned no connection")
				c.record("terminal.reconnect", "failure", attemptStarted)
				err = errors.Join(err, dialErr)
				return dialErr
			}
			select {
			case <-c.closed:
				_ = next.Close()
				c.record("terminal.reconnect", "cancelled", attemptStarted)
				return context.Canceled
			default:
			}
			conn = next
			c.setCurrent(next)
			c.notify(ReconnectRecovered)
			c.record("terminal.reconnect", "success", attemptStarted)
			return nil
		})
		if recoveryErr != nil {
			if c.ctx.Err() != nil || errors.Is(recoveryErr, context.Canceled) {
				lifetimeOutcome = "cancelled"
				c.done <- reconnectResult{130, nil}
				return
			}
			c.notify(ReconnectFailed)
			c.done <- reconnectResult{1, errors.Join(err, recoveryErr)}
			return
		}
	}
}

func (c *ReconnectingConn) notify(event ReconnectEvent) {
	if c.observer != nil {
		c.observer(event)
	}
}

func (c *ReconnectingConn) setUnavailable(conn Conn) {
	c.mu.Lock()
	if c.current == conn {
		c.current = nil
		c.available = make(chan struct{})
	}
	c.mu.Unlock()
}
func (c *ReconnectingConn) setCurrent(conn Conn) {
	c.mu.Lock()
	c.current = conn
	ch := c.available
	c.mu.Unlock()
	close(ch)
}
func (c *ReconnectingConn) waitCurrent(ctx context.Context) (Conn, error) {
	for {
		c.mu.RLock()
		conn, ch := c.current, c.available
		c.mu.RUnlock()
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ch:
		case <-c.closed:
			return nil, io.ErrClosedPipe
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
func (c *ReconnectingConn) Read(p []byte) (int, error) {
	return readBufferedChunksWithWait(p, &c.pending, c.out, c.outputBatchWindow)
}

func (c *ReconnectingConn) ObserveLocalWrite(size int, duration time.Duration) {
	if size > 0 {
		c.outputBytes.Add(int64(size))
		c.outputFrames.Add(1)
	}
	updateAtomicMax(&c.maxWriteLatencyMS, duration.Milliseconds())
	if started := c.lastInputAt.Swap(0); started > 0 {
		latency := time.Since(time.Unix(0, started))
		if latency >= 0 {
			c.echoSamples.Add(1)
			updateAtomicMax(&c.maxEchoLatencyMS, latency.Milliseconds())
		}
	}
}

func (c *ReconnectingConn) ObserveInputWrite(size int, duration time.Duration) {
	if size <= 0 {
		return
	}
	c.inputBytes.Add(int64(size))
	c.inputFrames.Add(1)
	updateAtomicMax(&c.maxInputLatencyMS, duration.Milliseconds())
	if duration >= flowControlStallThreshold {
		c.flowControlStalls.Add(1)
		updateAtomicMax(&c.maxFlowStallMS, duration.Milliseconds())
	}
	c.lastInputAt.Store(time.Now().UnixNano())
}

func (c *ReconnectingConn) recordOutputPerformance(outcome string) {
	if c.telemetry == nil {
		return
	}
	e := telemetry.Event{
		Name:          "terminal.output",
		At:            c.now(),
		MachineID:     c.correlation.MachineID,
		EnvironmentID: c.correlation.EnvironmentID,
		Outcome:       outcome,
		SizeBytes:     c.outputBytes.Load(),
		LatencyMS:     c.maxWriteLatencyMS.Load(),
		Count:         c.maxQueueChunks.Load(),
	}
	if e.Validate() == nil {
		c.telemetry.Record(e)
	}
	c.recordTerminalStage("stdin_to_socket", c.inputBytes.Load(), c.inputFrames.Load(), c.maxInputLatencyMS.Load(), outcome)
	c.recordTerminalStage("socket_read_to_stdout", c.outputBytes.Load(), c.outputFrames.Load(), c.maxWriteLatencyMS.Load(), outcome)
	c.recordTerminalStage("socket_read_to_queue", c.outputBytes.Load(), c.outputFrames.Load(), c.maxQueueDelayMS.Load(), outcome)
	c.recordTerminalStage("flow_control_stall", 0, c.flowControlStalls.Load(), c.maxFlowStallMS.Load(), outcome)
	c.recordTerminalStage("input_to_next_output", 0, c.echoSamples.Load(), c.maxEchoLatencyMS.Load(), outcome)
}

func (c *ReconnectingConn) recordTerminalStage(stage string, bytes, count, latencyMS int64, outcome string) {
	if c.telemetry == nil || count == 0 {
		return
	}
	e := telemetry.Event{Name: "terminal.stage", At: c.now(), MachineID: c.correlation.MachineID, EnvironmentID: c.correlation.EnvironmentID, Outcome: outcome, Stage: stage, SizeBytes: bytes, LatencyMS: latencyMS, Count: count}
	if e.Validate() == nil {
		c.telemetry.Record(e)
	}
}

func (c *ReconnectingConn) recordCompression(conn Conn) {
	if c.telemetry == nil {
		return
	}
	reporter, ok := conn.(terminalCompressionReporter)
	if !ok {
		return
	}
	stats := reporter.TerminalCompressionTelemetry()
	events := []telemetry.Event{
		{Name: "terminal.compression", At: c.now(), Stage: "raw_frames", Count: telemetryInt64(stats.RawFrames)},
		{Name: "terminal.compression", At: c.now(), Stage: "zstd_frames", Count: telemetryInt64(stats.ZstdFrames)},
		{Name: "terminal.compression", At: c.now(), Stage: "decoded_bytes", SizeBytes: telemetryInt64(stats.DecodedBytes)},
		{Name: "terminal.compression", At: c.now(), Stage: "encoded_bytes", SizeBytes: telemetryInt64(stats.EncodedBytes)},
		{Name: "terminal.compression", At: c.now(), Stage: "decode", Count: telemetryInt64SaturatingAdd(stats.RawFrames, stats.ZstdFrames), DurationNS: telemetryInt64(stats.DecodeNanos)},
		{Name: "terminal.compression", At: c.now(), Stage: "decode_failures", Outcome: "failure", Count: telemetryInt64(stats.DecodeFailures)},
	}
	for _, event := range events {
		if event.Count == 0 && event.SizeBytes == 0 && event.DurationNS == 0 {
			continue
		}
		if event.Validate() == nil {
			c.telemetry.Record(event)
		}
	}
}

func telemetryInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func telemetryInt64SaturatingAdd(left, right uint64) int64 {
	if left > math.MaxInt64 || right > uint64(math.MaxInt64)-left {
		return math.MaxInt64
	}
	return int64(left + right)
}

func updateAtomicMax(value *atomic.Int64, candidate int64) {
	for {
		current := value.Load()
		if candidate <= current || value.CompareAndSwap(current, candidate) {
			return
		}
	}
}
func (c *ReconnectingConn) Write(p []byte) (int, error) {
	for {
		conn, err := c.waitCurrent(c.ctx)
		if err != nil {
			return 0, err
		}
		n, writeErr := conn.Write(p)
		if writeErr == nil {
			return n, nil
		}
		if c.maxRetries <= 0 || c.reconnect == nil {
			return n, writeErr
		}
		c.setUnavailable(conn)
		if marker, ok := conn.(transportFailureMarker); ok {
			marker.MarkTransportLost(writeErr)
		} else {
			_ = conn.Close()
		}
		if !errors.Is(writeErr, ErrInputNotSent) {
			return n, errors.Join(ErrWriteUncertain, writeErr)
		}
		// Nothing was sent: the same keys go out once on the next link (waitCurrent waits for it).
	}
}
func (c *ReconnectingConn) Resize(r, col uint16) error {
	conn, err := c.waitCurrent(c.ctx)
	if err != nil {
		return err
	}
	return conn.Resize(r, col)
}
func (c *ReconnectingConn) CloseWrite() error {
	conn, err := c.waitCurrent(c.ctx)
	if err != nil {
		return err
	}
	if h, ok := conn.(InputHalfCloser); ok {
		return h.CloseWrite()
	}
	return nil
}
func (c *ReconnectingConn) Wait() (int, error) { r := <-c.done; return r.code, r.err }

// Redial replaces the current connection with a fresh one from the reconnect function, as after a
// lost transport, for example when the terminal's authorization changed. Output resumes at the cursor.
func (c *ReconnectingConn) Redial() {
	c.mu.RLock()
	conn := c.current
	c.mu.RUnlock()
	if conn == nil {
		return
	}
	c.redial.Store(true)
	_ = conn.Close()
}

// probeStalled dials once while a link is silent, keeping the silent link: a machine whose carrier restarted
// leaves it dead until TCP gives up (~2 min), while a network that comes back resumes it. A fresh link replaces
// it; the daemon carries unanswered input to it by input owner. ET does the same on keepalive loss
// (~/ref/EternalTerminal/src/base/ClientConnection.cpp:210).
func (c *ReconnectingConn) probeStalled(stalled Conn) {
	if c.reconnect == nil || c.maxRetries <= 0 || c.probing.Swap(true) {
		return
	}
	defer c.probing.Store(false)
	next, err := c.reconnect(c.ctx)
	if err != nil || next == nil {
		return
	}
	c.mu.RLock()
	current := c.current
	c.mu.RUnlock()
	if current != stalled || c.ctx.Err() != nil {
		_ = next.Close()
		return
	}
	c.probed.Store(&next)
	_ = stalled.Close()
}

func (c *ReconnectingConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.cancel != nil {
			c.cancel()
		}
		c.mu.RLock()
		conn := c.current
		c.mu.RUnlock()
		if conn != nil {
			err = conn.Close()
		}
	})
	return err
}
