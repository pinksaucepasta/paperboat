package edgehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"runtime"
	"strings"
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

// The upload was cut by the idle timeout but the transport's error is a plain stream reset / cancelled context (whichever of its read
// and write failed first). The handler must still answer 408 and not take it for a client that left: it used to look at request.Body,
// which http.MaxBytesReader wraps in production, so it never found the idle body and the visitor got an empty 200 (11-15% of
// TestStalledUploadThroughDurableTunnelGets408 runs under -race).
func TestErrorHandlerAnswers408WhenTheIdleCutSurfacedAsACancelledStream(t *testing.T) {
	idle := &proxyRequestBody{body: io.NopCloser(strings.NewReader("")), idle: time.Millisecond}
	idle.idleExpired.Store(true)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), idleBodyKey{}, idle))
	cancel() // the server cancels the request context when the idle read deadline expires
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/upload", nil).WithContext(ctx)
	request.Body = http.MaxBytesReader(recorder, idle, 1<<20) // as the gateway wraps it
	proxy := &httputil.ReverseProxy{}
	configureProxyDiagnostics(proxy, nil)
	proxy.ErrorHandler(recorder, request, errors.New("stream reset, error code: 0: context canceled"))
	if recorder.Code != http.StatusRequestTimeout {
		t.Fatalf("status %d, want 408", recorder.Code)
	}
}
