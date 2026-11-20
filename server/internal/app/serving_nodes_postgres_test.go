package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// workspace adds a personal account with one enrolled machine and a stored pair of regions (primary first), and
// returns the nodes that machine's attach holds carriers to.
func (f *wildcardFixture) workspace(name string, regions ...string) []string {
	f.t.Helper()
	id := name + "_" + f.stamp
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test")
	f.exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, id, id)
	f.exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation,configured_capabilities) VALUES ($1,$1,$1,'serving-test','linux','amd64','/workspace','online','occupied',$2,1,1,ARRAY['terminal_host']::text[])`, id, base64.RawURLEncoding.EncodeToString(public))
	f.exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$1,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080)`, id)
	f.exec(`INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$1,'runtime',1,'default','pending')`, id)
	f.exec(`INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'managed-only',$2)`, id, regions)
	f.t.Cleanup(func() {
		for _, query := range []string{`DELETE FROM runtime_carrier_attachments WHERE machine_id=$1`, `DELETE FROM workspace_edge_sets WHERE scope_id=$1`, `DELETE FROM control_routes WHERE id=$1`,
			`DELETE FROM control_connector_generations WHERE environment_id=$1`, `DELETE FROM user_machines WHERE id=$1`, `DELETE FROM control_environments WHERE id=$1`, `DELETE FROM users WHERE id=$1`} {
			f.exec(query, id)
		}
	})
	proof := previewattachment.MachineProof{UserID: id, MachineID: id, OperationID: "operation_serving", InstallationGeneration: 1}
	out, err := (&runtimeattachment.Service{DB: f.store}).Attach(context.Background(), proof, runtimewire.Request{OperationID: proof.OperationID, ProcessID: "process_serving", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 48080})
	if err != nil {
		f.t.Fatal(err)
	}
	nodes := []string{out.Binding.EdgeNodeID}
	if out.Backup != nil {
		nodes = append(nodes, out.Backup.Binding.EdgeNodeID)
	}
	for _, a := range out.More {
		nodes = append(nodes, a.Binding.EdgeNodeID)
	}
	return nodes
}

// Edges do not forward, so for any workspace the nodes the wildcard publishes inside its two regions are exactly the
// nodes its machines attach to. Three regions of 5, 2 and 2 ready nodes (a's fifth is beyond the per-region serving
// set), two workspaces with different pairs.
func TestServingNodesAreTheSameInDNSAndAttach(t *testing.T) {
	f := newWildcardFixture(t)
	regionOf := map[string]string{} // node id -> region
	ipOf := map[string]string{}     // published IPv4 -> node id
	next := 1
	for region, count := range map[string]int{"wc_a_" + f.stamp: 5, "wc_b_" + f.stamp: 2, "wc_c_" + f.stamp: 2} {
		f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Serving',true)`, region)
		t.Cleanup(func() {
			f.exec(`DELETE FROM paperboat.managed_node_authority WHERE region_id=$1`, region)
			f.exec(`DELETE FROM paperboat.operator_regions WHERE id=$1`, region)
		})
		for i := range count {
			name, v4 := fmt.Sprintf("%s%d", region[3:4], i+1), fmt.Sprintf("203.0.113.%d", next)
			id := f.node(name, v4, "", region, `process_epoch='epoch_'||id,carrier_endpoint_host='edge.example.test',carrier_endpoint_tcp_port=27443,carrier_endpoint_quic_port=27444,carrier_server_spki_sha256='sha256:'||repeat('3',64),carrier_server_certificate_chain_pem='fixture certificate'`)
			regionOf[id], ipOf[v4] = region, id
			next++
		}
	}
	a, b, c := "wc_a_"+f.stamp, "wc_b_"+f.stamp, "wc_c_"+f.stamp
	f.sync()
	published := map[string]bool{}
	for _, address := range f.public.calls[0].Addresses {
		published[ipOf[address]] = true
	}
	for name, pair := range map[string][]string{"one": {a, b}, "two": {c, a}} {
		held := f.workspace(name, pair...)
		var inPair []string
		for node := range published {
			if slices.Contains(pair, regionOf[node]) {
				inPair = append(inPair, node)
			}
		}
		slices.Sort(inPair)
		slices.Sort(held)
		if !slices.Equal(inPair, held) {
			t.Fatalf("workspace %s (regions %v): the wildcard publishes %v inside them, a machine holds carriers to %v", name, pair, inPair, held)
		}
		if want := 4 + 2; name == "one" && len(held) != want {
			t.Fatalf("workspace one holds %d carriers, want %d (4 of a, 2 of b)", len(held), want)
		}
	}
}

// TestPublicationNodesAreTheServingNodes: a tunnel's DNS records (publication_edge_nodes) name only the per-region
// serving set, so a region with 6 ready nodes contributes its first 4 by id and the other region its 2.
func TestPublicationNodesAreTheServingNodes(t *testing.T) {
	f := newWildcardFixture(t)
	regionA, regionB := "pn_a_"+f.stamp, "pn_b_"+f.stamp
	next := 1
	for region, count := range map[string]int{regionA: 6, regionB: 2} {
		f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Serving',true)`, region)
		t.Cleanup(func() {
			f.exec(`DELETE FROM paperboat.managed_node_authority WHERE region_id=$1`, region)
			f.exec(`DELETE FROM paperboat.operator_regions WHERE id=$1`, region)
		})
		for i := range count {
			f.node(fmt.Sprintf("p%s%d", region[3:4], i+1), fmt.Sprintf("198.51.100.%d", next), "", region, `process_epoch='epoch_'||id,carrier_endpoint_host='edge.example.test',carrier_endpoint_tcp_port=27443,carrier_endpoint_quic_port=27444,carrier_server_spki_sha256='sha256:'||repeat('3',64),carrier_server_certificate_chain_pem='fixture certificate'`)
			next++
		}
	}
	account, tunnel := "pn_"+f.stamp, "pn_t_"+f.stamp
	f.workspace("pn", regionA, regionB)
	endpoint := testutil.EndpointID("pn:" + f.stamp)
	host := testutil.PublicationMachine(t, f.store.SQL(), account)
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.user_machines WHERE id=$1`, host) })
	f.exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,$5,'actor')`, tunnel, account, endpoint, "https://"+testutil.EndpointLabel(endpoint)+".example.test", host)
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.tunnels WHERE id=$1`, tunnel) })

	got := queryIDs(t, f.store.SQL(), `SELECT node_id FROM paperboat.publication_edge_nodes('tunnel',$1)`, tunnel)
	var want []string
	for _, region := range []string{regionA, regionB} {
		want = append(want, queryIDs(t, f.store.SQL(), `SELECT id FROM paperboat.control_tunnel_nodes WHERE region=$1 ORDER BY id LIMIT $2`, region, edgeset.ServingNodesPerRegion)...)
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(got) != 6 || !slices.Equal(got, want) {
		t.Fatalf("publication_edge_nodes = %v, want the serving nodes %v", got, want)
	}

	var src string
	if err := f.store.SQL().QueryRow(`SELECT p.prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='paperboat' AND p.proname='publication_edge_nodes' AND p.pronargs=2`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, fmt.Sprintf("serving_rank<=%d", edgeset.ServingNodesPerRegion)) {
		t.Fatalf("publication_edge_nodes does not cap regions at edgeset.ServingNodesPerRegion (%d); keep the SQL digit in step with the Go constant", edgeset.ServingNodesPerRegion)
	}
}

func queryIDs(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}
