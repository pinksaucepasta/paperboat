package selfhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestScopedInstallationInventoryPaginationOnPostgres(t *testing.T) {
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
	prefix := "inventory_" + randomIdentity()
	owner, other := prefix+"_owner", prefix+"_other"
	team, foreignTeam := prefix+"_team", prefix+"_foreign"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		commands := []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.selfhost_installations WHERE scope_id IN($1,$2,$3,$4)`, []any{owner, other, team, foreignTeam}},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE left(id,length($1))=$1`, []any{prefix}},
			{`DELETE FROM paperboat.team_members WHERE team_id IN($1,$2)`, []any{team, foreignTeam}},
			{`DELETE FROM paperboat.teams WHERE team_id IN($1,$2)`, []any{team, foreignTeam}},
			{`DELETE FROM paperboat.users WHERE id IN($1,$2)`, []any{owner, other}},
		}
		for _, command := range commands {
			if _, err := store.SQL().ExecContext(cleanup, command.query, command.args...); err != nil {
				t.Errorf("inventory cleanup: %v", err)
			}
		}
	})
	for _, actor := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1),($3,$4,1)`, team, owner, foreignTeam, other)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($3,$4,1,'admin',true)`, team, owner, foreignTeam, other)
	key := "inventory-test-encryption-key-32-bytes-long"
	service := New(store, key)
	seed := scopedInstallationFixture(t, store, key, owner, "relay")
	visible := scopedInstallationFixture(t, store, key, owner, "relay")
	hidden := scopedInstallationFixture(t, store, key, other, "relay")
	exec(`UPDATE paperboat.selfhost_installations SET scope_kind='team',scope_id=$2 WHERE id=$1`, visible.InstallationID, team)
	exec(`UPDATE paperboat.selfhost_installations SET scope_kind='team',scope_id=$2 WHERE id=$1`, hidden.InstallationID, foreignTeam)
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,tls_spki_sha256,last_heartbeat_at,public_ingress_verified_at)
 SELECT $1||'_node_'||lpad(n::text,4,'0'),$1||'_node_'||lpad(n::text,4,'0'),'1.0','','edge.example.test',27443,27444,'ready',true,'test','test-a',ARRAY['edge'],ARRAY['http2','http3'],8,0,now(),now()+interval '1 hour',decode(repeat('00',32),'hex'),now(),now() FROM generate_series(1,300)n`, prefix)
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete)
 SELECT $1||'_installation_'||lpad(n::text,4,'0'),$1||'_node_'||lpad(n::text,4,'0'),decode(md5($1||n::text)||md5($1||n::text),'hex'),decode(md5($1||n::text)||md5($1||n::text),'hex'),i.runtime_ciphertext,'Registry same',CASE WHEN n%2=0 THEN 'relay' ELSE 'tunnel' END,CASE WHEN n<=150 THEN 'account' ELSE 'team' END,CASE WHEN n<=150 THEN $2 ELSE $4 END,$1||n::text,n%3<>0 FROM generate_series(1,300)n CROSS JOIN paperboat.selfhost_installations i WHERE i.id=$3`, prefix, owner, seed.InstallationID, team)
	first, err := service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Limit: 200, Q: "Registry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Installations) != 200 || first.Pagination.Total != 300 || first.Pagination.NextOffset == nil || *first.Pagination.NextOffset != 200 || first.Counts.Enrolled != 200 || first.Counts.Pending != 100 || first.Counts.Ready != 200 {
		t.Fatalf("first page: size=%d pagination=%+v counts=%+v", len(first.Installations), first.Pagination, first.Counts)
	}
	second, err := service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Limit: 200, Offset: 200, Q: "Registry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Installations) != 100 || second.Pagination.NextOffset != nil || second.Pagination.Total != 300 || second.Counts != first.Counts {
		t.Fatalf("second page: %+v", second.Pagination)
	}
	seen := map[string]bool{}
	previous := ""
	for _, page := range []InstallationInventory{first, second} {
		for _, i := range page.Installations {
			order := i.Name + "\x00" + i.Capability + "\x00" + i.InstallationID
			if seen[i.InstallationID] || order <= previous {
				t.Fatal("unstable or duplicate paged inventory")
			}
			seen[i.InstallationID] = true
			previous = order
		}
	}
	ready := true
	filtered, err := service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Limit: 1, Q: "Registry", Capability: "relay", EnrollmentState: "enrolled", Ready: &ready})
	if err != nil || filtered.Pagination.Total != 100 || filtered.Counts.Ready != 100 || len(filtered.Installations) != 1 {
		t.Fatalf("filters before limit: %+v %v", filtered, err)
	}
	ready = false
	pending, err := service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Limit: 1, Offset: 100, Q: "Registry", EnrollmentState: "pending", Ready: &ready})
	if err != nil || pending.Pagination.Total != 100 || pending.Counts.Pending != 100 || pending.Counts.Ready != 0 || len(pending.Installations) != 0 || pending.Pagination.NextOffset != nil {
		t.Fatalf("pending filtered exhausted page: %+v %v", pending, err)
	}
	literal, err := service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Q: "%"})
	if err != nil || literal.Pagination.Total != 0 {
		t.Fatalf("q interpreted wildcard: %+v %v", literal.Pagination, err)
	}
	all, err := service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Limit: 1})
	if err != nil || all.Pagination.Total != 302 {
		t.Fatalf("visible total %d: %v", all.Pagination.Total, err)
	}
	if _, err = service.ListScopedInventory(ctx, owner, Scope{Kind: "team", ID: foreignTeam}, false, InventoryOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign scope: %v", err)
	}
	if _, err = service.GetInstallation(ctx, owner, hidden.InstallationID, false); !errors.Is(err, ErrInstallationNotFound) {
		t.Fatalf("foreign detail: %v", err)
	}
	if got, err := service.GetInstallation(ctx, owner, visible.InstallationID, false); err != nil || got.ScopeID != team {
		t.Fatalf("exact visible detail: %+v %v", got, err)
	}
	exec(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, owner)
	all, err = service.ListScopedInventory(ctx, owner, Scope{}, false, InventoryOptions{Offset: 300})
	if err != nil || all.Pagination.Total != 151 || len(all.Installations) != 0 {
		t.Fatalf("revoked Team count/page: %s %v", fmt.Sprint(all.Pagination), err)
	}
	if _, err = service.GetInstallation(ctx, owner, visible.InstallationID, false); !errors.Is(err, ErrInstallationNotFound) {
		t.Fatalf("revoked detail: %v", err)
	}
}
