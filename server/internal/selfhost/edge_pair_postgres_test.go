package selfhost

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

// A workspace's edge pair is its stored edge set: GET reads it with the best RTT its machines measured, PUT
// changes it through the pool policy and the edge set, refuses unknown, mixed and unreachable edges, refuses a
// switch between Paperboat's edges and its own servers while publications run, and only a team owner or admin may.
func TestEdgePairOnPostgres(t *testing.T) {
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
	ctx := t.Context()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	// One machine per page, so the wake-up below must page through both of the owner's machines.
	pairPublishBatch = 1
	t.Cleanup(func() { pairPublishBatch = 1000 })
	stamp := randomIdentity()
	owner, other, admin, member := "pair_owner_"+stamp, "pair_other_"+stamp, "pair_admin_"+stamp, "pair_member_"+stamp
	team := "pair-team-" + stamp[:8]
	region := "pairsite_" + strings.ToLower(stamp[:8])
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	for _, actor := range []string{owner, other, admin, member} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, team, owner, admin, member)
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.tunnels WHERE account_id=$1`, []any{owner}},
			{`DELETE FROM paperboat.machine_node_latency WHERE node_id LIKE $1`, []any{"%" + stamp}},
			{`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id IN ($1,$2)`, []any{owner, team}},
			{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id IN ($1,$2)`, []any{owner, team}},
			{`DELETE FROM paperboat.selfhost_installations WHERE id LIKE $1`, []any{"%" + stamp}},
			{`DELETE FROM paperboat.managed_node_authority WHERE node_id LIKE $1`, []any{"%" + stamp}},
			{`DELETE FROM paperboat.operator_regions WHERE id=$1`, []any{region}},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, []any{"%" + stamp}},
			{`DELETE FROM paperboat.user_machines WHERE user_id IN ($1,$2)`, []any{owner, other}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id IN ($1,$2,$3,$4)`, []any{owner, other, admin, member}},
		} {
			if _, err := store.SQL().ExecContext(cleanup, statement.query, statement.args...); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	// Only this test's edges are ready, so the stored set can only name them.
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE roles @> ARRAY['edge'] AND id NOT LIKE $1`, "%"+stamp)
	node := func(name string, heartbeatAge time.Duration) string {
		id := name + "_" + stamp
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,endpoint_host,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at)
VALUES($1,'default','1.0','epoch','ready',true,now()-make_interval(secs=>$2::float8),'fsn1',$1,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',$1||'.example.test','edge.example.test',27443,27444,'sha256:'||repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id, heartbeatAge.Seconds())
		return id
	}
	own := func(name, kind, scopeID, node string) string {
		id := name + "_" + stamp
		exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(md5($1||'k')||md5($1||'kk'),'hex'),decode(md5($1||'h')||md5($1||'hh'),'hex'),'fixture',$3,'tunnel',$4,$5,$1,true)`, id, node, "Server "+name, kind, scopeID)
		return id
	}
	m1, m2, m3, stale := node("m1", 0), node("m2", 0), node("m3", 0), node("stale", time.Hour)
	n1, n2, nx := node("n1", 0), node("n2", 0), node("nx", 0)
	s1, s2, sx := own("s1", "account", owner, n1), own("s2", "account", owner, n2), own("sx", "account", other, nx)
	nt1, nt2 := node("nt1", 0), node("nt2", 0)
	t1, t2 := own("t1", "team", team, nt1), own("t2", "team", team, nt2)
	service := New(store, "selfhost-edge-pair-test-encryption-key-32")
	personal := Scope{Kind: "account", ID: owner}
	teamScope := Scope{Kind: "team", ID: team}
	id := func(e *PairEdge) string {
		if e == nil {
			return ""
		}
		return e.ID
	}

	// 1. A workspace with no set gets one from the managed pool; the pair carries the best RTT any machine measured.
	pair, err := service.EdgePair(ctx, owner, personal)
	if err != nil || pair.Primary.Kind != "managed" || pair.Fallback == nil || pair.Fallback.Kind != "managed" || pair.Primary.ID == pair.Fallback.ID {
		t.Fatalf("first pair=%+v err=%v", pair, err)
	}
	if pair.Primary.Status != "ready" || pair.Primary.Region != "fsn1" || pair.Primary.LastHeartbeat == nil || pair.Primary.BestRTTMillis != nil {
		t.Fatalf("primary fields=%+v", pair.Primary)
	}
	// A managed edge shows its operator-set site, else its region's display name; one with neither shows none.
	exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Test region',true)`, region)
	exec(`INSERT INTO paperboat.managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,$3,'encrypted','{}'::jsonb)`, pair.Primary.ID, region, make([]byte, 32))
	if shown, showErr := service.EdgePair(ctx, owner, personal); showErr != nil || shown.Primary.Site != "Test region" || shown.Fallback == nil || shown.Fallback.Site != "" {
		t.Fatalf("managed sites=%+v err=%v", shown, showErr)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET site='Mumbai' WHERE id=$1`, pair.Fallback.ID)
	if shown, showErr := service.EdgePair(ctx, owner, personal); showErr != nil || shown.Primary.Site != "Test region" || shown.Fallback == nil || shown.Fallback.Site != "Mumbai" {
		t.Fatalf("managed sites=%+v err=%v", shown, showErr)
	}
	first := testutil.PublicationMachine(t, store.SQL(), owner)
	second := first + "_2"
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'linux','amd64','/workspace','offline','released')`, second, owner)
	foreign := testutil.PublicationMachine(t, store.SQL(), other)
	for _, sample := range []struct {
		machine, node string
		rtt           int
		age           time.Duration
	}{{first, pair.Primary.ID, 40, time.Minute}, {second, pair.Primary.ID, 25, 5 * time.Minute}, {foreign, pair.Primary.ID, 3, time.Minute}, {first, pair.Fallback.ID, 9, 2 * time.Hour}} {
		exec(`INSERT INTO paperboat.machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) VALUES($1,$2,'edge',$3,now()-make_interval(secs=>$4::float8))`, sample.machine, sample.node, sample.rtt, sample.age.Seconds())
	}
	pair, err = service.EdgePair(ctx, owner, personal)
	if err != nil || pair.Primary.BestRTTMillis == nil || *pair.Primary.BestRTTMillis != 25 || pair.Primary.MeasuredAt == nil || pair.Fallback.BestRTTMillis != nil {
		t.Fatalf("rtt pair=%+v err=%v", pair, err)
	}

	// 2. PUT between Paperboat's edges stores the order, bumps the generation and wakes the workspace's machines.
	var generation int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, owner).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	pair, err = service.SetEdgePair(ctx, owner, personal, m3, m1)
	if err != nil || pair.Primary.ID != m3 || id(pair.Fallback) != m1 || pair.Primary.Kind != "managed" {
		t.Fatalf("managed pair=%+v err=%v", pair, err)
	}
	var nodes []string
	var next int64
	if err = store.Pool().QueryRow(ctx, `SELECT generation,node_ids FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, owner).Scan(&next, &nodes); err != nil || next != generation+1 || len(nodes) != 2 || nodes[0] != m3 || nodes[1] != m1 {
		t.Fatalf("stored set generation=%d (was %d) nodes=%v err=%v", next, generation, nodes, err)
	}
	for _, topic := range []string{"tunnels", "previews"} {
		var woken int
		if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id IN ($1,$2) AND topic=$3`, first, second, topic).Scan(&woken); err != nil || woken != 2 {
			t.Fatalf("machines woken on %s: %d %v", topic, woken, err)
		}
	}
	if got, readErr := service.EdgePair(ctx, owner, personal); readErr != nil || got.Primary.ID != m3 || id(got.Fallback) != m1 {
		t.Fatalf("pair after put=%+v err=%v", got, readErr)
	}

	// 3. Refusals: the same edge twice, unknown ids, another account's server, mixed kinds, an edge nobody has heard from.
	for name, ids := range map[string][2]string{
		"same":           {m1, m1},
		"same node":      {s1, s1},
		"unknown":        {m1, "no_such_edge"},
		"empty":          {"", m1},
		"foreign server": {s1, sx},
		"mixed":          {m1, s1},
		"silent":         {m1, stale},
		"not an edge":    {m1, first},
	} {
		if _, err = service.SetEdgePair(ctx, owner, personal, ids[0], ids[1]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if mode := workspaceMode(t, store, owner); mode != "managed-only" {
		t.Fatalf("a refused change left mode %s", mode)
	}

	// 4. A running tunnel blocks the switch to own servers, with its count, and the pool keeps its mode; a change of
	// pair inside Paperboat's edges is still allowed.
	endpointID := testutil.EndpointID("pair:" + stamp)
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,$5,'actor')`, "tunnel_"+stamp, owner, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".example.test", first)
	var live *LivePublicationsError
	if _, err = service.SetEdgePair(ctx, owner, personal, s1, s2); !errors.As(err, &live) || live.Count != 1 || !errors.Is(err, ErrLiveEdgeModeChange) {
		t.Fatalf("switch with a running tunnel: %v", err)
	}
	if _, err = service.SetPool(ctx, owner, "tunnel", Policy{Mode: "self-hosted-only", InstallationIDs: []string{s1}}); !errors.As(err, &live) || live.Count != 1 {
		t.Fatalf("pool switch with a running tunnel: %v", err)
	}
	if mode := workspaceMode(t, store, owner); mode != "managed-only" {
		t.Fatalf("a blocked switch left mode %s", mode)
	}
	if pair, err = service.SetEdgePair(ctx, owner, personal, m2, m3); err != nil || pair.Primary.ID != m2 {
		t.Fatalf("same-mode change with a running tunnel: %+v %v", pair, err)
	}

	// 5. With nothing running the switch goes through: the pool becomes the pair, the pair names installations.
	exec(`UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, "tunnel_"+stamp)
	pair, err = service.SetEdgePair(ctx, owner, personal, s2, s1)
	if err != nil || pair.Primary.ID != s2 || pair.Primary.Kind != "self_hosted" || pair.Primary.Name != "Server s2" || pair.Primary.Site != "Server s2" || pair.Primary.EndpointHost != n2+".example.test" || id(pair.Fallback) != s1 {
		t.Fatalf("self-hosted pair=%+v err=%v", pair, err)
	}
	if policy, poolErr := service.PoolScoped(ctx, owner, "tunnel", personal); poolErr != nil || policy.Mode != "self-hosted-only" || len(policy.InstallationIDs) != 2 || policy.InstallationIDs[0] != s2 || policy.InstallationIDs[1] != s1 {
		t.Fatalf("pool after switch=%+v err=%v", policy, poolErr)
	}
	if got, readErr := service.EdgePair(ctx, owner, personal); readErr != nil || got.Primary.ID != s2 || id(got.Fallback) != s1 {
		t.Fatalf("self-hosted pair read back=%+v err=%v", got, readErr)
	}
	if _, err = service.SetEdgePair(ctx, owner, personal, m1, m2); err != nil {
		t.Fatalf("switch back to Paperboat's edges: %v", err)
	}
	if policy, poolErr := service.PoolScoped(ctx, owner, "tunnel", personal); poolErr != nil || policy.Mode != "managed-only" || len(policy.InstallationIDs) != 0 {
		t.Fatalf("pool after switching back=%+v err=%v", policy, poolErr)
	}

	// 6. Authority: a pair is read by members and changed by the owner or an admin; the owner's own scope is nobody else's.
	if _, err = service.EdgePair(ctx, other, personal); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account read the owner's pair: %v", err)
	}
	if _, err = service.SetEdgePair(ctx, other, personal, m1, m2); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account changed the owner's pair: %v", err)
	}
	if _, err = service.EdgePair(ctx, other, teamScope); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a non-member read the team's pair: %v", err)
	}
	if got, readErr := service.EdgePair(ctx, member, teamScope); readErr != nil || got.Primary.Kind != "managed" {
		t.Fatalf("a member could not read the team's pair: %+v %v", got, readErr)
	}
	for _, actor := range []string{other, member} {
		if _, err = service.SetEdgePair(ctx, actor, teamScope, m1, m2); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s changed the team's pair: %v", actor, err)
		}
	}
	if pair, err = service.SetEdgePair(ctx, admin, teamScope, t2, t1); err != nil || pair.Primary.ID != t2 || id(pair.Fallback) != t1 || pair.Primary.Kind != "self_hosted" {
		t.Fatalf("admin's team pair=%+v err=%v", pair, err)
	}
	if _, err = service.SetEdgePair(ctx, owner, teamScope, s1, s2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("personal servers chosen for a team: %v", err)
	}
	if pair, err = service.SetEdgePair(ctx, owner, teamScope, m2, m1); err != nil || pair.Primary.ID != m2 || workspaceModeOf(t, store, "team", team) != "managed-only" {
		t.Fatalf("owner's team pair=%+v err=%v", pair, err)
	}
	if mode := workspaceMode(t, store, owner); mode != "managed-only" {
		t.Fatalf("a team change moved the personal mode to %s", mode)
	}

	// 7. The server list says when each server last reported in.
	list, err := service.ListScoped(ctx, owner, personal, false)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := service.ListScopedInventory(ctx, owner, personal, false, InventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, installations := range map[string][]Installation{"list": list, "inventory": inventory.Installations} {
		seen := false
		for _, installation := range installations {
			if installation.InstallationID == s1 {
				seen = true
				if installation.LastHeartbeat == nil || time.Since(*installation.LastHeartbeat) > time.Minute {
					t.Fatalf("%s: last heartbeat=%v", name, installation.LastHeartbeat)
				}
			}
		}
		if !seen {
			t.Fatalf("%s lacks %s", name, s1)
		}
	}
	one, err := service.GetInstallation(ctx, owner, s1, false)
	if err != nil || one.LastHeartbeat == nil {
		t.Fatalf("installation=%+v err=%v", one, err)
	}
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
