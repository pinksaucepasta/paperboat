package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// The account sees Paperboat's regions, not nodes: two nodes of a region are one item with a folded status; a disabled
// region, a region whose only node is another account's or a customer's, and a relay-only region do not appear.
func TestListHostedEdgesReturnsRegions(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	prefix := "zz_edgeinv_" + strings.ReplaceAll(now.Format("20060102150405.000000"), ".", "") + "_"
	account := "acct_" + prefix
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.selfhost_installations WHERE node_id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.operator_regions WHERE id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, account, "workos_"+account, account+"@example.test")
	for _, region := range []struct {
		suffix  string
		enabled bool
	}{{"ra", true}, {"rb", true}, {"rc", false}, {"rd", true}, {"re", true}, {"rg", true}, {"rh", true}} {
		exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,$2,$3)`, prefix+region.suffix, "Region "+region.suffix, region.enabled)
	}
	for _, item := range []struct {
		suffix, region, state string
		heartbeat             time.Time
		role                  string
	}{
		{"a1", "ra", "ready", now, "edge"}, {"a2", "ra", "draining", now, "edge"}, // ra: two nodes, one region
		{"b1", "rb", "ready", now.Add(-16 * time.Second), "edge"}, {"b2", "rb", "retired", now, "edge"}, // rb: silent
		{"c1", "rc", "ready", now, "edge"}, {"d1", "rd", "ready", now, "edge"}, {"e1", "re", "ready", now, "edge"},
		{"g1", "rg", "ready", now, "relay"}, {"h1", "rh", "draining", now, "edge"},
	} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,endpoint_host,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem)
VALUES($1,'default','1.0','epoch', $2,$2='ready',$3,$6,'domain',ARRAY[$4]::text[],ARRAY['http3','http2']::text[],100,0,$3,$3::timestamptz+interval '1 hour',$1||'.example.test','edge.example.test',443,444,$5,'certificate')`, prefix+item.suffix, item.state, item.heartbeat, item.role, "sha256:"+strings.Repeat("0", 64), prefix+item.region)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET allowed_account_ids=ARRAY['different_account'] WHERE id=$1`, prefix+"d1")
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'encrypted','test edge','tunnel','account',$4,$5,true)`, prefix+"installation", prefix+"e1", make([]byte, 32), account, prefix+"claim")
	service := NewEdgeService(database, "unused")
	service.SetClock(func() time.Time { return now })
	// One a page: the cursor (a region id) walks them all.
	got := []HostedEdge{}
	cursor := prefix
	for pages := 0; pages < 10; pages++ {
		page, err := service.ListHostedEdges(ctx, account, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, edge := range page.Items {
			if strings.HasPrefix(edge.ID, prefix) {
				got = append(got, edge)
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	want := []HostedEdge{{ID: prefix + "ra", Name: "Region ra", Status: "ready"}, {ID: prefix + "rb", Name: "Region rb", Status: "unavailable"}, {ID: prefix + "rh", Name: "Region rh", Status: "draining"}}
	if len(got) != len(want) {
		t.Fatalf("regions=%+v, want ra rb rh only (rc is disabled, rd another account's, re a customer's, rg a relay region)", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("region %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"endpoint_host", "example.test", "heartbeat", "site", "a1", "a2"} {
		if strings.Contains(strings.ReplaceAll(string(raw), prefix, ""), leaked) {
			t.Fatalf("region JSON leaks %q: %s", leaked, raw)
		}
	}
	if _, err := service.ListHostedEdges(ctx, "", "", 1); err != ErrInvalidEdgePage {
		t.Fatalf("empty account: %v", err)
	}
}
