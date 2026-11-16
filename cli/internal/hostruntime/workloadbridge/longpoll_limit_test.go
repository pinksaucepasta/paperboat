package workloadbridge

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

type waitingService struct {
	stalledDetachService
	waiting chan struct{}
	release chan struct{}
}

func (s *waitingService) WaitNext(ctx context.Context, _, _ string, _ uint64) (history.Event, error) {
	s.waiting <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return history.Event{}, ctx.Err()
}

// F21-164: every attachment holds an output wait for up to 25 s. Counted in the request limit, 90 terminals and ~35
// viewers filled it and every further create or attach was refused with the terminal ceiling message. Waits beyond the
// request limit must not block other requests or each other.
func TestOutputWaitsDoNotFillTheRequestLimit(t *testing.T) {
	sessions := &waitingService{waiting: make(chan struct{}), release: make(chan struct{})}
	token := make([]byte, 32)
	server := &Server{Token: token, Sessions: sessions, MaximumConcurrent: 2,
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1}
		},
		AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil },
	}
	handler := server.handler()
	request := func(path, body string) string {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("X-Paperboat-Owner-Capability", base64.RawURLEncoding.EncodeToString(token))
		req.Header.Set("X-Paperboat-Worker-ID", "worker")
		req.Header.Set("X-Paperboat-Worker-Epoch", "1")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Body.String()
	}
	const waits = 4 // twice the request limit
	done := make(chan struct{}, waits)
	started := 0
	defer func() {
		close(sessions.release)
		for range started {
			<-done
		}
	}()
	for range waits {
		started++
		go func() {
			request("/session/WaitNext", `{"Sessionid":"s","Attachmentid":"a","From":0}`)
			done <- struct{}{}
		}()
		select {
		case <-sessions.waiting:
		case <-time.After(2 * time.Second):
			t.Fatal("an output wait beyond the request limit was refused instead of waiting")
		}
	}
	if body := request("/session/List", `{}`); strings.Contains(body, `"Error":{`) {
		t.Fatalf("List with %d output waits open = %s, want an answer", waits, body)
	}
}
