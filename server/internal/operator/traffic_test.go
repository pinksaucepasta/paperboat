package operator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/retention"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

func TestTrafficPeriods(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 4, 0, 0, time.UTC)
	for period, want := range map[string]struct {
		from  string
		grain string
	}{"today": {"2026-11-19", "hour"}, "7d": {"2026-11-13", "day"}, "30d": {"2026-10-21", "day"}, "month": {"2026-11-01", "day"}} {
		from, grain, err := trafficPeriod(period, now)
		if err != nil || from.Format("2006-01-02") != want.from || grain != want.grain {
			t.Errorf("%s: %v %s %v", period, from, grain, err)
		}
	}
	if _, _, err := trafficPeriod("year", now); err == nil {
		t.Error("an unknown period was accepted")
	}
}

// Every source lands in the rollup exactly once: direct bytes as the hosts report them (growth of a cumulative minute
// counts once), relay and edge bytes as the node heartbeat reports them, split managed/self-hosted; the dashboard reads
// the same numbers back, hours past the window move into days without changing a total, and day totals expire.
func TestTrafficRollupSumsRetentionAndPagesOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
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
	t.Cleanup(func() { store.Close() }) // registered first, so it runs after the cleanups that use the store
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	actor, relay, edge, silent, selfRelay := "usr_tr_op_"+stamp, "tr_relay_"+stamp, "tr_edge_"+stamp, "tr_silent_"+stamp, "tr_self_"+stamp
	exec(`INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$2,'unused test login','active','operations')`, actor, actor+"@example.test")
	now := time.Now().UTC()
	for _, n := range []struct{ id, role string }{{relay, "relay"}, {edge, "edge"}, {silent, "relay"}, {selfRelay, "relay"}} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,site)
 VALUES($1,'default','1.0','e',$1||'.example.test',443,444,'ready',true,$2,1,'hel','hel-a',ARRAY[$3],ARRAY['http3'],1000,0,$2,$4,'Helsinki')`, n.id, now, n.role, now.Add(time.Hour))
	}
	runtimeHash := sha256.Sum256([]byte(stamp)) // unique per run: runtime_hash is unique across the database
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'fixture','customer relay','relay','account','acct',$1,true)`, "tr_inst_"+stamp, selfRelay, runtimeHash[:])
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{`DELETE FROM selfhost_installations WHERE node_id=$1`, `DELETE FROM control_tunnel_nodes WHERE id=$1`, `DELETE FROM node_capacity WHERE node_id=$1`, `DELETE FROM node_capacity_daily WHERE node_id=$1`, `DELETE FROM traffic_hourly WHERE node_id=$1`, `DELETE FROM traffic_daily WHERE node_id=$1`} {
			for _, id := range []string{relay, edge, silent, selfRelay} {
				store.Pool().Exec(bg, q, id)
			}
		}
		store.Pool().Exec(bg, `DELETE FROM operator_accounts WHERE id=$1`, actor)
	})
	service := NewService(store)
	read := func(period string) TrafficPage {
		t.Helper()
		v, err := service.Read(ctx, auth.User{ID: actor, Role: auth.Role("operations")}, "traffic", period, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		return v.(TrafficPage)
	}
	total := func(p TrafficPage, category string) (int64, int64) {
		for _, c := range p.Totals {
			if c.Category == category {
				return c.UploadBytes, c.DownloadBytes
			}
		}
		t.Fatalf("category %s missing", category)
		return 0, 0
	}
	before := read("today")
	directUp0, directDown0 := total(before, "direct")
	relayUp0, _ := total(before, "relay_managed")

	// Direct: a cumulative minute counter (insert, then growth) counts once; a relay-mode event is not direct traffic.
	hour := now.Truncate(time.Hour)
	op := "native:tr" + stamp
	exec(`INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,mode,hosting,upload_bytes,download_bytes,interval_start,interval_end) VALUES($1,'w','terminal','r','n','direct','none',100,50,$2,$2)`, op, hour)
	exec(`UPDATE resource_bandwidth_events SET upload_bytes=160,download_bytes=80 WHERE operation_id=$1`, op)
	exec(`INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,mode,hosting,upload_bytes,download_bytes,interval_start,interval_end) VALUES($1,'w','terminal','r','n','relay','managed',999,999,$2,$2)`, op+"r", hour)
	// Nodes: the first sample is the baseline, the second adds the difference; a self-hosted node is its own category.
	sample := func(id, role string, in, out uint64, at time.Time) {
		t.Helper()
		if err := controlplane.RecordNodeCapacity(ctx, store, controlplane.NodeSample{NodeID: id, Role: role, ProcessEpoch: "e", Metrics: nodemetrics.Report{BytesIn: in, BytesOut: out}}, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		id, role string
		in, out  uint64
	}{{relay, "relay", 1000, 2000}, {edge, "edge", 10, 20}, {selfRelay, "relay", 5, 6}} {
		sample(s.id, s.role, s.in, s.out, now.Add(-time.Minute))
		sample(s.id, s.role, s.in+300, s.out+700, now)
	}
	after := read("today")
	if up, down := total(after, "direct"); up-directUp0 != 160 || down-directDown0 != 80 {
		t.Fatalf("direct grew by %d/%d, want 160/80", up-directUp0, down-directDown0)
	}
	if up, _ := total(after, "relay_managed"); up-relayUp0 != 300 {
		t.Fatalf("our relays grew by %d, want 300", up-relayUp0)
	}
	if up, down := total(after, "relay_self_hosted"); up < 300 || down < 700 {
		t.Fatalf("self-hosted relay %d/%d", up, down)
	}
	// Sums across sources agree with the sources themselves.
	var rollup, source int64
	if err := store.Pool().QueryRow(ctx, `SELECT coalesce(sum(upload_bytes+download_bytes),0) FROM traffic_hourly WHERE node_id IN ($1,$2,$3)`, relay, edge, selfRelay).Scan(&rollup); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT coalesce(sum(bytes_in+bytes_out),0) FROM node_capacity_daily WHERE node_id IN ($1,$2,$3)`, relay, edge, selfRelay).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if rollup != source || source != 3*1000 {
		t.Fatalf("rollup %d, node counters %d, want 3000", rollup, source)
	}
	// Coverage: the silent registered relay is expected but not reporting.
	for _, c := range after.Totals {
		if c.Category == "relay_managed" && (c.Expected < 2 || c.Reporting >= c.Expected || c.CoveragePercent == nil || *c.CoveragePercent >= 100) {
			t.Fatalf("relay coverage %+v", c)
		}
	}
	// Per-node table: ours only, silent node listed with zero bytes, paged by id.
	seen := map[string]TrafficNode{}
	for cursor := ""; ; {
		v, err := service.Read(ctx, auth.User{ID: actor, Role: auth.Role("operations")}, "traffic_nodes", "today:relay", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		page := v.(TrafficNodePage)
		for _, n := range page.Items {
			if n.NodeID <= cursor {
				t.Fatalf("out of order %q after %q", n.NodeID, cursor)
			}
			seen[n.NodeID] = n
		}
		if page.NextAfter == "" {
			break
		}
		cursor = page.NextAfter
	}
	if n := seen[relay]; !n.Reporting || n.UploadBytes != 300 || n.DownloadBytes != 700 {
		t.Fatalf("relay row %+v", n)
	}
	if n, ok := seen[silent]; !ok || n.Reporting || n.UploadBytes != 0 {
		t.Fatalf("silent relay row %+v present=%v", n, ok)
	}
	if _, ok := seen[selfRelay]; ok {
		t.Fatal("a customer's relay was listed among ours")
	}
	if _, ok := seen[edge]; ok {
		t.Fatal("an edge was listed among relays")
	}

	// Retention: hours past the window fold into days with the same totals, then the days expire.
	old := now.AddDate(0, 0, -20).Truncate(time.Hour)
	exec(`INSERT INTO traffic_hourly(hour,category,node_id,upload_bytes,download_bytes) VALUES($1,'relay_managed',$3,10,1),($2,'relay_managed',$3,20,2)`, old, old.Add(time.Hour), relay)
	cfg := config.Default().Retention
	if _, err := retention.Run(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	var hours int
	var up, down int64
	if err := store.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM traffic_hourly WHERE node_id=$1 AND hour<$2),
 (SELECT coalesce(sum(upload_bytes),0) FROM traffic_daily WHERE node_id=$1 AND day=($3 AT TIME ZONE 'UTC')::date),
 (SELECT coalesce(sum(download_bytes),0) FROM traffic_daily WHERE node_id=$1 AND day=($3 AT TIME ZONE 'UTC')::date)`, relay, now.AddDate(0, 0, -9), old).Scan(&hours, &up, &down); err != nil {
		t.Fatal(err)
	}
	if hours != 0 || up < 10 || fmt.Sprint(down) == "0" {
		t.Fatalf("folded: %d hour rows left, day totals %d/%d", hours, up, down)
	}
	exec(`INSERT INTO traffic_daily(day,category,node_id,upload_bytes,download_bytes) VALUES(current_date-500,'direct',$1,1,1)`, relay)
	if _, err := retention.Run(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM traffic_daily WHERE node_id=$1 AND day<current_date-400`, relay).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired day rows %d err %v", expired, err)
	}
}
