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
	finished    atomic.Bool
	interrupted atomic.Bool
	once        sync.Once
	closeErr    error
}

func (b *proxyRequestBody) Read(payload []byte) (int, error) {
	n, err := b.body.Read(payload)
	if err == io.EOF {
		b.finished.Store(true)
	}
	if b.interrupted.Load() && uploadReadTimeout(err) {
		return n, &proxyUploadStopped{cause: err}
	}
	return n, err
}

func (b *proxyRequestBody) Close() error {
	b.once.Do(func() {
		var deadlineErr error
		if !b.finished.Load() {
			b.interrupted.Store(true)
			deadlineErr = b.controller.SetReadDeadline(time.Now())
			if deadlineErr != nil {
				b.interrupted.Store(false)
			}
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
