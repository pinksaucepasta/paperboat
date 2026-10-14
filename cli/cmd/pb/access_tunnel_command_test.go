package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeAccessTunnelRuntime struct {
	mu       sync.Mutex
	started  int
	released int
	session  accessTunnelSession
	startErr error
}

type accessTunnelRoundTripper func(*http.Request) (*http.Response, error)

func (f accessTunnelRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type accessTunnelFaultBody struct {
	data     []byte
	readErr  error
	closeErr error
	read     bool
	offset   int
}

func (b *accessTunnelFaultBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		if b.readErr != nil {
			return 0, b.readErr
		}
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	if n > 0 {
		return n, nil
	}
	return 0, io.EOF
}

func (b *accessTunnelFaultBody) Close() error { return b.closeErr }

func (f *fakeAccessTunnelRuntime) Start(_ context.Context, selector, listen string) (accessTunnelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	if f.startErr != nil {
		return accessTunnelSession{}, f.startErr
	}
	out := f.session
	if out.Schema == "" {
		out = accessTunnelSession{Schema: accessTunnelSchema, Kind: "private_tcp_access", ID: "access_1", TunnelID: "tun_1", RouteID: "route_tcp_1", ListenAddress: "127.0.0.1:24000"}
	}
	return out, nil
}
func (f *fakeAccessTunnelRuntime) Release(context.Context, accessTunnelSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	return nil
}

func withAccessTunnelRuntime(t *testing.T, runtime accessTunnelRuntime) {
	t.Helper()
	old := accessTunnelRuntimeForCommand
	accessTunnelRuntimeForCommand = func() (accessTunnelRuntime, error) { return runtime, nil }
	t.Cleanup(func() { accessTunnelRuntimeForCommand = old })
}

func TestAccessTunnelCommandCancelsAndCleansUpWithoutSecrets(t *testing.T) {
	runtime := &fakeAccessTunnelRuntime{}
	withAccessTunnelRuntime(t, runtime)
	ctx, cancel := context.WithCancel(context.Background())
	output := &cancelAfterWrite{cancel: cancel}
	command := accessTunnelCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(output)
	command.SetArgs([]string{"tun_1", "--json"})
	err := command.Execute()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.started != 1 || runtime.released != 1 {
		t.Fatalf("started=%d released=%d", runtime.started, runtime.released)
	}
	lower := strings.ToLower(output.String())
	for _, forbidden := range []string{"grant", "token", "secret", "credential"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("unsafe output=%s", output.String())
		}
	}
}

func TestAccessTunnelCommandRejectsNonLiteralLoopbackBeforeRuntime(t *testing.T) {
	runtime := &fakeAccessTunnelRuntime{}
	withAccessTunnelRuntime(t, runtime)
	for _, listen := range []string{"localhost:0", "0.0.0.0:9000", "192.168.1.2:9000", "127.0.0.1:http"} {
		command := accessTunnelCobraCommandV1()
		command.SetArgs([]string{"tun_1", "--listen", listen})
		if err := command.Execute(); !errors.Is(err, ErrAccessTunnelInvalid) {
			t.Fatalf("listen=%q error=%v", listen, err)
		}
	}
	if runtime.started != 0 {
		t.Fatalf("started=%d", runtime.started)
	}
}

func TestLocalAccessTunnelClientMapsAuthenticationAuthorizationAndAvailability(t *testing.T) {
	for _, test := range []struct {
		status int
		want   error
	}{{http.StatusUnauthorized, ErrAccessTunnelAuthentication}, {http.StatusForbidden, ErrAccessTunnelForbidden}, {http.StatusServiceUnavailable, ErrAccessTunnelUnavailable}, {http.StatusNotFound, ErrAccessTunnelRuntimeRPCUnavailable}} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer local-token" {
					t.Fatal("missing local token")
				}
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			client := &localAccessTunnelClient{base: base, token: "local-token", client: server.Client()}
			_, err := client.Start(context.Background(), "tun_1", "127.0.0.1:0")
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAccessTunnelUnexpectedServerStatusRetainsSafeStatusCause(t *testing.T) {
	err := accessTunnelHTTPError(http.StatusInternalServerError)
	if !errors.Is(err, ErrAccessTunnelUnavailable) || errors.Is(err, ErrAccessTunnelInvalid) {
		t.Fatalf("server failure received invalid-request classification: %T", err)
	}
	var status interface{ DiagnosticStatus() int }
	if !errors.As(err, &status) || status.DiagnosticStatus() != http.StatusInternalServerError {
		t.Fatalf("server status cause was not preserved: %T", err)
	}
	if strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "hostd") {
		t.Fatalf("status failure exposed implementation detail: %q", err.Error())
	}
}

func TestLocalAccessTunnelClientStrictlyBoundsAndDecodesResponse(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"duplicate", `{"schema":"paperboat.private-tcp-access/v1","schema":"paperboat.private-tcp-access/v1","kind":"private_tcp_access","id":"access_1","tunnel_id":"tun_1","route_id":"route_1","listen_address":"127.0.0.1:24000"}`},
		{"unknown", `{"schema":"paperboat.private-tcp-access/v1","kind":"private_tcp_access","id":"access_1","tunnel_id":"tun_1","route_id":"route_1","listen_address":"127.0.0.1:24000","grant":"must-not-appear"}`},
		{"oversized", strings.Repeat("x", accessTunnelResponseLimit+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			client := &localAccessTunnelClient{base: base, token: "local-token", client: server.Client()}
			if _, err := client.Start(context.Background(), "tun_1", "127.0.0.1:0"); !errors.Is(err, ErrAccessTunnelInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestLocalAccessTunnelClientAcceptsSafeBoundSessionAndRelease(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["selector"] != "route_tcp_1" || body["listen_address"] != "127.0.0.1:0" {
			t.Fatalf("body=%#v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"schema":"paperboat.private-tcp-access/v1","kind":"private_tcp_access","id":"access_1","tunnel_id":"tun_1","route_id":"route_tcp_1","listen_address":"127.0.0.1:24000"}`))
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := &localAccessTunnelClient{base: base, token: "local-token", client: server.Client()}
	session, err := client.Start(context.Background(), "route_tcp_1", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Release(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestLocalAccessTunnelClientPreservesOperationalBodyAndCloseCausesSafely(t *testing.T) {
	readFailure := errors.New("private body read detail")
	closeFailure := errors.New("private body close detail")
	for _, test := range []struct {
		name        string
		body        *accessTunnelFaultBody
		wantInvalid bool
	}{
		{name: "read and close failure", body: &accessTunnelFaultBody{readErr: readFailure, closeErr: closeFailure}},
		{name: "close failure", body: &accessTunnelFaultBody{data: []byte(`{}`), closeErr: closeFailure}},
		{name: "oversize plus close failure", body: &accessTunnelFaultBody{data: []byte(strings.Repeat("x", accessTunnelResponseLimit+1)), closeErr: closeFailure}, wantInvalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, _ := url.Parse("http://hostd.invalid")
			client := &localAccessTunnelClient{
				base: base, token: "local-token",
				client: &http.Client{Transport: accessTunnelRoundTripper(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: test.body, Request: request}, nil
				})},
			}
			_, err := client.Start(context.Background(), "tun_1", "127.0.0.1:0")
			if err == nil {
				t.Fatal("expected local host service failure")
			}
			if !errors.Is(err, closeFailure) {
				t.Fatalf("close cause was lost: %T", err)
			}
			if test.wantInvalid {
				if !errors.Is(err, ErrAccessTunnelInvalid) {
					t.Fatalf("oversized response classification lost: %T", err)
				}
			} else if !errors.Is(err, ErrAccessTunnelUnavailable) {
				t.Fatalf("operational response failure classification lost: %T", err)
			}
			if errors.Is(err, readFailure) != (test.name == "read and close failure") {
				t.Fatalf("read cause mismatch: %T", err)
			}
			if text := err.Error(); strings.Contains(text, "private body") || strings.Contains(text, "hostd.invalid") {
				t.Fatalf("error disclosed local detail: %q", text)
			}
		})
	}
}

func TestProductionAccessTunnelRuntimeKeepsMissingTokenCauseWithoutPath(t *testing.T) {
	stateRoot := t.TempDir()
	runtimeDir := filepath.Join(stateRoot, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "worker-local.json"), []byte(`{"schema":"paperboat.worker-local/v1","listen_address":"127.0.0.1:1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
	_, err := newProductionAccessTunnelRuntime()
	if !errors.Is(err, ErrAccessTunnelUnavailable) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing token cause/classification lost: %T", err)
	}
	if strings.Contains(err.Error(), stateRoot) || strings.Contains(err.Error(), "local-control-token") {
		t.Fatalf("error disclosed local path: %q", err)
	}
}

func TestAccessTunnelHumanOutputIsCanonicalSafeEndpoint(t *testing.T) {
	runtime := &fakeAccessTunnelRuntime{}
	withAccessTunnelRuntime(t, runtime)
	ctx, cancel := context.WithCancel(context.Background())
	output := &cancelAfterWrite{cancel: cancel}
	command := accessTunnelCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(output)
	command.SetArgs([]string{"tun_1"})
	_ = command.Execute()
	if got := output.String(); got != "Private access for route route_tcp_1 is listening on 127.0.0.1:24000\n" {
		t.Fatalf("output=%q", got)
	}
}
