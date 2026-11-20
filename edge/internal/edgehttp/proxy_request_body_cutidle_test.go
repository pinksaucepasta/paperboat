package edgehttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

// timeoutReader blocks until released, then fails the way a read whose deadline expired does.
type timeoutReader struct{ release chan struct{} }

func (r timeoutReader) Read([]byte) (int, error) { <-r.release; return 0, os.ErrDeadlineExceeded }
func (r timeoutReader) Close() error             { return nil }

// The error handler asks cutIdle from another goroutine while a read is ending. Once the idle timeout has passed it must say yes at
// every instant, also between the read failing and Read returning: a window in which it said no made the gateway read the cancelled
// context as "client left", write nothing, and send the visitor an empty 200 instead of 408.
func TestCutIdleHoldsAtEveryInstantOnceTheIdleTimeoutHasPassed(t *testing.T) {
	for range 300 {
		release := make(chan struct{})
		body := &proxyRequestBody{body: timeoutReader{release}, controller: http.NewResponseController(httptest.NewRecorder()), idle: time.Millisecond}
		finished := make(chan struct{})
		go func() { _, _ = body.Read(make([]byte, 1)); close(finished) }()
		for body.waiting.Load() == 0 {
			runtime.Gosched()
		}
		time.Sleep(3 * time.Millisecond) // the idle timeout has passed while the read waits
		close(release)
		for {
			if !body.cutIdle() {
				t.Fatal("cutIdle was false after the idle timeout passed: the upload would be answered as a closed client")
			}
			select {
			case <-finished:
				if !body.cutIdle() {
					t.Fatal("cutIdle was false after the read returned")
				}
				goto next
			default:
			}
		}
	next:
	}
}
