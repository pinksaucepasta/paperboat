package runtime

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
)

// pbvm4 feat9: at pb update the old worker's preview carrier closed while the edge still routed
// the preview to it, and 21 preview GETs failed. The handover is make-before-break: the old
// worker sends a goaway and keeps serving, the server names the new worker's carrier, the edge
// keeps the old one serving until the new one attaches, then the old one closes. A GET every
// 50 ms through all of it never fails.
func TestEndpointRouteMovesToTheNextWorkerWithoutAFailedRequest(t *testing.T) {
	oldRuntime := runtimeAdmission(t)
	oldEndpoint := endpointAdmission(oldRuntime, "public")
	newRuntime := oldRuntime
	newRuntime.Binding.SessionID, newRuntime.Binding.ProcessGeneration, newRuntime.AttachmentGeneration = "session_2", 2, 2
	newEndpoint := endpointAdmission(newRuntime, "public")
	newEndpoint.AttachmentGeneration = oldEndpoint.AttachmentGeneration + 1
	epoch, node := oldRuntime.Binding.EdgeProcessEpoch, oldRuntime.Binding.EdgeNodeID

	source := &runtimeSourceFake{admissions: []runtimewire.Admission{oldRuntime, oldEndpoint}}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := endpointRegistry(t, epoch)
	w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpoints, Interval: 20 * time.Millisecond, EndpointInterval: 20 * time.Millisecond, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Shutdown(context.Background()) }()
	waitRuntime(t, func() bool { return len(expected.Snapshot()) == 2 })

	identity := func(a runtimewire.Admission) datacarrier.Identity {
		ea, err := control.ExpectedRuntimeAdmission(a, node, epoch, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return ea.Identity
	}
	oldServer, oldClient := handoverCarrier(t, identity(oldEndpoint), expected, "old")
	go func() { _ = w.Handle(ctx, oldServer) }()
	host := oldEndpoint.Hostname
	waitRuntime(t, func() bool { r, ok := endpoints.Lookup(host); return ok && r.Server == oldServer })

	transport, err := edgehttp.NewDataCarrierPreviewTransport(edgehttp.DataCarrierPreviewTransportConfig{Registry: endpoints, StreamOpenTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var failures []string
	served := map[string]int{}
	stop := make(chan struct{})
	probing := make(chan struct{})
	go func() {
		defer close(probing)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			request := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
			request.Host = host
			response, err := transport.RoundTrip(request)
			var body []byte
			if err == nil {
				body, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			mu.Lock()
			if err != nil || response.StatusCode != http.StatusOK {
				failures = append(failures, time.Now().Format("15:04:05.000")+" "+errText(err))
			} else {
				served[string(body)]++
			}
			mu.Unlock()
		}
	}()
	time.Sleep(200 * time.Millisecond)

	// The old worker goes away: goaway first, then the server names the new worker's carrier.
	sendHostGoaway(t, oldClient, identity(oldEndpoint), oldEndpoint.Binding.RouteID)
	waitRuntime(t, oldServer.DrainingNow)
	source.mu.Lock()
	source.admissions = []runtimewire.Admission{newRuntime, newEndpoint}
	source.mu.Unlock()
	time.Sleep(300 * time.Millisecond) // several pulls: the old carrier still serves the route
	if r, ok := endpoints.Lookup(host); !ok || r.Server != oldServer {
		t.Fatal("the route left the old carrier before the new one attached")
	}

	newServer, _ := handoverCarrier(t, identity(newEndpoint), expected, "new")
	go func() { _ = w.Handle(ctx, newServer) }()
	waitRuntime(t, func() bool { r, ok := endpoints.Lookup(host); return ok && r.Server == newServer })
	time.Sleep(200 * time.Millisecond)
	_ = oldClient.Close()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-probing

	mu.Lock()
	defer mu.Unlock()
	if len(failures) != 0 {
		t.Fatalf("%d requests failed during the handover: %v", len(failures), failures)
	}
	if served["old"] == 0 || served["new"] == 0 {
		t.Fatalf("served %v, want requests on both workers", served)
	}
}

func errText(err error) string {
	if err == nil {
		return "status not 200"
	}
	return err.Error()
}

// handoverCarrier is one worker's carrier to the edge, authorized by the edge's expected
// admissions, whose host answers every request with name.
func handoverCarrier(t *testing.T, identity datacarrier.Identity, expected *datacarrier.ExpectedAdmissionRegistry, name string) (*datacarrier.Server, *datacarrier.Client) {
	t.Helper()
	local, remote := net.Pipe()
	config := datacarrier.DefaultConfig()
	config.Identity = identity
	config.Authorize = expected
	server, err := datacarrier.NewServer(context.Background(), remote, config)
	if err != nil {
		t.Fatal(err)
	}
	client, err := datacarrier.NewClient(context.Background(), local, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() {
		for {
			stream, _, err := client.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				_ = datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, name)
				}), 4096, time.Second)
			}()
		}
	}()
	return server, client
}

func sendHostGoaway(t *testing.T, client *datacarrier.Client, id datacarrier.Identity, routeID string) {
	t.Helper()
	stream, err := client.OpenStream(context.Background(), connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: id.AccountID, TunnelID: id.TunnelID, ConnectorID: id.ConnectorID, SessionID: id.SessionID, ProcessGeneration: id.ProcessGeneration, Generation: id.Generation, RouteID: routeID, RequestID: "request_goaway", Kind: connectorprotocol.Goaway})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := connectorprotocol.WriteGoawayNotice(stream, connectorprotocol.GoawayHostDraining, time.Now().Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
}
