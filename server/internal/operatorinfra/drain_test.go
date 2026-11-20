package operatorinfra

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// drainFixture enrolls a managed edge node in a new region and gives it one machine holding a live carrier.
func drainFixture(t *testing.T) (s *Service, store *db.DB, admin string, node Node, machine string) {
	t.Helper()
	store = infraTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin = "infra_admin_" + x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, admin)
	s = NewService(store, "operator-infrastructure-test-key-32-bytes")
	region, err := s.MutateRegion(ctx, admin, "region-create-"+x, RegionMutation{Action: "create", ID: "region_" + x, DisplayName: "Test region", Reason: "case:test"})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := s.MutateNode(ctx, admin, "node-enroll-"+x, NodeMutation{Action: "enroll", NodeID: "managed_" + x, RegionID: region.ID, EdgePool: "managed:" + region.ID, FailureDomain: "zone-a", EndpointHost: "edge.example.test", EndpointTCPPort: 24443, EndpointQUICPort: 24444, Reason: "case:test", Roles: []string{"edge"}, Transports: []string{"http3"}, CapacityLimit: 8, UsagePublicKey: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	user, env, route := "drain_user_"+x, "drain_env_"+x, "drain_route_"+x
	machine = "drain_m_" + x
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, user, env)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080)`, route, env)
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active) SELECT $1,m.id,m.user_id,1,1,'process',1,$2,'epoch',m.public_identity_key,1,$1,$1,'{}',now()+interval '1 hour',true FROM paperboat.user_machines m WHERE m.id=$3`, route, enrolled.Node.ID, machine)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_environments WHERE id=$1`, env)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	return s, store, admin, enrolled.Node, machine
}

func topicGenerations(t *testing.T, store *db.DB, machine string) int {
	t.Helper()
	var n int
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic IN ('tunnels','previews')`, machine).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOperatorNodeListShowsCarriers(t *testing.T) {
	s, store, admin, node, _ := drainFixture(t)
	ctx := context.Background()
	page, err := s.ListNodes(ctx, admin, node.RegionID, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Carriers != 1 {
		t.Fatalf("listed carriers: %+v err=%v", page.Items, err)
	}
	if item := page.Items[0]; item.LoadPercent != nil || item.InDNS != nil || item.IngressVerified {
		t.Fatalf("a node that reported nothing shows load %v in_dns %v ingress_verified %v", item.LoadPercent, item.InDNS, item.IngressVerified)
	}
	for _, query := range []string{
		`INSERT INTO paperboat.node_capacity(node_id,role,process_epoch,cpu_sustained_percent,reported_at) VALUES($1,'edge','epoch',42,now())`,
		`INSERT INTO paperboat.edge_wildcard_nodes(node_id,out_for_load) VALUES($1,true)`,
		`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now() WHERE id=$1`,
	} {
		if _, err = store.SQL().ExecContext(ctx, query, node.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.node_capacity WHERE node_id=$1`, node.ID)
	})
	page, err = s.ListNodes(ctx, admin, node.RegionID, "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("listed: %+v err=%v", page.Items, err)
	}
	if item := page.Items[0]; item.LoadPercent == nil || *item.LoadPercent != 42 || item.InDNS == nil || *item.InDNS || !item.IngressVerified {
		t.Fatalf("load %v in_dns %v ingress_verified %v, want 42, false, true", item.LoadPercent, item.InDNS, item.IngressVerified)
	}
	drained, err := s.MutateNode(ctx, admin, "drain-"+node.ID, NodeMutation{Action: "drain", NodeID: node.ID, Revision: node.Revision, Reason: "case:test"})
	if err != nil || drained.Node.Carriers != 1 || drained.Node.State != "draining" {
		t.Fatalf("a drained node keeps its carriers until they end: %+v err=%v", drained.Node, err)
	}
}

func TestOperatorDrainMoveWakesAttachedMachines(t *testing.T) {
	s, store, admin, node, machine := drainFixture(t)
	ctx := context.Background()
	if topicGenerations(t, store, machine) != 0 {
		t.Fatal("machine woke before any drain")
	}
	// A plain drain wakes nobody: existing connections finish and the machine looks again when its attachment expires.
	drained, err := s.MutateNode(ctx, admin, "drain-"+node.ID, NodeMutation{Action: "drain", NodeID: node.ID, Revision: node.Revision, Reason: "case:test"})
	if err != nil || topicGenerations(t, store, machine) != 0 {
		t.Fatalf("plain drain woke the machine: %v", err)
	}
	moved, err := s.MutateNode(ctx, admin, "drain-move-"+node.ID, NodeMutation{Action: "drain-move", NodeID: node.ID, Revision: drained.Node.Revision, Reason: "case:test"})
	if err != nil || moved.Node.State != "draining" || moved.Node.Ready {
		t.Fatalf("drain-move: %+v err=%v", moved.Node, err)
	}
	if got := topicGenerations(t, store, machine); got != 2 {
		t.Fatalf("drain and move woke the machine on %d topics, want tunnels and previews", got)
	}
	var secs float64
	if err = store.SQL().QueryRowContext(ctx, `SELECT extract(epoch from drain_deadline-now()) FROM paperboat.control_tunnel_nodes WHERE id=$1`, node.ID).Scan(&secs); err != nil || secs > 91 || secs < 60 {
		t.Fatalf("the deadline did not shorten to the move grace: %v s err=%v", secs, err)
	}
	var audits int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='operator.infrastructure.node.drain-move' AND resource_id=$1`, node.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("drain-move audit rows=%d err=%v", audits, err)
	}
	// Drain and move of a retired node is refused.
	retired, err := s.MutateNode(ctx, admin, "retire-"+node.ID, NodeMutation{Action: "retire", NodeID: node.ID, Revision: moved.Node.Revision, Reason: "case:test"})
	if err != nil || retired.Node.State != "retired" {
		t.Fatalf("retire: %+v err=%v", retired.Node, err)
	}
	if _, err = s.MutateNode(ctx, admin, "drain-move-again-"+node.ID, NodeMutation{Action: "drain-move", NodeID: node.ID, Revision: retired.Node.Revision, Reason: "case:test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("drain-move of a retired node: %v", err)
	}
}
