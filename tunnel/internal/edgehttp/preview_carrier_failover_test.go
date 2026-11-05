package edgehttp

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
)

func failoverRoute(server *datacarrier.Server, generation uint64) DataCarrierPreviewRoute {
	return DataCarrierPreviewRoute{RouteID: "route_endpoint", Hostname: "app.preview.example.test", Kind: dataCarrierPreviewRouteKind, AccessMode: "public",
		EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: server, EndpointRoute: true, AttachmentGeneration: generation,
		OperationID: "route_endpoint", PreviewID: "preview_endpoint", ConfigContentHash: "sha256:hash", Endpoint: "https://app.preview.example.test",
		ExpiresAt: time.Now().Add(time.Hour), MachineIdentityPublicKey: "machine_key", MachineIdentityThumbprint: "machine_thumbprint"}
}

// failoverPair attaches the old worker's carrier, then the new worker's: the old one becomes the
// standby. The new worker's connector reads one request and closes (a carrier closing
// mid-request); the old one answers "old". It returns the transport and a channel of the
// requests the old one served.
func failoverPair(t *testing.T) (*DataCarrierPreviewTransport, <-chan string) {
	t.Helper()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	oldIdentity := testEdgePreviewIdentity(1, 1)
	newIdentity := testEdgePreviewIdentity(1, 2)
	newIdentity.SessionID = "session_02"
	oldServer, oldClient := testEdgePreviewCarrierPair(t, oldIdentity)
	newServer, newClient := testEdgePreviewCarrierPair(t, newIdentity)
	if err := registry.Attach(failoverRoute(oldServer, 7)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Attach(failoverRoute(newServer, 8)); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Lookup("app.preview.example.test"); !ok || got.Server != newServer {
		t.Fatal("the newer carrier did not take the route")
	}
	go func() {
		stream, _, err := newClient.AcceptStream(context.Background())
		if err != nil {
			return
		}
		_, _ = http.ReadRequest(bufio.NewReader(stream))
		_ = newClient.Close()
	}()
	oldServed := make(chan string, 4)
	go func() {
		for {
			stream, _, err := oldClient.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				_ = datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					oldServed <- r.Method
					_, _ = io.WriteString(w, "old")
				}), 4096, time.Second)
			}()
		}
	}()
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return transport, oldServed
}

// A GET whose carrier closes mid-request is sent once more on the route's other live carrier.
func TestPreviewGetOnAClosingCarrierIsRetriedOnTheOther(t *testing.T) {
	transport, oldServed := failoverPair(t)
	request := httptest.NewRequest(http.MethodGet, "https://app.preview.example.test/", nil)
	request.Host = "app.preview.example.test"
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("GET on a closing carrier: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "old" {
		t.Fatalf("status %d body %q, want the other carrier's answer", response.StatusCode, body)
	}
	if method := <-oldServed; method != http.MethodGet {
		t.Fatalf("other carrier served %s", method)
	}
}

// A POST already sent on a carrier that then closes is never sent again.
func TestPreviewPostOnAClosingCarrierIsNotRetried(t *testing.T) {
	transport, oldServed := failoverPair(t)
	request := httptest.NewRequest(http.MethodPost, "https://app.preview.example.test/", strings.NewReader("payment"))
	request.Host = "app.preview.example.test"
	if response, err := transport.RoundTrip(request); err == nil {
		_ = response.Body.Close()
		t.Fatalf("POST on a closing carrier answered %d, want the failure", response.StatusCode)
	}
	select {
	case method := <-oldServed:
		t.Fatalf("the POST was sent again (%s) on the other carrier", method)
	case <-time.After(200 * time.Millisecond):
	}
}

// A carrier holds a route while it is that route's current carrier; once the same machine's newer carrier
// takes the route it is only the standby and holds none, which tells its retiring host the route moved.
func TestPreviewRouteHeldOnlyByItsCurrentCarrier(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	newIdentity := testEdgePreviewIdentity(1, 2)
	newIdentity.SessionID = "session_02"
	oldServer, _ := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(1, 1))
	newServer, _ := testEdgePreviewCarrierPair(t, newIdentity)
	if registry.Holds(oldServer) {
		t.Fatal("a carrier with no route holds one")
	}
	if err := registry.Attach(failoverRoute(oldServer, 7)); err != nil {
		t.Fatal(err)
	}
	if !registry.Holds(oldServer) {
		t.Fatal("the current carrier does not hold its route")
	}
	if err := registry.Attach(failoverRoute(newServer, 8)); err != nil {
		t.Fatal(err)
	}
	if registry.Holds(oldServer) || !registry.Holds(newServer) {
		t.Fatalf("after the handover: old holds=%v new holds=%v, want false true", registry.Holds(oldServer), registry.Holds(newServer))
	}
}
