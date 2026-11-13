package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/edge/internal/tlscert"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A server rollout reloads Caddy, which closes its kept-alive client connections; a request the
// edge had already written onto one gets no answer (production 2026-11-05 10:22:31.515 on anvit:
// POST /v1/edge/certificates/distributions/pull, end_of_stream, gate failure). The test server
// answers the first request, then cuts the next `cuts` ones without an answer: the first on the
// kept-alive connection, later ones on fresh connections. Every idempotent request survives up to
// controlAttempts-1 cuts with no control failure; register is never resent; a distribution request
// is signed again for each send, so no nonce reaches the server twice. The requests a user waits on
// (browser access, private access, the browser terminal handshake) survive the same cuts.
func TestHTTPClientRequestsSurviveRolloutCut(t *testing.T) {
	now := time.Now().UTC()
	privateInput := testPrivateAccessGrantRequest(now)
	privateDecision, _ := json.Marshal(PrivateAccessGrantDecision{
		Schema: connectorprotocol.PrivateAccessSchema, Kind: "private_access_authorization", DecisionID: "decision_1", Allowed: true, Reason: "allowed",
		ExpiresAt: now.Add(30 * time.Second), RequestID: privateInput.RequestID, CorrelationID: privateInput.CorrelationID,
		ResourceKind: privateInput.ResourceKind, ResourceID: privateInput.ResourceID, RouteID: privateInput.RouteID,
		ConnectorID: privateInput.ConnectorID, CarrierSessionID: privateInput.CarrierSessionID, RouteGeneration: privateInput.RouteGeneration,
		SessionGeneration: privateInput.SessionGeneration, ProcessGeneration: privateInput.ProcessGeneration, ConfigGeneration: privateInput.ConfigGeneration,
		AssignmentGeneration: privateInput.AssignmentGeneration, Protocol: privateInput.Protocol,
	})
	browser := func(c *HTTPClient) *BrowserAccessClient {
		return &BrowserAccessClient{HTTP: c, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}
	}
	terminal := func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(r.Context()); err != nil {
			return
		}
		if connection.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"host-credential","terminal_session_id":"term_1","attachment_id":"attach_1"}`)) != nil {
			return
		}
		_, _, _ = connection.Read(r.Context()) // held open until the edge closes the admission
	}
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
		serve            http.HandlerFunc
	}{
		{"heartbeat", "/v1/nodes/heartbeat", `{"ingress_limits":null}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.Heartbeat(ctx, NodeObservation{NodeID: "edge"})
			return err
		}, true, 0, nil},
		{"revocations", "/v1/trust/revocations", `{"revocations":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.Revocations(ctx)
			return err
		}, true, 1, nil},
		{"jwks", "/v1/trust/jwks", `{"keys":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.JWKS(ctx)
			return err
		}, true, 1, nil},
		{"usage report", "/v1/edge/usage-reports", `{"disposition":"accounted"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.ReportUsage(ctx, UsageReport{OperationID: "op", Key: usageKey(), Interval: [2]time.Time{time.Unix(1, 0), time.Unix(2, 0)}})
			return err
		}, true, 0, nil},
		{"desired routes", "/v1/edge/routes/desired-state", `{"complete":true,"version":"v1","routes":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := c.DesiredRoutes(ctx, "edge")
			return err
		}, true, 0, nil},
		{"route observations", "/v1/edge/routes/observations", ``, func(ctx context.Context, c *HTTPClient) error {
			return c.ObserveRoutes(ctx, "edge", nil)
		}, true, 0, nil},
		{"runtime carrier observations", runtimewire.EdgeObservePath, ``, func(ctx context.Context, c *HTTPClient) error {
			return c.ObserveRuntimeCarriers(ctx, "edge_1", "edge_epoch_1", []runtimewire.Observation{{RouteID: "route_1", SessionID: "session_1", AttachmentGeneration: 1}})
		}, true, 0, nil},
		{"distribution pull", CertificateDistributionPullPath, `{"version":1,"complete":true,"messages":[]}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := distribution(c).Pull(ctx, "edge_1", "edge_epoch_1")
			return err
		}, true, 0, nil},
		{"distribution ack", CertificateDistributionAckPath, ``, func(ctx context.Context, c *HTTPClient) error {
			return distribution(c).Ack(ctx, envelope, "ready", "")
		}, true, 0, nil},
		{"certificate request", CertificateDistributionRequestPath, ``, func(ctx context.Context, c *HTTPClient) error {
			return distribution(c).RequestCertificate(ctx, "edge_1", "edge_epoch_1", "app.example.test")
		}, true, 0, nil},
		{"browser access begin", "/v1/edge/browser-access/begin", `{"transaction_id":"transaction_1","state":"state_1","expires_at":"2030-01-01T00:00:00Z"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := browser(c).Begin(ctx, "app.example.test", "/")
			return err
		}, true, 0, nil},
		{"browser access redeem", "/v1/edge/browser-access/redeem", `{"token":"token_1","return_path":"/","expires_at":"2030-01-01T00:00:00Z"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := browser(c).Redeem(ctx, "transaction_1", "state_1", "handoff_1", "app.example.test")
			return err
		}, true, 0, nil},
		{"browser access authorize", "/v1/edge/browser-access/authorize", `{"decision_id":"decision_1"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := browser(c).Authorize(ctx, BrowserAuthorizeRequest{Host: "app.example.test", Token: "credential", ResourceKind: "preview"})
			return err
		}, true, 0, nil},
		{"browser access activate", "/v1/edge/browser-access/activate", `{"decision_id":"decision_1"}`, func(ctx context.Context, c *HTTPClient) error {
			_, err := browser(c).Activate(ctx, BrowserAuthorizeRequest{Host: "app.example.test", Token: "credential", ResourceKind: "lazy_policy"})
			return err
		}, true, 0, nil},
		{"private access authorize", PrivateAccessGrantAuthorizePath, string(privateDecision), func(ctx context.Context, c *HTTPClient) error {
			authorizer, err := NewPrivateAccessGrantClient(c, "edge_1", "edge_epoch_1")
			if err != nil {
				return err
			}
			_, err = authorizer.AuthorizePrivateAccessGrant(ctx, "signed-grant", privateInput)
			return err
		}, true, 0, nil},
		{"browser terminal handshake", "/v1/edge/browser-terminal/control", ``, func(ctx context.Context, c *HTTPClient) error {
			admission, err := (&BrowserTerminalClient{HTTP: c, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}).Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
			if err == nil {
				admission.Close()
			}
			return err
		}, true, 0, terminal},
		{"register", "/v1/nodes/register", ``, func(ctx context.Context, c *HTTPClient) error {
			trust, err := NewProcessCarrierServerTrust("edge", "epoch", "edge.example.test", time.Now().UTC(), time.Hour)
			if err != nil {
				return err
			}
			return c.RegisterNode(ctx, NodeRegistration{NodeID: "edge", ProcessEpoch: "epoch", Capacity: 1, CarrierEndpoint: ConnectorEndpoint{Host: "edge.example.test", TCPPort: 27443, QUICPort: 27444}, CarrierServerSPKISHA256: trust.SPKISHA256, CarrierServerCertificateChainPEM: trust.CertificateChainPEM})
		}, false, 0, nil},
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
					if tc.serve != nil {
						tc.serve(w, r)
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
