package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type connectProofError struct{ cause error }

func (*connectProofError) Error() string   { panic("private text must not be evaluated") }
func (e *connectProofError) Unwrap() error { return e.cause }

func TestInitialConnectFailureProof(t *testing.T) {
	cycle := &connectProofError{}
	cycle.cause = cycle
	var typedNil *connectProofError
	network := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	local := errors.Join(localapi.ErrTransportUnavailable, network)
	stream := errors.Join(tunnel.ErrPeerStreamOpen, &connectProofError{cause: network})
	apiReady := &api.APIError{Status: 409, Code: "machine_not_ready"}
	for _, test := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"ready API", fmt.Errorf("wrapper: %w", apiReady), true},
		{"tunnel API", &api.APIError{Status: 503, Code: "tunnel_unavailable"}, true},
		{"contradictory API denial", &api.APIError{Status: 401, Code: "machine_not_ready"}, false},
		{"local producer join", &connectProofError{cause: local}, true},
		{"stream producer join", stream, true},
		{"lost transport", tunnel.ErrTransportLost, true},
		{"duplicate transient", errors.Join(network, network), true},
		{"API and IO", errors.Join(apiReady, syscall.EIO), false},
		{"stream and IO", errors.Join(stream, syscall.EIO), false},
		{"local cancellation", errors.Join(local, context.Canceled), false},
		{"deadline wrapper", &url.Error{Op: "dial", Err: context.DeadlineExceeded}, false},
		{"unknown text", &connectProofError{}, false},
		{"cycle", cycle, false},
		{"typed nil", typedNil, false},
		{"nil", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableInitialConnectFailure(test.err); got != test.retry {
				t.Fatalf("retry = %v, want %v", got, test.retry)
			}
		})
	}
	deep := error(syscall.ECONNREFUSED)
	for range 33 {
		deep = &connectProofError{cause: deep}
	}
	if retryableInitialConnectFailure(deep) {
		t.Fatal("truncated chain authorized retry")
	}
}

func TestInitialConnectFailureActualAPIResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"machine_not_ready","message":"private provider message"}}`)
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{}, server.Client())
	_, err := client.Me(context.Background())
	if !retryableInitialConnectFailure(err) {
		t.Fatal("owned HTTP status cause rejected legitimate readiness response")
	}
	// The API owns path construction; a purpose-built response transport makes
	// the truncated body independent of URL/query conventions.
	client = api.New(server.URL, config.Credential{}, &http.Client{Transport: connectProofRoundTripper{body: io.NopCloser(strings.NewReader(`{"error":{"code":"machine_not_ready"`))}})
	_, err = client.Me(context.Background())
	if retryableInitialConnectFailure(err) {
		t.Fatal("truncated readiness response authorized retry")
	}
	client = api.New(server.URL, config.Credential{}, &http.Client{Transport: connectProofRoundTripper{body: io.NopCloser(io.MultiReader(strings.NewReader(`{"error":{"code":"machine_not_ready"`), connectProofReadFailure{}))}})
	_, err = client.Me(context.Background())
	if retryableInitialConnectFailure(err) || !errors.Is(err, syscall.EIO) {
		t.Fatal("failed readiness body read authorized retry or lost cause")
	}
	client = api.New(server.URL, config.Credential{}, &http.Client{Transport: connectProofRoundTripper{body: io.NopCloser(strings.NewReader(`{"error":{"code":"machine_not_ready","details":42}}`))}})
	_, err = client.Me(context.Background())
	var typeError *json.UnmarshalTypeError
	if retryableInitialConnectFailure(err) || !errors.As(err, &typeError) {
		t.Fatal("malformed readiness details authorized retry or lost typed cause")
	}
}

type connectProofRoundTripper struct{ body io.ReadCloser }

func (r connectProofRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: r.body, Request: request}, nil
}

type connectProofReadFailure struct{}

func (connectProofReadFailure) Read([]byte) (int, error) { return 0, syscall.EIO }
