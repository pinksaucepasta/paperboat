package edgehttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

func TestDataCarrierPreviewRegistryFencesReplacementGeneration(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1", MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	identityOne := testEdgePreviewIdentity(1, 1)
	serverOne, clientOne := testEdgePreviewCarrierPair(t, identityOne)
	identityTwo := testEdgePreviewIdentity(2, 2)
	serverTwo, clientTwo := testEdgePreviewCarrierPair(t, identityTwo)
	defer clientOne.Close()
	defer serverOne.Close()
	defer clientTwo.Close()
	defer serverTwo.Close()

	routeOne := DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: serverOne}
	if err := registry.Attach(routeOne); err != nil {
		t.Fatal(err)
	}
	routeTwo := DataCarrierPreviewRoute{RouteID: routeOne.RouteID, Hostname: routeOne.Hostname, Kind: routeOne.Kind, EdgeProcessEpoch: routeOne.EdgeProcessEpoch, Revision: 1, Server: serverTwo}
	if err := registry.Attach(routeTwo); err != nil {
		t.Fatalf("replacement attach: %v", err)
	}
	if got, ok := registry.Lookup(routeOne.Hostname); !ok || got.Identity != identityTwo || got.Server != serverTwo {
		t.Fatalf("lookup after replacement = %+v, ok=%v", got, ok)
	}
	if err := serverOne.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverOne.Done():
	case <-time.After(time.Second):
		t.Fatal("old server did not close")
	}
	if got, ok := registry.Lookup(routeOne.Hostname); !ok || got.Server != serverTwo {
		t.Fatalf("old server completion removed replacement: %+v, ok=%v", got, ok)
	}
	if err := registry.Attach(routeOne); !errors.Is(err, ErrDataCarrierPreviewRegistryStale) {
		t.Fatalf("stale replacement error = %v, want %v", err, ErrDataCarrierPreviewRegistryStale)
	}
	olderProcessIdentity := identityTwo
	olderProcessIdentity.Generation = 3
	olderProcessIdentity.ProcessGeneration = 1
	olderProcessServer, olderProcessClient := testEdgePreviewCarrierPair(t, olderProcessIdentity)
	defer olderProcessClient.Close()
	defer olderProcessServer.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: routeOne.RouteID, Hostname: routeOne.Hostname, Kind: routeOne.Kind, EdgeProcessEpoch: routeOne.EdgeProcessEpoch, Revision: 2, Server: olderProcessServer}); !errors.Is(err, ErrDataCarrierPreviewRegistryStale) {
		t.Fatalf("older process with newer config error = %v", err)
	}
	otherAccountIdentity := identityTwo
	otherAccountIdentity.AccountID = "account_02"
	otherAccountIdentity.Generation = 3
	otherAccountIdentity.ProcessGeneration = 3
	otherAccountServer, otherAccountClient := testEdgePreviewCarrierPair(t, otherAccountIdentity)
	defer otherAccountClient.Close()
	defer otherAccountServer.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: routeOne.RouteID, Hostname: routeOne.Hostname, Kind: routeOne.Kind, EdgeProcessEpoch: routeOne.EdgeProcessEpoch, Revision: 2, Server: otherAccountServer}); !errors.Is(err, ErrDataCarrierPreviewRegistryStale) {
		t.Fatalf("cross-account replacement error = %v", err)
	}

	conflictServer, conflictClient := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(3, 3))
	defer conflictClient.Close()
	defer conflictServer.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_02", Hostname: routeOne.Hostname, Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: routeOne.EdgeProcessEpoch, Revision: 1, Server: conflictServer}); !errors.Is(err, ErrDataCarrierPreviewRegistryConflict) {
		t.Fatalf("host conflict error = %v, want %v", err, ErrDataCarrierPreviewRegistryConflict)
	}
}

func TestPreviewCarrierRouteMatcherFencesReplacementAndPolicyOwnership(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1", MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	identityOne := testEdgePreviewIdentity(1, 1)
	serverOne, clientOne := testEdgePreviewCarrierPair(t, identityOne)
	defer clientOne.Close()
	defer serverOne.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: serverOne}); err != nil {
		t.Fatal(err)
	}
	matcher := NewPreviewCarrierRouteMatcher(registry)
	oldLease, oldMatch, err := matcher.Acquire(context.Background(), "app.preview.example.test", "/events")
	if err != nil || oldMatch.Rule.ID != "preview_01" || oldMatch.Generation != identityOne.Generation {
		t.Fatalf("old preview lease = %+v, %v", oldMatch, err)
	}
	identityTwo := identityOne
	identityTwo.ProcessGeneration = 2
	identityTwo.Generation = 2
	serverTwo, clientTwo := testEdgePreviewCarrierPair(t, identityTwo)
	defer clientTwo.Close()
	defer serverTwo.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: serverTwo}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldLease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("replacement did not cancel old preview lease")
	}
	newLease, newMatch, err := matcher.Acquire(context.Background(), "app.preview.example.test", "/events")
	if err != nil || newMatch.Generation != identityTwo.Generation {
		t.Fatalf("new preview lease = %+v, %v", newMatch, err)
	}
	_ = newLease.Close()
	_ = oldLease.Close()

	var seen route.RouteMatch
	policy, err := New(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 1024, Routes: NewCompositeRouteMatcher(matcher), RequestID: func() string { return "preview-request" }}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		seen, ok = RouteMatchFromContext(r.Context())
		if !ok {
			t.Error("preview route match missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/events", nil)
	request.Host = "app.preview.example.test"
	recorder := httptest.NewRecorder()
	policy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || seen.Rule.ID != "preview_01" || seen.Generation != identityTwo.Generation {
		t.Fatalf("attached preview status=%d match=%+v", recorder.Code, seen)
	}
	unknown := httptest.NewRequest(http.MethodGet, "https://unknown.preview.example.test/", nil)
	unknown.Host = "unknown.preview.example.test"
	recorder = httptest.NewRecorder()
	policy.ServeHTTP(recorder, unknown)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unowned preview status=%d", recorder.Code)
	}
}

func TestDataCarrierPreviewRegistryRejectsNonDNSHostname(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	server, client := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(1, 1))
	defer client.Close()
	defer server.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "bad_name.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); !errors.Is(err, ErrDataCarrierPreviewRegistryInvalid) {
		t.Fatalf("hostname error=%v", err)
	}
}

func TestDataCarrierPreviewRegistryAcceptsPrivateAdmissionWithoutPublicDowngrade(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	server, client := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(1, 1))
	defer server.Close()
	defer client.Close()
	private := DataCarrierPreviewRoute{
		RouteID: "preview_private", Hostname: "private.preview.example.test", Kind: "preview_private_https_wss", AccessMode: "private", EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server,
	}
	if err := registry.Attach(private); err != nil {
		t.Fatalf("private route error = %v", err)
	}
	if registry.Len() != 1 || registry.Snapshot()[0].AccessMode != "private" {
		t.Fatalf("private route was not preserved: %+v", registry.Snapshot())
	}
}

func TestDataCarrierPreviewRegistryProjectsOnlyAuthenticatedReadyRoutes(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if kind, state, reason, found := registry.RouteState("missing.preview.example.test"); found || kind != "" || state != "" || reason != "" {
		t.Fatalf("missing route state = %q %q %q %v", kind, state, reason, found)
	}
	server, client := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(1, 1))
	defer client.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
		t.Fatal(err)
	}
	if kind, state, reason, found := registry.RouteState("app.preview.example.test"); !found || kind != dataCarrierPreviewRouteKind || state != "ready" || reason != "" {
		t.Fatalf("ready route state = %q %q %q %v", kind, state, reason, found)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if kind, state, reason, found := registry.RouteState("app.preview.example.test"); found || kind != "" || state != "" || reason != "" {
		t.Fatalf("closed route state = %q %q %q %v", kind, state, reason, found)
	}
}

func TestDataCarrierPreviewTransportRejectsPrivatePublicRequest(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	now := time.Now().UTC()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "route_private", Hostname: "private.preview.example.test", Kind: dataCarrierPreviewPrivateRouteKind, AccessMode: "private", EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server, PreviewID: "preview_private", OperationID: "operation_private", OwnerMachineID: identity.HostID, OwnerSessionID: "owner_session_1", LeaseGeneration: 1, AttachmentGeneration: 1, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Endpoint: "https://private.preview.example.test", ExpiresAt: now.Add(time.Minute), MachineIdentityPublicKey: "machine-public", MachineIdentityThumbprint: "machine-thumbprint"}); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://private.preview.example.test/", nil)
	request.Header.Set("Cookie", "paperboat_session=must-not-authorize")
	request.Header.Set("X-Paperboat-Access-Authorization", "must-not-authorize")
	if _, err := transport.RoundTrip(request); !errors.Is(err, ErrDataCarrierPreviewTransport) {
		t.Fatalf("private public request error = %v", err)
	}
}

func TestDataCarrierPreviewTransportStreamsHTTPAndTrailers(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		stream, open, acceptErr := client.AcceptStream(context.Background())
		if acceptErr != nil {
			serveDone <- acceptErr
			return
		}
		if open.RouteID != "preview_01" || open.Kind != "https" || open.AccountID != identity.AccountID || open.TunnelID != identity.TunnelID || open.ConnectorID != identity.ConnectorID || open.SessionID != identity.SessionID || open.Generation != identity.Generation {
			serveDone <- errors.New("unexpected stream-open metadata")
			_ = stream.Close()
			return
		}
		serveDone <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Host != "app.preview.example.test" || request.URL.Path != "/events" {
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Add("Trailer", "X-Paperboat-Final")
			writer.WriteHeader(http.StatusOK)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = io.WriteString(writer, "data: ready\n\n")
			writer.Header().Set("X-Paperboat-Final", "complete")
		}), 4096, time.Second)
	}()
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/events", nil)
	request.Host = "app.preview.example.test"
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		select {
		case serveErr := <-serveDone:
			t.Fatalf("read response body: %v; serve: %v", err, serveErr)
		default:
			t.Fatal(err)
		}
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || string(body) != "data: ready\n\n" || response.Trailer.Get("X-Paperboat-Final") != "complete" {
		t.Fatalf("response status=%d headers=%v trailer=%v body=%q", response.StatusCode, response.Header, response.Trailer, body)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream HTTP server did not stop")
	}
}

func TestDataCarrierPreviewTransportRetriesAfterStaleResponseHeaderTimeout(t *testing.T) {
	for _, duringOpen := range []bool{false, true} {
		name := "response_headers"
		if duringOpen {
			name = "stream_open"
		}
		t.Run(name, func(t *testing.T) { testPreviewRouteReplacementRetry(t, duringOpen) })
	}
}

func testPreviewRouteReplacementRetry(t *testing.T, duringOpen bool) {
	t.Helper()
	oldAccepted := make(chan struct{})
	oldClosed := make(chan struct{})
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{
		BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1", MaximumRoutes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	identityOne := testEdgePreviewIdentity(1, 1)
	var authorize datacarrier.Authorizer
	if duringOpen {
		authorize = datacarrier.AuthorizerFunc(func(ctx context.Context, _ datacarrier.Identity, _ datacarrier.StreamOpen) error {
			close(oldAccepted)
			<-ctx.Done()
			return ctx.Err()
		})
	}
	serverOne, clientOne := testEdgePreviewCarrierPair(t, identityOne, authorize)
	identityTwo := testEdgePreviewIdentity(2, 2)
	serverTwo, clientTwo := testEdgePreviewCarrierPair(t, identityTwo)
	route := DataCarrierPreviewRoute{
		RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind,
		EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: serverOne,
	}
	if err := registry.Attach(route); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{
		Registry: registry, StreamOpenTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	if duringOpen {
		close(oldClosed)
	} else {
		go func() {
			stream, _, acceptErr := clientOne.AcceptStream(context.Background())
			if acceptErr != nil {
				close(oldClosed)
				return
			}
			close(oldAccepted)
			_, _ = io.Copy(io.Discard, stream)
			_ = stream.Close()
			close(oldClosed)
		}()
	}

	newServed := make(chan error, 1)
	go func() {
		stream, _, acceptErr := clientTwo.AcceptStream(context.Background())
		if acceptErr != nil {
			newServed <- acceptErr
			return
		}
		newServed <- datacarrier.ServeHTTPStream(context.Background(), stream,
			http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.Host != "app.preview.example.test" || request.URL.Path != "/" {
					t.Error("retry did not deliver the exact request to the replacement")
					http.Error(writer, "invalid retry", http.StatusBadGateway)
					return
				}
				writer.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(writer, "healthy")
			}), 4096, time.Second)
	}()

	result := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/", nil)
	request.Host = "app.preview.example.test"
	go func() {
		response, roundTripErr := (retryPreviewTransport{next: transport}).RoundTrip(request)
		result <- struct {
			response *http.Response
			err      error
		}{response, roundTripErr}
	}()

	select {
	case <-oldAccepted:
	case <-time.After(time.Second):
		t.Fatal("stale carrier stream was not opened")
	}
	if err := registry.Attach(DataCarrierPreviewRoute{
		RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind,
		EdgeProcessEpoch: "edge_epoch_1", Revision: 2, Server: serverTwo,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("retrying stale response headers: %v", got.err)
		}
		body, readErr := io.ReadAll(got.response.Body)
		closeErr := got.response.Body.Close()
		if readErr != nil || closeErr != nil || got.response.StatusCode != http.StatusOK || string(body) != "healthy" {
			t.Fatalf("retried response status=%d body=%q read=%v close=%v", got.response.StatusCode, body, readErr, closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale response-header timeout did not retry the replacement route")
	}
	select {
	case err := <-newServed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement carrier did not serve the retried request")
	}
	select {
	case <-oldClosed:
	case <-time.After(time.Second):
		t.Fatal("stale response-header timeout did not close the old stream")
	}
}

func TestDataCarrierPreviewTransportCancellationClosesStream(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	serverAccepted := make(chan struct{})
	serverClosed := make(chan struct{})
	go func() {
		stream, _, acceptErr := client.AcceptStream(context.Background())
		if acceptErr == nil {
			close(serverAccepted)
			buffer := make([]byte, 1)
			_, _ = stream.Read(buffer)
			_ = stream.Close()
		}
		close(serverClosed)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/events", nil).WithContext(ctx)
	request.Host = "app.preview.example.test"
	result := make(chan error, 1)
	go func() {
		_, err := transport.RoundTrip(request)
		result <- err
	}()
	select {
	case <-serverAccepted:
	case <-time.After(time.Second):
		t.Fatal("carrier stream was not accepted")
	}
	cancel()
	select {
	case <-serverClosed:
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not close carrier stream")
	}
	select {
	case err := <-result:
		if err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, ErrDataCarrierPreviewTransport) {
			t.Fatalf("canceled transport error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled transport did not return")
	}
}

func TestDataCarrierPreviewTransportReturnsEarlyResponseDuringStreamingUpload(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		stream, _, acceptErr := client.AcceptStream(context.Background())
		if acceptErr != nil {
			serveDone <- acceptErr
			return
		}
		defer stream.Close()
		reader := bufio.NewReader(stream)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				serveDone <- readErr
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, writeErr := io.WriteString(stream, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		serveDone <- writeErr
	}()
	bodyReader, bodyWriter := io.Pipe()
	request := httptest.NewRequest(http.MethodPost, "https://app.preview.example.test/upload", bodyReader)
	request.Host = "app.preview.example.test"
	result := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, roundTripErr := transport.RoundTrip(request)
		result <- struct {
			response *http.Response
			err      error
		}{response, roundTripErr}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.response.StatusCode != http.StatusAccepted {
			t.Fatalf("status=%d", got.response.StatusCode)
		}
		if err := got.response.Body.Close(); err != nil {
			t.Fatalf("early response cleanup: %T", err)
		}
		if _, err := bodyWriter.Write([]byte("late upload")); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("response Close left upload reader running")
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTrip waited for an unfinished streaming request body")
	}
	_ = bodyWriter.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming upload handler did not stop")
	}
}

func testEdgePreviewIdentity(generation, processGeneration uint64) datacarrier.Identity {
	return datacarrier.Identity{AccountID: "account_01", HostID: "host_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", SessionID: "session_01", ProcessGeneration: processGeneration, Generation: generation}
}

func testEdgePreviewCarrierPair(t *testing.T, identity datacarrier.Identity, authorizers ...datacarrier.Authorizer) (*datacarrier.Server, *datacarrier.Client) {
	t.Helper()
	local, remote := net.Pipe()
	config := datacarrier.DefaultConfig()
	config.MaximumStreams = 8
	config.AcceptBacklog = 8
	config.QueueDepth = 4
	config.Identity = identity
	config.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	if len(authorizers) > 0 && authorizers[0] != nil {
		config.Authorize = authorizers[0]
	}
	server, err := datacarrier.NewServer(context.Background(), remote, config)
	if err != nil {
		_ = local.Close()
		_ = remote.Close()
		t.Fatalf("new edge server: %v", err)
	}
	client, err := datacarrier.NewClient(context.Background(), local, config)
	if err != nil {
		_ = server.Close()
		_ = local.Close()
		t.Fatalf("new connector client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return server, client
}

func TestPreviewResponseOutlivesAdmissionTTLUntilRouteRevoked(t *testing.T) {
	server, client := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(1, 1))
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	route := DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server, ExpiresAt: time.Now().Add(50 * time.Millisecond)}
	if err := registry.Attach(route); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		stream, _, err := client.AcceptStream(ctx)
		if err != nil {
			sent <- err
			return
		}
		defer stream.Close()
		request, err := http.ReadRequest(bufio.NewReader(stream))
		if err != nil {
			sent <- err
			return
		}
		defer request.Body.Close()
		if _, err = io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n"); err != nil {
			sent <- err
			return
		}
		timer := time.NewTimer(time.Until(route.ExpiresAt.Add(50 * time.Millisecond)))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			sent <- ctx.Err()
			return
		}
		_, err = io.WriteString(stream, "a")
		sent <- err
		var b [1]byte
		_, _ = stream.Read(b[:])
	}()
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/", nil).WithContext(ctx)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var b [1]byte
	if _, err := io.ReadFull(response.Body, b[:]); err != nil || b[0] != 'a' {
		t.Fatal("admitted response did not survive credential expiry", err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { _, err := response.Body.Read(b[:]); readDone <- err }()
	if err := registry.Detach(route.RouteID, server.Identity(), route.Revision); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("revoked response remained readable")
		}
	case <-ctx.Done():
		t.Fatal("route revocation did not interrupt blocked response")
	}
}

// A large upload keeps the origin from answering until the body is complete, so
// the response-header budget must start after the request has been written.
func TestDataCarrierPreviewTransportHeaderTimeoutStartsAfterUpload(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
		t.Fatal(err)
	}
	const uploadChunks, chunkSize = 8, 1024
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		stream, _, acceptErr := client.AcceptStream(context.Background())
		if acceptErr != nil {
			serveDone <- acceptErr
			return
		}
		serveDone <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			received, copyErr := io.Copy(io.Discard, request.Body)
			if copyErr != nil || received != uploadChunks*chunkSize {
				t.Errorf("origin received %d bytes err=%v", received, copyErr)
				http.Error(writer, "truncated upload", http.StatusBadRequest)
				return
			}
			writer.WriteHeader(http.StatusAccepted)
		}), 4096, 5*time.Second)
	}()
	bodyReader, bodyWriter := io.Pipe()
	go func() {
		chunk := make([]byte, chunkSize)
		for range uploadChunks {
			time.Sleep(60 * time.Millisecond)
			if _, writeErr := bodyWriter.Write(chunk); writeErr != nil {
				return
			}
		}
		_ = bodyWriter.Close()
	}()
	request := httptest.NewRequest(http.MethodPost, "https://app.preview.example.test/upload", bodyReader)
	request.Host = "app.preview.example.test"
	request.ContentLength = uploadChunks * chunkSize
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("upload longer than the header budget failed: %v", err)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

// A WebSocket upgrade must cross the preview data carrier: the 101 response
// carries a writable body that bridges the upgraded connection.
func TestDataCarrierPreviewTransportPreservesUpgrade(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer client.Close()
	defer server.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		stream, _, acceptErr := client.AcceptStream(context.Background())
		if acceptErr != nil {
			return
		}
		_ = datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			connection, buffered, hijackErr := writer.(http.Hijacker).Hijack()
			if hijackErr != nil {
				return
			}
			defer connection.Close()
			_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			_ = buffered.Flush()
			echo := make([]byte, 4)
			if _, readErr := io.ReadFull(buffered, echo); readErr == nil {
				_, _ = connection.Write(echo)
			}
		}), 4096, 5*time.Second)
	}()
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/ws", nil)
	request.Host = "app.preview.example.test"
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("upgrade request failed: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status=%d, want 101", response.StatusCode)
	}
	writer, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("101 body is not writable")
	}
	defer writer.Close()
	if _, err := writer.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(writer, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo=%q err=%v", echo, err)
	}
}
