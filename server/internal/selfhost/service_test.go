package selfhost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"os"
	"testing"
)

// The fixture begins after enrollment; enrollment protocol tests exercise claim
// signatures, receipt recovery and enrollment-code concurrency separately.
func scopedInstallationFixture(t *testing.T, database *db.DB, key, account, capability string) Registered {
	t.Helper()
	ctx := context.Background()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := Registered{InstallationID: "shi_" + randomIdentity(), NodeID: "shn_" + randomIdentity(), RuntimeCredential: randomToken(32), NodeGeneration: 1}
	r.EdgePool = r.InstallationID
	encrypted, err := secrets.Encrypt(key, r.RuntimeCredential)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,tls_spki_sha256) VALUES($1,$2,'1.0','','edge.example.test',27443,27444,'registered',false,'test','test-a',ARRAY['edge'],ARRAY['http2','http3'],8,0,now(),now()+interval '1 hour',$3)`, r.NodeID, r.InstallationID, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$4,$5,'Scoped fixture',$6,'account',$7,$1,true)`, r.InstallationID, r.NodeID, []byte(pub), digest(r.RuntimeCredential), encrypted, capability, account)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_pool_policies WHERE $1=ANY(installation_ids)`, r.InstallationID)
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE id=$1`, r.InstallationID)
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, r.NodeID)
	})
	return r
}

func TestInstallationAuthorityOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	key := "selfhost-test-encryption-key-32-bytes-long"
	service := New(database, key)
	account, other := "sha_"+randomIdentity(), "shb_"+randomIdentity()
	for _, id := range []string{account, other} {
		if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, account, other)
	})
	installed := scopedInstallationFixture(t, database, key, account, "relay")
	inventory, err := service.ListScoped(ctx, account, Scope{Kind: "account"}, false)
	if err != nil || len(inventory) != 1 {
		t.Fatalf("scoped inventory: %v %v", inventory, err)
	}
	if inventory[0].EndpointHost != "edge.example.test" || inventory[0].NodeID != installed.NodeID {
		t.Fatalf("inventory must expose the endpoint separately from the node ID: %+v", inventory[0])
	}
	if got, err := service.RuntimeNode(ctx, installed.RuntimeCredential); err != nil || got != installed.NodeID {
		t.Fatalf("runtime authority: %s %v", got, err)
	}
	restarted := New(database, key)
	if credential, err := restarted.RuntimeCredential(ctx, installed.NodeID); err != nil || credential != installed.RuntimeCredential {
		t.Fatalf("persisted runtime authority: %v", err)
	}
	if _, err := service.RuntimeNode(ctx, randomToken(32)); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatalf("unknown credential accepted: %v", err)
	}
	if err := service.RemoveScoped(ctx, other, installed.InstallationID, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("other account removed installation: %v", err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET expires_at=now()-interval '1 second' WHERE id=$1`, installed.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RuntimeNode(ctx, installed.RuntimeCredential); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatal("expired runtime accepted")
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET expires_at=NULL WHERE id=$1`, installed.InstallationID); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveScoped(ctx, account, installed.InstallationID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RuntimeNode(ctx, installed.RuntimeCredential); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatal("removed runtime accepted")
	}
	if _, err := service.RuntimeCredential(ctx, installed.NodeID); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatal("removed credential recovered")
	}
	if got, err := service.IsSelfHosted(ctx, installed.NodeID); err != nil || !got {
		t.Fatal("removed node can become managed")
	}
	var generation uint64
	if err := database.SQL().QueryRowContext(ctx, `SELECT node_generation FROM paperboat.control_tunnel_nodes WHERE id=$1 AND NOT ready AND state='retired'`, installed.NodeID).Scan(&generation); err != nil || generation <= installed.NodeGeneration {
		t.Fatalf("removed generation fence: %d %v", generation, err)
	}
}

func TestOperatorLabelsRejectTerminalControls(t *testing.T) {
	for _, value := range []string{"name\x1b[2J", "name\tother", "name\u202eevil", "name\nline"} {
		if validLabel(value, 80) {
			t.Fatalf("unsafe label accepted: %q", value)
		}
	}
	if !validLabel("Friends’ relay", 80) {
		t.Fatal("ordinary international label rejected")
	}
}
