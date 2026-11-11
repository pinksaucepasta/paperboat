package runtimeattachment

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func edgeRequest(route, id, kind string) connector.StreamOpen {
	return connector.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: "account-test", TunnelID: "machine-test", ConnectorID: "machine-test", SessionID: "session-test", ProcessGeneration: 1, Generation: 1, RouteID: route, RequestID: id, Kind: kind}
}

// edgeOpen opens a stream the way the edge does: a raw stream, then its preface.
func edgeOpen(t *testing.T, edge *connector.DataCarrier, open connector.StreamOpen) io.ReadWriteCloser {
	t.Helper()
	stream, err := edge.OpenStream(t.Context())
	if err == nil {
		err = connectorprotocol.WriteStreamOpen(stream, open)
	}
	if err != nil {
		t.Fatalf("edge request %s: %v", open.RequestID, err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// sendEdgeGoaway is the edge's drain notice (tunnel datacarrier): one goaway stream on the runtime
// route, then it waits at most 1 s for the host to close it.
func sendEdgeGoaway(t *testing.T, edge *connector.DataCarrier) {
	t.Helper()
	sendEdgeGoawayOn(t, edge, "route-test")
}

// sendEdgeGoawayOn names route, one of the routes attached on the carrier.
func sendEdgeGoawayOn(t *testing.T, edge *connector.DataCarrier, route string) {
	t.Helper()
	stream := edgeOpen(t, edge, edgeRequest(route, "goaway", connectorprotocol.Goaway))
	if err := connectorprotocol.WriteGoawayNotice(stream, connectorprotocol.GoawayEdgeDraining, time.Now().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { _, _ = stream.Read(make([]byte, 1)); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("host kept the goaway stream open")
	}
}

func nextEdge(t *testing.T, h *runtimeCarrierHarness, within time.Duration) edgeDial {
	t.Helper()
	select {
	case dial := <-h.edges:
		return dial
	case <-time.After(within):
		t.Fatal("host dialed no carrier")
		return edgeDial{}
	}
}

// hubFor waits for the live carrier hub to edgeID.
func hubFor(t *testing.T, service *Service, edgeID string) *preview.DataCarrierPreviewHub {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		hubs, _ := service.Hubs()
		for _, hub := range hubs {
			if node, _ := hub.Edge(); node == edgeID {
				return hub
			}
		}
	}
	t.Fatalf("no carrier to %s came into service", edgeID)
	return nil
}

// served: a request the edge opens on route reaches the host.
func served(t *testing.T, edge *connector.DataCarrier, registration *preview.DataCarrierPreviewRegistration, id string) {
	t.Helper()
	edgeOpen(t, edge, edgeRequest("route-held", id, "http"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	stream, open, err := registration.Accept(ctx)
	if err != nil || open.RequestID != id {
		t.Fatalf("request %s failed: %+v, %v", id, open, err)
	}
	_ = stream.Close()
}

func exchange(t *testing.T, from io.Writer, to io.Reader, text string) {
	t.Helper()
	go func() { _, _ = io.WriteString(from, text) }()
	got := make([]byte, len(text))
	if _, err := io.ReadFull(to, got); err != nil || string(got) != text {
		t.Fatalf("held stream across the edge switch: %q, %v", got, err)
	}
}

// Edge deploy (make-before-break): the edge process says it is draining while the host holds an open
// stream on its carrier (a terminal or preview). The host dials the replacement carrier to the same
// edge at once and stays attached on the same hub: the held stream keeps working, every request
// the edge opens before and after the switch (on either carrier) arrives, the old process closing its carrier takes
// nothing offline, and host-opened streams use the replacement.
func TestEdgeGoawayMovesTheRuntimeCarrierWithoutFailingRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{expiry: time.Now().Add(time.Hour), status: func() int { return http.StatusOK }})
	old := nextEdge(t, h, 5*time.Second)
	hub := hubFor(t, h.service, "edge-test")
	registration, err := hub.Register("route-held")
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close()
	held := edgeOpen(t, old.edge, edgeRequest("route-held", "held", "http"))
	hostHeld, _, err := registration.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hostHeld.Close()
	served(t, old.edge, registration, "before")

	sendEdgeGoaway(t, old.edge)
	replacement := nextEdge(t, h, 2*time.Second)
	if replacement.edgeID != "edge-test" {
		t.Fatalf("replacement dialed %s, want the same edge", replacement.edgeID)
	}
	for index := range 5 {
		served(t, replacement.edge, registration, fmt.Sprintf("after-%d", index))
	}
	// A connection the old process already accepted may still bring it a request during the drain.
	served(t, old.edge, registration, "late-on-draining")
	exchange(t, held, hostHeld, "edge-to-host")
	exchange(t, hostHeld, held, "host-to-edge")

	// The old edge process exits once its streams finished: an expected close.
	_ = hostHeld.Close()
	_ = old.edge.Close()
	time.Sleep(1500 * time.Millisecond) // a few attachment rounds
	if hubs, _ := h.service.Hubs(); len(hubs) != 1 || hubs[0] != hub {
		t.Fatalf("host carriers changed when the drained carrier closed: %d hubs", len(hubs))
	}
	served(t, replacement.edge, registration, "after-close")

	// Host-opened streams (here the daemon's own goaway at shutdown) go to the replacement.
	go func() { _ = h.service.Shutdown(context.Background()) }()
	for {
		stream, open, err := replacement.edge.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("host stream did not reach the replacement: %v", err)
		}
		_ = stream.Close()
		if open.Kind == connectorprotocol.Goaway {
			return
		}
	}
}

// No replacement can be dialed within the drain: the held stream keeps working on the draining
// carrier, which stays attached, while the host makes the other edge active, where requests go
// through.
func TestEdgeGoawayWithoutReplacementFallsBackToTheOtherEdge(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{
		expiry: time.Now().Add(time.Hour), status: func() int { return http.StatusOK }, confirmActive: true,
		failDial: func(edgeID string, dial int) bool { return edgeID == "edge-test" && dial > 1 },
		tweak: func(a *admission) {
			backup := a.primary()
			backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch = "edge-backup", "epoch-backup-890123456789012345678"
			a.Backup = &backup
		},
	})
	edges := map[string]*connector.DataCarrier{}
	for len(edges) < 2 {
		dial := nextEdge(t, h, 5*time.Second)
		edges[dial.edgeID] = dial.edge
	}
	primaryHub, backupHub := hubFor(t, h.service, "edge-test"), hubFor(t, h.service, "edge-backup")
	primaryRoute, err := primaryHub.Register("route-held")
	if err != nil {
		t.Fatal(err)
	}
	defer primaryRoute.Close()
	backupRoute, err := backupHub.Register("route-held")
	if err != nil {
		t.Fatal(err)
	}
	defer backupRoute.Close()
	held := edgeOpen(t, edges["edge-test"], edgeRequest("route-held", "held", "http"))
	hostHeld, _, err := primaryRoute.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hostHeld.Close()

	sendEdgeGoaway(t, edges["edge-test"])
	select {
	case active := <-h.actives:
		if active != "edge-backup" {
			t.Fatalf("host made %s active, want the other edge", active)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not move the active edge off the draining one")
	}
	served(t, edges["edge-backup"], backupRoute, "on-backup")
	exchange(t, held, hostHeld, "still-open")
	if hubFor(t, h.service, "edge-test") != primaryHub {
		t.Fatal("the draining carrier was cut")
	}
}

// Revocation during the drain still ends the held stream at once.
func TestRevocationDuringEdgeDrainEndsTheHeldStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var revoked atomic.Bool
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{expiry: time.Now().Add(2 * time.Second), status: func() int {
		if revoked.Load() {
			return http.StatusForbidden
		}
		return http.StatusOK
	}})
	old := nextEdge(t, h, 5*time.Second)
	registration, err := hubFor(t, h.service, "edge-test").Register("route-held")
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close()
	edgeOpen(t, old.edge, edgeRequest("route-held", "held", "http"))
	hostHeld, _, err := registration.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hostHeld.Close()
	sendEdgeGoaway(t, old.edge)
	replacement := nextEdge(t, h, 2*time.Second)

	revoked.Store(true)
	for _, edge := range []*connector.DataCarrier{old.edge, replacement.edge} {
		select {
		case <-edge.Done():
		case <-ctx.Done():
			t.Fatal("revocation during the drain left a carrier open")
		}
	}
	_ = hostHeld.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := hostHeld.Read(make([]byte, 1)); err == nil {
		t.Fatal("held stream read data after revocation")
	}
}

// Edge deploy with an engine tunnel on the runtime carrier: the edge names the tunnel's route on
// its goaway (any route attached on the carrier). The hub hands the notice to the pool, never to
// the tunnel's origin forwarder, which would drop it as an unknown kind and leave the host to
// redial only after the old edge closed the carrier. A held tunnel response keeps streaming on the
// draining carrier, and the next tunnel request goes through on the replacement with no gap.
func TestEdgeGoawayKeepsAHeldTunnelConnectionAndServesTheNextOnTheReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hold" {
			_, _ = io.WriteString(w, "engine-ok")
			return
		}
		_, _ = io.WriteString(w, "first|")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second")
	}))
	defer origin.Close()
	defer close(release)
	const tunnelID, routeID = "tunnel_engine", "route_engine"
	payload := fmt.Sprintf(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":%q,"generation":1,"name":"engine","desired_state":"active","access_mode":"public","stable_endpoint":"https://brave-blue-beacon-0421.tunnel.example.test","stable_endpoint_id":"endpoint_123e4567-e89b-42d3-a456-426614174000","expires_at":null,"routes":[{"id":%q,"name":"default","protocol":"http","match_type":"catch_all","path_prefix":null,"origin_scheme":"http","origin_address":%q,"preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"mtls_credential_reference":null,"connect_timeout_ms":10000,"idle_timeout_ms":90000,"max_concurrent_streams":16,"desired_state":"active"}]}`, tunnelID, routeID, strings.TrimPrefix(origin.URL, "http://"))
	snapshot, err := hoststate.NewConfigSnapshot(tunnelID, 1, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	tunnels := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != runtimewire.HostTunnelsPath {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
		}
		list := []runtimewire.EngineTunnel{{TunnelID: tunnelID, ConfigGeneration: 1, ConfigContentHash: snapshot.ContentHash, Snapshot: json.RawMessage(snapshot.Payload)}}
		body, _ := json.Marshal(map[string]any{"data": map[string]any{"tunnels": list}})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{expiry: time.Now().Add(time.Hour), status: func() int { return http.StatusOK }, other: tunnels,
		tweak: func(a *admission) { a.EndpointsServed, a.TunnelEndpointsServed = true, true }})
	old := nextEdge(t, h, 5*time.Second)
	engine := &EngineTunnels{Service: h.service, Forwarder: tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{}}, Prober: tunnelmanager.NetworkOriginProber{}}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	waitFor(t, ctx, func() bool { return engine.Served(tunnelID) })
	hub := hubFor(t, h.service, "edge-test")
	// tunnelGet sends one tunnel request from edge and returns its response.
	tunnelGet := func(edge *connector.DataCarrier, path string) *bufio.Reader {
		t.Helper()
		stream := edgeOpen(t, edge, edgeRequest(routeID, "request-"+strings.TrimPrefix(path, "/"), "https"))
		if _, err := io.WriteString(stream, "GET "+path+" HTTP/1.1\r\nHost: brave-blue-beacon-0421.tunnel.example.test\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		return bufio.NewReader(stream)
	}
	body := func(reader *bufio.Reader) string {
		t.Helper()
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("tunnel request failed: %v", err)
		}
		defer response.Body.Close()
		got, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("tunnel response cut: %q, %v", got, err)
		}
		return string(got)
	}
	held := tunnelGet(old.edge, "/hold")
	response, err := http.ReadResponse(held, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("first|"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}

	sendEdgeGoawayOn(t, old.edge, routeID)
	replacement := nextEdge(t, h, 2*time.Second)
	if got := body(tunnelGet(replacement.edge, "/next")); got != "engine-ok" {
		t.Fatalf("tunnel request on the replacement: %q", got)
	}
	release <- struct{}{}
	if rest, err := io.ReadAll(response.Body); err != nil || string(rest) != "second" {
		t.Fatalf("held tunnel connection across the edge drain: %q, %v", rest, err)
	}
	_ = old.edge.Close()
	time.Sleep(1500 * time.Millisecond) // a few attachment rounds and engine reconciles
	if hubs, _ := h.service.Hubs(); len(hubs) != 1 || hubs[0] != hub {
		t.Fatalf("host carriers changed when the drained carrier closed: %d hubs", len(hubs))
	}
	if got := body(tunnelGet(replacement.edge, "/after")); got != "engine-ok" {
		t.Fatalf("tunnel request after the old carrier closed: %q", got)
	}
}
