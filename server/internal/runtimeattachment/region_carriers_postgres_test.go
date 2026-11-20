package runtimeattachment

import (
	"slices"
	"testing"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// regionFixture is the agent fixture with its workspace's pair stored as two regions: ra holds the fixture's first
// edge, rb its backup edge, and nodes added with addNode join either (or neither).
type regionFixture struct {
	*agentFixture
	ra, rb string
}

func newRegionFixture(t *testing.T) *regionFixture {
	f := &regionFixture{agentFixture: newAgentFixture(t, "regions")}
	f.ra, f.rb = "ra_"+f.prefix, "rb_"+f.prefix
	f.exec(t, `UPDATE control_tunnel_nodes SET region=CASE WHEN id=$1 THEN $3 ELSE $4 END WHERE id IN ($1,$2)`, f.node, f.backupNode, f.ra, f.rb)
	f.exec(t, `INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'managed-only',$2)`, f.account, []string{f.ra, f.rb})
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM workspace_edge_sets WHERE scope_id=$1`, f.account)
		// Carrier rows reference their node without a cascade: delete them first or the nodes below leak into later tests.
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM runtime_carrier_attachments WHERE machine_id=$1`, f.machine)
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM control_tunnel_nodes WHERE id LIKE $1`, f.prefix+"_x%")
	})
	return f
}

// addNode adds a ready edge of region and returns its id.
func (f *regionFixture) addNode(t *testing.T, name, region string) string {
	id := f.prefix + "_x" + name
	f.exec(t, `INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem)
VALUES($1,'default','1.0','epoch_'||$2,'ready',true,now(),$3,$1,ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour',$2||'.example.test',27443,27444,'sha256:'||repeat('3',64),'fixture certificate')`, id, name, region)
	return id
}

func (f *regionFixture) drain(t *testing.T, node, deadline string) {
	f.exec(t, `UPDATE control_tunnel_nodes SET state='draining',ready=false,drain_deadline=now()+$2::interval WHERE id=$1`, node, deadline)
}

// attach returns the nodes of every carrier of the answer, in answer order, and the answer.
func (f *regionFixture) attach(t *testing.T) ([]string, runtimewire.HostAdmission) {
	t.Helper()
	out, err := f.service.Attach(f.ctx, f.proof, f.request)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []string{out.Binding.EdgeNodeID}
	if out.Backup != nil {
		nodes = append(nodes, out.Backup.Binding.EdgeNodeID)
	}
	for _, a := range out.More {
		nodes = append(nodes, a.Binding.EdgeNodeID)
	}
	return nodes, out
}

func (f *regionFixture) held(t *testing.T) []string {
	t.Helper()
	rows, err := f.database.SQL().QueryContext(f.ctx, `SELECT edge_node_id FROM runtime_carrier_attachments WHERE route_id=$1 ORDER BY edge_node_id`, f.route)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// Edges do not forward, so a machine holds a carrier to every ready node of both regions: two nodes of the primary
// region come first, then the backup region's.
func TestAttachHoldsEveryReadyNodeOfBothRegions(t *testing.T) {
	f := newRegionFixture(t)
	second := f.addNode(t, "a2", f.ra)
	nodes, _ := f.attach(t)
	if len(nodes) != 3 || !slices.Contains(nodes[:2], f.node) || !slices.Contains(nodes[:2], second) || nodes[2] != f.backupNode {
		t.Fatalf("carriers=%v, want the two nodes of %s then %s", nodes, f.ra, f.backupNode)
	}
	if held := f.held(t); len(held) != 3 {
		t.Fatalf("held rows=%v", held)
	}
}

func TestAttachSkipsDrainingNodeAndKeepsSameRegionNode(t *testing.T) {
	f := newRegionFixture(t)
	second := f.addNode(t, "a2", f.ra)
	f.drain(t, second, "10 minutes")
	nodes, _ := f.attach(t)
	if !slices.Equal(sorted(nodes), sorted([]string{f.node, f.backupNode})) {
		t.Fatalf("carriers=%v, want %s and %s without the draining %s", nodes, f.node, f.backupNode, second)
	}
}

// The primary region is all draining: the machine is served from the backup region, and its terminal carrier is there.
func TestAttachFallsBackToBackupRegion(t *testing.T) {
	f := newRegionFixture(t)
	second := f.addNode(t, "a2", f.ra)
	f.drain(t, f.node, "10 minutes")
	f.drain(t, second, "10 minutes")
	nodes, out := f.attach(t)
	if !slices.Equal(nodes, []string{f.backupNode}) || out.ActiveEdgeNodeID != f.backupNode {
		t.Fatalf("carriers=%v active=%q, want only %s", nodes, out.ActiveEdgeNodeID, f.backupNode)
	}
}

func TestAttachCapsAtMaximumRegionCarriers(t *testing.T) {
	f := newRegionFixture(t)
	for i := range 10 {
		f.addNode(t, "c"+string(rune('a'+i)), f.ra)
	}
	nodes, _ := f.attach(t)
	if len(nodes) != runtimewire.MaximumRegionCarriers || slices.Contains(nodes, f.backupNode) {
		t.Fatalf("carriers=%d %v, want %d nodes of the primary region", len(nodes), nodes, runtimewire.MaximumRegionCarriers)
	}
}

// Drain lets existing connections finish: the carrier on a draining node stays valid, and its row stays through a
// re-attach, until the node's drain deadline; Drain and move's short deadline ends it quickly.
func TestCarrierStaysValidOnDrainingNodeUntilItsDeadline(t *testing.T) {
	f := newRegionFixture(t)
	second := f.addNode(t, "a2", f.ra)
	f.attach(t)
	valid := func() bool {
		t.Helper()
		var ok bool
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT paperboat.runtime_carrier_valid($1,$2,'epoch_a2',false)`, f.route, second).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !valid() {
		t.Fatal("a carrier on a ready node is not valid")
	}
	f.drain(t, second, "10 minutes")
	if !valid() {
		t.Fatal("a carrier on a node in Drain is invalid before its deadline")
	}
	nodes, out := f.attach(t)
	if slices.Contains(nodes, second) || !slices.Contains(f.held(t), second) {
		t.Fatalf("answer %v and held %v: the draining node must leave the answer but keep its row until the deadline", nodes, f.held(t))
	}
	if out.ActiveEdgeNodeID == second || out.ActiveEdgeNodeID == "" {
		t.Fatalf("the terminal carrier stayed on the draining node: %q", out.ActiveEdgeNodeID)
	}
	f.exec(t, `UPDATE control_tunnel_nodes SET drain_deadline=now()-interval '1 second' WHERE id=$1`, second)
	if valid() {
		t.Fatal("a carrier on a node past its drain deadline is still valid")
	}
	f.attach(t)
	if slices.Contains(f.held(t), second) {
		t.Fatalf("the row of a node past its deadline survived a re-attach: %v", f.held(t))
	}
	// A draining node without a deadline (not the state Drain writes) gives no grace.
	f.exec(t, `UPDATE control_tunnel_nodes SET state='draining',ready=false,drain_deadline=NULL WHERE id=$1`, f.node)
	var ok bool
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT paperboat.runtime_carrier_valid($1,$2,'epoch_endpoint',false)`, f.route, f.node).Scan(&ok); err != nil || ok {
		t.Fatalf("a draining node without a deadline kept its carrier valid: %v %v", ok, err)
	}
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// selfHosted turns the workspace into a self-hosted-only one whose own server is the fixture's backup node (region rb),
// and adds managed nodes: the fixture's first edge (region ra) and one in a second managed region, plus a second node
// of ra that must not be taken for terminals (one carrier per managed region).
func (f *regionFixture) selfHosted(t *testing.T) (secondManaged, extraInFirst string) {
	t.Helper()
	f.exec(t, `UPDATE control_tunnel_nodes SET tls_spki_sha256=decode(repeat('ab',32),'hex') WHERE id=$1`, f.backupNode)
	f.exec(t, `INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),'fixture','own server','tunnel','account',$3,$1,true)`, f.prefix+"_own", f.backupNode, f.account)
	f.exec(t, `INSERT INTO selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2]::text[])`, f.account, f.prefix+"_own")
	f.exec(t, `UPDATE workspace_edge_sets SET mode='self-hosted-only',regions=ARRAY[$2]::text[] WHERE scope_kind='account' AND scope_id=$1`, f.account, f.rb)
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM selfhost_pool_policies WHERE scope_id=$1`, f.account)
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM selfhost_installations WHERE id=$1`, f.prefix+"_own")
	})
	return f.addNode(t, "m2", "rm_"+f.prefix), f.addNode(t, "a2", f.ra)
}

func (f *regionFixture) regionOf(t *testing.T, node string) string {
	t.Helper()
	var region string
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT region FROM control_tunnel_nodes WHERE id=$1`, node).Scan(&region); err != nil {
		t.Fatal(err)
	}
	return region
}

// A self-hosted-only workspace's terminals do not depend on one managed edge: its machine holds a terminal carrier on a
// managed node of each of two managed regions (not two of one), in front of its own server.
func TestAttachSelfHostedHoldsTwoManagedTerminalCarriersInTwoRegions(t *testing.T) {
	f := newRegionFixture(t)
	f.selfHosted(t)
	nodes, out := f.attach(t)
	if len(nodes) != 3 || nodes[2] != f.backupNode || len(out.TerminalEdgeNodeIDs) != 2 || !slices.Equal(out.TerminalEdgeNodeIDs, nodes[:2]) {
		t.Fatalf("carriers=%v terminals=%v, want two managed terminal carriers then the own server %s", nodes, out.TerminalEdgeNodeIDs, f.backupNode)
	}
	if f.regionOf(t, nodes[0]) == f.regionOf(t, nodes[1]) {
		t.Fatalf("both terminal carriers are in region %s", f.regionOf(t, nodes[0]))
	}
	if out.ActiveEdgeNodeID != out.TerminalEdgeNodeIDs[0] {
		t.Fatalf("active=%q, want the first terminal carrier %q", out.ActiveEdgeNodeID, out.TerminalEdgeNodeIDs[0])
	}
	if held := f.held(t); len(held) != 3 {
		t.Fatalf("held rows=%v", held)
	}
}

// Managed region 1 down: the active terminal carrier moves to the other managed region.
func TestManagedTerminalFailsOverToSecondRegion(t *testing.T) {
	f := newRegionFixture(t)
	f.selfHosted(t)
	_, first := f.attach(t)
	active, other := first.TerminalEdgeNodeIDs[0], first.TerminalEdgeNodeIDs[1]
	if first.ActiveEdgeNodeID != active {
		t.Fatalf("active=%q, want %q", first.ActiveEdgeNodeID, active)
	}
	f.exec(t, `UPDATE control_tunnel_nodes SET ready=false WHERE region=$1 AND id LIKE $2`, f.regionOf(t, active), f.prefix+"%")
	nodes, second := f.attach(t)
	if second.ActiveEdgeNodeID != other || !slices.Equal(second.TerminalEdgeNodeIDs, []string{other}) || slices.Contains(nodes, active) {
		t.Fatalf("after region %s went down: carriers=%v active=%q terminals=%v, want the terminal carrier on %s", f.regionOf(t, active), nodes, second.ActiveEdgeNodeID, second.TerminalEdgeNodeIDs, other)
	}
}
