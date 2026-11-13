package edgehttp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/control/controltest"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A server rollout's proxy cuts the edge's browser-terminal control channel 10 minutes after its
// reload (Caddy stream_close_delay). The held browser terminal keeps its output flowing through the
// cut and the resume: every record arrives, in order, with no gap above 1 s.
func TestHeldBrowserTerminalCrossesAControlPlaneCut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plane := controltest.Start(t)
	controlHTTP, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: plane.URL, Credential: "edge-control-credential-0123456789abcdef", Timeout: 3 * time.Second, Client: plane.Client})
	if err != nil {
		t.Fatal(err)
	}
	identity := testEdgePreviewIdentity(1, 1)
	carrierServer, carrierClient := testEdgePreviewCarrierPair(t, identity)
	defer carrierServer.Close()
	defer carrierClient.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	route := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: carrierServer, ExpiresAt: time.Now().Add(time.Minute)}
	if err := registry.Attach(route); err != nil {
		t.Fatal(err)
	}
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	if err := hub.ActivateRoute(route.RouteID, BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan struct{})
	defer close(hostDone)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{browserTerminalSubprotocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		select {
		case <-hostDone:
		case <-r.Context().Done():
		}
	}))
	defer host.Close()
	hostURL, err := url.Parse(host.URL)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := New(Config{
		PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry),
		RuntimeCarrierTransport: browserTerminalTestRoundTripper{target: hostURL}, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: "edge.example.test",
		BrowserTerminal: &control.BrowserTerminalClient{HTTP: controlHTTP, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"},
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(policy)
	defer edge.Close()
	header := make(http.Header)
	header.Set("Origin", "https://dashboard.example.test")
	browser, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+"/v1/browser-terminal/"+route.Hostname, &websocket.DialOptions{
		Host: "edge.example.test", HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
		Subprotocols: []string{browserTerminalSubprotocol, browserTerminalTicketPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.CloseNow()
	publisher := registerWhenSubscribed(t, ctx, hub, BrowserTerminalSessionKey{RouteID: route.RouteID, TerminalSessionID: "term_1", ProcessGeneration: 1}, identity)
	const records = 40
	go func() {
		for i := range records {
			if i == 10 {
				plane.Cut()
			}
			if publisher.Publish([]byte(fmt.Sprint("r", i))) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	last := time.Now()
	for i := range records {
		_, payload, err := browser.Read(ctx)
		if err != nil || string(payload) != fmt.Sprint("\x01r", i) {
			t.Fatalf("record %d: %q, %v", i, payload, err)
		}
		if gap := time.Since(last); gap > time.Second {
			t.Fatalf("output stalled %v before record %d", gap, i)
		}
		last = time.Now()
	}
	requests := plane.Requests()
	if len(requests) != 2 || requests[1].Resume != "attach_1" {
		t.Fatalf("control requests %+v, want the admission then its resume", requests)
	}
}
