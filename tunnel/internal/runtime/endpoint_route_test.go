package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
)

func endpointRegistry(t *testing.T, epoch string) *edgehttp.DataCarrierPreviewRegistry {
	t.Helper()
	r, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// endpointAdmission is a preview endpoint route on the same runtime carrier as runtime.
func endpointAdmission(runtime runtimewire.Admission, access string) runtimewire.Admission {
	a := runtime
	a.Binding.RouteID, a.Binding.RouteGeneration, a.RouteRevision = "route_endpoint", 1, 1
	a.AttachmentGeneration, a.Hostname, a.EndpointID, a.AccessMode = 7, "bright-otter.preview.example.test", "preview_endpoint", access
	a.RouteKind = runtimewire.EndpointPublicRouteKind
	if access != "public" {
		a.RouteKind = runtimewire.EndpointRestrictedRouteKind
	}
	return a
}

// PLAN 3a: an endpoint route rides on the agent's runtime carrier. It is served from the preview
// registry (so preview hostnames, readiness and inspection keep working), reported ready like the
// runtime route, never touched by the preview carrier worker, and removed when the server
// withdraws it while the runtime route stays.
func TestEndpointRouteRidesOnTheAgentRuntimeCarrier(t *testing.T) {
	runtimeRoute := runtimeAdmission(t)
	endpoint := endpointAdmission(runtimeRoute, "team")
	epoch := runtimeRoute.Binding.EdgeProcessEpoch
	ea, err := control.ExpectedRuntimeAdmission(endpoint, runtimeRoute.Binding.EdgeNodeID, epoch, time.Now())
	if err != nil {
		t.Fatalf("endpoint admission rejected: %v", err)
	}
	if !ea.EndpointRoute() || ea.PreviewID != "preview_endpoint" || ea.OperationID != "route_endpoint" || ea.AccessMode != "team" {
		t.Fatalf("endpoint admission = %+v", ea)
	}
	for _, bad := range []func(*runtimewire.Admission){
		func(a *runtimewire.Admission) { a.EndpointID = "" },
		func(a *runtimewire.Admission) { a.AccessMode = "" },
		func(a *runtimewire.Admission) { a.RouteKind = runtimewire.EndpointPublicRouteKind },
	} {
		value := endpoint
		bad(&value)
		if _, err := control.ExpectedRuntimeAdmission(value, runtimeRoute.Binding.EdgeNodeID, epoch, time.Now()); err == nil {
			t.Fatalf("inconsistent endpoint admission accepted: %+v", value)
		}
	}
	source := &runtimeSourceFake{admissions: []runtimewire.Admission{runtimeRoute, endpoint}}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: runtimeRoute.Binding.EdgeNodeID, ProcessEpoch: epoch})
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
	server := newTestCarrierServer(t, ea.Identity)
	go func() { _ = w.Handle(ctx, server) }()
	readyRoutes := func() map[string]bool {
		source.mu.Lock()
		defer source.mu.Unlock()
		out := map[string]bool{}
		for _, o := range source.observations {
			out[o.RouteID] = o.Ready
		}
		return out
	}
	waitRuntime(t, func() bool { r := readyRoutes(); return r["route_endpoint"] && r[runtimeRoute.Binding.RouteID] })
	route, ok := endpoints.Lookup("bright-otter.preview.example.test")
	if !ok || !route.EndpointRoute || route.Server != server || route.PreviewID != "preview_endpoint" || route.AccessMode != "team" {
		t.Fatalf("endpoint route not served from the preview registry: %+v %v", route, ok)
	}
	if routes.Len() != 1 {
		t.Fatalf("runtime registry holds %d routes, want only the runtime route", routes.Len())
	}
	// A restricted (team) endpoint route's browser stream is authorized by the runtime admission with
	// the private-access decision the preview transport builds from the registry route (F7-03).
	open := datacarrier.StreamOpen{AccountID: route.Identity.AccountID, TunnelID: route.Identity.TunnelID, ConnectorID: route.Identity.ConnectorID,
		SessionID: route.Identity.SessionID, ProcessGeneration: route.Identity.ProcessGeneration, Generation: route.Identity.Generation, RouteID: route.RouteID}
	decisionCtx := datacarrier.WithPrivateAccessDecision(ctx, datacarrier.PrivateAccessDecision{
		Allowed: true, ExpiresAt: time.Now().Add(time.Minute), ResourceID: route.PreviewID, RouteID: route.RouteID, OperationID: route.OperationID,
		CarrierSessionID: route.Identity.SessionID, RouteGeneration: route.Revision, ProcessGeneration: route.Identity.ProcessGeneration,
		ConfigGeneration: route.Identity.Generation, Protocol: "http",
	})
	if err := expected.AuthorizeStream(decisionCtx, route.Identity, open); err != nil {
		t.Fatalf("restricted endpoint browser stream refused by the runtime admission: %v (route %+v)", err, route)
	}

	// The server withdraws the endpoint (preview stopped): the route leaves, the runtime route stays.
	source.mu.Lock()
	source.admissions = []runtimewire.Admission{runtimeRoute}
	source.mu.Unlock()
	waitRuntime(t, func() bool { _, ok := endpoints.Lookup("bright-otter.preview.example.test"); return !ok })
	if routes.Len() != 1 {
		t.Fatal("withdrawing the endpoint route detached the runtime route")
	}
}

// A new preview's route is admitted by the small endpoint-only pull within a second, without
// waiting for the next full runtime pull; a failed fast pull keeps the routes already admitted.
func TestEndpointRouteIsAdmittedByTheFastPullBetweenFullPulls(t *testing.T) {
	runtimeRoute := runtimeAdmission(t)
	endpoint := endpointAdmission(runtimeRoute, "public")
	epoch := runtimeRoute.Binding.EdgeProcessEpoch
	source := &runtimeSourceFake{admissions: []runtimewire.Admission{runtimeRoute}}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: runtimeRoute.Binding.EdgeNodeID, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := endpointRegistry(t, epoch)
	w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpoints, Interval: 4 * time.Minute, EndpointInterval: 20 * time.Millisecond, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Shutdown(context.Background()) }()
	ea, err := control.ExpectedRuntimeAdmission(runtimeRoute, runtimeRoute.Binding.EdgeNodeID, epoch, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := newTestCarrierServer(t, ea.Identity)
	go func() { _ = w.Handle(ctx, server) }()
	waitRuntime(t, func() bool { return routes.Len() == 1 })
	started := time.Now()
	source.mu.Lock()
	source.admissions = []runtimewire.Admission{runtimeRoute, endpoint}
	source.mu.Unlock()
	waitRuntime(t, func() bool { _, ok := endpoints.Lookup("bright-otter.preview.example.test"); return ok })
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("endpoint route admitted after %v, want the fast pull (< 1 s)", waited)
	}
	source.mu.Lock()
	source.endpointErr = errors.New("control unavailable")
	source.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if _, ok := endpoints.Lookup("bright-otter.preview.example.test"); !ok {
		t.Fatal("a failed fast pull withdrew an admitted endpoint route")
	}
}
