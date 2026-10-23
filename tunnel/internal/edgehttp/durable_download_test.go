package edgehttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

// A large download through the durable tunnel path reaches a slow client intact: the edge holds
// up to the carrier's stream window while the client lags, and must neither cut nor stall it.
func TestDurableTunnelCarriesALargeDownloadToASlowClient(t *testing.T) {
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	carrier, connector := testEdgePreviewCarrierPair(t, identity)
	rule := replicaRule()
	rule.Generation = 1
	rule.Target = rule.RouteID
	rule.Protocol = "http"
	rule.Hostname = "upload.customer.test"
	rule.MatchType = route.MatchExact
	rule.PathPrefix = "/"
	rule.AccessMode = "public"
	rule.ObservedState = "ready"
	rule.Node = "edge_01"
	rule.EdgeProcessEpoch = "epoch_0001"
	rule.EdgeFailureDomain = "region_01"
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.AttachReplica(carrier, "key", "thumb", replicaState(rule, 3, rule.AssignmentID, "region_01", time.Millisecond, 2)); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierRouteTransport(DataCarrierRouteTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	if err := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}
	const total = 24 << 20
	policy, err := NewGatewayWithTransports(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 16 << 10, MaxBodyBytes: 64 << 20, Routes: routes}, "127.0.0.1:1", nil, transport)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(policy)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	go func() {
		stream, _, acceptErr := connector.AcceptStream(context.Background())
		if acceptErr != nil {
			return
		}
		_ = datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			chunk := bytes.Repeat([]byte("0123456789abcdef"), 64<<10/16)
			writer.Header().Set("Content-Length", "25165824")
			for sent := 0; sent < total; sent += len(chunk) {
				if _, err := writer.Write(chunk); err != nil {
					return
				}
			}
		}), 16<<10, 5*time.Second)
	}()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/blob", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "upload.customer.test"
	client := server.Client()
	client.Timeout = 120 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	defer response.Body.Close()
	var n int64
	buffer := make([]byte, 64<<10)
	for {
		m, readErr := response.Body.Read(buffer)
		n += int64(m)
		time.Sleep(time.Duration(m) * 500 * time.Nanosecond) // a slow client: about 2 MB/s
		if readErr == io.EOF {
			err = nil
			break
		}
		if readErr != nil {
			err = readErr
			break
		}
	}
	if err != nil || n != total {
		t.Fatalf("downloaded %d of %d bytes (status %d, %s): %v", n, total, response.StatusCode, response.Proto, err)
	}
}
