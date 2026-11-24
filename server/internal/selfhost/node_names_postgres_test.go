package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

type recordedNames struct {
	published, reconciled map[string][]string
	err                   error
}

func (r *recordedNames) Publish(_ context.Context, d tunnelcert.DesiredAddressPublication) error {
	if r.err == nil {
		r.published[d.Hostname] = d.Addresses
	}
	return r.err
}

func (r *recordedNames) Reconcile(_ context.Context, d tunnelcert.DesiredAddressPublication) error {
	r.reconciled[d.Hostname] = d.Addresses
	return nil
}

// A global node paired by its detected address gets a published name before it is claimed; an operator-given host and a
// customer's node never do; a removed node's name is withdrawn.
func TestDetectedNodeIsNamedBeforeItIsClaimedPostgres(t *testing.T) {
	f := newEnrollExpectFixture(t)
	names := &recordedNames{published: map[string][]string{}, reconciled: map[string][]string{}, err: errors.New("provider down")}
	f.service.Names = &NodeNames{DB: f.service.database, Domain: "nodes.example.test", Publisher: names}
	global := Scope{Kind: "global"}
	enroll := func(node *fakeClaimNode, scope Scope, address string) ([]Installation, error) {
		f.service.DialAddress = func(context.Context, string) (string, error) { return node.server.Listener.Addr().String(), nil }
		listed, err := f.service.Enroll(t.Context(), f.owner, EnrollRequest{Address: address, Code: node.code(), Scope: scope}, scope == global)
		var installs []Installation
		for _, i := range listed {
			if i.Name != node.name {
				continue
			}
			installs = append(installs, i)
			t.Cleanup(func() {
				f.exec(`DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, i.NodeID)
				f.exec(`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, i.InstallationID)
				f.exec(`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, i.NodeID)
			})
		}
		return installs, err
	}
	f.exec(`INSERT INTO paperboat.selfhost_regions(scope_kind,scope_id,slug,display_name) VALUES('account',$1,'nodenames','nodenames') ON CONFLICT DO NOTHING`, f.owner)
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.operator_regions WHERE id='nodenames'`) })
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.selfhost_regions WHERE scope_id=$1`, f.owner) })
	node := newFakeClaimNode(t, "named", fakeEdge("nodenames"))
	node.detected = true
	if _, err := enroll(node, global, "93.184.216.35"); err == nil || node.claims.Load() != 0 {
		t.Fatalf("the node was claimed (%d) without its record: %v", node.claims.Load(), err)
	}
	names.err = nil
	installs, err := enroll(node, global, "93.184.216.35")
	if err != nil || len(installs) != 1 {
		t.Fatalf("enroll: %+v %v", installs, err)
	}
	name := installs[0].EndpointHost
	if !strings.HasSuffix(name, ".nodes.example.test") || node.claimedName != name || !slices.Equal(names.published[name], []string{"93.184.216.35"}) {
		t.Fatalf("endpoint %q, claimed %q, published %v", name, node.claimedName, names.published)
	}
	// Paired again at another address: same name, new record, ingress to be verified again.
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_ipv4='93.184.216.35',public_ingress_verified_at=now() WHERE id=$1`, installs[0].NodeID)
	if _, err = enroll(node, global, "93.184.216.36"); err != nil || !slices.Equal(names.published[name], []string{"93.184.216.36"}) {
		t.Fatalf("moved: %v %v", names.published, err)
	}
	var verified sql.NullTime
	if err = f.service.database.SQL().QueryRowContext(t.Context(), `SELECT public_ingress_verified_at FROM paperboat.control_tunnel_nodes WHERE id=$1`, installs[0].NodeID).Scan(&verified); err != nil || verified.Valid {
		t.Fatalf("the old address's verification was kept: %v %v", verified, err)
	}
	if err = f.service.Names.Sync(t.Context()); err != nil || !slices.Equal(names.reconciled[name], []string{"93.184.216.36"}) {
		t.Fatalf("sync: %v %v", names.reconciled, err)
	}
	f.exec(`INSERT INTO paperboat.edge_dns_publications(hostname,owner_id,resource_generation,readiness_version,observed_at,publication_generation,desired_addresses,state,created_at,updated_at) VALUES($1,$2,1,'test',now(),1,'["93.184.216.36"]','verified',now(),now())`, name, nodeNamePublication(name, nil, f.service.now()).Owner)
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.edge_dns_publications WHERE hostname=$1`, name) })
	clear(names.reconciled)
	if err = f.service.Names.Sync(t.Context()); err != nil || len(names.reconciled) != 0 {
		t.Fatalf("a published name was written again: %v %v", names.reconciled, err)
	}
	if err = f.service.RemoveScoped(t.Context(), f.owner, installs[0].InstallationID, true); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Names.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if addresses, ok := names.reconciled[name]; !ok || len(addresses) != 0 {
		t.Fatalf("a removed node's name was not withdrawn: %v", names.reconciled)
	}
	operatorGiven := newFakeClaimNode(t, "own-host", fakeRelay("nodenames"))
	customer := newFakeClaimNode(t, "customer-host", fakeRelay("nodenames"))
	customer.detected = true
	for node, scope := range map[*fakeClaimNode]Scope{operatorGiven: global, customer: {Kind: "account", ID: f.owner}} {
		installs, err = enroll(node, scope, "93.184.216.35")
		if err != nil || len(installs) != 1 || installs[0].EndpointHost != "93.184.216.35" || node.claimedName != "" {
			t.Fatalf("%s was renamed: %+v %v", node.name, installs, err)
		}
	}
}
