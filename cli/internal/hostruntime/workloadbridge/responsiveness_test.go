package workloadbridge

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type stalledDetachService struct {
	session.Service
	entered chan struct{}
	release chan struct{}
}

func (s *stalledDetachService) DetachAllAttachments() error {
	close(s.entered)
	<-s.release
	return nil
}
func (*stalledDetachService) List() []session.Snapshot { return nil }
func (*stalledDetachService) Create(context.Context, session.CreateRequest) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func TestOwnerHealthAndListWhileWorkerDetachStalls(t *testing.T) {
	sessions := &stalledDetachService{entered: make(chan struct{}), release: make(chan struct{})}
	token := make([]byte, 32)
	server := &Server{Token: token, Sessions: sessions, MaximumConcurrent: 8,
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "replacement", Epoch: 2}
		},
		AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil },
	}
	handler := server.handler()
	request := func(path string) {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("X-Paperboat-Owner-Capability", base64.RawURLEncoding.EncodeToString(token))
		req.Header.Set("X-Paperboat-Worker-ID", "replacement")
		req.Header.Set("X-Paperboat-Worker-Epoch", "2")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != 200 || strings.Contains(recorder.Body.String(), `"Error":{`) {
			t.Errorf("%s failed: %s", path, recorder.Body.String())
		}
	}
	done := make(chan struct{})
	go func() { request("/session/Create"); close(done) }()
	<-sessions.entered
	defer func() { close(sessions.release); <-done }()
	observed := make(chan struct{})
	go func() { request("/owner/health"); request("/session/List"); close(observed) }()
	select {
	case <-observed:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("health/List blocked behind worker detach")
	}
}

func TestOwnerHealthAndListWhileRetirementStalls(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	token := make([]byte, 32)
	server := &Server{Token: token, Sessions: &stalledDetachService{}, SessionOnly: true,
		MaximumConcurrent: 8, Retire: func(context.Context) error { close(entered); <-release; return nil },
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1}
		},
	}
	handler := server.handler()
	request := func(path string) {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("X-Paperboat-Owner-Capability", base64.RawURLEncoding.EncodeToString(token))
		req.Header.Set("X-Paperboat-Worker-ID", "worker")
		req.Header.Set("X-Paperboat-Worker-Epoch", "1")
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	done := make(chan struct{})
	go func() { request("/owner/retire"); close(done) }()
	<-entered
	observed := make(chan struct{})
	defer func() { close(release); <-done; <-observed }()
	go func() { request("/owner/health"); request("/session/List"); close(observed) }()
	select {
	case <-observed:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("health/List blocked behind retirement")
	}
}
