package runtime

import (
	"context"
	"fmt"
	"io"
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
	failure error // what the probe answers before the machine serves; nil: carrier-pending
}

func (c *lateServingCarrier) ProbeRoutes(_ context.Context, rules []route.RouteRule) error {
	if len(rules) == 0 {
		return nil
	}
	probe := c.probes.Add(1)
	if !c.serving.Load() {
		if c.failure != nil && probe > 1 { // the start-up probe finds it pending; a non-pending error would fail Start
			return c.failure
		}
		return fmt.Errorf("%w: the machine does not serve the route yet", edgehttp.ErrDataCarrierRouteUnavailable)
	}
	return nil
}

func startProbeRetryWorker(t *testing.T, carrier *lateServingCarrier, wake, tunnelWake chan struct{}, interval time.Duration) (*RouteWorker, control.RouteAssignment, *route.Registry) {
	t.Helper()
	publicKey, thumbprint := durableWorkerIdentity(t)
	assignment := durableWorkerAssignment(publicKey, thumbprint, "route_late", "assignment_late", route.TunnelHTTPSWSS)
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	registry := route.NewRegistry("", "")
	state := node.New("edge_1")
	state.MarkReady()
	worker := &RouteWorker{
		Registry: registry, Observer: &appendRouteObserver{}, State: state, Carrier: carrier, Wake: wake, TunnelWake: tunnelWake,
		Source: &workerSnapshotSource{snapshot: control.RouteSnapshot{Routes: []control.RouteAssignment{assignment}, Complete: true, Canonical: true}},
		NodeID: "edge_1", ProcessEpoch: "edge_epoch_1", DurableAdmissions: durable, Interval: interval, DrainTimeout: time.Second,
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Shutdown(ctx)
		_ = durable.Close()
		_ = registry.Close()
	})
	return worker, assignment, registry
}

// The wake for a new tunnel reaches the edge before the machine serves the route: the edge probes it again within a
// second instead of at its next control tick (prod `pb tunnel create --wait` 7-16 s, both edges waiting a tick).
func TestRouteWorkerReprobesARouteItsMachineServesJustAfterTheTunnelWake(t *testing.T) {
	assertReprobedAfterTunnelWake(t, &lateServingCarrier{})
}

// The wake's probe can also fail on a carrier stream cut (prod the test host edge, 00:21:56Z: route_probe end_of_stream);
// that route was staged only on the next 5 s tick (`pb tunnel create --wait` 5.9 s instead of 1.5 s).
func TestRouteWorkerReprobesARouteWhoseWakeProbeHitAStreamCut(t *testing.T) {
	assertReprobedAfterTunnelWake(t, &lateServingCarrier{failure: io.ErrUnexpectedEOF})
}

func assertReprobedAfterTunnelWake(t *testing.T, carrier *lateServingCarrier) {
	t.Helper()
	tunnelWake := make(chan struct{}, 1)
	_, assignment, registry := startProbeRetryWorker(t, carrier, nil, tunnelWake, 5*time.Second)
	before := carrier.probes.Load()
	tunnelWake <- struct{}{}
	for deadline := time.Now().Add(time.Second); carrier.probes.Load() == before; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the tunnel wake did not probe the route")
		}
	}
	carrier.serving.Store(true) // the machine starts serving right after the edge's probe
	served := time.Now()
	for {
		if _, err := registry.Match(assignment.PublicHost, "/"); err == nil {
			break
		}
		if time.Since(served) > 2*time.Second {
			t.Fatalf("route not served within 2 s of its machine serving it (interval 5 s, %d probes)", carrier.probes.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	settled := carrier.probes.Load()
	time.Sleep(time.Second)
	if extra := carrier.probes.Load() - settled; extra != 0 {
		t.Fatalf("%d probes after the route was served; retries must stop", extra)
	}
}

// Edge .23 gate (e6f0122a4): after a restart every route is carrier-pending and wakes keep coming; re-arming the
// 250 ms re-probe on each of them reconciled 20-40 times in 10 s and timed out the edge's observations. A pending
// carrier with a TunnelWake every second for 10 s (prod 5 s interval) reconciles at most 8 times.
func TestRouteWorkerDoesNotBurstWhileTunnelWakesFlapOverAPendingCarrier(t *testing.T) {
	carrier := &lateServingCarrier{}
	tunnelWake := make(chan struct{}, 1)
	worker, _, _ := startProbeRetryWorker(t, carrier, nil, tunnelWake, 5*time.Second)
	got := flapWakes(worker, tunnelWake)
	if got > 8 {
		t.Fatalf("%d reconciles in 10 s of tunnel wakes over a pending carrier; want <= 8", got)
	}
	if got < 2 {
		t.Fatalf("%d reconciles: the tunnel wakes were not served", got)
	}
}

// Carrier connects (Wake) never start a re-probe series: each reconciles once, as before the series existed.
func TestCarrierWakesOverAPendingCarrierDoNotStartReprobes(t *testing.T) {
	carrier := &lateServingCarrier{}
	wake := make(chan struct{}, 1)
	worker, _, _ := startProbeRetryWorker(t, carrier, wake, nil, time.Minute)
	if got := flapWakes(worker, wake); got > 10 {
		t.Fatalf("%d reconciles for 10 carrier wakes over a pending carrier; want <= 10 (no re-probes)", got)
	}
}

// flapWakes sends a wake every second for 10 s and returns the reconciles run meanwhile.
func flapWakes(worker *RouteWorker, wake chan struct{}) int64 {
	start := worker.reconciles.Load()
	for second := 0; second < 10; second++ {
		select {
		case wake <- struct{}{}:
		default:
		}
		time.Sleep(time.Second)
	}
	return worker.reconciles.Load() - start
}
