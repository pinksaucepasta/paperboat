package control

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A server rollout closes the edge's kept-alive control connection while a request is in flight
// (production 2026-11-05 07:06:24.887, end_of_stream on a usage report). Every idempotent node
// request is resent once on a fresh connection and no control failure is recorded. A certificate
// distribution request carries a single-use signed nonce, so it is not resent.
func TestHTTPClientRequestsSurviveConnectionClosedMidRequest(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		call       func(context.Context, *HTTPClient) error
		resent     bool
	}{
		{"heartbeat", `{"ingress_limits":null}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.Heartbeat(ctx, NodeObservation{NodeID: "edge"})
			return err
		}, true},
		{"usage report", `{"disposition":"accounted"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.ReportUsage(ctx, UsageReport{OperationID: "op", Key: usageKey(), Interval: [2]time.Time{time.Unix(1, 0), time.Unix(2, 0)}})
			return err
		}, true},
		{"desired routes", `{"complete":true,"version":"v1","routes":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.DesiredRoutes(ctx, "edge")
			return err
		}, true},
		{"route observations", ``, func(ctx context.Context, c *HTTPClient) error {
			return c.ObserveRoutes(ctx, "edge", nil)
		}, true},
		{"runtime carrier observations", ``, func(ctx context.Context, c *HTTPClient) error {
			return c.ObserveRuntimeCarriers(ctx, "edge_1", "edge_epoch_1", []runtimewire.Observation{{RouteID: "route_1", SessionID: "session_1", AttachmentGeneration: 1}})
		}, true},
		{"certificate request", ``, func(ctx context.Context, c *HTTPClient) error {
			distribution, err := NewCertificateDistributionClient(c)
			if err != nil {
				return err
			}
			return distribution.RequestCertificate(ctx, "edge_1", "edge_epoch_1", "app.example.test")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				if tc.body == "" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
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
			_, key, _ := ed25519.GenerateKey(nil)
			signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
			if err != nil || client.SetDistributionRequestSigner(signer) != nil {
				t.Fatal(err)
			}
			if err := tc.call(context.Background(), client); err != nil {
				t.Fatalf("first request: %v", err)
			}
			err = tc.call(context.Background(), client) // reuses the kept-alive connection the server closes
			if tc.resent {
				if err != nil || requests.Load() != 3 || failures.Load() != 0 {
					t.Fatalf("err %v, server saw %d requests (want 3, one resent), control failures %d", err, requests.Load(), failures.Load())
				}
				return
			}
			if err == nil || requests.Load() != 2 || failures.Load() != 1 {
				t.Fatalf("err %v, server saw %d requests (want 2, none resent), control failures %d", err, requests.Load(), failures.Load())
			}
		})
	}
}
