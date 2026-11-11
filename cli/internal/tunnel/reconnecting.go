package tunnel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
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

type revokedError struct{ error }

func (e revokedError) Unwrap() error { return e.error }

// Revoked marks a refusal because access to the terminal was withdrawn. It is permanent like StopReconnect, and
// the only kind that ends a terminal whose link is silent at once: pb wraps every other non-retryable dial error
// in StopReconnect too, including the local-daemon dial that fails while the network is down.
func Revoked(err error) error {
	if err == nil {
		return nil
	}
	return StopReconnect(revokedError{error: err})
}

type TelemetryContext struct {
	MachineID     string
	EnvironmentID string
}

var ErrWriteUncertain = errors.New("terminal write outcome is uncertain")

// ReattachAttempts allows ten minutes of one-second retries through a peer update; 0 disables recovery.
// reattachRefillAfter is how long a reattached link must stay up before its next loss gets a fresh budget:
// three 10 s stall periods (helperStallAfter).
var reattachRefillAfter = 30 * time.Second

func ReattachAttempts(dialRetries int) int { return min(dialRetries, 1) * 600 }

var ErrTransportLost = errors.New("terminal transport lost")

// ErrLocalDaemonLost: the link to the Paperboat daemon on this computer broke (it restarted or stopped).
var ErrLocalDaemonLost = fmt.Errorf("local Paperboat daemon connection lost: %w", ErrTransportLost)

// ErrHandedOver: the host ended this link because the same terminal was attached again (the stalled-link probe resumed
// it); the newer link has the terminal. Without an adopted probe it is redialed like a lost link.
var ErrHandedOver = fmt.Errorf("terminal handed to a newer attachment: %w", ErrTransportLost)

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
	passive           bool
	outputQueueChunks int
	outputBatchWindow time.Duration
	observer          func(ReconnectEvent)
	probeWait         func() // tests: runs before supervise waits for a running probe
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
	passive     bool
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	current     Conn
	available   chan struct{}
	redial      atomic.Bool
	probed      atomic.Pointer[probedLink] // a fresh link dialed while the current one was stalled
	probing     sync.Mutex                 // held by the one running probeStalled; supervise waits on it after a handover end
	linkStalled atomic.Bool
	// refused is a permanent refusal the stalled-link probe met (access revoked); supervise ends with it.
	refused           atomic.Pointer[error]
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
	probeWait         func()
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
	c := &ReconnectingConn{ctx: runCtx, cancel: cancel, current: initial, available: available, reconnect: reconnect, maxRetries: maxRetries, delay: min(delay, time.Second), out: make(chan []byte, options.outputQueueChunks), done: make(chan reconnectResult, 1), closed: make(chan struct{}), telemetry: sink, now: now, started: now(), correlation: correlation, outputBatchWindow: options.outputBatchWindow, observer: options.observer, passive: options.passive, probeWait: options.probeWait}
	go c.supervise(initial)
	return c
}

func (c *ReconnectingConn) supervise(conn Conn) {
	defer close(c.out)
	var recovery *reattach.Recovery
	var firstAttempt bool
	attachedAt := time.Now()
	lifetimeOutcome := "failure"
	defer func() {
		c.recordOutputPerformance(lifetimeOutcome)
		c.record("terminal.lifetime", lifetimeOutcome, c.started)
	}()
	for {
		connection := rand.Text()
		var silent atomic.Bool                             // this link's own state; linkStalled follows whichever link is current
		probeCtx, cancelProbe := context.WithCancel(c.ctx) // this link's probe; ends once supervise is done with the link
		if observed, ok := conn.(LinkStateObserver); ok {
			stalledConn := conn
			observed.ObserveLinkState(func(stalled bool) {
				silent.Store(stalled)
				c.linkStalled.Store(stalled)
				if stalled {
					c.recordOldLink(connection, "supervise", "stalled", nil)
					c.notify(ReconnectStalled)
					if !c.passive {
						go c.probeStalled(probeCtx, stalledConn, connection)
					}
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
					c.recordOldLink(connection, "supervise", "read_failed", err)
					return
				}
			}
		}()
		code, err := conn.Wait()
		c.recordOldLink(connection, "supervise", "wait_returned", err)
		if err == nil && silent.Load() && c.ctx.Err() == nil {
			// A shell exit is heard before it is reported, and hearing the host clears the stall; a clean end of a
			// link still flagged silent is the daemon tearing it down (F3-15 N4), not the terminal ending.
			err = ErrTransportLost
		}
		cleanupCtx := diagnostics.WithReconnect(c.ctx, "old-link", "supervise", connection)
		closeEnd := diagnostics.ReconnectStep(cleanupCtx, "old_close")
		closeErr := conn.Close()
		closeEnd(closeErr)
		joinEnd := diagnostics.ReconnectStep(cleanupCtx, "old_read_join")
		<-readDone
		joinEnd(nil)
		c.recordCompression(conn)
		if errors.Is(err, ErrHandedOver) {
			// The host ends the old link as the probe's attachment completes, which can get here before the probe stores
			// its link: redialing then took the terminal back from it. Let the running probe finish (each attempt is
			// bounded by probeAttemptTimeout) so its link is adopted. A shell exit or an error end does not wait.
			if c.probeWait != nil {
				c.probeWait()
			}
			c.probing.Lock()
			c.probing.Unlock()
		}
		cancelProbe()
		if refused := c.refused.Swap(nil); refused != nil {
			c.notify(ReconnectFailed)
			c.done <- reconnectResult{1, *refused}
			return
		}
		next := c.probed.Swap(nil)
		if next != nil && (next.stalled != conn || c.ctx.Err() != nil) {
			// The probe connected after this supervisor had replaced the link it was made for, or the terminal was
			// closed. Adopting it attached a second time when the replacement ended (an exited terminal came back and
			// replayed its output), or after Close, which left the link and this goroutine behind.
			c.recordOldLink(connection, "supervise", "probe_discarded", nil)
			_ = next.conn.Close()
			next = nil
		}
		if next != nil {
			c.recordOldLink(connection, "supervise", "probe_adopted", nil)
			c.redial.Store(false)
			c.linkStalled.Store(false)
			c.setUnavailable(conn)
			c.reason.Store("no reply from the machine")
			conn = next.conn
			attachedAt = time.Now()
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
		// A link that stayed up reattachRefillAfter gets a fresh budget, like EternalTerminal's per-disconnect
		// pollReconnect loop (~/ref/EternalTerminal/src/base/ClientConnection.cpp:210), so a held terminal outlives
		// many peer restarts; a link that dies right after each reattach keeps spending one budget (51db8129).
		// ET dials immediately on loss, then sleeps only after a failed attempt.
		if c.passive || recovery == nil || time.Since(attachedAt) >= reattachRefillAfter {
			recovery = reattach.New(reattach.Policy{Attempts: c.maxRetries, Backoff: func(int) time.Duration {
				if firstAttempt {
					firstAttempt = false
					return 0
				}
				return c.delay
			}, Retryable: func(err error) bool { var stopped stopReconnectError; return !errors.As(err, &stopped) }})
		}
		firstAttempt = true
		recoveryErr := recovery.Run(c.ctx, func(dialCtx context.Context) error {
			attemptStarted := c.now()
			next, dialErr := c.dialReconnect(dialCtx, "supervise", connection)
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
			attachedAt = time.Now()
			c.setCurrent(next)
			// A probe that stored its link while this redial ran was made for the link just replaced.
			c.closeProbed()
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

// probeAttemptTimeout: a silent link is given up by TCP after about 2 min and then redialed by supervise, so a
// probe attempt that takes longer can only hold the probe slot.
const probeAttemptTimeout = 2 * time.Minute

// probedLink is a fresh link and the stalled link it was dialed to replace.
type probedLink struct{ stalled, conn Conn }

// probeStalled dials, with backoff, while a link is silent, keeping the silent link: a machine whose carrier restarted
// leaves it dead until TCP gives up (~2 min), while a network that comes back resumes it. A fresh link replaces
// it; the daemon carries unanswered input to it by input owner. ET does the same on keepalive loss
// (~/ref/EternalTerminal/src/base/ClientConnection.cpp:210).
func (c *ReconnectingConn) probeStalled(ctx context.Context, stalled Conn, connection string) {
	if c.reconnect == nil || c.maxRetries <= 0 || !c.probing.TryLock() {
		return
	}
	defer c.probing.Unlock()
	current := func() bool {
		c.mu.RLock()
		current := c.current
		c.mu.RUnlock()
		// ctx ends once supervise is done with the link, so a probe can never store a link nobody will adopt.
		return current == stalled && ctx.Err() == nil
	}
	still := func() bool { return c.linkStalled.Load() && current() }
	backoff := reattach.Backoff(c.delay, c.delay)
	for attempt := 1; attempt <= c.maxRetries && still(); attempt++ {
		dialCtx, cancel := context.WithTimeout(ctx, probeAttemptTimeout)
		next, err := c.dialReconnect(dialCtx, "probeStalled", connection)
		cancel()
		var stop stopReconnectError
		if errors.As(err, &stop) {
			// Revoked access ends the terminal now; keeping the silent link only delayed it until the transport
			// gave up, about 150 s of silence (F3-15 N13). Any other permanent refusal (pb wraps every dial error
			// that is not retryable, such as the open timeout while the network is down) stops probing and keeps
			// the link, which the normal reconnect replaces once it is lost (F3-15 N4).
			var revoked revokedError
			if errors.As(err, &revoked) && still() {
				c.refused.Store(&err)
				_ = stalled.Close()
			}
			return
		}
		if err == nil && next != nil {
			// An attached probe has the terminal: the host hands it over from the old link, whose last message
			// clears its stall before the host ends it. Discarding the probe then left no link at all and the
			// terminal silent (F3-15 N4/N13, probe_discarded). Like ET and mosh, the newest attachment wins.
			if !current() {
				c.recordOldLink(connection, "probeStalled", "probe_discarded", nil)
				_ = next.Close()
				return
			}
			c.recordOldLink(connection, "probeStalled", "probe_selected", nil)
			link := &probedLink{stalled: stalled, conn: next}
			c.probed.Store(link)
			if !current() && c.probed.CompareAndSwap(link, nil) {
				// The supervisor left the stalled link between the check above and the store, and is redialing
				// itself: nobody would adopt this link, and the terminal must not hold two.
				c.recordOldLink(connection, "probeStalled", "probe_discarded", nil)
				_ = next.Close()
				return
			}
			_ = stalled.Close()
			return
		}
		if reattach.Wait(ctx, backoff(attempt)) != nil {
			return
		}
	}
}

func (c *ReconnectingConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.cancel != nil {
			c.cancel()
		}
		c.closeProbed() // before the current link ends, so the supervisor finds none to adopt
		c.mu.RLock()
		conn := c.current
		c.mu.RUnlock()
		if conn != nil {
			err = conn.Close()
		}
	})
	return err
}

// closeProbed closes a probe link nobody will adopt.
func (c *ReconnectingConn) closeProbed() {
	if stale := c.probed.Swap(nil); stale != nil {
		_ = stale.conn.Close()
	}
}
