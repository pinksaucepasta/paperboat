package app

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

type recordedWildcards struct {
	mu    sync.Mutex
	calls []tunnelcert.DesiredAddressPublication
}

func (p *recordedWildcards) Reconcile(_ context.Context, d tunnelcert.DesiredAddressPublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, d)
	return nil
}

func (p *recordedWildcards) reset() { p.calls = nil }

type wildcardFixture struct {
	t        *testing.T
	store    *db.DB
	stamp    string
	public   *recordedWildcards
	runtime  *recordedWildcards
	wildcard *edgeWildcards
	now      time.Time
}

func newWildcardFixture(t *testing.T) *wildcardFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
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
	f := &wildcardFixture{t: t, store: store, stamp: fmt.Sprint(time.Now().UnixNano()), public: &recordedWildcards{}, runtime: &recordedWildcards{}, now: time.Now()}
	cfg := config.Config{RuntimeBaseDomain: "runtime.example.test", Preview: config.Preview{BaseDomain: "preview.example.test"}}
	cfg.NodeCapacity = config.Default().NodeCapacity
	cfg.Certificates.ManageEdgeWildcards = true
	f.wildcard = newEdgeWildcards(store, cfg, f.public, f.runtime)
	f.wildcard.now = func() time.Time { return f.now }
	f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'On',true),($2,'Off',false)`, "wc_on_"+f.stamp, "wc_off_"+f.stamp)
	t.Cleanup(func() {
		f.exec(`DELETE FROM paperboat.operator_regions WHERE id IN ($1,$2)`, "wc_on_"+f.stamp, "wc_off_"+f.stamp)
	})
	return f
}

func (f *wildcardFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

// node adds a ready, verified managed edge named n<name>; mutate then changes it with a SQL SET clause.
func (f *wildcardFixture) node(name, v4, v6, region, mutate string) string {
	id := "nwc_" + name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,registry_expires_at,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at,public_ingress_ipv4,public_ingress_ipv6,public_ingress_verified_at) VALUES($1,'test','1.0','epoch','ready',true,now(),ARRAY['edge'],now()+interval '1 hour','test','test',ARRAY['http3','http2'],100,0,now(),nullif($2,'')::inet,nullif($3,'')::inet,now())`, id, v4, v6)
	if region != "" {
		f.exec(`INSERT INTO paperboat.managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,decode(md5($1)||md5($1),'hex'),'\x00','{}')`, id, region)
	}
	if mutate != "" {
		f.exec(`UPDATE paperboat.control_tunnel_nodes SET `+mutate+` WHERE id=$1`, id)
	}
	f.t.Cleanup(func() {
		f.exec(`DELETE FROM paperboat.managed_node_authority WHERE node_id=$1`, id)
		f.exec(`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
	})
	return id
}

func (f *wildcardFixture) sync() {
	f.t.Helper()
	if err := f.wildcard.Sync(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wildcardFixture) want(addresses ...string) {
	f.t.Helper()
	for name, got := range map[string][]tunnelcert.DesiredAddressPublication{"*.preview.example.test": f.public.calls, "*.runtime.example.test": f.runtime.calls} {
		if len(got) != 1 {
			f.t.Fatalf("%s: %d publications, want 1", name, len(got))
		}
		d := got[0]
		if d.Hostname != name || d.Owner != "edge_wildcard:"+name || !slices.Equal(d.Addresses, addresses) || d.ReadinessVersion != "edge-heartbeat" || !d.IPv6ReachabilityVerified {
			f.t.Fatalf("%s published %+v, want addresses %v", name, d, addresses)
		}
		if until := d.ValidUntil.Sub(d.ObservedAt); until != edgeWildcardValidity {
			f.t.Fatalf("%s valid for %v", name, until)
		}
	}
}

func TestEdgeWildcardPublishesReadyNodesOnly(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	f.node("a", "203.0.113.1", "2001:db8::1", on, "")
	f.node("b", "203.0.113.2", "", on, "")
	f.node("registered", "203.0.113.10", "", on, "state='registered',ready=false")
	f.node("stale", "203.0.113.11", "", on, "last_heartbeat_at=now()-interval '1 minute'")
	f.node("unverified", "203.0.113.12", "", on, "public_ingress_verified_at=NULL")
	f.node("disabled", "203.0.113.13", "", "wc_off_"+f.stamp, "")
	f.node("retired", "203.0.113.14", "", on, "")
	f.exec(`UPDATE paperboat.managed_node_authority SET retired_at=now() WHERE node_id=$1`, "nwc_retired_"+f.stamp)
	f.node("selfhosted", "203.0.113.15", "", "", "") // no managed authority row: an owner's own server
	f.node("relay", "203.0.113.16", "", on, "roles=ARRAY['relay']")
	f.sync()
	f.want("203.0.113.1", "2001:db8::1", "203.0.113.2")
}

func TestEdgeWildcardDropsDrainingNode(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	f.node("a", "203.0.113.1", "", on, "")
	drained := f.node("b", "203.0.113.2", "", on, "")
	f.sync()
	f.want("203.0.113.1", "203.0.113.2")
	f.public.reset()
	f.runtime.reset()
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,drain_deadline=now()+interval '10 minutes' WHERE id=$1`, drained)
	f.sync()
	f.want("203.0.113.1")
}

func TestEdgeWildcardNeverPublishesEmptySet(t *testing.T) {
	f := newWildcardFixture(t)
	id := f.node("a", "203.0.113.1", "", "wc_on_"+f.stamp, "")
	f.sync()
	f.want("203.0.113.1")
	f.public.reset()
	f.runtime.reset()
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false WHERE id=$1`, id)
	f.sync()
	if len(f.public.calls)+len(f.runtime.calls) != 0 {
		t.Fatalf("an empty set was published: %+v %+v", f.public.calls, f.runtime.calls)
	}
}

func TestEdgeWildcardRefreshesAfterWindow(t *testing.T) {
	f := newWildcardFixture(t)
	f.node("a", "203.0.113.1", "", "wc_on_"+f.stamp, "")
	f.sync()
	f.want("203.0.113.1")
	f.public.reset()
	f.runtime.reset()
	f.now = f.now.Add(edgeWildcardRefresh - time.Second)
	f.sync()
	if len(f.public.calls)+len(f.runtime.calls) != 0 {
		t.Fatal("an unchanged set was published again inside the refresh window")
	}
	f.now = f.now.Add(2 * time.Second)
	f.sync()
	f.want("203.0.113.1")
}

// setLoad stores a node's latest self-report: sustained CPU and, when conns >= 0, its connection count.
func (f *wildcardFixture) setLoad(id string, cpu float64, conns int, reportedAgo time.Duration) {
	f.t.Helper()
	f.exec(`INSERT INTO paperboat.node_capacity(node_id,role,process_epoch,cpu_sustained_percent,connections,reported_at) VALUES($1,'edge','epoch',$2,nullif($3,-1),now()-make_interval(secs=>$4::float8))
ON CONFLICT (node_id) DO UPDATE SET cpu_sustained_percent=excluded.cpu_sustained_percent,connections=excluded.connections,reported_at=excluded.reported_at`, id, cpu, conns, reportedAgo.Seconds())
	f.t.Cleanup(func() { f.exec(`DELETE FROM paperboat.node_capacity WHERE node_id=$1`, id) })
}

func loadOf(v float64) *float64 { return &v }

func ids(nodes []wildcardNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.id)
	}
	return out
}

func TestWildcardLoadHysteresis(t *testing.T) {
	steady := wildcardNode{id: "b", region: "r", load: loadOf(10)}
	state := map[string]bool{}
	for _, step := range []struct {
		load *float64
		in   bool
	}{{loadOf(84), true}, {loadOf(85), false}, {loadOf(75), false}, {loadOf(70), false}, {loadOf(69), true}, {loadOf(84), true}, {nil, true}} {
		var kept []wildcardNode
		kept, state = selectWildcardNodes([]wildcardNode{{id: "a", region: "r", load: step.load}, steady}, state, 85, 70)
		if got := slices.Contains(ids(kept), "a"); got != step.in {
			t.Fatalf("load %v: node in the set = %v, want %v", step.load, got, step.in)
		}
		if !slices.Contains(ids(kept), "b") {
			t.Fatalf("load %v: the steady node left the set", step.load)
		}
	}
}

func TestWildcardNeverEmptiesARegion(t *testing.T) {
	nodes := []wildcardNode{
		{id: "a", region: "r1", load: loadOf(90)}, {id: "b", region: "r1", load: loadOf(95)}, {id: "c", region: "r1", load: loadOf(90)},
		{id: "d", region: "r2", load: loadOf(10)}, {id: "e", region: "r2", load: loadOf(99)},
	}
	kept, state := selectWildcardNodes(nodes, map[string]bool{}, 85, 70)
	if !slices.Equal(ids(kept), []string{"a", "d"}) {
		t.Fatalf("kept %v: want the least loaded of r1 (tie by id) and the unaffected r2 node", ids(kept))
	}
	if state["a"] || !state["b"] || !state["c"] || state["d"] || !state["e"] {
		t.Fatalf("decisions %v", state)
	}
}

func TestWildcardLoadStatePersistsAcrossWorkers(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	a, b := f.node("a", "203.0.113.1", "", on, ""), f.node("b", "203.0.113.2", "", on, "")
	f.setLoad(a, 10, -1, 0)
	f.setLoad(b, 90, -1, 0)
	f.sync()
	f.want("203.0.113.1")
	var out bool
	if err := f.store.SQL().QueryRow(`SELECT out_for_load FROM paperboat.edge_wildcard_nodes WHERE node_id=$1`, b).Scan(&out); err != nil || !out {
		t.Fatalf("stored decision for the overloaded node: %v %v", out, err)
	}
	// A new worker (a leader change) starts with no memory: 75 % is between the thresholds, so the stored decision holds.
	f.setLoad(b, 75, -1, 0)
	second := *f.wildcard
	second.reconciled = nil
	f.wildcard = &second
	f.public.reset()
	f.runtime.reset()
	f.sync()
	f.want("203.0.113.1")
	f.setLoad(b, 60, -1, 0)
	f.public.reset()
	f.runtime.reset()
	f.sync()
	f.want("203.0.113.1", "203.0.113.2")
	// A node that leaves the ready set forgets its decision.
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false WHERE id=$1`, b)
	f.sync()
	var rows int
	if err := f.store.SQL().QueryRow(`SELECT count(*) FROM paperboat.edge_wildcard_nodes WHERE node_id=$1`, b).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a drained node kept its decision: %d %v", rows, err)
	}
}

func TestNodeLoadPercentUsesTheWorstReportedFigure(t *testing.T) {
	f := newWildcardFixture(t)
	id := f.node("a", "203.0.113.1", "", "wc_on_"+f.stamp, "capacity_limit=100,capacity_used=20")
	load := func() *float64 {
		f.t.Helper()
		var v *float64
		if err := f.store.SQL().QueryRow(`SELECT paperboat.node_load_percent($1)::float8`, id).Scan(&v); err != nil {
			f.t.Fatal(err)
		}
		return v
	}
	if load() != nil {
		t.Fatal("a node with no report has a load")
	}
	f.setLoad(id, 30, 60, 0)
	if v := load(); v == nil || *v != 60 {
		t.Fatalf("connections 60 of 100 against cpu 30: %v", v)
	}
	f.setLoad(id, 30, -1, 0) // no connection count: the node's own capacity_used (20)
	if v := load(); v == nil || *v != 30 {
		t.Fatalf("cpu 30 against capacity_used 20: %v", v)
	}
	f.setLoad(id, 5, 90, 0)
	if v := load(); v == nil || *v != 90 {
		t.Fatalf("connections 90: %v", v)
	}
	f.setLoad(id, 99, 90, 3*time.Minute)
	if load() != nil {
		t.Fatal("a report older than two minutes still counts")
	}
}
