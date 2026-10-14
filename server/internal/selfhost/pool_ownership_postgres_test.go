package selfhost

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
)

func TestGlobalHostedPoolAndPendingEnrollmentOnPostgres(t *testing.T) {
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
	owner, other := "pool_owner_"+randomIdentity(), "pool_other_"+randomIdentity()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, actor := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, query := range []string{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id IN($1,$2)`, `DELETE FROM paperboat.users WHERE id IN($1,$2)`} {
			if _, err := store.SQL().ExecContext(cleanup, query, owner, other); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
	})
	key := "selfhost-pool-test-encryption-key-32-bytes-long"
	service := New(store, key)
	global := scopedInstallationFixture(t, store, key, owner, "relay")
	private := scopedInstallationFixture(t, store, key, owner, "relay")
	pending := scopedInstallationFixture(t, store, key, owner, "relay")
	exec(`UPDATE paperboat.control_tunnel_nodes SET roles=ARRAY['relay'],transports=ARRAY['derp_quic','derp_wss'] WHERE id IN($1,$2,$3)`, global.NodeID, private.NodeID, pending.NodeID)
	exec(`UPDATE paperboat.selfhost_installations SET scope_kind='global',scope_id='global' WHERE id=$1`, global.InstallationID)
	exec(`UPDATE paperboat.selfhost_installations SET claim_complete=false WHERE id=$1`, pending.InstallationID)
	// A partially acknowledged claim must not gain authority from node readiness.
	exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,last_heartbeat_at=now() WHERE id=$1`, pending.NodeID)
	eligible := func(node, actor string, want bool) {
		t.Helper()
		var actual bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,'relay')`, node, actor).Scan(&actual); err != nil || actual != want {
			t.Fatalf("pool eligibility=%v want %v: %v", actual, want, err)
		}
	}
	for _, actor := range []string{owner, other} {
		eligible(global.NodeID, actor, true)
	}
	for _, mode := range []string{"mixed", "self-hosted-only"} {
		if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: mode, InstallationIDs: []string{global.InstallationID}}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("global installation selected as customer-owned: %v", err)
		}
	}
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "self-hosted-only", InstallationIDs: []string{private.InstallationID}}); err != nil {
		t.Fatal(err)
	}
	eligible(global.NodeID, owner, false)
	eligible(private.NodeID, owner, true)
	// Defense in depth: even malformed persisted IDs cannot classify global
	// hosted capacity as customer-owned self-hosted-only capacity.
	exec(`UPDATE paperboat.selfhost_pool_policies SET installation_ids=ARRAY[$2,$3] WHERE scope_kind='account' AND scope_id=$1 AND capability='relay'`, owner, private.InstallationID, global.InstallationID)
	eligible(global.NodeID, owner, false)
	eligible(private.NodeID, owner, true)
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "mixed", InstallationIDs: []string{}}); err != nil {
		t.Fatal(err)
	}
	eligible(global.NodeID, owner, true)
	list, err := service.ListScoped(ctx, owner, Scope{Kind: "account", ID: owner}, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, installation := range list {
		if installation.InstallationID == pending.InstallationID {
			found = true
			if installation.EnrollmentState != "pending" || installation.Ready {
				t.Fatalf("pending enrollment presented as usable: state=%s ready=%v", installation.EnrollmentState, installation.Ready)
			}
		}
	}
	if !found {
		t.Fatal("owner cannot inspect pending enrollment")
	}
	eligible(pending.NodeID, owner, false)
	if _, err = service.RuntimeNode(ctx, pending.RuntimeCredential); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatalf("pending runtime credential authorized: %v", err)
	}
	if _, err = service.RuntimeCredential(ctx, pending.NodeID); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatalf("pending runtime credential recoverable: %v", err)
	}
	if _, err = service.SetPool(ctx, owner, "relay", Policy{Mode: "self-hosted-only", InstallationIDs: []string{pending.InstallationID}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("pending enrollment selected: %v", err)
	}
	if err = service.RemoveScoped(ctx, other, pending.InstallationID, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("outsider removed pending enrollment: %v", err)
	}
	if err = service.RemoveScoped(ctx, owner, pending.InstallationID, false); err != nil {
		t.Fatalf("owner cannot remove pending enrollment: %v", err)
	}
	list, err = service.ListScoped(ctx, owner, Scope{Kind: "account", ID: owner}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, installation := range list {
		if installation.InstallationID == pending.InstallationID {
			t.Fatal("removed pending enrollment retained")
		}
	}
	eligible(pending.NodeID, owner, false)
}
