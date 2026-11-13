package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/edge/internal/node"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

// After an edge restart the machines' carriers reconnect later. Their routes
// are pending: control stays available and each route serves once its
// carrier is back.
func TestRouteWorkerEdgeRestartKeepsControlAvailableUntilCarriersReconnect(t *testing.T) {
	publicKey, thumbprint := durableWorkerIdentity(t)
	a := durableWorkerAssignmentForTunnel(publicKey, thumbprint, "a")
	b := durableWorkerAssignmentForTunnel(publicKey, thumbprint, "b")
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{
		NodeID: "edge_1", ProcessEpoch: "edge_epoch_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	missing := func(tunnel string) error {
		return fmt.Errorf("%w: exact carrier missing for %s", edgehttp.ErrDataCarrierRouteUnavailable, tunnel)
	}
	carrier := &workerCarrierProbe{probeErrors: map[string]error{a.TunnelID: missing(a.TunnelID), b.TunnelID: missing(b.TunnelID)}}
	registry := route.NewRegistry("", "")
	state := node.New("edge_1")
	state.MarkReady()
	unavailable := &route.UnavailableHosts{}
	worker := &RouteWorker{
		Registry: registry, Observer: &appendRouteObserver{}, State: state, Carrier: carrier,
		Source: &workerSnapshotSource{snapshot: control.RouteSnapshot{
			Routes: []control.RouteAssignment{a, b}, Complete: true, Canonical: true,
		}},
		NodeID: "edge_1", ProcessEpoch: "edge_epoch_1", DurableAdmissions: durable, Unavailable: unavailable,
		Pulse: make(chan time.Time), DrainTimeout: time.Second,
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("start with carriers not reconnected yet: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Shutdown(ctx)
	}()
	assertServing := func(step string, want map[string]bool) {
		t.Helper()
		if !state.Snapshot().Ready {
			t.Fatalf("%s: control marked unavailable", step)
		}
		for _, assignment := range []control.RouteAssignment{a, b} {
			_, matchErr := registry.Match(assignment.PublicHost, "/")
			if serving := matchErr == nil; serving != want[assignment.PublicHost] {
				t.Fatalf("%s: %s serving=%t (%v), want %t", step, assignment.PublicHost, serving, matchErr, want[assignment.PublicHost])
			}
			if unavailable.Has(assignment.PublicHost) == want[assignment.PublicHost] {
				t.Fatalf("%s: %s unavailable page=%t", step, assignment.PublicHost, unavailable.Has(assignment.PublicHost))
			}
		}
	}
	assertServing("restart", map[string]bool{})
	if err := worker.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile while carriers pending: %v", err)
	}

	delete(carrier.probeErrors, b.TunnelID)
	if err := worker.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after carrier b reconnects: %v", err)
	}
	assertServing("b back", map[string]bool{b.PublicHost: true})
	if err := worker.reconcile(context.Background()); err != nil {
		t.Fatalf("steady reconcile with a still pending: %v", err)
	}

	delete(carrier.probeErrors, a.TunnelID)
	if err := worker.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after carrier a reconnects: %v", err)
	}
	assertServing("both back", map[string]bool{a.PublicHost: true, b.PublicHost: true})
}

func TestProbeFailureKeepsNonCarrierFailures(t *testing.T) {
	pending := fmt.Errorf("%w: exact carrier missing", edgehttp.ErrDataCarrierRouteUnavailable)
	if err := probeFailure([]error{pending, pending}); err != nil {
		t.Fatalf("carrier-pending failures = %v, want nil", err)
	}
	if err := probeFailure([]error{pending, route.ErrGenerationNotReady}); !errors.Is(err, route.ErrGenerationNotReady) || errors.Is(err, edgehttp.ErrDataCarrierRouteUnavailable) {
		t.Fatalf("mixed failures = %v, want only the generation failure", err)
	}
}

// A carrier that connects between control ticks publishes its pending route at
// once: the worker is woken by the carrier registry, not by its 1 min interval.
func TestRouteWorkerPublishesPendingRouteWhenItsCarrierConnects(t *testing.T) {
	publicKey, thumbprint := durableWorkerIdentity(t)
	assignment := durableWorkerAssignment(publicKey, thumbprint, "route_wake", "assignment_wake", route.TunnelHTTPSWSS)
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{
		NodeID: "edge_1", ProcessEpoch: "edge_epoch_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	carriers, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 8})
	if err != nil {
		t.Fatal(err)
	}
	registry := route.NewRegistry("", "")
	t.Cleanup(func() { _ = carriers.Close(); _ = durable.Close(); _ = registry.Close() })
	state := node.New("edge_1")
	state.MarkReady()
	worker := &RouteWorker{
		Registry: registry, Observer: &appendRouteObserver{}, State: state, Carrier: carriers, Wake: carriers.Connected(),
		Source: &workerSnapshotSource{snapshot: control.RouteSnapshot{
			Routes: []control.RouteAssignment{assignment}, Complete: true, Canonical: true,
		}},
		NodeID: "edge_1", ProcessEpoch: "edge_epoch_1", DurableAdmissions: durable,
		Interval: time.Minute, DrainTimeout: time.Second,
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("start with the carrier not connected yet: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Shutdown(ctx)
	}()
	if _, err := registry.Match(assignment.PublicHost, "/"); err == nil {
		t.Fatal("route served before its carrier connected")
	}

	pair, err := newTRK35IntegrationCarrierPair(assignment, durable)
	if err != nil {
		t.Fatal(err)
	}
	defer pair.Close()
	connected := time.Now()
	if err := carriers.AttachReplica(pair.server, publicKey, thumbprint, trk35IntegrationReplicaState(assignment)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := registry.Match(assignment.PublicHost, "/"); err == nil {
			break
		}
		if time.Since(connected) > time.Second {
			t.Fatal("route not served within 1 s of its carrier connecting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("route served %s after its carrier connected", time.Since(connected).Round(time.Millisecond))
	if !state.Snapshot().Ready {
		t.Fatal("control marked unavailable")
	}
}

// A burst of carrier connects leaves one pending wake: at most one extra
// reconcile follows the one in flight.
func TestCarrierConnectBurstCoalescesIntoOneWake(t *testing.T) {
	publicKey, thumbprint := durableWorkerIdentity(t)
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	carriers, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = carriers.Close(); _ = durable.Close() })
	for _, suffix := range []string{"a", "b", "c"} {
		assignment := durableWorkerAssignmentForTunnel(publicKey, thumbprint, suffix)
		pair, err := newTRK35IntegrationCarrierPair(assignment, durable)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pair.Close() })
		if err := carriers.AttachReplica(pair.server, publicKey, thumbprint, trk35IntegrationReplicaState(assignment)); err != nil {
			t.Fatal(err)
		}
	}
	if pending := len(carriers.Connected()); pending != 1 {
		t.Fatalf("pending wakes after 3 connects = %d, want 1", pending)
	}
}
