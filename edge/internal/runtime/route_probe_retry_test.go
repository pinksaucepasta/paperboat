package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/edge/internal/node"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

// lateServingCarrier answers carrier-pending until the machine serves the route.
type lateServingCarrier struct {
	workerCarrierProbe
	serving atomic.Bool
	probes  atomic.Int32
}

func (c *lateServingCarrier) ProbeRoutes(_ context.Context, rules []route.RouteRule) error {
	if len(rules) == 0 {
		return nil
	}
	c.probes.Add(1)
	if !c.serving.Load() {
		return fmt.Errorf("%w: the machine does not serve the route yet", edgehttp.ErrDataCarrierRouteUnavailable)
	}
	return nil
}

// The wake for a new tunnel reaches the edge before the machine serves the route: the edge probes it again within a
// second instead of at its next control tick (prod `pb tunnel create --wait` 7-16 s, both edges waiting a tick).
func TestRouteWorkerReprobesARouteItsMachineServesJustAfterTheWake(t *testing.T) {
	publicKey, thumbprint := durableWorkerIdentity(t)
	assignment := durableWorkerAssignment(publicKey, thumbprint, "route_late", "assignment_late", route.TunnelHTTPSWSS)
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	registry := route.NewRegistry("", "")
	t.Cleanup(func() { _ = durable.Close(); _ = registry.Close() })
	state := node.New("edge_1")
	state.MarkReady()
	carrier := &lateServingCarrier{}
	wake := make(chan struct{}, 1)
	worker := &RouteWorker{
		Registry: registry, Observer: &appendRouteObserver{}, State: state, Carrier: carrier, TunnelWake: wake,
		Source: &workerSnapshotSource{snapshot: control.RouteSnapshot{Routes: []control.RouteAssignment{assignment}, Complete: true, Canonical: true}},
		NodeID: "edge_1", ProcessEpoch: "edge_epoch_1", DurableAdmissions: durable, Interval: time.Minute, DrainTimeout: time.Second,
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Shutdown(ctx)
	}()
	before := carrier.probes.Load()
	wake <- struct{}{}
	for deadline := time.Now().Add(time.Second); carrier.probes.Load() == before; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the wake did not probe the route")
		}
	}
	carrier.serving.Store(true) // the machine starts serving right after the edge's probe
	served := time.Now()
	for {
		if _, err := registry.Match(assignment.PublicHost, "/"); err == nil {
			break
		}
		if time.Since(served) > 2*time.Second {
			t.Fatalf("route not served within 2 s of its machine serving it (%d probes)", carrier.probes.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	settled := carrier.probes.Load()
	time.Sleep(time.Second)
	if extra := carrier.probes.Load() - settled; extra != 0 {
		t.Fatalf("%d probes after the route was served; retries must stop", extra)
	}
}
