package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/tlscert"
)

// A server rollout reloads Caddy, which closes its kept-alive client connections; a request the
// edge had already written onto one gets no answer (production 2026-11-05 10:22:31.515 on anvit:
// POST /v1/edge/certificates/distributions/pull, end_of_stream, gate failure). The test server
// answers the first request, then cuts the next `cuts` ones without an answer: the first on the
// kept-alive connection, later ones on fresh connections. Every idempotent request survives up to
// controlAttempts-1 cuts with no control failure; register is never resent; a distribution request
// is signed again for each send, so no nonce reaches the server twice.
func TestHTTPClientRequestsSurviveRolloutCut(t *testing.T) {
	envelope := tlscert.DistributionEnvelope{Action: "stage", CertificateID: "certificate_1", Fingerprint: [32]byte{1}, Binding: tlscert.Binding{AccountID: "account_1", TunnelID: "tunnel_1", DomainID: "domain_1", Hostname: "app.example.test", DomainGeneration: 1, CertificateGeneration: 1, EdgeNodeID: "edge_1", EdgeProcessEpoch: "edge_epoch_1", AssignmentGeneration: 1}}
	distribution := func(c *HTTPClient) *CertificateDistributionClient {
		client, err := NewCertificateDistributionClient(c)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	for _, tc := range []struct {
		name, path, body string
		call             func(context.Context, *HTTPClient) error
		idempotent       bool
		replayable       int // 1: a GET, which net/http itself resends once on a reused connection
	}{
		{"heartbeat", "/v1/nodes/heartbeat", `{"ingress_limits":null}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.Heartbeat(ctx, NodeObservation{NodeID: "edge"})
			return err
		}, true, 0},
		{"revocations", "/v1/trust/revocations", `{"revocations":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.Revocations(ctx)
			return err
		}, true, 1},
		{"usage report", "/v1/edge/usage-reports", `{"disposition":"accounted"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.ReportUsage(ctx, UsageReport{OperationID: "op", Key: usageKey(), Interval: [2]time.Time{time.Unix(1, 0), time.Unix(2, 0)}})
			return err
		}, true, 0},
		{"desired routes", "/v1/edge/routes/desired-state", `{"complete":true,"version":"v1","routes":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.DesiredRoutes(ctx, "edge")
			return err
		}, true, 0},
		{"route observations", "/v1/edge/routes/observations", ``, func(ctx context.Context, c *HTTPClient) error {
			return c.ObserveRoutes(ctx, "edge", nil)
		}, true, 0},
		{"runtime carrier observations", runtimewire.EdgeObservePath, ``, func(ctx context.Context, c *HTTPClient) error {
			return c.ObserveRuntimeCarriers(ctx, "edge_1", "edge_epoch_1", []runtimewire.Observation{{RouteID: "route_1", SessionID: "session_1", AttachmentGeneration: 1}})
		}, true, 0},
		{"distribution pull", CertificateDistributionPullPath, `{"version":1,"complete":true,"messages":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := distribution(c).Pull(ctx, "edge_1", "edge_epoch_1")
			return err
		}, true, 0},
		{"distribution ack", CertificateDistributionAckPath, ``, func(ctx context.Context, c *HTTPClient) error {
			return distribution(c).Ack(ctx, envelope, "ready", "")
		}, true, 0},
		{"certificate request", CertificateDistributionRequestPath, ``, func(ctx context.Context, c *HTTPClient) error {
			return distribution(c).RequestCertificate(ctx, "edge_1", "edge_epoch_1", "app.example.test")
		}, true, 0},
		{"register", "/v1/nodes/register", ``, func(ctx context.Context, c *HTTPClient) error {
			trust, err := NewProcessCarrierServerTrust("edge", "epoch", "edge.example.test", time.Now().UTC(), time.Hour)
			if err != nil {
				return err
			}
			return c.RegisterNode(ctx, NodeRegistration{NodeID: "edge", ProcessEpoch: "epoch", Capacity: 1, CarrierEndpoint: ConnectorEndpoint{Host: "edge.example.test", TCPPort: 27443, QUICPort: 27444}, CarrierServerSPKISHA256: trust.SPKISHA256, CarrierServerCertificateChainPEM: trust.CertificateChainPEM})
		}, false, 0},
	} {
		for cuts := 1; cuts <= controlAttempts+tc.replayable; cuts++ {
			t.Run(fmt.Sprintf("%s/%d cuts", tc.name, cuts), func(t *testing.T) {
				var requests atomic.Int32
				var noncesMu sync.Mutex
				nonces := map[string]bool{}
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if nonce := r.Header.Get(distributionProofNonceHeader); nonce != "" {
						noncesMu.Lock()
						if nonces[nonce] {
							t.Errorf("%s: nonce sent twice", r.URL.Path)
						}
						nonces[nonce] = true
						noncesMu.Unlock()
					}
					if r.URL.Path != tc.path {
						t.Errorf("path %s, want %s", r.URL.Path, tc.path)
					}
					if n := requests.Add(1); n >= 2 && n <= int32(1+cuts) {
						conn, _, err := http.NewResponseController(w).Hijack()
						if err != nil {
							t.Errorf("hijack: %v", err)
							return
						}
						_ = conn.Close() // the old proxy closes the connection without answering
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
				var failure error
				client, err := NewHTTPClient(HTTPConfig{
					BaseURL: server.URL, Credential: testControlCredential, Timeout: 5 * time.Second, Client: server.Client(),
					ControlFailure: func(_ context.Context, _ string, err error) { failures.Add(1); failure = err },
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
				err = tc.call(context.Background(), client) // written onto the kept-alive connection the rollout closes
				if tc.idempotent && cuts < controlAttempts+tc.replayable {
					if err != nil || requests.Load() != int32(2+cuts) || failures.Load() != 0 {
						t.Fatalf("err %v, server saw %d requests (want %d), control failures %d", err, requests.Load(), 2+cuts, failures.Load())
					}
					return
				}
				want := int32(1 + controlAttempts + tc.replayable) // bounded: every send was cut
				if !tc.idempotent {
					want = 2 // register is sent once
				}
				var located interface{ DiagnosticPath() string }
				if err == nil || requests.Load() != want || failures.Load() != 1 || !errors.As(failure, &located) || located.DiagnosticPath() != tc.path {
					t.Fatalf("err %v, server saw %d requests (want %d), control failures %d (%v)", err, requests.Load(), want, failures.Load(), failure)
				}
			})
		}
	}
}
