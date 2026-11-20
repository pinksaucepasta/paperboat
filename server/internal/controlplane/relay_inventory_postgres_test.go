package controlplane

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// The account's relay list is the nodes its pool policy lets it use, each with a heartbeat-derived status.
// Relays are listed by region, like edges: a region with a ready and a draining node is one item (ready), the account's
// own relay region shows its id as its name, and another account's relay region, a restricted node's region and an
// edge-only region are absent; an only-mine pool hides Paperboat's regions.
func TestListHostedRelaysReturnsRegions(t *testing.T) {
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
	prefix := "zz_relayinv_" + strings.ReplaceAll(now.Format("20060102150405.000000"), ".", "") + "_"
	account, other := "acct_"+prefix, "other_"+prefix
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.selfhost_installations WHERE node_id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.operator_regions WHERE id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, account, other)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{account, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test")
	}
	exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Region ra',true)`, prefix+"ra")
	for _, item := range []struct {
		suffix, region, state, role string
		heartbeat                   time.Time
	}{
		{"a1", "ra", "ready", "relay", now}, {"a2", "ra", "draining", "relay", now}, // Paperboat's region: two nodes
		{"b1", "rb", "ready", "relay", now},                        // the account's own relay
		{"c1", "rc", "ready", "relay", now},                        // another account's relay
		{"d1", "rd", "ready", "relay", now},                        // restricted to another account
		{"f1", "rf", "ready", "relay", now.Add(-16 * time.Second)}, // silent
		{"g1", "rg", "ready", "edge", now},                         // an edge-only region
	} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,tls_spki_sha256)
VALUES($1,'default','1.0','epoch',$2,$2='ready',$3,$6,'domain',ARRAY[$4]::text[],ARRAY['derp_quic']::text[],100,0,$3,$3::timestamptz+interval '1 hour',$5)`, prefix+item.suffix, item.state, item.heartbeat, item.role, make([]byte, 32), prefix+item.region)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET allowed_account_ids=ARRAY['different_account'] WHERE id=$1`, prefix+"d1")
	for index, own := range []struct{ suffix, scope string }{{"b1", account}, {"c1", other}} {
		key := make([]byte, 32)
		key[0] = byte(index + 1) // the runtime hash is unique per installation
		exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'encrypted','my relay','relay','account',$4,$5,true)`, prefix+"inst_"+own.suffix, prefix+own.suffix, key, own.scope, prefix+"claim_"+own.suffix)
	}
	service := NewEdgeService(database, "unused")
	service.SetClock(func() time.Time { return now })
	list := func() []HostedRelay {
		t.Helper()
		var got []HostedRelay
		cursor := prefix
		for pages := 0; pages < 10; pages++ { // one per page: the cursor (a region id) walks them all
			page, err := service.ListHostedRelays(ctx, account, cursor, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, relay := range page.Items {
				if strings.HasPrefix(relay.ID, prefix) {
					got = append(got, relay)
				}
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		return got
	}
	want := []HostedRelay{{ID: prefix + "ra", Name: "Region ra", Status: "ready"}, {ID: prefix + "rb", Name: prefix + "rb", Status: "ready"}, {ID: prefix + "rf", Name: prefix + "rf", Status: "unavailable"}}
	if got := list(); !slices.Equal(got, want) {
		t.Fatalf("relay regions=%+v, want %+v (not rc: another's relay, rd: restricted, rg: an edge)", got, want)
	}
	// Only-mine pools hide Paperboat's regions.
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'relay','self-hosted-only',ARRAY[$2]::text[])`, account, prefix+"inst_b1")
	if got := list(); !slices.Equal(got, []HostedRelay{{ID: prefix + "rb", Name: prefix + "rb", Status: "ready"}}) {
		t.Fatalf("self-hosted-only pool listed %+v", got)
	}
	if _, err := service.ListHostedRelays(ctx, "", "", 1); err != ErrInvalidEdgePage {
		t.Fatalf("empty account: %v", err)
	}
}
