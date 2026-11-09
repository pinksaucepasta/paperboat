package controlplane

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// The account's relay list is the nodes its pool policy lets it use, each with a heartbeat-derived status.
func TestHostedRelayInventoryFollowsPoolAndHeartbeatOnPostgres(t *testing.T) {
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
	for _, item := range []struct {
		suffix, state, role string
		heartbeat           time.Time
	}{{"a", "ready", "relay", now}, {"b", "ready", "relay", now.Add(-16 * time.Second)}, {"c", "draining", "relay", now}, {"d", "ready", "relay", now}, {"e", "ready", "relay", now}, {"f", "ready", "relay", now}, {"g", "ready", "edge", now}} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,tls_spki_sha256)
VALUES($1,'default','1.0','epoch',$2,$2='ready',$3,'fsn1','domain',ARRAY[$4]::text[],ARRAY['derp_quic']::text[],100,0,$3,$3::timestamptz+interval '1 hour',$5)`, prefix+item.suffix, item.state, item.heartbeat, item.role, make([]byte, 32))
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET allowed_account_ids=ARRAY['different_account'] WHERE id=$1`, prefix+"d")
	// e is the account's own relay; f is another account's, which this account must not see.
	for _, own := range []struct{ suffix, scope string }{{"e", account}, {"f", other}} {
		exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'encrypted','my relay','relay','account',$4,$5,true)`, prefix+"inst_"+own.suffix, prefix+own.suffix, make([]byte, 32), own.scope, prefix+"claim_"+own.suffix)
	}
	service := NewEdgeService(database, "unused")
	service.SetClock(func() time.Time { return now })
	got := map[string]HostedRelay{}
	cursor := ""
	for pages := 0; pages < 10; pages++ { // two per page: the cursor walks all of them
		page, err := service.ListHostedRelays(ctx, account, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, relay := range page.Items {
			if strings.HasPrefix(relay.ID, prefix) {
				got[relay.ID] = relay
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	want := map[string]string{"a": "ready", "b": "unavailable", "c": "draining", "e": "ready"}
	if len(got) != len(want) {
		t.Fatalf("relays listed %v, want a b c e only (not d: other account, f: another's relay, g: an edge)", got)
	}
	for suffix, status := range want {
		if got[prefix+suffix].Status != status || got[prefix+suffix].Region != "fsn1" {
			t.Fatalf("relay %s = %+v, want %s", suffix, got[prefix+suffix], status)
		}
	}
	if own := got[prefix+"e"]; !own.SelfHosted || own.Name != "my relay" || got[prefix+"a"].SelfHosted || got[prefix+"a"].Name != "" {
		t.Fatalf("self-hosted flags: own %+v hosted %+v", own, got[prefix+"a"])
	}
	// Only-mine pools hide Paperboat's relays.
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'relay','self-hosted-only',ARRAY[$2]::text[])`, account, prefix+"inst_e")
	page, err := service.ListHostedRelays(ctx, account, prefix, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != prefix+"e" {
		t.Fatalf("self-hosted-only pool listed %+v err %v", page.Items, err)
	}
	if _, err := service.ListHostedRelays(ctx, "", "", 1); err != ErrInvalidEdgePage {
		t.Fatalf("empty account: %v", err)
	}
}
