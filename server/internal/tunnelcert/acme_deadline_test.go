package tunnelcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Each HTTP exchange completes promptly, but the real RFC8555 order stays
// pending. The issuance deadline must bound the client's polling across those
// exchanges, rather than merely bounding each HTTP request.
func TestACMEIssuanceDeadlineBoundsPendingOrder(t *testing.T) {
	for _, tc := range []struct {
		name             string
		issuance, parent time.Duration
	}{
		// The deadlines are large enough for the fixture's first exchanges (directory, nonce, order,
		// authorization) to finish before they fire, also under -race on a busy machine; the longer
		// bound is far more than the slack, so an unbounded poll loop is still caught.
		{"issuer_deadline", 600 * time.Millisecond, 4 * time.Second},
		{"parent_deadline", 3 * time.Second, 600 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Replay-Nonce", "deadline-fixture-nonce")
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/directory":
					json.NewEncoder(w).Encode(map[string]string{"newNonce": base + "/nonce", "newOrder": base + "/new-order"})
				case "/nonce":
					w.WriteHeader(http.StatusOK)
				case "/new-order":
					w.Header().Set("Location", base+"/order")
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"status": "pending", "authorizations": []string{base + "/authorization"}, "finalize": base + "/finalize"})
				case "/authorization":
					json.NewEncoder(w).Encode(map[string]any{"status": "valid", "identifier": map[string]string{"type": "dns", "value": "app.example.test"}})
				case "/order":
					polls.Add(1)
					w.Header().Set("Retry-After", "1")
					json.NewEncoder(w).Encode(map[string]any{"status": "pending", "authorizations": []string{base + "/authorization"}, "finalize": base + "/finalize"})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			base = server.URL
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			issuer, err := NewACMEIssuer(ACMEIssuerConfig{DirectoryURL: base + "/directory", AccountKID: base + "/account", AccountKey: key, DNS: deadlineUnusedDNS{}, HTTPClient: server.Client(), Timeout: tc.issuance, ChallengeZone: "challenge.example.test"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
			defer cancel()
			started := time.Now()
			domain := testDomain("app.example.test", StrategyDelegatedDNS01)
			domain.ChallengeReference = "dns-challenge://deadline_fixture"
			_, err = issuer.IssueDNS01(ctx, domain)
			if err == nil {
				t.Fatal("pending order unexpectedly issued")
			}
			if polls.Load() == 0 {
				t.Fatalf("fixture did not exercise real ACME order polling: %v", err)
			}
			bound := tc.issuance
			if tc.parent < bound {
				bound = tc.parent
			}
			if elapsed := time.Since(started); elapsed > bound+time.Second {
				t.Fatalf("pending order exceeded issuance/parent bound: %v > %v", elapsed, bound)
			}
		})
	}
}

type deadlineUnusedDNS struct{}

func (deadlineUnusedDNS) Present(context.Context, DNS01Record) (DNS01Record, error) {
	panic("already authorized order must not present DNS")
}
func (deadlineUnusedDNS) Wait(context.Context, DNS01Record) error {
	panic("already authorized order must not wait DNS")
}
func (deadlineUnusedDNS) Cleanup(context.Context, DNS01Record) error {
	panic("already authorized order must not clean DNS")
}
