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

func TestRouteWorkerFencesChangedViewerPolicyWithoutDroppingOtherRoutes(t *testing.T) {
	publicKey, thumbprint := durableWorkerIdentity(t)
	public := durableWorkerAssignment(publicKey, thumbprint, "route_policy", "assignment_policy_public", route.TunnelHTTPSWSS)
	other := public
	other.RouteID, other.AssignmentID = "route_policy_other", "assignment_policy_other"
	other.PublicHost, other.MatchHostname = "other.example.test", "other.example.test"
	restricted := public
	restricted.AssignmentID = "assignment_policy_restricted"
	restricted.AssignmentGeneration = 2
	restricted.AccessMode = "team"
	restricted.ViewerPolicyGeneration = 2
	restricted.State = "staged"

	registry := route.NewRegistry("", "")
	admissions, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	state := node.New("edge_1")
	state.MarkReady()
	source := &workerSnapshotSource{snapshot: control.RouteSnapshot{Routes: []control.RouteAssignment{public, other}, Complete: true, Canonical: true}}
	observer := &appendRouteObserver{}
	carrierError := errors.New("restricted route carrier is not ready")
	var readinessErr error
	worker := &RouteWorker{
		Registry: registry, Source: source, Observer: observer, State: state, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1",
		Carrier: &workerCarrierProbe{}, DurableAdmissions: admissions,
		Ready: func(context.Context, []route.RouteRule) error { return readinessErr },
		Pulse: make(chan time.Time), DrainTimeout: time.Second,
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Shutdown(ctx)
	}()
	publicLease, _, err := registry.Acquire(context.Background(), public.PublicHost, "/active")
	if err != nil {
		t.Fatal(err)
	}
	otherLease, _, err := registry.Acquire(context.Background(), other.PublicHost, "/active")
	if err != nil {
		t.Fatal(err)
	}

	source.snapshot.Routes = []control.RouteAssignment{public, other, restricted}
	readinessErr = carrierError
	if err := worker.reconcile(context.Background()); !errors.Is(err, carrierError) {
		t.Fatalf("restricted readiness error = %v, want %v", err, carrierError)
	}
	select {
	case <-publicLease.Context().Done():
	default:
		t.Fatal("old public route stream was not canceled")
	}
	select {
	case <-otherLease.Context().Done():
		t.Fatal("unrelated route stream was canceled by the policy fence")
	default:
	}
	if _, err := registry.Match(public.PublicHost, "/new"); !errors.Is(err, route.ErrNoMatch) {
		t.Fatalf("old public route still admits requests: %v", err)
	}
	if match, err := registry.Match(other.PublicHost, "/new"); err != nil || match.Rule.RouteID != other.RouteID {
		t.Fatalf("unaffected route unavailable after fence: %+v, %v", match, err)
	}
	seenDetached := false
	for _, observation := range observer.observations {
		if observation.AssignmentID == public.AssignmentID && observation.ObservedState == "detached" {
			seenDetached = observation.AccessMode == "public" && observation.ViewerPolicyGeneration == public.ViewerPolicyGeneration
		}
	}
	if !seenDetached {
		t.Fatalf("old assignment did not receive an exact detached observation: %+v", observer.observations)
	}
	for _, admission := range admissions.Snapshot() {
		if admission.AssignmentID == public.AssignmentID {
			t.Fatalf("old public assignment remained authorized after fence: %+v", admission)
		}
	}

	_ = otherLease.Close()
	readinessErr = nil
	if err := worker.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	match, err := registry.Match(restricted.PublicHost, "/restricted")
	if err != nil || match.Rule.AccessMode != "team" || match.Rule.ViewerPolicyGeneration != 2 {
		t.Fatalf("restricted route projection = %+v, %v", match.Rule, err)
	}
	seenReady := false
	for _, observation := range observer.observations {
		if observation.AssignmentID == restricted.AssignmentID && observation.ObservedState == "ready" {
			seenReady = observation.AccessMode == "team" && observation.ViewerPolicyGeneration == restricted.ViewerPolicyGeneration
		}
	}
	if !seenReady {
		t.Fatalf("restricted assignment did not receive exact ready observation: %+v", observer.observations)
	}
	_ = publicLease.Close()
}
