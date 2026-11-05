package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/node"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
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
