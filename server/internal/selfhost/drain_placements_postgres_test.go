package selfhost

import (
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

const pinTestKey = "drain-test-key-with-at-least-32-bytes"

// pinFixture is a self-hosted-only workspace with a tunnel pinned to the first of its servers, with a verified custom
// domain. sibling is empty when the workspace has only one server.
type pinFixture struct {
	*drainFixture
	owner, tunnel, domain string
	first, sibling        Registered
}

func (f *drainFixture) ready(node, ip string) {
	f.t.Helper()
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET endpoint_host=$2::text,state='ready',ready=true,last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',public_ingress_verified_at=now(),public_ingress_ipv4=$2::inet WHERE id=$1`, node, ip)
}

func newPinFixture(t *testing.T, withSibling bool) *pinFixture {
	t.Helper()
	f := &pinFixture{drainFixture: newDrainFixture(t)}
	ctx := t.Context()
	f.owner = f.user("owner")
	f.first = scopedInstallationFixture(t, f.store, pinTestKey, f.owner, "tunnel")
	f.ready(f.first.NodeID, "93.184.216.41")
	pool := Policy{Mode: "self-hosted-only", InstallationIDs: []string{f.first.InstallationID}}
	if withSibling {
		f.sibling = scopedInstallationFixture(t, f.store, pinTestKey, f.owner, "tunnel")
		f.ready(f.sibling.NodeID, "93.184.216.42")
		pool.InstallationIDs = append(pool.InstallationIDs, f.sibling.InstallationID)
	}
	if _, err := f.service.SetPool(ctx, f.owner, "tunnel", pool); err != nil {
		t.Fatal(err)
	}
	f.tunnel, f.domain = "pin_tunnel_"+f.stamp, "pin_domain_"+f.stamp
	route, endpointID := "pin_route_"+f.stamp, testutil.EndpointID(f.stamp)
	label := testutil.EndpointLabel(endpointID)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.selfhost_placements WHERE resource_id=$1`,
			`DELETE FROM paperboat.tunnel_domains WHERE tunnel_id=$1`,
			`DELETE FROM paperboat.tunnels WHERE id=$1`,
		} {
			f.exec(q, f.tunnel)
		}
		f.exec(`DELETE FROM paperboat.operations WHERE account_id=$1`, f.owner)
		f.exec(`DELETE FROM paperboat.workspace_domains WHERE account_id=$1`, f.owner)
		f.exec(`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, f.owner)
	})
	f.exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,$5,'actor')`, f.tunnel, f.owner, endpointID, "https://"+label+".example.test", testutil.PublicationMachine(t, f.store.SQL(), f.owner))
	f.exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,$1,'http','managed','managed.example.test','http','127.0.0.1:3000','actor','actor')`, route, f.tunnel)
	testutil.VerifiedWorkspaceDomain(t, f.store.SQL(), f.owner, "", label+".example.test")
	repo, err := tunnelv1.NewRepository(f.store)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(f.domain))
	if _, err = repo.CreateResourceDomain(ctx, tunnelv1.DomainRecord{OperationID: "op_" + f.domain, AuditEventID: "aud_" + f.domain, AccountID: f.owner, TunnelID: f.tunnel, DomainID: f.domain, RouteID: route, Hostname: "pin." + label + ".example.test", MatchType: "exact", ChallengeReference: "dns:test", DNSProvider: "generic", CertificateStrategy: "managed", IdempotencyKey: f.domain, RequestHash: hash, ActorID: f.owner, ActorType: "user", CorrelationID: f.domain, Now: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.SetPlacement(ctx, f.owner, "tunnel", f.tunnel, &f.first.InstallationID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *pinFixture) pinnedInstallation() string {
	f.t.Helper()
	p, err := f.service.Placement(f.t.Context(), f.owner, "tunnel", f.tunnel)
	if err != nil || p.InstallationID == nil {
		f.t.Fatalf("placement: %+v %v", p, err)
	}
	return *p.InstallationID
}

func (f *pinFixture) expectedValues() (values []string) {
	f.t.Helper()
	if err := f.store.Pool().QueryRow(f.t.Context(), `SELECT coalesce(array_agg(r->>'value'),'{}') FROM paperboat.tunnel_domains d CROSS JOIN LATERAL jsonb_array_elements(d.expected_records) r WHERE d.id=$1`, f.domain).Scan(&values); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *pinFixture) edgeNodes() (nodes []string) {
	f.t.Helper()
	if err := f.store.Pool().QueryRow(f.t.Context(), `SELECT coalesce(array_agg(node_id),'{}') FROM paperboat.publication_edge_nodes('tunnel',$1)`, f.tunnel).Scan(&nodes); err != nil {
		f.t.Fatal(err)
	}
	return
}

func TestOwnerDrainMoveRepinsPublicationsToASibling(t *testing.T) {
	f := newPinFixture(t, true)
	if got := f.edgeNodes(); !slices.Equal(got, []string{f.first.NodeID}) {
		t.Fatalf("before the drain: edges=%v", got)
	}
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, true); err != nil {
		t.Fatal(err)
	}
	if got := f.pinnedInstallation(); got != f.sibling.InstallationID {
		t.Fatalf("pin stayed on %s, want the sibling %s", got, f.sibling.InstallationID)
	}
	if got := f.expectedValues(); !slices.Equal(got, []string{"93.184.216.42"}) {
		t.Fatalf("domain records after the move: %v", got)
	}
	if got := f.edgeNodes(); !slices.Equal(got, []string{f.sibling.NodeID}) {
		t.Fatalf("publication edges after the move: %v", got)
	}
}

// F1 follow-up: drain-and-move keeps the old node's route until the sibling is observed serving it or the drain
// deadline passes, whichever comes first, while DNS and publication edges already point at the sibling only.
func TestOwnerDrainMoveKeepsTheOldRouteUntilTheSiblingServes(t *testing.T) {
	f := newPinFixture(t, true)
	route := "pin_route_" + f.stamp
	eligible := func(node string) (ok bool) {
		t.Helper()
		if err := f.store.Pool().QueryRow(t.Context(), `SELECT paperboat.node_route_eligible($1,$2,$3)`, node, f.owner, route).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !eligible(f.first.NodeID) {
		t.Fatal("pinned node not eligible before the drain")
	}
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.selfhost_placement_handoffs WHERE resource_id=$1`, f.tunnel) })
	if got := f.edgeNodes(); !slices.Equal(got, []string{f.sibling.NodeID}) {
		t.Fatalf("publication edges after the move: %v (DNS must not keep the draining node)", got)
	}
	if got := f.expectedValues(); !slices.Equal(got, []string{"93.184.216.42"}) {
		t.Fatalf("domain records after the move: %v", got)
	}
	if !eligible(f.first.NodeID) {
		t.Fatal("drain-move cut the old node's route before the sibling served it")
	}
	if !eligible(f.sibling.NodeID) {
		t.Fatal("sibling not eligible after the move")
	}
	// The sibling observed serving the tunnel ends the handoff.
	endpoint, endpointRoute := f.tunnel, route
	f.exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,(SELECT created_by_host_id FROM paperboat.tunnels WHERE id=$1),'public','persistent',$1)`, endpoint, f.owner)
	f.exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,'handoff.example.test','http',$1)`, endpointRoute, endpoint)
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.endpoints WHERE id=$1`, endpoint) })
	// Another route of the same tunnel served on the sibling does not end this route's handoff.
	other := "pin_route_other_" + f.stamp
	f.exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,$1,'http','managed','other.example.test','http','127.0.0.1:3001','actor','actor')`, other, f.tunnel)
	f.exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,'other-handoff.example.test','http',$1)`, other, endpoint)
	f.exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,'epoch_handoff','session_handoff_other',true,now())`, other, endpoint, f.sibling.NodeID)
	if !eligible(f.first.NodeID) {
		t.Fatal("another route served on the sibling ended this route's handoff")
	}
	f.exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,'epoch_handoff','session_handoff',true,now())`, endpointRoute, endpoint, f.sibling.NodeID)
	if eligible(f.first.NodeID) {
		t.Fatal("old node kept the route after the sibling served it")
	}
	f.exec(`DELETE FROM paperboat.endpoint_edge_assignments WHERE route_id=$1 AND edge_node_id=$2`, endpointRoute, f.sibling.NodeID)
	if !eligible(f.first.NodeID) {
		t.Fatal("handoff lost while the sibling was not serving yet")
	}
	// The drain deadline passing ends it too.
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET drain_deadline=now()-interval '1 second' WHERE id=$1`, f.first.NodeID)
	if eligible(f.first.NodeID) {
		t.Fatal("old node kept the route after the drain deadline")
	}
}

func TestOwnerPlainDrainKeepsPins(t *testing.T) {
	f := newPinFixture(t, true)
	before := f.expectedValues()
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, false); err != nil {
		t.Fatal(err)
	}
	if got := f.pinnedInstallation(); got != f.first.InstallationID {
		t.Fatalf("a plain drain moved the pin to %s", got)
	}
	if got := f.expectedValues(); !slices.Equal(got, before) {
		t.Fatalf("a plain drain changed the domain records: %v, were %v", got, before)
	}
}

func TestOwnerDrainMoveWithoutASiblingKeepsThePin(t *testing.T) {
	f := newPinFixture(t, false)
	before := f.expectedValues()
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, true); err != nil {
		t.Fatal(err)
	}
	if got := f.pinnedInstallation(); got != f.first.InstallationID {
		t.Fatalf("the pin moved to %s with no sibling", got)
	}
	if got := f.expectedValues(); !slices.Equal(got, before) {
		t.Fatalf("domain records changed with no sibling: %v, were %v", got, before)
	}
}

// A sibling publication_edge_nodes would reject (here a stale heartbeat) is not a drain-and-move target: the pin
// stays rather than move to a server that cannot serve it.
func TestOwnerDrainMoveSkipsASiblingThatCannotServe(t *testing.T) {
	f := newPinFixture(t, true)
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '1 minute' WHERE id=$1`, f.sibling.NodeID)
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, true); err != nil {
		t.Fatal(err)
	}
	if got := f.pinnedInstallation(); got != f.first.InstallationID {
		t.Fatalf("the pin moved to %s, a sibling with a stale heartbeat", got)
	}
}

// Re-placing on the same server after only its second address changed rewrites the domain's records: the primary
// target is the same, so comparing dns_target alone kept the old set.
func TestPlacementRetargetsWhenOnlyTheSecondAddressChanges(t *testing.T) {
	f := newPinFixture(t, false)
	if got := f.expectedValues(); !slices.Equal(got, []string{"93.184.216.41"}) {
		t.Fatalf("records before: %v", got)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_ipv6='2606:4700::41' WHERE id=$1`, f.first.NodeID)
	if _, err := f.service.SetPlacement(t.Context(), f.owner, "tunnel", f.tunnel, &f.first.InstallationID); err != nil {
		t.Fatal(err)
	}
	if got := f.expectedValues(); !slices.Equal(got, []string{"93.184.216.41", "2606:4700::41"}) {
		t.Fatalf("records after the IPv6 address was added: %v", got)
	}
}
