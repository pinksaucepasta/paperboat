package runtimeattachment

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
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
	stream := edgeOpen(t, edge, edgeRequest("route-test", "goaway", sharedprotocol.Goaway))
	if err := sharedprotocol.WriteGoawayNotice(stream, sharedprotocol.GoawayEdgeDraining, time.Now().Add(30*time.Second)); err != nil {
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
		if open.Kind == sharedprotocol.Goaway {
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
