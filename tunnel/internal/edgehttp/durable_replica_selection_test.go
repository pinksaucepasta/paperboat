package edgehttp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

func TestDurableReplicaSelectionEligibilityLatencyAndDeterministicTie(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	rule := replicaRule()
	fastIdentity := replicaIdentity("host_fast", "connector_fast", "session_fast", 3)
	slowIdentity := replicaIdentity("host_slow", "connector_slow", "session_slow", 3)
	fast, fastClient := testEdgePreviewCarrierPair(t, fastIdentity)
	slow, slowClient := testEdgePreviewCarrierPair(t, slowIdentity)
	defer fast.Close()
	defer fastClient.Close()
	defer slow.Close()
	defer slowClient.Close()
	fastState := replicaState(rule, 3, "assignment_fast", "zone_a", 5*time.Millisecond, 10)
	slowState := replicaState(rule, 3, "assignment_slow", "zone_b", 20*time.Millisecond, 10)
	if err := registry.AttachReplica(fast, "key-fast", "thumb-fast", fastState); err != nil {
		t.Fatal(err)
	}
	if err := registry.AttachReplica(slow, "key-slow", "thumb-slow", slowState); err != nil {
		t.Fatal(err)
	}
	selected, err := registry.entryFor(rule, "request_same")
	if err != nil || selected.server != fast {
		t.Fatalf("selected=%v err=%v, want fast", selected, err)
	}

	fastState.HealthyRoutes = nil
	if err := registry.UpdateReplicaState(fast, fastState); err != nil {
		t.Fatal(err)
	}
	selected, err = registry.entryFor(rule, "request_same")
	if err != nil || selected.server != slow {
		t.Fatalf("selected=%v err=%v, want healthy slow", selected, err)
	}

	slowState.Ready = false
	if err := registry.UpdateReplicaState(slow, slowState); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.entryFor(rule, "request_same"); !errors.Is(err, ErrDataCarrierRouteUnavailable) {
		t.Fatalf("error=%v", err)
	}

	fastState.HealthyRoutes = []string{rule.ID}
	slowState.Ready = true
	fastState.Latency, slowState.Latency = 10*time.Millisecond, 10*time.Millisecond
	if err := registry.UpdateReplicaState(fast, fastState); err != nil {
		t.Fatal(err)
	}
	if err := registry.UpdateReplicaState(slow, slowState); err != nil {
		t.Fatal(err)
	}
	first, err := registry.entryFor(rule, "stable_request")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, nextErr := registry.entryFor(rule, "stable_request")
		if nextErr != nil || next != first {
			t.Fatalf("selection changed: first=%p next=%p err=%v", first, next, nextErr)
		}
	}
	if registry.Len() != 2 {
		t.Fatalf("registry length=%d; selection must not broadcast or detach peers", registry.Len())
	}
	for _, mutate := range []func(*route.RouteRule){
		func(value *route.RouteRule) { value.RouteGeneration++ },
		func(value *route.RouteRule) {
			value.ConfigContentHash = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		},
	} {
		forged := rule
		mutate(&forged)
		if _, err := registry.entryFor(forged, "forged_request"); !errors.Is(err, ErrDataCarrierRouteUnavailable) {
			t.Fatalf("forged route tuple error=%v, want unavailable", err)
		}
	}
}

func TestDurableReplicaOldSessionCleanupCannotRemoveReplacement(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	identity := replicaIdentity("host_one", "connector_one", "session_same", 4)
	oldServer, oldClient := testEdgePreviewCarrierPair(t, identity)
	newServer, newClient := testEdgePreviewCarrierPair(t, identity)
	defer oldClient.Close()
	defer newClient.Close()
	defer newServer.Close()
	rule := replicaRule()
	rule.ConfigGeneration = 4
	state := replicaState(rule, 4, "assignment_replacement", "zone_a", time.Millisecond, 4)
	if err := registry.AttachReplica(oldServer, "key", "thumb", state); err != nil {
		t.Fatal(err)
	}
	if err := registry.AttachReplica(newServer, "key", "thumb", state); err != nil {
		t.Fatal(err)
	}
	if err := oldServer.Close(); err != nil {
		t.Fatal(err)
	}
	selected, err := registry.entryFor(rule, "request_after_cleanup")
	if err != nil || selected.server != newServer || registry.Len() != 1 {
		t.Fatalf("selected=%v len=%d err=%v", selected, registry.Len(), err)
	}
	if err := registry.UpdateReplicaState(oldServer, state); !errors.Is(err, ErrDataCarrierRouteUnavailable) {
		t.Fatalf("stale update error=%v", err)
	}
}

func TestDurableReplicaExactAssignmentSelectsOnlyAuthorizedConnector(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	rule := replicaRule()
	firstIdentity := replicaIdentity("host_first", "connector_first", "session_first", rule.ConfigGeneration)
	secondIdentity := replicaIdentity("host_second", "connector_second", "session_second", rule.ConfigGeneration)
	first, firstClient := testEdgePreviewCarrierPair(t, firstIdentity)
	second, secondClient := testEdgePreviewCarrierPair(t, secondIdentity)
	defer first.Close()
	defer firstClient.Close()
	defer second.Close()
	defer secondClient.Close()
	firstState := replicaState(rule, rule.ConfigGeneration, "assignment_first", "zone_a", time.Millisecond, 4)
	secondState := replicaState(rule, rule.ConfigGeneration, "assignment_second", "zone_b", 20*time.Millisecond, 4)
	if err := registry.AttachReplica(first, "key-first", "thumb-first", firstState); err != nil {
		t.Fatal(err)
	}
	if err := registry.AttachReplica(second, "key-second", "thumb-second", secondState); err != nil {
		t.Fatal(err)
	}

	exact := rule
	exact.HostID = secondIdentity.HostID
	exact.ConnectorID = secondIdentity.ConnectorID
	exact.ConnectorSessionID = secondIdentity.SessionID
	exact.ConnectorProcessGeneration = secondIdentity.ProcessGeneration
	exact.AssignmentID = "assignment_second"
	selected, err := registry.entryForExactAssignment(exact)
	if err != nil || selected.server != second {
		t.Fatalf("selected=%v err=%v, want exact second replica", selected, err)
	}
	for _, mutate := range []func(*route.RouteRule){
		func(value *route.RouteRule) { value.ConnectorID = firstIdentity.ConnectorID },
		func(value *route.RouteRule) { value.AssignmentID = "assignment_first" },
		func(value *route.RouteRule) { value.AssignmentGeneration++ },
		func(value *route.RouteRule) { value.RouteGeneration++ },
		func(value *route.RouteRule) {
			value.ConfigContentHash = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		},
	} {
		forged := exact
		mutate(&forged)
		if _, err := registry.entryForExactAssignment(forged); !errors.Is(err, ErrDataCarrierRouteUnavailable) {
			t.Fatalf("forged exact assignment error=%v, want unavailable", err)
		}
	}
}

func replicaIdentity(host, connector, session string, generation uint64) datacarrier.Identity {
	return datacarrier.Identity{AccountID: "account_replica", HostID: host, TunnelID: "tunnel_replica", ConnectorID: connector, SessionID: session, ProcessGeneration: generation, Generation: generation}
}

func replicaRule() route.RouteRule {
	return route.RouteRule{ID: "route_replica", RouteID: "route_replica", RouteGeneration: 7, AssignmentID: "assignment_representative", AssignmentGeneration: 11, Kind: route.TunnelHTTPSWSS, Protocol: "http", AccountID: "account_replica", HostID: "host_pinned", TunnelID: "tunnel_replica", ConnectorID: "connector_pinned", ConnectorSessionID: "session_pinned", ConnectorProcessGeneration: 3, ConfigGeneration: 3, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
}

func replicaState(rule route.RouteRule, generation uint64, assignmentID, failureDomain string, latency time.Duration, capacity uint32) ReplicaState {
	routeID := carrierRouteID(rule)
	return ReplicaState{Ready: true, Generation: generation, RouteIDs: []string{routeID}, HealthyRoutes: []string{routeID}, RouteBindings: []ReplicaRouteBinding{{RouteID: routeID, AssignmentID: assignmentID, AssignmentGeneration: rule.AssignmentGeneration, RouteGeneration: rule.RouteGeneration, ConfigContentHash: rule.ConfigContentHash}}, FailureDomain: failureDomain, Latency: latency, Capacity: capacity}
}

// A public decision names one carrier session, so a request routed to a sibling replica carries
// that replica's own decision instead of the representative's (which Authorize would refuse).
func TestDurableReplicaPublicRequestUsesSelectedReplicaDecision(t *testing.T) {
	rule := replicaRule()
	rule.AccessMode, rule.Node, rule.EdgeProcessEpoch, rule.ViewerPolicyGeneration = "public", "edge_01", "epoch_0001", 1
	authority := func(_ context.Context, r route.RouteRule) (connectorprotocol.IngressDecision, error) {
		d := publicTCPDecision(time.Now().UTC(), 443)
		d.Binding.RouteID, d.Binding.RouteGeneration = r.RouteID, r.RouteGeneration
		d.ConnectorID, d.SessionID, d.ProcessGeneration, d.ConfigGeneration, d.AssignmentGeneration = r.ConnectorID, r.ConnectorSessionID, r.ConnectorProcessGeneration, r.ConfigGeneration, r.AssignmentGeneration
		return d, nil
	}
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4, IngressAuthority: authority})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	representative, _ := testEdgePreviewCarrierPair(t, replicaIdentity(rule.HostID, rule.ConnectorID, rule.ConnectorSessionID, 3))
	sibling, siblingClient := testEdgePreviewCarrierPair(t, replicaIdentity("host_fast", "connector_fast", "session_fast", 3))
	if err := registry.AttachReplica(representative, "key-a", "thumb-a", replicaState(rule, 3, rule.AssignmentID, "zone_a", 20*time.Millisecond, 4)); err != nil {
		t.Fatal(err)
	}
	if err := registry.AttachReplica(sibling, "key-b", "thumb-b", replicaState(rule, 3, "assignment_fast", "zone_b", time.Millisecond, 4)); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		stream, err := registry.OpenRouteStream(context.Background(), rule, "request_sibling")
		if stream != nil {
			defer stream.Close()
		}
		opened <- err
	}()
	acceptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, _, err := siblingClient.AcceptStream(acceptCtx)
	if err != nil {
		t.Fatalf("sibling accepted no stream: %v (open: %v)", err, <-opened)
	}
	defer stream.Close()
	got, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC())
	if err != nil || got.SessionID != "session_fast" || got.ConnectorID != "connector_fast" {
		t.Fatalf("decision session=%q connector=%q err=%v, want the sibling's own", got.SessionID, got.ConnectorID, err)
	}
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
}

// A replica whose host sent goaway (the worker a handover replaced) is picked only when no live one
// is left, and a replica whose open fails hands the request to the next one: nothing was sent yet.
func TestDurableReplicaSkipsDrainingAndRetriesFailedOpen(t *testing.T) {
	rule := replicaRule()
	rule.AccessMode = "public"
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	denied := datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error {
		return errors.New("refused")
	})
	fastIdentity := replicaIdentity("host_fast", "connector_fast", "session_fast", 3)
	fast, fastClient := testEdgePreviewCarrierPair(t, fastIdentity)
	refusing, _ := testEdgePreviewCarrierPair(t, replicaIdentity("host_refusing", "connector_refusing", "session_refusing", 3), denied)
	slow, slowClient := testEdgePreviewCarrierPair(t, replicaIdentity("host_slow", "connector_slow", "session_slow", 3))
	for _, replica := range []struct {
		server  *datacarrier.Server
		latency time.Duration
	}{{fast, time.Millisecond}, {refusing, 2 * time.Millisecond}, {slow, 20 * time.Millisecond}} {
		if err := registry.AttachReplica(replica.server, "key", "thumb", replicaState(rule, 3, "assignment_"+replica.server.Identity().HostID, replica.server.Identity().HostID, replica.latency, 4)); err != nil {
			t.Fatal(err)
		}
	}
	goaway, err := fastClient.OpenStream(context.Background(), connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: fastIdentity.AccountID, TunnelID: fastIdentity.TunnelID, ConnectorID: fastIdentity.ConnectorID, SessionID: fastIdentity.SessionID, ProcessGeneration: fastIdentity.ProcessGeneration, Generation: fastIdentity.Generation, RouteID: rule.RouteID, RequestID: "request_goaway", Kind: connectorprotocol.Goaway})
	if err != nil {
		t.Fatal(err)
	}
	if err := connectorprotocol.WriteGoawayNotice(goaway, connectorprotocol.GoawayHostDraining, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fast.Draining():
	case <-time.After(2 * time.Second):
		t.Fatal("fast replica did not drain")
	}
	opened := make(chan error, 1)
	go func() {
		stream, err := registry.OpenRouteStream(context.Background(), rule, "request_retry")
		if stream != nil {
			defer stream.Close()
		}
		opened <- err
	}()
	acceptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, _, err := slowClient.AcceptStream(acceptCtx)
	if err != nil {
		t.Fatalf("slow replica accepted no stream: %v (open: %v)", err, <-opened)
	}
	defer stream.Close()
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
}

// A route update bumps the route generation: the host applies it before the edge activates the new rule, so the
// active rule still names the older generation while the carrier already reports the newer one. That is served,
// never a 502; a carrier older than the rule is not.
func TestDurableReplicaServesAnOlderRuleFromACarrierOnANewerRouteGeneration(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	rule := replicaRule()
	server, client := testEdgePreviewCarrierPair(t, replicaIdentity("host_a", "connector_a", "session_a", 3))
	defer server.Close()
	defer client.Close()
	state := replicaState(rule, 3, "assignment_a", "zone_a", time.Millisecond, 10)
	if err := registry.AttachReplica(server, "key", "thumb", state); err != nil {
		t.Fatal(err)
	}
	state.RouteBindings[0].RouteGeneration++
	if err := registry.UpdateReplicaState(server, state); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.entryFor(rule, "request_old_rule"); err != nil {
		t.Fatalf("old rule on a newer carrier binding: %v", err)
	}
	newer := rule
	newer.RouteGeneration += 2
	if _, err := registry.entryFor(newer, "request_new_rule"); !errors.Is(err, ErrDataCarrierRouteUnavailable) {
		t.Fatalf("rule newer than the carrier: %v, want unavailable", err)
	}
}
