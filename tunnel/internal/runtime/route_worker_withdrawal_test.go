package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/node"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

func TestRouteWorkerWithdrawsPausedGroupWhileOtherGroupFails(t *testing.T) {
	testRouteWorkerWithdrawsGroup(t, true)
}

func TestRouteWorkerDetachesUnpublishedGroupWhileOtherGroupFails(t *testing.T) {
	testRouteWorkerWithdrawsGroup(t, false)
}

func testRouteWorkerWithdrawsGroup(t *testing.T, published bool) {
	public, thumb := durableWorkerIdentity(t)
	good := durableWorkerAssignment(public, thumb, "route_good", "assignment_good", route.TunnelHTTPSWSS)
	good.PublicHost, good.MatchHostname = "smart-cloudy-voyage-6024.tunnels.example.test", "smart-cloudy-voyage-6024.tunnels.example.test"
	good.MatchType = route.MatchManagedExact
	bad := durableWorkerAssignment(public, thumb, "route_bad", "assignment_bad", route.TunnelHTTPSWSS)
	bad.TunnelID, bad.ConnectorID, bad.ConnectorSessionID = "tunnel_bad", "connector_bad", "session_bad"
	bad.PublicHost, bad.MatchHostname = "bad.example.test", "bad.example.test"
	registry := route.NewRegistry("", "")
	admissions, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	source := &workerSnapshotSource{snapshot: control.RouteSnapshot{Routes: []control.RouteAssignment{good, bad}, Complete: true, Canonical: true}}
	observer := &appendRouteObserver{}
	unavailable := errors.New("other group's origin unavailable")
	worker := &RouteWorker{Registry: registry, Source: source, Observer: observer, State: node.New("edge_1"), NodeID: "edge_1", ProcessEpoch: "edge_epoch_1", Carrier: &workerCarrierProbe{}, DurableAdmissions: admissions, DrainTimeout: time.Second, Ready: func(_ context.Context, rules []route.RouteRule) error {
		for _, r := range rules {
			if r.TunnelID == bad.TunnelID {
				return unavailable
			}
		}
		return nil
	}}
	if published {
		if err := worker.reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Match(good.PublicHost, "/"); err != nil {
			t.Fatal(err)
		}
	}
	good.State = "draining"
	source.snapshot.Routes = []control.RouteAssignment{good, bad}
	if err := worker.reconcile(context.Background()); err != nil && !errors.Is(err, unavailable) {
		t.Fatal(err)
	}
	if _, err := registry.Match(good.PublicHost, "/"); !errors.Is(err, route.ErrNoMatch) {
		t.Fatalf("paused route still selectable: %v", err)
	}
	detached := false
	for _, o := range observer.observations {
		if o.AssignmentID == good.AssignmentID && o.ObservedState == "detached" {
			detached = true
		}
	}
	if !detached {
		t.Fatal("paused healthy group never acknowledged detached")
	}
	pending := admissions.Snapshot()
	if len(pending) != 1 || pending[0].AssignmentID != bad.AssignmentID {
		t.Fatalf("failed group retry admission lost: %v", pending)
	}
	if err := worker.reconcile(context.Background()); !errors.Is(err, unavailable) {
		t.Fatalf("failed group no longer retries: %v", err)
	}
}
