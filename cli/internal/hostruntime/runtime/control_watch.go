//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// controlDownFor is how long one failed control request marks Paperboat unreachable for the commands that would
// otherwise wait out their own request timeout (an ENV lookup holds a launch for up to 15 s on a blackholed API).
// The host's other control traffic keeps failing, and so keeps the mark fresh, for as long as the outage lasts; any
// answer clears it at once.
const controlDownFor = 30 * time.Second

// controlWatch is the host's single view of whether Paperboat answers: it wraps the transport every control request
// of the process goes through. Prior art: tailscale's controlclient keeps one "last control error" for the whole
// client instead of asking per feature.
type controlWatch struct {
	next     http.RoundTripper
	mu       sync.Mutex
	failedAt time.Time
}

func (w *controlWatch) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := w.next.RoundTrip(request)
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case err == nil:
		w.failedAt = time.Time{}
	case !errors.Is(err, context.Canceled): // a caller's own cancel says nothing about the path; a client timeout surfaces as "request canceled"
		w.failedAt = time.Now()
	}
	return response, err
}

// ControlDown reports that the last control request failed on the network less than controlDownFor ago.
func (w *controlWatch) ControlDown() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.failedAt.IsZero() && time.Since(w.failedAt) < controlDownFor
}

// controlDownError is what a launch gets instead of waiting: a network timeout, so the user is told "needs Paperboat
// online" exactly as after the real wait.
type controlDownError struct{}

func (controlDownError) Error() string {
	return "Paperboat is unreachable (a recent control request got no answer)"
}
func (controlDownError) Timeout() bool   { return true }
func (controlDownError) Temporary() bool { return true }

// controlKnownDown asks the transport, when it is the watch, whether Paperboat is known to be unreachable.
func controlKnownDown(transport http.RoundTripper) bool {
	watch, ok := transport.(interface{ ControlDown() bool })
	return ok && watch.ControlDown()
}
