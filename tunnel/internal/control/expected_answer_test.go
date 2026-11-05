package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The server's answers to a request (a refusal, "stale", a denied user) and a caller that gave up
// are request outcomes, not control-plane unavailability: they record no control failure, so the
// strict edge gate fails only on real unavailability. A 503 still records one.
func TestControlAnswersAreOutcomesNotFailures(t *testing.T) {
	newClient := func(t *testing.T, base string, transport http.RoundTripper) (*HTTPClient, *[]string, *int) {
		var outcomes []string
		failures := 0
		client, err := NewHTTPClient(HTTPConfig{BaseURL: base, Credential: testControlCredential, Timeout: 3 * time.Second, Client: &http.Client{Transport: transport},
			ControlTrace: func(context.Context, string) (string, string, func(string, string)) {
				return "", "", func(outcome, code string) { outcomes = append(outcomes, outcome+":"+code) }
			},
			ControlFailure: func(context.Context, string, error) { failures++ }})
		if err != nil {
			t.Fatal(err)
		}
		_, key, _ := ed25519.GenerateKey(nil)
		signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
		if err != nil || client.SetDistributionRequestSigner(signer) != nil {
			t.Fatal(err)
		}
		return client, &outcomes, &failures
	}
	answering := func(status int, body string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) { return response(status, body), nil })
	}
	calls := map[string]func(context.Context, *HTTPClient) error{
		"heartbeat": func(ctx context.Context, c *HTTPClient) error {
			_, err := c.Heartbeat(ctx, NodeObservation{NodeID: "edge_1"})
			return err
		},
		"certificate": func(ctx context.Context, c *HTTPClient) error {
			distribution, err := NewCertificateDistributionClient(c)
			if err != nil {
				return err
			}
			return distribution.RequestCertificate(ctx, "edge_1", "edge_epoch_1", "stray.example.test")
		},
		"inspector": func(ctx context.Context, c *HTTPClient) error {
			_, err := (&InspectorAccessClient{HTTP: c, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}).Authorize(ctx, "token", "preview", "preview_1", "route_1", "inspect")
			return err
		},
		"browser": func(ctx context.Context, c *HTTPClient) error {
			_, err := (&BrowserAccessClient{HTTP: c, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}).Begin(ctx, "app.example.test", "/")
			return err
		},
		"grant": func(ctx context.Context, c *HTTPClient) error {
			authorizer, err := NewPrivateAccessGrantClient(c, "edge_1", "edge_epoch_1")
			if err != nil {
				return err
			}
			_, err = authorizer.AuthorizePrivateAccessGrant(ctx, "grant", testPrivateAccessGrantRequest(time.Now().UTC()))
			return err
		},
	}
	denied := `{"schema":"paperboat.preview-tunnel/v1","kind":"private_access_authorization","allowed":false,"reason":"denied"}`
	for _, tc := range []struct {
		name, call string
		status     int
		body       string
	}{
		{"stale heartbeat", "heartbeat", http.StatusConflict, `{"code":"node_observation_stale"}`},
		{"no certificate for a stray name", "certificate", http.StatusConflict, `{"code":"certificate_distribution_unavailable"}`},
		{"inspector not authorized", "inspector", http.StatusForbidden, `{"code":"inspector_not_authorized"}`},
		{"browser not signed in", "browser", http.StatusUnauthorized, `{}`},
		{"browser denied", "browser", http.StatusForbidden, `{}`},
		{"private access machine unauthenticated", "grant", http.StatusUnauthorized, denied},
		{"private access forbidden", "grant", http.StatusForbidden, denied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, outcomes, failures := newClient(t, "https://control.test", answering(tc.status, tc.body))
			if err := calls[tc.call](context.Background(), client); err == nil {
				t.Fatal("answer was not returned as an error")
			}
			if *failures != 0 || strings.Join(*outcomes, ",") != "rejected:invalid" {
				t.Fatalf("outcomes %v, control failures %d", *outcomes, *failures)
			}
		})
	}
	t.Run("caller gave up", func(t *testing.T) {
		client, outcomes, failures := newClient(t, "https://control.test", roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}))
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		if err := calls["browser"](ctx, client); err == nil {
			t.Fatal("canceled request succeeded")
		}
		if *failures != 0 || strings.Join(*outcomes, ",") != "canceled:shutdown" {
			t.Fatalf("outcomes %v, control failures %d", *outcomes, *failures)
		}
	})
	t.Run("browser terminal refused", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			if _, _, err := connection.Read(r.Context()); err == nil {
				_ = connection.Close(websocket.StatusPolicyViolation, "no_access")
			}
		}))
		defer server.Close()
		client, outcomes, failures := newClient(t, server.URL, server.Client().Transport)
		if _, err := (&BrowserTerminalClient{HTTP: client, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}).Admit(context.Background(), "ticket", "https://dashboard.example.test", "machine.runtime.example.test"); err == nil {
			t.Fatal("refused admission succeeded")
		}
		if *failures != 0 || strings.Join(*outcomes, ",") != "rejected:invalid" {
			t.Fatalf("outcomes %v, control failures %d", *outcomes, *failures)
		}
	})
	t.Run("unavailable is still a failure", func(t *testing.T) {
		client, outcomes, failures := newClient(t, "https://control.test", answering(http.StatusServiceUnavailable, `{}`))
		if err := calls["heartbeat"](context.Background(), client); err == nil {
			t.Fatal("503 succeeded")
		}
		if *failures != 1 || strings.Join(*outcomes, ",") != "failed:control_request_failed" {
			t.Fatalf("outcomes %v, control failures %d", *outcomes, *failures)
		}
	})
}

// The server answers an on-demand certificate request with 409 when it has no leaf for the host
// yet (being issued, not ready, DNS pending) or ever: a typed answer and no control failure. A 503
// there is unavailability and still records one.
func TestOnDemandCertificateConflictIsNotIssuedYet(t *testing.T) {
	for _, tc := range []struct {
		status   int
		typed    bool
		failures int
	}{{http.StatusConflict, true, 0}, {http.StatusServiceUnavailable, false, 1}} {
		failures := 0
		client, err := NewHTTPClient(HTTPConfig{BaseURL: "https://control.test", Credential: testControlCredential, Timeout: time.Second,
			Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != CertificateDistributionRequestPath {
					t.Fatalf("path = %s", r.URL.Path)
				}
				return response(tc.status, `{"code":"certificate_distribution_unavailable"}`), nil
			})},
			ControlFailure: func(context.Context, string, error) { failures++ }})
		if err != nil {
			t.Fatal(err)
		}
		_, key, _ := ed25519.GenerateKey(nil)
		signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
		if err != nil || client.SetDistributionRequestSigner(signer) != nil {
			t.Fatal(err)
		}
		distribution, err := NewCertificateDistributionClient(client)
		if err != nil {
			t.Fatal(err)
		}
		requester, err := NewOnDemandCertificateRequester(distribution, "edge_1", "edge_epoch_1")
		if err != nil {
			t.Fatal(err)
		}
		err = requester.RequestCertificate(context.Background(), "custom.example.test")
		if err == nil || errors.Is(err, ErrCertificateNotIssuedYet) != tc.typed || failures != tc.failures {
			t.Fatalf("status %d: err %v, control failures %d (want typed=%v, %d)", tc.status, err, failures, tc.typed, tc.failures)
		}
	}
}
