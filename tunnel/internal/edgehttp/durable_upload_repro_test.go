package edgehttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

// A large upload through the durable tunnel path must reach a slow connector
// intact: the edge must not end the stream while the origin is still reading.
func TestDurableTunnelCarriesALargeSlowUpload(t *testing.T) {
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
	const total = 16 << 20
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
			buffer := make([]byte, 64<<10)
			var received int
			for {
				n, readErr := request.Body.Read(buffer)
				received += n
				time.Sleep(time.Duration(n) * 60 * time.Nanosecond) // a slow origin: about 16 MB/s
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					http.Error(writer, readErr.Error(), http.StatusBadRequest)
					return
				}
			}
			if received != total {
				http.Error(writer, "short body", http.StatusBadRequest)
			}
		}), 16<<10, 5*time.Second)
	}()
	body := bytes.Repeat([]byte("0123456789abcdef"), total/16)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/digest", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "upload.customer.test"
	client := server.Client()
	client.Timeout = 120 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%q (proto %s)", response.StatusCode, message, response.Proto)
	}
}
