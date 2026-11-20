package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
)

// blockingPeerNetwork holds every configuration build until release is closed, like a cold cache after a restart.
type blockingPeerNetwork struct {
	peerNetworkStub
	started chan struct{}
	release chan struct{}
}

func (b *blockingPeerNetwork) Configuration(ctx context.Context, in peersessions.NetworkConfigRequest) (peersessions.NetworkConfigResult, error) {
	b.started <- struct{}{}
	<-b.release
	return b.peerNetworkStub.Configuration(ctx, in)
}

// A reconnect burst into a cold cache: past the in-flight bound a request is answered 503 with a jittered
// Retry-After at once (no build, no support report), and a request after the burst drains is served again.
func TestPeerNetworkConfigShedsAReconnectBurstPastTheBound(t *testing.T) {
	const bound, burst = 4, 40
	service := &blockingPeerNetwork{started: make(chan struct{}, burst), release: make(chan struct{})}
	handler := peerNetworkConfig(shedPeerNetwork(service, bound))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/peer-network/config", bytes.NewReader([]byte(`{"operation_id":"operation_123"}`)))
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "account_1"}, Client: &auth.ClientPrincipal{SessionID: "cli_1"}}))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	var building sync.WaitGroup
	served := make([]*httptest.ResponseRecorder, bound)
	for i := range bound {
		building.Add(1)
		go func() { defer building.Done(); served[i] = request() }()
	}
	for range bound {
		<-service.started // the bound's worth of builds are in flight
	}
	retryAfters := map[string]bool{}
	for range burst - bound {
		w := request()
		seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
		if w.Code != http.StatusServiceUnavailable || err != nil || seconds < 1 || seconds > 3 {
			t.Fatalf("shed request: status=%d retry-after=%q (%v), want 503 with 1-3 s", w.Code, w.Header().Get("Retry-After"), err)
		}
		retryAfters[w.Header().Get("Retry-After")] = true
	}
	if len(retryAfters) < 2 {
		t.Fatalf("every shed request got the same Retry-After %v: the burst would return as one wave", retryAfters)
	}
	close(service.release)
	building.Wait()
	for i, w := range served {
		if w.Code != http.StatusOK {
			t.Fatalf("in-flight request %d = %d, want 200", i, w.Code)
		}
	}
	service.started = make(chan struct{}, 1)
	if w := request(); w.Code != http.StatusOK {
		t.Fatalf("request after the burst drained = %d, want 200", w.Code)
	}
}
