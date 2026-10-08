package edgehttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/admission"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
)

type browserTerminalAdmissionFunc func(context.Context, string, string, string) (control.BrowserTerminalAdmission, error)

func (f browserTerminalAdmissionFunc) Admit(ctx context.Context, ticket, origin, host string) (control.BrowserTerminalAdmission, error) {
	return f(ctx, ticket, origin, host)
}

func TestRuntimeBrowserTerminalReplacesTicketWithHostCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer server.Close()
	defer client.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	value := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: datacarrier.RuntimeCarrierRoute, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err := registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	if err := hub.ActivateRoute(value.RouteID, BrowserTerminalRouteFence{Identity: identity, Revision: value.Revision, AttachmentGeneration: value.AttachmentGeneration}); err != nil {
		t.Fatal(err)
	}
	probeLease, probeMatch, err := NewPreviewCarrierRouteMatcher(registry).Acquire(ctx, value.Hostname, "/v1/browser-terminal")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := hub.Subscribe(BrowserTerminalSessionKey{RouteID: probeMatch.Rule.RouteID, TerminalSessionID: "term_1", ProcessGeneration: probeMatch.Rule.ConnectorProcessGeneration}, browserTerminalFenceForMatch(probeMatch), "attach_probe")
	if err != nil {
		t.Fatalf("hub route fence did not match the runtime route: %+v: %v", browserTerminalFenceForMatch(probeMatch), err)
	}
	probe.Close()
	_ = probeLease.Close()
	var admissions atomic.Int32
	closed := make(chan struct{})
	gotInputs := make(chan string, 2)
	hostDone := make(chan struct{})
	var cleanupOnce sync.Once
	cleanup := func() { cleanupOnce.Do(func() { close(closed); close(hostDone) }) }
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/browser-terminal" || r.Header.Get("Origin") != "" || r.Header.Get("Sec-WebSocket-Protocol") != browserTerminalSubprotocol {
			t.Errorf("unexpected upstream websocket request: path=%q origin=%q protocol=%q", r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Sec-WebSocket-Protocol"))
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer host-credential-") {
			t.Errorf("host credential missing or browser ticket forwarded: %q", r.Header.Get("Authorization"))
			return
		}
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{browserTerminalSubprotocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		kind, payload, err := connection.Read(r.Context())
		if err != nil || kind != websocket.MessageBinary {
			gotInputs <- "invalid"
			return
		}
		gotInputs <- string(payload)
		if err := connection.Write(r.Context(), websocket.MessageBinary, []byte("host-tls-reply")); err != nil {
			return
		}
		<-hostDone
	}))
	defer host.Close()
	hostURL, err := url.Parse(host.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := browserTerminalTestRoundTripper{target: hostURL}
	policy, err := New(Config{
		PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry),
		RuntimeCarrierTransport: transport, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: "edge.example.test",
		BrowserTerminal: browserTerminalAdmissionFunc(func(_ context.Context, ticket, origin, requestHost string) (control.BrowserTerminalAdmission, error) {
			if ticket != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" || origin != "https://dashboard.example.test" || requestHost != value.Hostname {
				t.Fatal("wrong browser admission binding")
			}
			id := admissions.Add(1)
			return control.BrowserTerminalAdmission{Credential: "host-credential-" + strconv.Itoa(int(id)), TerminalSessionID: "term_1", AttachmentID: "attach_" + strconv.Itoa(int(id)), Closed: closed, Close: func() {}}, nil
		}),
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(policy)
	defer edge.Close()
	defer cleanup()
	edgeURL := "ws" + strings.TrimPrefix(edge.URL, "http") + "/v1/browser-terminal/" + value.Hostname
	dial := func() *websocket.Conn {
		header := make(http.Header)
		header.Set("Origin", "https://dashboard.example.test")
		connection, response, dialErr := websocket.Dial(ctx, edgeURL, &websocket.DialOptions{
			Host:       "edge.example.test",
			HTTPHeader: header, Subprotocols: []string{browserTerminalSubprotocol, browserTerminalTicketPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			CompressionMode: websocket.CompressionDisabled,
		})
		if dialErr != nil {
			var responseBody string
			responseStatus := 0
			if response != nil {
				responseStatus = response.StatusCode
				if response.Body != nil {
					body, _ := io.ReadAll(response.Body)
					responseBody = string(body)
					_ = response.Body.Close()
				}
			}
			t.Fatalf("browser websocket dial: %v; status=%d body=%q", dialErr, responseStatus, responseBody)
		}
		return connection
	}
	first, second := dial(), dial()
	defer first.CloseNow()
	defer second.CloseNow()
	for index, connection := range []*websocket.Conn{first, second} {
		payload := []byte("tls-input-" + strconv.Itoa(index+1))
		message := append([]byte{browserTerminalTLSDiscriminator}, payload...)
		if err := connection.Write(ctx, websocket.MessageBinary, message); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case input := <-gotInputs:
			seen[input] = true
		case <-ctx.Done():
			t.Fatal("host did not receive both independent browser input channels")
		}
	}
	if !seen["tls-input-1"] || !seen["tls-input-2"] {
		t.Fatalf("host inputs = %v", seen)
	}
	for _, connection := range []*websocket.Conn{first, second} {
		kind, payload, err := connection.Read(ctx)
		if err != nil || kind != websocket.MessageBinary || string(payload) != "\x00host-tls-reply" {
			t.Fatalf("host TLS bridge output = %q kind=%v err=%v", payload, kind, err)
		}
	}
	publisher, err := hub.RegisterPublisher(BrowserTerminalSessionKey{RouteID: value.RouteID, TerminalSessionID: "term_1", ProcessGeneration: identity.ProcessGeneration}, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if err := publisher.Publish([]byte("opaque-cose-record")); err != nil {
		t.Fatal(err)
	}
	for _, connection := range []*websocket.Conn{first, second} {
		kind, payload, err := connection.Read(ctx)
		if err != nil || kind != websocket.MessageBinary || string(payload) != "\x01opaque-cose-record" {
			t.Fatalf("shared terminal output = %q kind=%v err=%v", payload, kind, err)
		}
	}
}

type browserTerminalTestRoundTripper struct{ target *url.URL }

func (t browserTerminalTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.URL = new(url.URL)
	*forwarded.URL = *request.URL
	forwarded.URL.Scheme = "http"
	forwarded.URL.Host = t.target.Host
	forwarded.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(forwarded)
}

func TestRuntimeCarrierGatewayRequiresHelperAuthAndPreservesRenewedStream(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	value := DataCarrierPreviewRoute{RouteID: "runtime_route", Hostname: "machine.runtime.example.test", Kind: datacarrier.RuntimeCarrierRoute, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err = registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	matcher := NewPreviewCarrierRouteMatcher(registry)
	lease, match, err := matcher.Acquire(context.Background(), value.Hostname, "/v1/browser-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if string(match.Rule.Kind) != datacarrier.RuntimeCarrierRoute || match.Rule.Target != "" || !matcher.HasHTTPSHostname(value.Hostname) {
		t.Fatal("runtime carrier acquired public/raw route")
	}
	value.ExpiresAt = value.ExpiresAt.Add(time.Minute)
	if err = registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Context().Done():
		t.Fatal("lease renewal disconnected active stream")
	default:
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewGatewayWithTransports(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: matcher, RuntimeCarrierTransport: transport, HelperAccess: helperAccessFunc(func(_ context.Context, token string) (admission.Claims, error) {
		if token != "runtime-test-token" {
			return admission.Claims{}, errors.New("unauthorized")
		}
		return admission.Claims{JTI: "test", EnvironmentID: "machine", MachineID: identity.HostID, CredentialClass: "browser_terminal_operation", ExpiresAt: time.Now().Add(time.Minute)}, nil
	}), Revocations: revocationFunc(func(context.Context, admission.Claims) (bool, error) { return false, nil }), RevocationCheckInterval: time.Second}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/browser-terminal", "/arbitrary"} {
		req := httptest.NewRequest("GET", "https://"+value.Hostname+path, nil)
		rec := httptest.NewRecorder()
		gateway.ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if path == "/arbitrary" {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Fatalf("unauthorized %s=%d", path, rec.Code)
		}
	}
	done := make(chan error, 1)
	go func() {
		stream, open, err := client.AcceptStream(context.Background())
		if err != nil {
			done <- err
			return
		}
		if open.RouteID != value.RouteID || open.Kind != "https" {
			_ = stream.Close()
			done <- errors.New("wrong runtime stream binding")
			return
		}
		done <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/browser-terminal" || r.Header.Get("Authorization") != "Bearer runtime-test-token" {
				w.WriteHeader(400)
				return
			}
			_, _ = io.WriteString(w, "runtime via carrier")
		}), 4096, time.Second)
	}()
	req := httptest.NewRequest("GET", "https://"+value.Hostname+"/v1/browser-terminal", nil)
	req.Header.Set("Authorization", "Bearer runtime-test-token")
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "runtime via carrier" {
		t.Fatalf("carrier response %d %q", rec.Code, rec.Body.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime request leaked")
	}
	if err = registry.Detach(value.RouteID, identity, value.Revision); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("detachment retained stream")
	}
	if matcher.HasHTTPSHostname(value.Hostname) {
		t.Fatal("detached hostname still admitted for TLS")
	}
}

func TestRuntimeCarrierTransportPreservesWebSocketUpgrade(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer server.Close()
	defer client.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	value := DataCarrierPreviewRoute{RouteID: "runtime_ws", Hostname: "machine.runtime.example.test", Kind: datacarrier.RuntimeCarrierRoute, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err := registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		stream, open, err := client.AcceptStream(context.Background())
		if err != nil {
			done <- err
			return
		}
		defer stream.Close()
		if open.Kind != "websocket" {
			done <- errors.New("upgrade used wrong stream kind")
			return
		}
		reader := bufio.NewReader(stream)
		request, err := http.ReadRequest(reader)
		if err != nil {
			done <- err
			return
		}
		request.Body.Close()
		if _, err = io.WriteString(stream, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nhello"); err != nil {
			done <- err
			return
		}
		payload := make([]byte, 4)
		_, err = io.ReadFull(reader, payload)
		if err == nil && string(payload) != "ping" {
			err = errors.New("upgrade input changed")
		}
		done <- err
	}()
	request := httptest.NewRequest(http.MethodGet, "https://"+value.Hostname+"/v1/browser-terminal", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	upgraded, ok := response.Body.(io.ReadWriteCloser)
	if response.StatusCode != http.StatusSwitchingProtocols || !ok {
		t.Fatalf("upgrade status=%d writable=%v", response.StatusCode, ok)
	}
	output := make([]byte, 5)
	if _, err := io.ReadFull(upgraded, output); err != nil || string(output) != "hello" {
		t.Fatalf("upgrade output=%q error=%v", output, err)
	}
	if _, err := upgraded.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade did not finish")
	}
}
