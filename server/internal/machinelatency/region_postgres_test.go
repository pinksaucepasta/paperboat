package machinelatency_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machinelatency"
)

// A machine's measurements are listed as the best RTT per region and role, named by the region (its display name for
// Paperboat's), never by node.
func TestNetworkLatencyIsPerRegion(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e = db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "latr-owner-"+suffix, "latr-machine-"+suffix, "latr-env-"+suffix
	r1, r2 := "latr_one_"+suffix, "latr_two_"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() { store.Close() })
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.user_machines WHERE id=$1`,
			`DELETE FROM paperboat.control_environments WHERE id=$2`,
			`DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $3`,
			`DELETE FROM paperboat.operator_regions WHERE id IN ($4,$5)`,
			`DELETE FROM paperboat.users WHERE id=$6`,
		} {
			_, _ = store.SQL().ExecContext(ctx, strings.NewReplacer("$1", "'"+machine+"'", "$2", "'"+environment+"'", "$3", "'latr-%"+suffix+"'", "$4", "'"+r1+"'", "$5", "'"+r2+"'", "$6", "'"+owner+"'").Replace(q))
		}
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Region One',true),($2,'Region Two',true)`, r1, r2)
	for _, node := range []struct {
		id, region, role string
		rtt              int
	}{{"latr-e1-" + suffix, r1, "edge", 30}, {"latr-e2-" + suffix, r1, "edge", 9}, {"latr-e3-" + suffix, r2, "edge", 50}, {"latr-r1-" + suffix, r1, "relay", 20}} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at)
VALUES($1,'p','v1','epoch_1',$2,'fd-1',ARRAY[$3],ARRAY['http2'],10,0,now(),now()+interval '1 hour')`, node.id, node.region, node.role)
		exec(`INSERT INTO paperboat.machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) VALUES($1,$2,$3,$4,now())`, machine, node.id, node.role, node.rtt)
	}
	items, err := (&machinelatency.Service{DB: store.SQL()}).List(ctx, machine)
	if err != nil {
		t.Fatal(err)
	}
	want := []machinelatency.Item{{Region: r1, Name: "Region One", Role: "edge", RTTMillis: 9}, {Region: r2, Name: "Region Two", Role: "edge", RTTMillis: 50}, {Region: r1, Name: "Region One", Role: "relay", RTTMillis: 20}}
	if len(items) != len(want) {
		t.Fatalf("items=%+v, want one per region and role %+v", items, want)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Fatalf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
	raw, err := json.Marshal(items)
	if err != nil || strings.Contains(string(raw), "node") || strings.Contains(string(raw), "latr-e") {
		t.Fatalf("latency JSON names a node: %s %v", raw, err)
	}
}
