package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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
		f.exec(`UPDATE paperboat.control_tunnel_nodes SET region=$2 WHERE id=$1`, id, region) // the serving set ranks by this column
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
	drained := f.node("b", "203.0.113.2", "2001:db8::2", on, "")
	f.sync()
	f.want("203.0.113.1", "203.0.113.2", "2001:db8::2")
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

func TestSelectByLoadTable(t *testing.T) {
	node := func(id, region string, load *float64) loadNode { return loadNode{id: id, region: region, load: load} }
	steady := node("b", "r", loadOf(10))
	for _, tc := range []struct {
		name string
		prev bool // the previous decision for node a
		load *float64
		out  bool
	}{
		{"84 stays in", false, loadOf(84), false},
		{"85 goes out", false, loadOf(85), true},
		{"75 stays out", true, loadOf(75), true},
		{"70 stays out", true, loadOf(70), true},
		{"69 comes back", true, loadOf(69), false},
		{"84 does not flap back out", false, loadOf(84), false},
		{"unknown load stays in", false, nil, false},
		{"unknown load of a node that was out comes back", true, nil, false},
	} {
		got, _ := selectByLoad([]loadNode{node("a", "r", tc.load), steady}, map[string]bool{"a": tc.prev}, 85, 70)
		if got["a"] != tc.out || got["b"] {
			t.Errorf("%s: decisions %v, want a out=%v and b in", tc.name, got, tc.out)
		}
	}
	// A region is never left empty: the least loaded node of r1 (tie by input order) stays; r2 is decided on its own.
	nodes := []loadNode{
		node("a", "r1", loadOf(90)), node("b", "r1", loadOf(95)), node("c", "r1", loadOf(90)),
		node("d", "r2", loadOf(10)), node("e", "r2", loadOf(99)),
	}
	got, _ := selectByLoad(nodes, map[string]bool{}, 85, 70)
	if got["a"] || !got["b"] || !got["c"] || got["d"] || !got["e"] {
		t.Fatalf("regions: decisions %v", got)
	}
	// The same rule on relays: a single relay of a region stays however loaded it is.
	if got, _ = selectByLoad([]loadNode{node("r1", "x", loadOf(100))}, map[string]bool{"r1": true}, 85, 70); got["r1"] {
		t.Fatalf("the last relay of a region was left out: %v", got)
	}
}

// The node kept because its whole region was over is still over: the state a round starts from is the threshold
// decision, not the forced one, so it returns only below the in threshold while another node serves.
func TestSelectByLoadForcedSurvivorKeepsThreshold(t *testing.T) {
	node := func(id string, load float64) loadNode { return loadNode{id: id, region: "r", load: loadOf(load)} }
	held, over := selectByLoad([]loadNode{node("a", 90), node("b", 88)}, map[string]bool{}, 85, 70)
	if !held["a"] || held["b"] || !over["a"] || !over["b"] {
		t.Fatalf("round 1: held %v over %v, want b kept in rotation while both stay over", held, over)
	}
	held, over = selectByLoad([]loadNode{node("a", 60), node("b", 80)}, over, 85, 70)
	if held["a"] || !held["b"] || over["a"] || !over["b"] {
		t.Fatalf("round 2: held %v over %v, want a serving and b held out (80 is not below 70)", held, over)
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
	if err := f.store.SQL().QueryRow(`SELECT out_for_load FROM paperboat.node_load_out WHERE node_id=$1`, b).Scan(&out); err != nil || !out {
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
	if err := f.store.SQL().QueryRow(`SELECT count(*) FROM paperboat.node_load_out WHERE node_id=$1`, b).Scan(&rows); err != nil || rows != 0 {
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

// relay adds a ready managed relay named name to region with the given sustained CPU (a negative cpu: no report).
func (f *wildcardFixture) relay(name, region string, cpu float64) string {
	id := f.node(name, "", "", region, "roles=ARRAY['relay','peer_relay']")
	if cpu >= 0 {
		f.setRelayLoad(id, cpu)
	}
	return id
}

func (f *wildcardFixture) setRelayLoad(id string, cpu float64) {
	f.t.Helper()
	f.exec(`INSERT INTO paperboat.node_capacity(node_id,role,process_epoch,cpu_sustained_percent,reported_at) VALUES($1,'relay','epoch',$2,now())
ON CONFLICT (node_id) DO UPDATE SET cpu_sustained_percent=excluded.cpu_sustained_percent,reported_at=excluded.reported_at`, id, cpu)
	f.t.Cleanup(func() { f.exec(`DELETE FROM paperboat.node_capacity WHERE node_id=$1`, id) })
}

func (f *wildcardFixture) relayLoad() *relayLoad {
	cfg := config.Config{}
	cfg.NodeCapacity = config.Default().NodeCapacity
	return newRelayLoad(f.store, cfg)
}

// outNodes lists the nodes the relay load rule currently holds out.
func (f *wildcardFixture) outNodes() []string {
	f.t.Helper()
	rows, err := f.store.SQL().Query(`SELECT node_id FROM paperboat.node_load_out WHERE out_for_load AND node_id LIKE '%'||$1 ORDER BY node_id`, f.stamp)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestRelayLoadNeverLeavesARegionEmpty(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	a, b, c := f.relay("a", on, 90), f.relay("b", on, 95), f.relay("c", on, 88)
	solo := f.relay("solo", "wc_off_"+f.stamp, 99) // a disabled region: not decided on at all
	if err := f.relayLoad().Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.outNodes(); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("held out %v, want %v: all three are above 85, the least loaded (%s) stays", got, []string{a, b}, c)
	}
	var rows int
	if err := f.store.SQL().QueryRow(`SELECT count(*) FROM paperboat.node_load_out WHERE node_id=$1`, solo).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a relay in a disabled region was decided on: %d %v", rows, err)
	}
	// Next round c (88 -> 80) was kept only because all three were over: it is still judged against 70, so a (79), now
	// the least loaded of three over nodes, is the survivor, not c.
	f.setRelayLoad(a, 79)
	f.setRelayLoad(c, 80)
	if err := f.relayLoad().Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.outNodes(); !slices.Equal(got, []string{b, c}) {
		t.Fatalf("second round held out %v, want %v: the survivor is %s (79), not %s (80)", got, []string{b, c}, a, c)
	}
}

func TestRelayLoadStatePersistsAcrossWorkers(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	f.relay("a", on, 10)
	b := f.relay("b", on, 90)
	selfHosted := f.node("own", "", "", "", "roles=ARRAY['relay']") // no managed authority row
	f.setRelayLoad(selfHosted, 99)
	sync := func() {
		f.t.Helper()
		if err := f.relayLoad().Sync(context.Background()); err != nil { // a new worker each time: no memory
			f.t.Fatal(err)
		}
	}
	sync()
	if got := f.outNodes(); !slices.Equal(got, []string{b}) {
		t.Fatalf("held out %v, want only %s (a self-hosted owner's relay never takes part)", got, b)
	}
	f.setRelayLoad(b, 75) // between the thresholds: the stored decision holds
	sync()
	if got := f.outNodes(); !slices.Equal(got, []string{b}) {
		t.Fatalf("at 75 held out %v, want %s still out", got, b)
	}
	f.setRelayLoad(b, 60)
	sync()
	if got := f.outNodes(); len(got) != 0 {
		t.Fatalf("at 60 held out %v, want none", got)
	}
	f.setRelayLoad(b, 90)
	sync()
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false WHERE id=$1`, b)
	sync() // a node that leaves the ready set forgets its decision
	var rows int
	if err := f.store.SQL().QueryRow(`SELECT count(*) FROM paperboat.node_load_out WHERE node_id=$1`, b).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a drained relay kept its decision: %d %v", rows, err)
	}
}

// dual adds a dual-stack node of the family's numbering: n<i> has 203.0.113.<i> and 2001:db8::<i>.
func (f *wildcardFixture) dual(name string, i int, region, mutate string) string {
	return f.node(name, fmt.Sprintf("203.0.113.%d", i), fmt.Sprintf("2001:db8::%d", i), region, mutate)
}

// A node held out for load takes its A and its AAAA out of the answer together, and brings both back (85 out, 70 in).
func TestEdgeWildcardLoadRuleKeepsBothFamilies(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	a, b := f.dual("a", 1, on, ""), f.dual("b", 2, on, "")
	f.setLoad(a, 90, -1, 0)
	f.setLoad(b, 10, -1, 0)
	f.sync()
	f.want("203.0.113.2", "2001:db8::2")
	f.public.reset()
	f.runtime.reset()
	f.setLoad(a, 75, -1, 0) // between the thresholds the decision holds: still out with both families, so nothing is republished
	f.sync()
	if len(f.public.calls)+len(f.runtime.calls) != 0 {
		t.Fatalf("a node at 75 %% came back or the set was rewritten: %+v %+v", f.public.calls, f.runtime.calls)
	}
	f.setLoad(a, 60, -1, 0)
	f.sync()
	f.want("203.0.113.1", "2001:db8::1", "203.0.113.2", "2001:db8::2")
}

// All of a region's nodes above the out threshold: the least loaded stays with its A and AAAA, the others leave with both.
func TestEdgeWildcardNeverEmptiesRegionWithBothFamilies(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	a, b, c := f.dual("a", 1, on, ""), f.dual("b", 2, on, ""), f.dual("c", 3, on, "")
	f.setLoad(a, 90, -1, 0)
	f.setLoad(b, 95, -1, 0)
	f.setLoad(c, 88, -1, 0)
	f.sync()
	f.want("203.0.113.3", "2001:db8::3")
}

// The cap on the answer cuts the last of each region's ranks, not whole regions and not the highest node ids: regions
// of 6, 3 and 2 nodes have a serving set of 4, 3 and 2, and a cap of 8 takes ranks round-robin across regions.
func TestWildcardCapIsPerRegionNotByNodeId(t *testing.T) {
	f := newWildcardFixture(t)
	var logs bytes.Buffer
	f.wildcard.log = slog.New(slog.NewTextHandler(&logs, nil))
	regions := map[string]int{"wc_a_" + f.stamp: 6, "wc_b_" + f.stamp: 3, "wc_c_" + f.stamp: 2}
	next := 1
	address := map[string]string{} // "<region>/<rank>" -> its IPv4
	for _, region := range []string{"wc_a_" + f.stamp, "wc_b_" + f.stamp, "wc_c_" + f.stamp} {
		f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Cap',true)`, region)
		t.Cleanup(func() {
			f.exec(`DELETE FROM paperboat.managed_node_authority WHERE region_id=$1`, region)
			f.exec(`DELETE FROM paperboat.operator_regions WHERE id=$1`, region)
		})
		for rank := 1; rank <= regions[region]; rank++ {
			// Node ids sort by rank within a region; lowest-id-first over all regions would give a six nodes and c none.
			address[fmt.Sprintf("%s/%d", region, rank)] = fmt.Sprintf("203.0.113.%d", next)
			f.node(fmt.Sprintf("%s%d", region[3:4], rank), fmt.Sprintf("203.0.113.%d", next), "", region, "")
			next++
		}
	}
	a, b, c := "wc_a_"+f.stamp, "wc_b_"+f.stamp, "wc_c_"+f.stamp
	at := func(region string, ranks ...int) (out []string) {
		for _, rank := range ranks {
			out = append(out, address[fmt.Sprintf("%s/%d", region, rank)])
		}
		return out
	}
	// No cap in play: every region's first four nodes (a: 1-4, b: 1-3, c: 1-2), round-robin by rank then region.
	f.wildcard.maxNodes = 32
	f.sync()
	var want []string
	want = append(want, at(a, 1)...)
	want = append(want, at(b, 1)...)
	want = append(want, at(c, 1)...)
	want = append(want, at(a, 2)...)
	want = append(want, at(b, 2)...)
	want = append(want, at(c, 2)...)
	want = append(want, at(a, 3)...)
	want = append(want, at(b, 3)...)
	want = append(want, at(a, 4)...)
	f.want(want...)
	if bytes.Contains(logs.Bytes(), []byte("edge_wildcard_dns_truncated")) {
		t.Fatalf("truncation logged without a cut: %s", logs.String())
	}
	// A cap of 8 cuts the last rank of a (4) and keeps every region: 9 serving nodes, the 9th (a rank 4) is dropped.
	f.wildcard.maxNodes = 8
	f.wildcard.reconciled = nil
	f.public.reset()
	f.runtime.reset()
	f.sync()
	f.want(want[:8]...)
	f.public.reset()
	f.runtime.reset()
	f.wildcard.reconciled = nil
	f.sync()
	if n := bytes.Count(logs.Bytes(), []byte("edge_wildcard_dns_truncated")); n != 1 {
		t.Fatalf("truncation logged %d times for one unchanged count, want once: %s", n, logs.String())
	}
}
