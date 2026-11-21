package selfhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

// pairFixture is an owner with Paperboat's managed regions (ra with two nodes, rb, rc, a silent rs, a disabled rd),
// their own servers (own1 with two servers, own2), another account's server, and a team with two servers of its
// own. Only these fixtures' edges are ready, so a stored pair can only name their regions.
type pairFixture struct {
	t                                 *testing.T
	ctx                               context.Context
	store                             *db.DB
	service                           *Service
	stamp                             string
	owner, other, admin, member, team string
	ra, rb, rc, rs, rd                string // managed regions
	own1, own2, ownx, teamr1, teamr2  string // the owner's, another account's and the team's own regions
	ma1, ma2, mb1, mc1                string // managed nodes (ma1 and ma2 are both in ra)
	s1, s1b, s2, sx, t1, t2           string // installations
	n1, n1b, n2                       string // the nodes of s1, s1b, s2
	first, second, foreign            string // machines
	personal, teamScope               Scope
}

func newPairFixture(t *testing.T) *pairFixture {
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
	f := &pairFixture{t: t, ctx: t.Context(), store: store, stamp: randomIdentity()}
	if err = db.Migrate(f.ctx, store); err != nil {
		t.Fatal(err)
	}
	// One machine per page, so the wake-up must page through both of the owner's machines.
	pairPublishBatch = 1
	t.Cleanup(func() { pairPublishBatch = 1000 })
	stamp := f.stamp
	short := strings.ToLower(stamp[:8])
	f.owner, f.other, f.admin, f.member = "pair_owner_"+stamp, "pair_other_"+stamp, "pair_admin_"+stamp, "pair_member_"+stamp
	f.team = "pair-team-" + short
	f.ra, f.rb, f.rc, f.rs, f.rd = "pair_ra_"+short, "pair_rb_"+short, "pair_rc_"+short, "pair_rs_"+short, "pair_rd_"+short
	f.own1, f.own2, f.ownx, f.teamr1, f.teamr2 = "own1_"+short, "own2_"+short, "ownx_"+short, "teamr1_"+short, "teamr2_"+short
	for _, actor := range []string{f.owner, f.other, f.admin, f.member} {
		f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team, f.owner)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, f.team, f.owner, f.admin, f.member)
	t.Cleanup(f.cleanup)
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE roles @> ARRAY['edge'] AND id NOT LIKE $1`, "%"+stamp)
	for _, region := range []string{f.ra, f.rb, f.rc, f.rs} {
		f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Region '||$1,true)`, region)
	}
	f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Region '||$1,false)`, f.rd)
	f.ma1, f.ma2, f.mb1, f.mc1 = f.node("ma1", f.ra, 0), f.node("ma2", f.ra, 0), f.node("mb1", f.rb, 0), f.node("mc1", f.rc, 0)
	f.node("silent", f.rs, time.Hour)
	// A node of a disabled region is no candidate: its managed authority names the region.
	f.exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,decode(md5($1)||md5($1),'hex'),$2,'encrypted','managed fixture','tunnel','global','global',$1,true)`, f.node("disabled", f.rd, 0), make([]byte, 32))
	f.n1, f.n1b, f.n2 = f.node("n1", f.own1, 0), f.node("n1b", f.own1, 0), f.node("n2", f.own2, 0)
	nx := f.node("nx", f.ownx, 0)
	nt1, nt2 := f.node("nt1", f.teamr1, 0), f.node("nt2", f.teamr2, 0)
	f.s1, f.s1b, f.s2, f.sx = f.own("s1", "account", f.owner, f.n1), f.own("s1b", "account", f.owner, f.n1b), f.own("s2", "account", f.owner, f.n2), f.own("sx", "account", f.other, nx)
	f.t1, f.t2 = f.own("t1", "team", f.team, nt1), f.own("t2", "team", f.team, nt2)
	f.service = New(store, "selfhost-edge-pair-test-encryption-key-32")
	f.personal, f.teamScope = Scope{Kind: "account", ID: f.owner}, Scope{Kind: "team", ID: f.team}
	f.first = testutil.PublicationMachine(t, store.SQL(), f.owner)
	f.second = f.first + "_2"
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'linux','amd64','/workspace','offline','released')`, f.second, f.owner)
	f.foreign = testutil.PublicationMachine(t, store.SQL(), f.other)
	return f
}

func (f *pairFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

func (f *pairFixture) cleanup() {
	cleanup := context.Background()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM paperboat.tunnels WHERE account_id=$1`, []any{f.owner}},
		{`DELETE FROM paperboat.machine_node_latency WHERE node_id LIKE $1`, []any{"%" + f.stamp}},
		{`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id IN ($1,$2)`, []any{f.owner, f.team}},
		{`DELETE FROM paperboat.selfhost_regions WHERE scope_id IN ($1,$2,$3)`, []any{f.owner, f.other, f.team}},
		{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id IN ($1,$2)`, []any{f.owner, f.team}},
		{`DELETE FROM paperboat.selfhost_installations WHERE id LIKE $1`, []any{"%" + f.stamp}},
		{`DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, []any{"%" + f.stamp}},
		{`DELETE FROM paperboat.operator_regions WHERE id=ANY($1)`, []any{[]string{f.ra, f.rb, f.rc, f.rs, f.rd}}},
		{`DELETE FROM paperboat.user_machines WHERE user_id IN ($1,$2)`, []any{f.owner, f.other}},
		{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{f.team}},
		{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{f.team}},
		{`DELETE FROM paperboat.users WHERE id IN ($1,$2,$3,$4)`, []any{f.owner, f.other, f.admin, f.member}},
	} {
		if _, err := f.store.SQL().ExecContext(cleanup, statement.query, statement.args...); err != nil {
			f.t.Errorf("cleanup: %v", err)
		}
	}
}

// node adds a ready edge in region whose last heartbeat is heartbeatAge old.
func (f *pairFixture) node(name, region string, heartbeatAge time.Duration) string {
	id := name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,endpoint_host,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at)
VALUES($1,'default','1.0','epoch','ready',true,now()-make_interval(secs=>$2::float8),$3,$1,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',$1||'.example.test','edge.example.test',27443,27444,'sha256:'||repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id, heartbeatAge.Seconds(), region)
	return id
}

func (f *pairFixture) own(name, kind, scopeID, node string) string {
	id := name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(md5($1||'k')||md5($1||'kk'),'hex'),decode(md5($1||'h')||md5($1||'hh'),'hex'),'fixture',$3,'tunnel',$4,$5,$1,true)`, id, node, "Server "+name, kind, scopeID)
	return id
}

func (f *pairFixture) set(actor string, scope Scope, kind, primary, fallback string) EdgePair {
	f.t.Helper()
	pair, err := f.service.SetEdgePair(f.ctx, actor, scope, kind, primary, fallback)
	if err != nil {
		f.t.Fatalf("SetEdgePair(%s, %s, %s): %v", kind, primary, fallback, err)
	}
	return pair
}

func (f *pairFixture) stored(scope Scope) (mode string, regions []string, generation int64) {
	f.t.Helper()
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT mode,regions,generation FROM paperboat.workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2`, scope.Kind, scope.ID).Scan(&mode, &regions, &generation); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *pairFixture) pool(scope Scope) Policy {
	f.t.Helper()
	policy, err := f.service.PoolScoped(f.ctx, f.owner, "tunnel", scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return policy
}

func regionID(r *PairRegion) string {
	if r == nil {
		return ""
	}
	return r.ID
}

// A managed pair is two of Paperboat's regions: the first read assigns it, the best RTT of any node of a region is
// the region's, a PUT stores the order, bumps the generation and wakes the workspace's machines, and a region with
// two nodes is one place.
func TestSetEdgePairManagedRegions(t *testing.T) {
	f := newPairFixture(t)
	managed := []string{f.ra, f.rb, f.rc}
	pair, err := f.service.EdgePair(f.ctx, f.owner, f.personal)
	if err != nil || pair.Kind != "managed" || pair.Fallback == nil || pair.Primary.ID == pair.Fallback.ID || !slices.Contains(managed, pair.Primary.ID) || !slices.Contains(managed, pair.Fallback.ID) {
		t.Fatalf("first pair=%+v err=%v", pair, err)
	}
	if pair.Primary.Status != "ready" || pair.Primary.Name != "Region "+pair.Primary.ID || pair.Primary.BestRTTMillis != nil {
		t.Fatalf("primary fields=%+v", pair.Primary)
	}
	for _, sample := range []struct {
		machine, node string
		rtt           int
		age           time.Duration
	}{{f.first, f.ma1, 40, time.Minute}, {f.second, f.ma1, 25, 5 * time.Minute}, {f.second, f.ma2, 60, time.Minute}, {f.foreign, f.ma1, 3, time.Minute}, {f.first, f.mb1, 9, 2 * time.Hour}} {
		f.exec(`INSERT INTO paperboat.machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) VALUES($1,$2,'edge',$3,now()-make_interval(secs=>$4::float8))`, sample.machine, sample.node, sample.rtt, sample.age.Seconds())
	}
	pair = f.set(f.owner, f.personal, "managed", f.ra, f.rb)
	_, _, generation := f.stored(f.personal)
	if pair.Primary.BestRTTMillis != nil {
		// The set answer already reads the samples: ra's best is the other account's 3 ms excluded, so 25 ms.
		if *pair.Primary.BestRTTMillis != 25 {
			t.Fatalf("primary rtt=%d, want 25 (best of this account's machines over both nodes)", *pair.Primary.BestRTTMillis)
		}
	} else {
		t.Fatalf("primary has no rtt: %+v", pair.Primary)
	}
	if pair.Fallback == nil || pair.Fallback.BestRTTMillis != nil {
		t.Fatalf("the stale sample of rb must be ignored: %+v", pair.Fallback)
	}
	pair = f.set(f.owner, f.personal, "managed", f.rc, f.ra)
	mode, regions, next := f.stored(f.personal)
	if mode != "managed-only" || next != generation+1 || !slices.Equal(regions, []string{f.rc, f.ra}) || pair.Primary.ID != f.rc || regionID(pair.Fallback) != f.ra {
		t.Fatalf("stored pair mode=%s regions=%v generation=%d (was %d) answer=%+v", mode, regions, next, generation, pair)
	}
	for _, topic := range []string{"tunnels", "previews"} {
		var woken int
		if err = f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id IN ($1,$2) AND topic=$3`, f.first, f.second, topic).Scan(&woken); err != nil || woken != 2 {
			t.Fatalf("machines woken on %s: %d %v", topic, woken, err)
		}
	}
	if got, readErr := f.service.EdgePair(f.ctx, f.owner, f.personal); readErr != nil || got.Primary.ID != f.rc || regionID(got.Fallback) != f.ra {
		t.Fatalf("pair after put=%+v err=%v", got, readErr)
	}
	// The pair is what placement obeys: a node of a region outside it is not eligible, one inside is.
	for node, want := range map[string]bool{f.ma1: true, f.ma2: true, f.mc1: true, f.mb1: false} {
		var ok bool
		if err = f.store.SQL().QueryRowContext(f.ctx, `SELECT paperboat.node_workspace_edge_eligible($1,$2,NULL)`, node, f.owner).Scan(&ok); err != nil || ok != want {
			t.Fatalf("eligible(%s)=%v err=%v, want %v", node, ok, err, want)
		}
	}
}

// A region's status folds over its nodes the workspace may use: ready if any is, else draining if any is, else
// unavailable.
func TestEdgePairStatusFoldsOverTheRegionsNodes(t *testing.T) {
	f := newPairFixture(t)
	f.set(f.owner, f.personal, "managed", f.ra, f.rb)
	status := func() string {
		t.Helper()
		pair, err := f.service.EdgePair(f.ctx, f.owner, f.personal)
		if err != nil {
			t.Fatal(err)
		}
		return pair.Primary.Status
	}
	if got := status(); got != "ready" {
		t.Fatalf("two ready nodes: %s", got)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,drain_deadline=now()+interval '10 minutes' WHERE id=$1`, f.ma1)
	if got := status(); got != "ready" {
		t.Fatalf("one ready, one draining: %s", got)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,drain_deadline=now()+interval '10 minutes' WHERE id=$1`, f.ma2)
	if got := status(); got != "draining" {
		t.Fatalf("both draining: %s", got)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,drain_deadline=NULL,last_heartbeat_at=now()-interval '1 hour' WHERE id IN ($1,$2)`, f.ma1, f.ma2)
	if got := status(); got != "unavailable" {
		t.Fatalf("both silent: %s", got)
	}
}

// The pair as JSON shows regions only: never a node id, endpoint host, site or heartbeat.
func TestEdgePairShowsRegionsOnly(t *testing.T) {
	f := newPairFixture(t)
	for name, pair := range map[string]EdgePair{
		"managed":     f.set(f.owner, f.personal, "managed", f.ra, f.rb),
		"self-hosted": f.set(f.owner, f.personal, "self_hosted", f.own1, f.own2),
	} {
		raw, err := json.Marshal(pair)
		if err != nil {
			t.Fatal(err)
		}
		shown := string(raw)
		for _, leaked := range []string{f.stamp, "example.test", "endpoint_host", "last_heartbeat", "node", "site"} {
			if strings.Contains(shown, leaked) {
				t.Fatalf("%s pair leaks %q: %s", name, leaked, shown)
			}
		}
		var generic map[string]any
		if err = json.Unmarshal(raw, &generic); err != nil || generic["kind"] == nil || generic["primary"] == nil || generic["fallback"] == nil {
			t.Fatalf("%s pair shape: %s", name, shown)
		}
	}
}

// Switching to the workspace's own regions: refused while a tunnel runs, then the pool becomes every own server of the
// two regions; the owner or an admin changes a team's pair, a member only reads it.
func TestSetEdgePairSelfHostedRegions(t *testing.T) {
	f := newPairFixture(t)
	f.set(f.owner, f.personal, "managed", f.ra, f.rb)
	first := f.first
	endpointID := testutil.EndpointID("pair:" + f.stamp)
	f.exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,$5,'actor')`, "tunnel_"+f.stamp, f.owner, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".example.test", first)
	var live *LivePublicationsError
	if _, err := f.service.SetEdgePair(f.ctx, f.owner, f.personal, "self_hosted", f.own1, f.own2); !errors.As(err, &live) || live.Count != 1 || !errors.Is(err, ErrLiveEdgeModeChange) {
		t.Fatalf("switch with a running tunnel: %v", err)
	}
	if _, err := f.service.SetPool(f.ctx, f.owner, "tunnel", Policy{Mode: "self-hosted-only", InstallationIDs: []string{f.s1}}); !errors.As(err, &live) || live.Count != 1 {
		t.Fatalf("pool switch with a running tunnel: %v", err)
	}
	if mode := workspaceMode(t, f.store, f.owner); mode != "managed-only" {
		t.Fatalf("a blocked switch left mode %s", mode)
	}
	if pair := f.set(f.owner, f.personal, "managed", f.rb, f.rc); pair.Primary.ID != f.rb {
		t.Fatalf("same-mode change with a running tunnel: %+v", pair)
	}
	// With nothing running the switch goes through: the pool is every own server of the two regions.
	f.exec(`UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, "tunnel_"+f.stamp)
	pair := f.set(f.owner, f.personal, "self_hosted", f.own2, f.own1)
	if pair.Kind != "self_hosted" || pair.Primary.ID != f.own2 || pair.Primary.Name != f.own2 || regionID(pair.Fallback) != f.own1 || pair.Primary.Status != "ready" {
		t.Fatalf("self-hosted pair=%+v", pair)
	}
	if policy := f.pool(f.personal); policy.Mode != "self-hosted-only" || !slices.Equal(sorted(policy.InstallationIDs...), sorted(f.s1, f.s1b, f.s2)) {
		t.Fatalf("pool after switch=%+v", policy)
	}
	if mode, regions, _ := f.stored(f.personal); mode != "self-hosted-only" || !slices.Equal(regions, []string{f.own2, f.own1}) {
		t.Fatalf("stored self-hosted pair=%s %v", mode, regions)
	}
	if got, err := f.service.EdgePair(f.ctx, f.owner, f.personal); err != nil || got.Kind != "self_hosted" || got.Primary.ID != f.own2 || regionID(got.Fallback) != f.own1 {
		t.Fatalf("self-hosted pair read back=%+v err=%v", got, err)
	}
	f.set(f.owner, f.personal, "managed", f.ra, f.rb)
	if policy := f.pool(f.personal); policy.Mode != "managed-only" || len(policy.InstallationIDs) != 0 {
		t.Fatalf("pool after switching back=%+v", policy)
	}

	// Authority: a pair is read by members and changed by the owner or an admin; the owner's own scope is nobody else's.
	if _, err := f.service.EdgePair(f.ctx, f.other, f.personal); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account read the owner's pair: %v", err)
	}
	if _, err := f.service.SetEdgePair(f.ctx, f.other, f.personal, "managed", f.ra, f.rb); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account changed the owner's pair: %v", err)
	}
	if _, err := f.service.EdgePair(f.ctx, f.other, f.teamScope); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a non-member read the team's pair: %v", err)
	}
	if got, err := f.service.EdgePair(f.ctx, f.member, f.teamScope); err != nil || got.Kind != "managed" {
		t.Fatalf("a member could not read the team's pair: %+v %v", got, err)
	}
	for _, actor := range []string{f.other, f.member} {
		if _, err := f.service.SetEdgePair(f.ctx, actor, f.teamScope, "managed", f.ra, f.rb); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s changed the team's pair: %v", actor, err)
		}
	}
	if pair = f.set(f.admin, f.teamScope, "self_hosted", f.teamr2, f.teamr1); pair.Primary.ID != f.teamr2 || regionID(pair.Fallback) != f.teamr1 || pair.Kind != "self_hosted" {
		t.Fatalf("admin's team pair=%+v", pair)
	}
	if _, err := f.service.SetEdgePair(f.ctx, f.owner, f.teamScope, "self_hosted", f.own1, f.own2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("personal servers' regions chosen for a team: %v", err)
	}
	if pair = f.set(f.owner, f.teamScope, "managed", f.rb, f.ra); pair.Primary.ID != f.rb || workspaceModeOf(t, f.store, "team", f.team) != "managed-only" {
		t.Fatalf("owner's team pair=%+v", pair)
	}
	if mode := workspaceMode(t, f.store, f.owner); mode != "managed-only" {
		t.Fatalf("a team change moved the personal mode to %s", mode)
	}
}

// Refusals name regions and change nothing: the same region twice, unknown, empty and malformed names, an unknown kind,
// a region of the wrong kind (a mix of Paperboat's and the owner's regions), another account's region, a region nobody
// has heard from and a disabled one.
func TestSetEdgePairRefusesMixedKindAndUnknownRegion(t *testing.T) {
	f := newPairFixture(t)
	f.set(f.owner, f.personal, "managed", f.ra, f.rb)
	_, before, generation := f.stored(f.personal)
	for name, in := range map[string][3]string{
		"same":                  {"managed", f.ra, f.ra},
		"unknown":               {"managed", f.ra, "no_such_region"},
		"empty":                 {"managed", "", f.ra},
		"malformed":             {"managed", f.ra, "Bad Region"},
		"a node id":             {"managed", f.ra, f.ma1},
		"kind":                  {"mixed", f.ra, f.rb},
		"own region as managed": {"managed", f.ra, f.own1},
		"managed as own":        {"self_hosted", f.own1, f.ra},
		"mixed own and managed": {"self_hosted", f.own1, f.rb},
		"foreign server":        {"self_hosted", f.own1, f.ownx},
		"silent":                {"managed", f.ra, f.rs},
		"disabled":              {"managed", f.ra, f.rd},
	} {
		_, err := f.service.SetEdgePair(f.ctx, f.owner, f.personal, in[0], in[1], in[2])
		var invalid *InvalidPairError
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &invalid) {
			t.Fatalf("%s accepted or untyped: %v", name, err)
		}
		if strings.Contains(invalid.Message, f.stamp) {
			t.Fatalf("%s names a node: %q", name, invalid.Message)
		}
	}
	// The message names the region the user typed (its display name for Paperboat's), never a node.
	_, err := f.service.SetEdgePair(f.ctx, f.owner, f.personal, "managed", f.ra, f.rs)
	var invalid *InvalidPairError
	if !errors.As(err, &invalid) || invalid.Message != "Region "+f.rs+" is not available to this workspace right now; choose a region that is ready." {
		t.Fatalf("silent region message: %v", err)
	}
	if mode := workspaceMode(t, f.store, f.owner); mode != "managed-only" {
		t.Fatalf("a refused change left mode %s", mode)
	}
	if _, after, next := f.stored(f.personal); !slices.Equal(before, after) || next != generation {
		t.Fatalf("a refused change moved the pair: %v -> %v, generation %d -> %d", before, after, generation, next)
	}
}

// A region whose nodes are all drained past their deadline is not ready: the reconcile sweep would drop it, so the PUT
// refuses it, names no node, and accepts it again once a second region is ready.
func TestSetEdgePairRefusesARegionWhoseNodesAreAllDrained(t *testing.T) {
	f := newPairFixture(t)
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,drain_deadline=now()-interval '1 minute' WHERE id=$1`, f.mb1)
	_, err := f.service.SetEdgePair(f.ctx, f.owner, f.personal, "managed", f.ra, f.rb)
	var invalid *InvalidPairError
	if !errors.Is(err, ErrInvalid) || !errors.As(err, &invalid) {
		t.Fatalf("drained region accepted or untyped: %v", err)
	}
	if strings.Contains(invalid.Message, f.stamp) {
		t.Fatalf("drained region names a node: %q", invalid.Message)
	}
	f.set(f.owner, f.personal, "managed", f.ra, f.rc)
}

// A server's region comes from enrolment; one enrolled into a region of the pair joins the pool of a self-hosted
// workspace at once, and one removed leaves it.
func TestNewNodeInPairRegionJoinsPool(t *testing.T) {
	f := newPairFixture(t)
	f.set(f.owner, f.personal, "self_hosted", f.own1, f.own2)
	if policy := f.pool(f.personal); !slices.Equal(sorted(policy.InstallationIDs...), sorted(f.s1, f.s1b, f.s2)) {
		t.Fatalf("initial pool=%v", policy.InstallationIDs)
	}
	// A new server in a region of the pair, and one in a region outside it.
	inPair, outside := f.own("s3", "account", f.owner, f.node("n3", f.own1, 0)), f.own("s4", "account", f.owner, f.node("n4", "elsewhere_"+strings.ToLower(f.stamp[:8]), 0))
	if _, err := f.store.SQL().ExecContext(f.ctx, syncTunnelPoolSQL, "account", f.owner); err != nil {
		t.Fatal(err)
	}
	if policy := f.pool(f.personal); !slices.Equal(sorted(policy.InstallationIDs...), sorted(f.s1, f.s1b, f.s2, inPair)) || slices.Contains(policy.InstallationIDs, outside) {
		t.Fatalf("pool after enrolling into the pair=%v (outside %s)", policy.InstallationIDs, outside)
	}
	if got, err := f.service.EdgePair(f.ctx, f.owner, f.personal); err != nil || got.Primary.Status != "ready" {
		t.Fatalf("pair after the new node=%+v %v", got, err)
	}
}

func TestRemovedNodeLeavesPool(t *testing.T) {
	f := newPairFixture(t)
	f.set(f.owner, f.personal, "self_hosted", f.own1, f.own2)
	if err := f.service.RemoveScoped(f.ctx, f.owner, f.s1b, false); err != nil {
		t.Fatal(err)
	}
	if policy := f.pool(f.personal); !slices.Equal(sorted(policy.InstallationIDs...), sorted(f.s1, f.s2)) {
		t.Fatalf("pool after removing a server=%v, want %v", policy.InstallationIDs, sorted(f.s1, f.s2))
	}
	// The region still has a server, so the pair still serves.
	if got, err := f.service.EdgePair(f.ctx, f.owner, f.personal); err != nil || got.Primary.Status != "ready" {
		t.Fatalf("pair after removing a server=%+v %v", got, err)
	}
}

// Every server list item carries its region, and the server list says when each server last reported in.
func TestInstallationItemsCarryRegion(t *testing.T) {
	f := newPairFixture(t)
	list, err := f.service.ListScoped(f.ctx, f.owner, f.personal, false)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := f.service.ListScopedInventory(f.ctx, f.owner, f.personal, false, InventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, installations := range map[string][]Installation{"list": list, "inventory": inventory.Installations} {
		seen := false
		for _, installation := range installations {
			if installation.InstallationID == f.s1 {
				seen = true
				if installation.Region != f.own1 {
					t.Fatalf("%s: region=%q, want %q", name, installation.Region, f.own1)
				}
				if installation.LastHeartbeat == nil || time.Since(*installation.LastHeartbeat) > time.Minute {
					t.Fatalf("%s: last heartbeat=%v", name, installation.LastHeartbeat)
				}
			}
		}
		if !seen {
			t.Fatalf("%s lacks %s", name, f.s1)
		}
	}
	one, err := f.service.GetInstallation(f.ctx, f.owner, f.s1, false)
	if err != nil || one.LastHeartbeat == nil || one.Region != f.own1 {
		t.Fatalf("installation=%+v err=%v", one, err)
	}
	raw, err := json.Marshal(one)
	if err != nil || !strings.Contains(string(raw), `"region":"`+f.own1+`"`) {
		t.Fatalf("installation JSON lacks region: %s %v", raw, err)
	}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func workspaceMode(t *testing.T, store *db.DB, account string) string {
	return workspaceModeOf(t, store, "account", account)
}

func workspaceModeOf(t *testing.T, store *db.DB, kind, id string) string {
	t.Helper()
	var mode string
	team := any(nil)
	if kind == "team" {
		team = id
	}
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT paperboat.workspace_edge_mode($1,$2)`, id, team).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

// Only Paperboat's managed edges are limited to the first four of a region (edgeset.ServingOnly): a workspace's own
// servers are not, and attach holds carriers to every one of them, so publication must keep all of them. Six own
// servers in one region and one in another: publication_edge_nodes names all seven (its LIMIT 8 is the carrier cap).
func TestPublicationNodesKeepEveryOwnServerOfARegion(t *testing.T) {
	f := newPairFixture(t)
	nodes := []string{f.n1, f.n1b, f.n2}
	for _, name := range []string{"n1c", "n1d", "n1e", "n1f"} {
		node := f.node(name, f.own1, 0)
		f.own("s"+name, "account", f.owner, node)
		nodes = append(nodes, node)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now(),public_ingress_ipv4='198.51.100.7' WHERE id=ANY($1::text[])`, nodes)
	f.set(f.owner, f.personal, "self_hosted", f.own1, f.own2)
	tunnel := "own_pub_" + f.stamp
	endpoint := testutil.EndpointID(tunnel)
	f.exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,$5,'actor')`, tunnel, f.owner, endpoint, "https://"+testutil.EndpointLabel(endpoint)+".example.test", f.first)
	t.Cleanup(func() {
		if _, err := f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnels WHERE id=$1`, tunnel); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	var got int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM paperboat.publication_edge_nodes('tunnel',$1)`, tunnel).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("publication_edge_nodes named %d servers, want all 7 of the owner's", got)
	}
}
