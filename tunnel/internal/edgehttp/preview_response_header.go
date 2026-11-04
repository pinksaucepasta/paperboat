package edgehttp

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// responseHeaderWait cancels the header read when the origin stays silent for
// timeout after the request was fully written.
type responseHeaderWait struct {
	timeout  time.Duration
	cancel   context.CancelFunc
	mu       sync.Mutex
	timer    *time.Timer
	started  bool
	finished bool
	expired  bool
}

func (w *responseHeaderWait) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.finished {
		return
	}
	w.started = true
	w.timer = time.AfterFunc(w.timeout, func() {
		w.mu.Lock()
		w.expired = !w.finished
		w.mu.Unlock()
		w.cancel()
	})
}

// finish stops the timer and reports whether it expired first.
func (w *responseHeaderWait) finish() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finished = true
	if w.timer != nil {
		w.timer.Stop()
	}
	return w.expired
}

type boundedPreviewResponseHeaderReader struct {
	reader  io.Reader
	maximum int
	read    int
	match   int
	done    bool
}

func (r *boundedPreviewResponseHeaderReader) Read(payload []byte) (int, error) {
	n, err := r.reader.Read(payload)
	if r.done || n == 0 {
		return n, err
	}
	separator := [...]byte{'\r', '\n', '\r', '\n'}
	for index := 0; index < n; index++ {
		r.read++
		if r.read > r.maximum {
			return 0, fmt.Errorf("%w: response headers exceed %d bytes", ErrDataCarrierPreviewTransport, r.maximum)
		}
		if payload[index] == separator[r.match] {
			r.match++
			if r.match == len(separator) {
				r.done = true
				break
			}
		} else if payload[index] == separator[0] {
			r.match = 1
		} else {
			r.match = 0
		}
	}
	return n, err
}

type dataCarrierPreviewResponseHeaderTimeoutError struct {
	timeout time.Duration
}

func (e *dataCarrierPreviewResponseHeaderTimeoutError) Error() string {
	if e == nil {
		return ErrDataCarrierPreviewResponseHeaderTimeout.Error()
	}
	return fmt.Sprintf("%s after %s", ErrDataCarrierPreviewResponseHeaderTimeout, e.timeout)
}

func (e *dataCarrierPreviewResponseHeaderTimeoutError) Is(target error) bool {
	return target == ErrDataCarrierPreviewResponseHeaderTimeout
}

func (*dataCarrierPreviewResponseHeaderTimeoutError) Timeout() bool   { return true }
func (*dataCarrierPreviewResponseHeaderTimeoutError) Temporary() bool { return true }
