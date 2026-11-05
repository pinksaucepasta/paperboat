//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/operationrevocation"
)

// The host sends its live operation credential IDs in the server's bounded contract, chunked
// when they exceed one request, and a revoked ID ends its admitted operation.
func TestRevocationRefreshSendsLiveOperationsAndEndsRevoked(t *testing.T) {
	var mu sync.Mutex
	var requests [][]string
	recent := "jti-recently-revoked"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input struct {
			ActiveJTIs []string `json:"active_jtis"`
		}
		if decoder.Decode(&input) != nil || operationrevocation.ValidateLive(input.ActiveJTIs) != nil {
			http.Error(w, `{"error":{"code":"validation_failed"}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, input.ActiveJTIs)
		mu.Unlock()
		// The server answers the watched IDs it revoked plus recent revocations that fence new admissions.
		revoked := []string{recent}
		for _, id := range input.ActiveJTIs {
			if id == "jti-0007" || id == "jti-1500" {
				revoked = append(revoked, id)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"jtis": revoked})
	}))
	defer server.Close()
	cache := auth.NewRevocationCache()
	signals := map[string]<-chan struct{}{}
	for i := range operationrevocation.MaxLiveJTIs + 600 {
		id := fmt.Sprintf("jti-%04d", i)
		_, signal, release, err := cache.Watch(id)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		signals[id] = signal
	}
	lease, err := admission.Admit(context.Background(), time.Now().Add(time.Hour), signals["jti-1500"])
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	service, err := newRevocationRefreshService(server.URL+"/v1/helper-trust/revocations", revocationTestAuth{}, revocationTestAuth{}, func() (string, error) {
		return "operation_revocation_refresh", nil
	}, cache, server.Client().Transport, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.refresh(context.Background()); err != nil {
		t.Fatalf("revocation refresh rejected: %v", err)
	}
	mu.Lock()
	sent := requests
	mu.Unlock()
	if len(sent) != 2 || len(sent[0])+len(sent[1]) != operationrevocation.MaxLiveJTIs+600 {
		t.Fatalf("live operations were not sent in bounded chunks: %d requests", len(sent))
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("revoked operation kept running")
	}
	for id, signal := range signals {
		select {
		case <-signal:
			if id != "jti-0007" && id != "jti-1500" {
				t.Fatalf("operation %s ended without revocation", id)
			}
		default:
			if id == "jti-0007" {
				t.Fatal("revoked operation in the first chunk kept running")
			}
		}
	}
	if !cache.Revoked(auth.Claims{JTI: recent}) {
		t.Fatal("a recently revoked credential can still be admitted")
	}
	// A host with no live operation still sends the required empty list.
	empty := auth.NewRevocationCache()
	mu.Lock()
	requests = nil
	mu.Unlock()
	service.cache = empty
	err = service.refresh(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if err != nil || len(requests) != 1 || len(requests[0]) != 0 {
		t.Fatalf("idle host refresh = %v, requests %v", err, requests)
	}
}
