//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	clientapi "github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/controlbackoff"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
)

// A removed machine's carrier exited at start ("machine authentication token: control request rejected"), and every
// later pb install failed "recovering prior installation state: carrier control unavailable" (a Windows test runner,
// 2026-11-26).
func TestRefusedMachineCredentialKeepsTheCarrierRunning(t *testing.T) {
	refused := fmt.Errorf("machine authentication token: %w", enrollment.ErrRenewalRefused)
	if !transientNativePeerStart(refused) {
		t.Fatal("a refused renewal ends the carrier")
	}
	if err := allowPendingPeerEnrollment(context.Background(), scriptedRefusal{refused}); err != nil {
		t.Fatalf("carrier start with a refused renewal: %v", err)
	}
}

type scriptedRefusal struct{ err error }

func (e scriptedRefusal) Ensure(context.Context) error { return e.err }

// A removed machine's concurrent startup request opened the local 401 breaker; the peer start that followed got the
// held error (not an *APIError) and the carrier exited.
func TestHeldIdentityRejectionKeepsTheCarrierRunning(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	client := &http.Client{Transport: &controlbackoff.Transport{Base: http.DefaultTransport, Host: target.Host}}
	call := func() error {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/peer", nil)
		request.Header.Set("X-Paperboat-Machine-Proof", "proof")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		response.Body.Close()
		return nil
	}
	if err := call(); err != nil {
		t.Fatalf("first request: %v", err)
	}
	held := call()
	if !errors.Is(held, controlbackoff.ErrIdentityRejected) || hits.Load() != 1 {
		t.Fatalf("second request was not held: err=%v hits=%d", held, hits.Load())
	}
	held = fmt.Errorf("peer start: %w", held)
	if !transientNativePeerStart(held) {
		t.Fatalf("a held identity rejection ends the carrier: %v", held)
	}
	if err := allowPendingPeerEnrollment(context.Background(), scriptedRefusal{held}); err != nil {
		t.Fatalf("carrier start with a held identity rejection: %v", err)
	}
	var forbidden error = &clientapi.APIError{Status: http.StatusForbidden}
	if errors.Is(forbidden, clientapi.ErrUnauthenticated) {
		t.Fatal("403 reads as unauthenticated")
	}
}
