package workloadbridge

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

type recordingTransport struct{ requests []*http.Request }

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, r)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Request: r}, nil
}

// A worker that kept idle connections as long as the session owner reused one as the owner closed it, failed its
// health call with a connection reset and exited, taking hostd and every preview carrier down (canary-a
// 2026-11-11 17:53 and 19:33). The worker drops idle connections first, and the health call is replayable, so
// net/http retries it on a fresh connection.
func TestWorkerConnectionsCannotRaceTheOwnersIdleClose(t *testing.T) {
	c, err := NewClient("/run/owner.sock", bytes.Repeat([]byte{1}, 32), "worker", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.transport.IdleConnTimeout <= 0 || c.transport.IdleConnTimeout >= ownerIdleTimeout {
		t.Fatalf("worker idle timeout %s is not below the owner's %s", c.transport.IdleConnTimeout, ownerIdleTimeout)
	}
	recorder := &recordingTransport{}
	c.http.Transport = recorder
	if err := c.Health(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(t.Context(), "session", "Write", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(recorder.requests) != 2 || recorder.requests[0].Header.Get("Idempotency-Key") == "" || recorder.requests[0].GetBody == nil {
		t.Fatal("the health call is not replayable")
	}
	if recorder.requests[1].Header.Get("Idempotency-Key") != "" {
		t.Fatal("a mutating call was marked replayable")
	}
}
