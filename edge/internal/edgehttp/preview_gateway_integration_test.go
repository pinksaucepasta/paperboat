package edgehttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
)

// TestGatewayRoutesAttachedPreviewToCarrierTransport proves the complete live
// public-preview path. A route published in the canonical preview registry is
// selected by the strict gateway matcher and forwarded over its authenticated
// carrier, rather than falling through to the legacy upstream (which would
// return a plain 404).
func TestGatewayRoutesAttachedPreviewToCarrierTransport(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{
		BaseDomain:   "preview.example.test",
		ProcessEpoch: "edge_epoch_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{
		RouteID:          "preview_live",
		Hostname:         "app.preview.example.test",
		Kind:             dataCarrierPreviewRouteKind,
		EdgeProcessEpoch: "edge_epoch_1",
		Revision:         1,
		Server:           server,
	}); err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	var originSaw http.Header
	go func() {
		stream, open, acceptErr := client.AcceptStream(context.Background())
		if acceptErr != nil {
			serveDone <- acceptErr
			return
		}
		if open.RouteID != "preview_live" || open.Kind != "https" || open.AccountID != identity.AccountID || open.TunnelID != identity.TunnelID || open.ConnectorID != identity.ConnectorID || open.SessionID != identity.SessionID || open.Generation != identity.Generation {
			_ = stream.Close()
			serveDone <- errors.New("unexpected preview carrier stream metadata")
			return
		}
		serveDone <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			originSaw = request.Header.Clone()
			if request.Host != "app.preview.example.test" || request.URL.Path != "/" {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, "preview reached over carrier\n")
		}), 4096, time.Second)
	}()

	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{
		Registry:          registry,
		StreamOpenTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewGatewayWithTransports(Config{
		PreviewBaseDomain: "preview.example.test",
		TunnelBaseDomain:  "tunnels.example.test",
		RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes:    4096,
		MaxBodyBytes:      4096,
		Routes:            NewCompositeRouteMatcher(NewPreviewCarrierRouteMatcher(registry)),
		RequestID:         func() string { return "preview-live-request" },
	}, "127.0.0.1:1", transport, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/", nil)
	request.Host = "app.preview.example.test"
	request.RemoteAddr = "198.51.100.7:4000"
	request.Header.Set("X-Forwarded-Proto", "http") // a visitor's own claim must not survive
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Real-IP", "203.0.113.9")
	request.Header.Set("X-Forwarded-Host", "evil.example.test")
	request.Header.Set("Forwarded", "for=203.0.113.9;proto=http;host=evil.example.test")
	request.Header.Set("X-Forwarded-Port", "8080")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "preview reached over carrier\n" {
		t.Fatalf("gateway response = %d %q, want carrier response", response.Code, response.Body.String())
	}
	select {
	case serveErr := <-serveDone:
		if serveErr != nil {
			t.Fatal(serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("carrier HTTP handler did not stop")
	}
	// What the agent receives from the edge: the edge's own facts, one value each.
	for name, want := range map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "app.preview.example.test", "X-Forwarded-For": "198.51.100.7", "Forwarded": "for=198.51.100.7;proto=https;host=app.preview.example.test", "X-Forwarded-Port": "443", "X-Real-IP": "198.51.100.7"} {
		if got := originSaw.Values(name); len(got) != 1 || got[0] != want {
			t.Fatalf("%s reached the agent as %q, want %q", name, got, want)
		}
	}
}

// A gRPC client reads trailers and says so with "TE: trailers"; strict servers (grpc-java, Envoy) refuse a request
// without it. Over HTTP/2 through the gateway and the carrier, the origin must still see it, and the grpc-status
// trailer must come back to the client.
func TestGatewayCarriesGRPCTEHeaderAndTrailersOverHTTP2(t *testing.T) {
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(DataCarrierPreviewRoute{RouteID: "preview_live", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server}); err != nil {
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
			if request.Header.Get("Te") != "trailers" || request.Header.Get("Content-Type") != "application/grpc" {
				writer.WriteHeader(http.StatusBadRequest) // what a strict gRPC server answers without TE: trailers
				return
			}
			_, _ = io.Copy(io.Discard, request.Body)
			writer.Header().Set("Content-Type", "application/grpc")
			writer.Header().Set("Trailer", "Grpc-Status")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte{0, 0, 0, 0, 0})
			writer.Header().Set("Grpc-Status", "0")
		}), 4096, time.Second)
	}()
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewGatewayWithTransports(Config{
		PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewCompositeRouteMatcher(NewPreviewCarrierRouteMatcher(registry)),
		RequestID: func() string { return "grpc-request" },
	}, "127.0.0.1:1", transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewUnstartedServer(gateway)
	edge.EnableHTTP2 = true
	edge.StartTLS()
	defer edge.Close()

	request, err := http.NewRequest(http.MethodPost, edge.URL+"/pkg.Service/Method", bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "app.preview.example.test"
	request.Header.Set("Content-Type", "application/grpc")
	request.Header.Set("Te", "trailers")
	response, err := edge.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK || response.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("gRPC response = %s %d trailers %v: the origin did not get TE: trailers or the trailer was lost", response.Proto, response.StatusCode, response.Trailer)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}
