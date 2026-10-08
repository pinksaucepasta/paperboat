package tunnelmanager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type browserCauseAuth struct {
	browserTestAuth
	cause error
	proof bool
}

func (auth *browserCauseAuth) Token(ctx context.Context) (string, error) {
	if !auth.proof && auth.cause != nil {
		return "", auth.cause
	}
	return auth.browserTestAuth.Token(ctx)
}

func (auth *browserCauseAuth) Proof(ctx context.Context, operation, method, path string, body []byte) ([]byte, error) {
	if auth.proof && auth.cause != nil {
		return nil, auth.cause
	}
	return auth.browserTestAuth.Proof(ctx, operation, method, path, body)
}

type browserCauseTransport func(*http.Request) (*http.Response, error)

func (transport browserCauseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type browserCauseBody struct {
	io.Reader
	readErr, closeErr error
	closes            int
}

func (body *browserCauseBody) Read(data []byte) (int, error) {
	if body.readErr != nil {
		return 0, body.readErr
	}
	return body.Reader.Read(data)
}
func (body *browserCauseBody) Close() error { body.closes++; return body.closeErr }

func TestBrowserAuthorityProviderAndBodyFailuresKeepOriginalCauses(t *testing.T) {
	decision, open := browserDecisionFixture(time.Now().UTC())
	for _, proof := range []bool{false, true} {
		spy := &ingressErrorSpy{}
		auth := &browserCauseAuth{cause: errors.Join(syscall.EIO, spy), proof: proof}
		lookup, err := NewBrowserIngressAuthority("https://control.example.test", auth, browserCauseTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("failed provider unexpectedly contacted the control plane")
			return nil, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = lookup(t.Context(), open, decision)
		if !errors.Is(err, syscall.EIO) || !errors.Is(err, spy) || errors.Is(err, connectorprotocol.ErrIngressDenied) || err.Error() != "Tunnel ingress operation failed." {
			t.Fatal("provider failure lost its safe operational cause")
		}
	}
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		body := &browserCauseBody{Reader: strings.NewReader("PRIVATE-BODY"), closeErr: syscall.ENOSPC}
		if status == http.StatusOK {
			body.readErr = syscall.EIO
		}
		lookup, err := NewBrowserIngressAuthority("https://control.example.test", &browserTestAuth{}, browserCauseTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		current, err := lookup(t.Context(), open, decision)
		if !errors.Is(err, syscall.ENOSPC) || body.closes != 1 || current.DecisionID != "" || err.Error() != "Tunnel ingress operation failed." {
			t.Fatal("response close failure lost its cause or returned usable authority")
		}
		if status == http.StatusOK && !errors.Is(err, syscall.EIO) {
			t.Fatal("response read failure lost its original cause")
		}
		if status == http.StatusForbidden && (!errors.Is(err, connectorprotocol.ErrIngressDenied) || ingressDenialOnly(err)) {
			t.Fatal("denial hid independent cleanup failure")
		}
	}
}

func TestBrowserAuthorityHTTPFailureAndFreshRecoveryKeepReference(t *testing.T) {
	decision, open := browserDecisionFixture(time.Now().UTC())
	status := http.StatusServiceUnavailable
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(supportref.Header) != "support_01234567-89ab-4def-8123-456789abcdef" {
			t.Error("request lost its support reference")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			if err := json.NewEncoder(w).Encode(map[string]any{"data": decision}); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()
	lookup, err := NewBrowserIngressAuthority(server.URL, &browserTestAuth{}, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	ctx := supportref.WithContext(t.Context(), "support_01234567-89ab-4def-8123-456789abcdef")
	_, err = lookup(ctx, open, decision)
	if err == nil || errors.Is(err, connectorprotocol.ErrIngressDenied) || errorreport.ProjectFault(ctx, "paperboat-daemon", "browser_authorization", "peer_authority", "peer_authority_failed", err).HTTPStatus != http.StatusServiceUnavailable {
		t.Fatal("upstream outage became a viewer denial or lost HTTP status")
	}
	status = http.StatusOK
	current, err := lookup(ctx, open, decision)
	if err != nil || current.DecisionID != decision.DecisionID {
		t.Fatal("fresh authority did not recover")
	}
	status = http.StatusForbidden
	if _, err := lookup(ctx, open, decision); err != connectorprotocol.ErrIngressDenied {
		t.Fatal("genuine authority denial changed")
	}
}

func TestBrowserAuthorityMalformedResponseRetainsParserCause(t *testing.T) {
	decision, open := browserDecisionFixture(time.Now().UTC())
	lookup, err := NewBrowserIngressAuthority("https://control.example.test", &browserTestAuth{}, browserCauseTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":! PRIVATE-RESPONSE}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = lookup(t.Context(), open, decision)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || errors.Is(err, connectorprotocol.ErrIngressDenied) || err.Error() != "Tunnel ingress operation failed." {
		t.Fatal("malformed authority response lost its safe parser cause")
	}
}
