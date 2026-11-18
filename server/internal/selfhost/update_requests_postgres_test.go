package selfhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Pressing Update records one request per node; the node picks it up at its check-in with its own runtime
// credential, reports each step, and a finished request can be replaced by the next. Another account cannot
// press it, another node cannot report on it, and a finished request cannot be reopened by a late report.
func TestSelfhostUpdateRequestOnPostgres(t *testing.T) {
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
	stamp := randomIdentity()
	owner, other, node, installation := "upd_owner_"+stamp, "upd_other_"+stamp, "upd_node_"+stamp, "upd_inst_"+stamp
	credential := strings.Repeat("c", 40) + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	for _, actor := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, q := range []string{`DELETE FROM paperboat.selfhost_update_requests WHERE node_id=$1`, `DELETE FROM paperboat.selfhost_installations WHERE node_id=$1`, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`} {
			if _, err := store.SQL().ExecContext(cleanup, q, node); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, owner, other); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,endpoint_host)
VALUES($1,'default','1.0','epoch','ready',true,now(),'fsn1',$1,ARRAY['relay'],ARRAY['derp_quic'],100,0,now(),now()+interval '1 hour','relay.example.test')`, node)
	hash := sha256.Sum256([]byte(credential))
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(md5($1||'k')||md5($1||'kk'),'hex'),$3,'fixture','Relay','relay','account',$4,$1,true)`, installation, node, hash[:], owner)
	service := New(store, "selfhost-update-test-encryption-key-32-by")

	if _, err := service.RequestUpdate(ctx, other, installation, false, "2026.11.18.1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another account pressed Update: %v", err)
	}
	first, err := service.RequestUpdate(ctx, owner, installation, false, "2026.11.18.1")
	if err != nil || first.State != "requested" || first.TargetVersion != "2026.11.18.1" {
		t.Fatalf("request = %+v, %v", first, err)
	}
	if _, err := service.RequestUpdate(ctx, owner, installation, false, "2026.11.18.1"); !errors.Is(err, ErrUpdateRunning) {
		t.Fatalf("second request while running = %v", err)
	}
	if _, err := service.UpdateCheck(ctx, strings.Repeat("x", 40)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("check with another credential = %v", err)
	}
	pending, err := service.UpdateCheck(ctx, credential)
	if err != nil || pending == nil || pending.ID != first.ID {
		t.Fatalf("check = %+v, %v", pending, err)
	}
	if err := service.UpdateReport(ctx, credential, first.ID, "downloading", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateReport(ctx, credential, first.ID, "bogus", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown step = %v", err)
	}
	if err := service.UpdateReport(ctx, credential, first.ID, "rolled_back", "health check failed: relay not ready in 120s", "support_ref_1"); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateReport(ctx, credential, first.ID, "succeeded", "", ""); !errors.Is(err, ErrUpdateNotRunning) {
		t.Fatalf("late report reopened a finished update: %v", err)
	}
	if pending, err := service.UpdateCheck(ctx, credential); err != nil || pending != nil {
		t.Fatalf("finished update still pending: %+v, %v", pending, err)
	}
	list, err := service.ListScopedInventory(ctx, owner, Scope{Kind: "account", ID: owner}, false, InventoryOptions{})
	if err != nil || len(list.Installations) != 1 || list.Installations[0].Update == nil || list.Installations[0].Update.State != "rolled_back" ||
		list.Installations[0].Update.SupportReference != "support_ref_1" || !strings.Contains(list.Installations[0].Update.Detail, "health check failed") {
		t.Fatalf("inventory = %+v, %v", list.Installations, err)
	}
	if again, err := service.RequestUpdate(ctx, owner, installation, false, "2026.11.18.2"); err != nil || again.ID == first.ID || again.State != "requested" {
		t.Fatalf("request after a finished one = %+v, %v", again, err)
	}
}
