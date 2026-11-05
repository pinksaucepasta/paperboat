package control

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A server rollout closes the edge's kept-alive control connection while a
// heartbeat is in flight. The heartbeat is resent on a fresh connection and no
// control failure is recorded.
func TestHTTPClientHeartbeatSurvivesConnectionClosedMidRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 2 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close() // the old server goes away before answering
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ingress_limits":null}`)
	}))
	defer server.Close()
	var failures atomic.Int32
	client, err := NewHTTPClient(HTTPConfig{
		BaseURL: server.URL, Credential: testControlCredential, Timeout: 5 * time.Second, Client: server.Client(),
		ControlFailure: func(context.Context, string, error) { failures.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil {
			t.Fatalf("heartbeat %d: %v", attempt, err)
		}
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("server saw %d heartbeat requests, want 3 (one resent)", got)
	}
	if got := failures.Load(); got != 0 {
		t.Fatalf("control failures recorded = %d, want 0", got)
	}
}
