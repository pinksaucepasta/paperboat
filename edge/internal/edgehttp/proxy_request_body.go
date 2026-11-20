package edgehttp

import (
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// proxyRequestBody owns only the incoming HTTP body. Expiring the server read
// before Close releases net/http's body mutex without closing the connection.
type proxyRequestBody struct {
	body        io.ReadCloser
	controller  *http.ResponseController
	idle        time.Duration // cut the upload after waiting this long for client bytes; zero never
	deadlineMu  sync.Mutex    // orders the idle deadline against Close's interrupting deadline
	finished    atomic.Bool
	idleExpired atomic.Bool  // the idle deadline cut the upload: the error handler answers 408 whatever error surfaced
	waiting     atomic.Int64 // when the read in progress started waiting for client bytes (unix ns); zero between reads
	interrupted atomic.Bool
	once        sync.Once
	closeErr    error
}

// errRequestBodyIdle means the client sent no body bytes for the idle timeout (nginx
// client_body_timeout); the proxy answers 408.
var errRequestBodyIdle = errors.New("HTTP upload sent no data within the idle timeout")

func (b *proxyRequestBody) Read(payload []byte) (int, error) {
	started := time.Now()
	b.waiting.Store(started.UnixNano())
	b.idleDeadline(started.Add(b.idle))
	n, err := b.body.Read(payload)
	b.idleDeadline(time.Time{})
	timedOut, interrupted := uploadReadTimeout(err), b.interrupted.Load()
	// idleExpired is set BEFORE waiting is cleared: cutIdle (the error handler, on another goroutine) must see one of the two at every
	// instant. Clearing waiting first left a window in which neither held, the cancelled context read as "client left", nothing was
	// written and the visitor got an empty 200 instead of 408 (11 of 150 runs under -race, 2026-11-20).
	// A read the transport interrupted (Close, when it tears the request down after the stream reset the idle cut caused) is an idle cut
	// all the same if it had already waited the whole idle time: judging by who ended the read lost the cut, cutIdle stayed false and the
	// visitor got an empty 200 (38 of 300 runs under -race with only the ordering fix).
	if b.idle > 0 && timedOut && (!interrupted || time.Since(started) >= b.idle) {
		b.idleExpired.Store(true)
	}
	b.waiting.Store(0)
	if err == io.EOF {
		b.finished.Store(true)
	}
	if interrupted && timedOut {
		return n, &proxyUploadStopped{cause: err}
	}
	if b.idle > 0 && timedOut {
		return n, errors.Join(errRequestBodyIdle, err)
	}
	return n, err
}

// idleDeadline bounds only the wait for the client's next bytes: set before each read and cleared
// after it, so time the proxy spends writing upstream (backpressure) never counts. HTTP/3 has no
// read deadline (ErrNotSupported); QUIC's own idle timeout bounds it there.
func (b *proxyRequestBody) idleDeadline(deadline time.Time) {
	if b.idle <= 0 {
		return
	}
	b.deadlineMu.Lock()
	defer b.deadlineMu.Unlock()
	if !b.interrupted.Load() {
		_ = b.controller.SetReadDeadline(deadline)
	}
}

func (b *proxyRequestBody) Close() error {
	b.once.Do(func() {
		var deadlineErr error
		if !b.finished.Load() {
			b.deadlineMu.Lock()
			b.interrupted.Store(true)
			deadlineErr = b.controller.SetReadDeadline(time.Now())
			if deadlineErr != nil {
				b.interrupted.Store(false)
			}
			b.deadlineMu.Unlock()
		}
		cause := b.body.Close()
		if b.interrupted.Load() && uploadReadTimeout(cause) {
			cause = &proxyUploadStopped{cause: cause}
		}
		// The owned read has finished when body.Close returns. Restore the deadline
		// so subsequent requests on a reusable HTTP connection retain their budget.
		var resetErr error
		if b.interrupted.Load() {
			resetErr = b.controller.SetReadDeadline(time.Time{})
		}
		if deadlineErr == nil && resetErr == nil {
			b.closeErr = cause
		} else {
			b.closeErr = errors.Join(deadlineErr, cause, resetErr)
		}
	})
	return b.closeErr
}

type proxyUploadStopped struct{ cause error }

func (*proxyUploadStopped) Error() string   { return "HTTP upload stopped during response cleanup" }
func (e *proxyUploadStopped) Unwrap() error { return e.cause }

func uploadReadTimeout(err error) bool {
	return err != nil && requestErrorLeaves(err, func(leaf error) bool { timed, ok := leaf.(net.Error); return ok && timed.Timeout() }, true)
}

// requestUploadBody retains the original read cause because Request.Write's
// private requestBodyReadError has no Unwrap in the pinned Go implementation.
type requestUploadBody struct {
	body      io.ReadCloser
	closeBody func() error
	mu        sync.Mutex
	readErr   error
}

func (b *requestUploadBody) Read(payload []byte) (int, error) {
	n, err := b.body.Read(payload)
	if err != nil && err != io.EOF {
		b.mu.Lock()
		if b.readErr == nil {
			b.readErr = err
		}
		b.mu.Unlock()
	}
	return n, err
}
func (b *requestUploadBody) Close() error { return b.closeBody() }
func (b *requestUploadBody) preserve(err error) error {
	if b == nil || err == nil {
		return err
	}
	b.mu.Lock()
	cause := b.readErr
	b.mu.Unlock()
	if cause == nil {
		return err
	}
	kind := reflect.TypeOf(err)
	if kind.PkgPath() == "net/http" && kind.Name() == "requestBodyReadError" {
		return cause
	}
	return errors.Join(err, cause)
}

// cutIdle reports whether the idle deadline cut this upload: its read failed on it, or a read still waits past it
// (the transport can fail first: net/http cancels the request on the expired read, which aborts the stream).
func (b *proxyRequestBody) cutIdle() bool {
	if b.idle <= 0 {
		return false
	}
	waiting := b.waiting.Load()
	return b.idleExpired.Load() || waiting != 0 && time.Since(time.Unix(0, waiting)) >= b.idle
}
