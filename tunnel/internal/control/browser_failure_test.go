package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrowserControlAttemptFailureAndRecoveryKeepReference(t *testing.T) {
	const reference = "support_12345678-1234-4234-8234-123456789abc"
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Support-Reference") != reference || r.Header.Get("sentry-trace") != "test-trace" {
					t.Error("control correlation missing")
				}
				if requests.Add(1) == 1 {
					http.Error(w, "PRIVATE-CONTROL-BODY", http.StatusServiceUnavailable)
					return
				}
				if !terminal {
					_, _ = w.Write([]byte(`{"transaction_id":"transaction_1","state":"state_1","expires_at":"2026-10-08T00:00:00Z"}`))
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if _, _, err = conn.Read(r.Context()); err != nil {
					return
				}
				if err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"PRIVATE-CREDENTIAL","terminal_session_id":"term_1","attachment_id":"attach_1"}`)); err != nil {
					return
				}
				_, _, _ = conn.Read(r.Context())
			}))
			defer server.Close()
			var failures []error
			var outcomes []string
			client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: testControlCredential, Client: server.Client(), Timeout: time.Second,
				ControlTrace: func(ctx context.Context, _ string) (string, string, func(string, string)) {
					return "test-trace", reference, func(outcome, _ string) { outcomes = append(outcomes, outcome) }
				},
				ControlFailure: func(_ context.Context, ref string, err error) {
					if ref != reference {
						t.Error("fault reference lost")
					}
					failures = append(failures, err)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			call := func() error {
				if terminal {
					a, err := (&BrowserTerminalClient{HTTP: client, NodeID: "edge_1", ProcessEpoch: "epoch_01"}).Admit(ctx, "PRIVATE-TICKET", "https://PRIVATE-ORIGIN.invalid", "PRIVATE-HOST")
					if err == nil {
						a.Close()
						select {
						case <-a.Closed:
						case <-ctx.Done():
							t.Fatal("control reader not stopped")
						}
					}
					return err
				}
				_, err := (&BrowserAccessClient{HTTP: client, NodeID: "edge_1", ProcessEpoch: "epoch_01"}).Begin(ctx, "PRIVATE-HOST", "/PRIVATE-PATH")
				return err
			}
			err = call()
			var failure *RequestFailure
			if !errors.Is(err, ErrControlUnavailable) || !errors.As(err, &failure) || failure.Status != 503 || failure.SupportReference != reference {
				t.Fatalf("failure classification lost: %T", err)
			}
			if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("public control error leaked request/response data")
			}
			if err := call(); err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			if len(failures) != 1 || len(outcomes) != 2 || outcomes[0] != "failed" || outcomes[1] != "success" {
				t.Fatalf("attempt ownership failures=%d outcomes=%v", len(failures), outcomes)
			}
		})
	}
}

func TestPrivateGrantTransportCauseIsPrivateAndRecovers(t *testing.T) {
	cause := &url.Error{Op: "PRIVATE-OP", URL: "https://PRIVATE.invalid/path", Err: syscall.ECONNREFUSED}
	var calls, failures int
	client := controlClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, cause
		}
		return response(http.StatusForbidden, `{"schema":"paperboat.preview-tunnel/v1","kind":"private_access_authorization","allowed":false,"reason":"denied"}`), nil
	})
	client.controlFailure = func(_ context.Context, _ string, err error) {
		failures++
		var failure *RequestFailure
		if !errors.As(err, &failure) {
			t.Fatal("attempt missing typed status/cause")
		}
	}
	authorizer, err := NewPrivateAccessGrantClient(client, "edge_1", "edge_epoch_1")
	if err != nil {
		t.Fatal(err)
	}
	input := testPrivateAccessGrantRequest(time.Now().UTC())
	_, err = authorizer.AuthorizePrivateAccessGrant(context.Background(), "PRIVATE-GRANT", input)
	var transportCause *url.Error
	if !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, ErrPrivateAccessGrantUnavailable) || !errors.As(err, &transportCause) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("private grant transport cause/privacy lost")
	}
	_, err = authorizer.AuthorizePrivateAccessGrant(context.Background(), "PRIVATE-GRANT", input)
	var failure *RequestFailure
	if !errors.Is(err, ErrPrivateAccessGrantForbidden) || !errors.As(err, &failure) || failure.Status != 403 || failures != 2 {
		t.Fatal("transport recovery/rejection owner lost")
	}
}

func TestControlDocumentParserCauseDoesNotExposeInput(t *testing.T) {
	_, err := (PreviewCarrierAdmission{Endpoint: "https://PRIVATE.invalid/%zz"}).Normalize()
	var cause *url.Error
	if !errors.Is(err, ErrPreviewCarrierInvalid) || !errors.As(err, &cause) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("parser cause/privacy lost")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_1", ProcessEpoch: "epoch_01", Nonce: func() ([]byte, error) { return nil, syscall.EIO }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = signer.SignDistributionRequest(context.Background(), http.MethodPost, CertificateDistributionPullPath, nil)
	if !errors.Is(err, ErrDistributionProofInvalid) || !errors.Is(err, syscall.EIO) {
		t.Fatal("entropy failure cause lost")
	}
}
