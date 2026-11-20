package nodedrain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type fixture struct {
	t     *testing.T
	store *db.DB
	stamp string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, store: store, stamp: fmt.Sprint(time.Now().UnixNano())}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

func (f *fixture) node(name, roles string) string {
	id := "nd_" + name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,registry_expires_at,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at) VALUES($1,'test','1.0','epoch','ready',true,now(),$2::text[],now()+interval '1 hour','test','test',ARRAY['http3','http2'],100,0,now())`, id, roles)
	f.t.Cleanup(func() { f.exec(`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id) })
	return id
}

func (f *fixture) user(name string) string {
	id := "nd_user_" + name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test")
	f.t.Cleanup(func() { f.exec(`DELETE FROM paperboat.users WHERE id=$1`, id) })
	return id
}

// machine adds a machine of a new account and, when node is set, a carrier attachment on it that expires in expiresIn.
func (f *fixture) machine(name, node string, expiresIn time.Duration) string {
	account := f.user(name)
	env, machine, route := "nd_env_"+name+"_"+f.stamp, "nd_m_"+name+"_"+f.stamp, "nd_r_"+name+"_"+f.stamp
	f.exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, account)
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, account, env)
	f.t.Cleanup(func() {
		f.exec(`DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		f.exec(`DELETE FROM paperboat.control_environments WHERE id=$1`, env)
	})
	if node != "" {
		f.exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080)`, route, env)
		f.exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active) SELECT $1,m.id,m.user_id,1,1,'process',1,$2,'epoch',coalesce(m.public_identity_key, 'fixture-key'),1,'session_00000000-0000-4000-8000-000000000001','session_00000000-0000-4000-8000-000000000001','{}',now()+make_interval(secs=>$4::float8),true FROM paperboat.user_machines m WHERE m.id=$3`, route, node, machine, expiresIn.Seconds())
	}
	return machine
}

func (f *fixture) begin(node string, move bool) error {
	return f.store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error { return Begin(ctx, tx, node, move) })
}

func (f *fixture) deadline(node string) (state string, ready bool, in time.Duration) {
	f.t.Helper()
	var secs *float64
	if err := f.store.SQL().QueryRowContext(context.Background(), `SELECT state,ready,extract(epoch from drain_deadline-now()) FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&state, &ready, &secs); err != nil {
		f.t.Fatal(err)
	}
	if secs != nil {
		in = time.Duration(*secs * float64(time.Second))
	}
	return
}

func near(got, want time.Duration) bool { return got > want-20*time.Second && got <= want+time.Second }

func TestBeginDrainSetsStateAndGrace(t *testing.T) {
	f := newFixture(t)
	node := f.node("grace", "{edge}")
	if err := f.begin(node, false); err != nil {
		t.Fatal(err)
	}
	if state, ready, in := f.deadline(node); state != "draining" || ready || !near(in, DrainGrace) {
		t.Fatalf("after drain: %s ready=%v deadline in %v", state, ready, in)
	}
	// Draining again does not extend the deadline; Drain and move shortens it, and a later Drain does not lengthen it back.
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET drain_deadline=now()+interval '5 minutes' WHERE id=$1`, node)
	if err := f.begin(node, false); err != nil {
		t.Fatal(err)
	}
	if _, _, in := f.deadline(node); !near(in, 5*time.Minute) {
		t.Fatalf("a second drain moved the deadline to %v", in)
	}
	if err := f.begin(node, true); err != nil {
		t.Fatal(err)
	}
	if _, _, in := f.deadline(node); !near(in, MoveGrace) {
		t.Fatalf("drain and move deadline in %v, want %v", in, MoveGrace)
	}
	if err := f.begin(node, false); err != nil {
		t.Fatal(err)
	}
	if _, _, in := f.deadline(node); !near(in, MoveGrace) {
		t.Fatalf("a drain after move lengthened the deadline to %v", in)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='retired' WHERE id=$1`, node)
	if err := f.begin(node, false); !errors.Is(err, ErrRetired) {
		t.Fatalf("drain of a retired node: %v", err)
	}
	if err := f.begin("nd_missing_"+f.stamp, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drain of a missing node: %v", err)
	}
}

func (f *fixture) handovers(node string) []string {
	f.t.Helper()
	rows, err := f.store.SQL().QueryContext(context.Background(), `SELECT account_id,close_at-now()>interval '0' AND close_at-now()<=make_interval(secs=>$2::float8) FROM paperboat.relay_pool_handovers WHERE node_id=$1`, node, MoveGrace.Seconds())
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var account string
		var withinGrace bool
		if err = rows.Scan(&account, &withinGrace); err != nil || !withinGrace {
			f.t.Fatalf("handover for %s within the move grace=%v err=%v", account, withinGrace, err)
		}
		out = append(out, account)
	}
	sort.Strings(out)
	return out
}

func TestBeginMoveWritesRelayHandoverRows(t *testing.T) {
	f := newFixture(t)
	a, b, stale := f.user("a"), f.user("b"), f.user("stale")
	relay := f.node("relay", "{relay}")
	edge := f.node("edge", "{edge}")
	f.exec(`INSERT INTO paperboat.relay_subject_sets(node_id,process_epoch,node_generation,digest,subjects) VALUES($1,'epoch',1,repeat('a',64),$2::jsonb),($1,'old_epoch',1,repeat('b',64),$3::jsonb)`, relay, fmt.Sprintf(`[{"account_id":%q,"endpoint_id":"e1"},{"account_id":%q,"endpoint_id":"e2"},{"account_id":%q,"endpoint_id":"e3"}]`, a, b, a), fmt.Sprintf(`[{"account_id":%q,"endpoint_id":"e4"}]`, stale))
	// A plain drain and a move on an edge write no handovers; a move on the relay lists each served account once.
	if err := f.begin(relay, false); err != nil {
		t.Fatal(err)
	}
	if err := f.begin(edge, true); err != nil {
		t.Fatal(err)
	}
	if got := f.handovers(relay); len(got) != 0 {
		t.Fatalf("handovers after a plain drain: %v", got)
	}
	if got := f.handovers(edge); len(got) != 0 {
		t.Fatalf("handovers for an edge node: %v", got)
	}
	if err := f.begin(relay, true); err != nil {
		t.Fatal(err)
	}
	want := []string{a, b}
	sort.Strings(want)
	if got := f.handovers(relay); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("handovers = %v, want %v (current epoch only)", got, want)
	}
	if err := f.begin(relay, true); err != nil {
		t.Fatalf("a repeated move: %v", err)
	}
}

func TestResumeClearsDeadlineAndHandovers(t *testing.T) {
	f := newFixture(t)
	a := f.user("a")
	relay := f.node("resume", "{relay}")
	f.exec(`INSERT INTO paperboat.relay_subject_sets(node_id,process_epoch,node_generation,digest,subjects) VALUES($1,'epoch',1,repeat('a',64),$2::jsonb)`, relay, fmt.Sprintf(`[{"account_id":%q,"endpoint_id":"e1"}]`, a))
	if err := f.begin(relay, true); err != nil {
		t.Fatal(err)
	}
	if len(f.handovers(relay)) != 1 {
		t.Fatal("no handover to cancel")
	}
	err := f.store.InTx(t.Context(), func(ctx context.Context, tx *db.Tx) error { return Resume(ctx, tx, relay) })
	if err != nil {
		t.Fatal(err)
	}
	var deadline *time.Time
	var state string
	if err = f.store.SQL().QueryRowContext(t.Context(), `SELECT state,drain_deadline FROM paperboat.control_tunnel_nodes WHERE id=$1`, relay).Scan(&state, &deadline); err != nil || state != "registered" || deadline != nil {
		t.Fatalf("after resume: %s %v %v", state, deadline, err)
	}
	if got := f.handovers(relay); len(got) != 0 {
		t.Fatalf("handovers after resume: %v", got)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='retired' WHERE id=$1`, relay)
	if err = f.store.InTx(t.Context(), func(ctx context.Context, tx *db.Tx) error { return Resume(ctx, tx, relay) }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resume of a retired node: %v", err)
	}
}

func TestWakePublishesToAttachedMachinesOnly(t *testing.T) {
	f := newFixture(t)
	node, other := f.node("wake", "{edge}"), f.node("wake_other", "{edge}")
	attached := f.machine("attached", node, time.Hour)
	expired := f.machine("expired", node, -time.Minute)
	elsewhere := f.machine("elsewhere", other, time.Hour)
	idle := f.machine("idle", "", 0)
	old := wakeBatch
	wakeBatch = 1 // exercises paging
	t.Cleanup(func() { wakeBatch = old })
	if err := Wake(t.Context(), f.store.Pool(), node); err != nil {
		t.Fatal(err)
	}
	for machine, want := range map[string]int{attached: 2, expired: 0, elsewhere: 0, idle: 0} {
		var got int
		if err := f.store.SQL().QueryRowContext(t.Context(), `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic IN ('tunnels','previews')`, machine).Scan(&got); err != nil || got != want {
			t.Fatalf("machine %s woke on %d topics, want %d (err %v)", machine, got, want, err)
		}
	}
}
