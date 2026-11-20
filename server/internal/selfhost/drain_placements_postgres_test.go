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
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET endpoint_host=$2,state='ready',ready=true,last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',public_ingress_verified_at=now(),public_ingress_ipv4=$2 WHERE id=$1`, node, ip)
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
	if err := f.store.SQL().QueryRowContext(f.t.Context(), `SELECT coalesce(array_agg(r->>'value'),'{}') FROM paperboat.tunnel_domains d CROSS JOIN LATERAL jsonb_array_elements(d.expected_records) r WHERE d.id=$1`, f.domain).Scan(&values); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *pinFixture) edgeNodes() (nodes []string) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(f.t.Context(), `SELECT coalesce(array_agg(node_id),'{}') FROM paperboat.publication_edge_nodes('tunnel',$1)`, f.tunnel).Scan(&nodes); err != nil {
		f.t.Fatal(err)
	}
	return
}

func TestOwnerDrainMoveRepinsPublicationsToASibling(t *testing.T) {
	f := newPinFixture(t, true)
	if got := f.edgeNodes(); !slices.Equal(got, []string{f.first.NodeID}) || !slices.Equal(f.expectedValues(), []string{"93.184.216.41"}) {
		t.Fatalf("before the drain: edges=%v records=%v", got, f.expectedValues())
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

func TestOwnerPlainDrainKeepsPins(t *testing.T) {
	f := newPinFixture(t, true)
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, false); err != nil {
		t.Fatal(err)
	}
	if got := f.pinnedInstallation(); got != f.first.InstallationID {
		t.Fatalf("a plain drain moved the pin to %s", got)
	}
	if got := f.expectedValues(); !slices.Equal(got, []string{"93.184.216.41"}) {
		t.Fatalf("a plain drain changed the domain records: %v", got)
	}
}

func TestOwnerDrainMoveWithoutASiblingKeepsThePin(t *testing.T) {
	f := newPinFixture(t, false)
	if _, err := f.service.DrainScoped(t.Context(), f.owner, f.first.InstallationID, false, true); err != nil {
		t.Fatal(err)
	}
	if got := f.pinnedInstallation(); got != f.first.InstallationID {
		t.Fatalf("the pin moved to %s with no sibling", got)
	}
	if got := f.expectedValues(); !slices.Equal(got, []string{"93.184.216.41"}) {
		t.Fatalf("domain records changed with no sibling: %v", got)
	}
}
