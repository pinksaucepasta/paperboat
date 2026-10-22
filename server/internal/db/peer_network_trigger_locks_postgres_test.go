package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
)

// Removing a machine fires the peer-network triggers (machine, CLI session, certificate, environment), which look
// up the account's machine access sessions by CLI session, machine or environment. Without an index for those
// lookups they scanned the table, and a serializable transaction then holds a relation-level predicate lock on
// user_machine_access_sessions: every concurrent access-session write conflicts with it, so a disconnect failed
// all ten serialization retries (503 after 7.7 s) on a busy account. The triggers must only lock what they read.
func TestPeerNetworkTriggersTakeNoTableWidePredicateLockOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, machine, cli, env := "pl_user_"+s, "pl_machine_"+s, "pl_cli_"+s, "pl_env_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
VALUES($1,$1,$2,'active',1,'ready',1,now(),now())`, env, user)
	// A busy account: 40 machines with 600 access sessions between them, over many table pages.
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT CASE WHEN g=0 THEN $1 ELSE $1||'_'||g END,$2,CASE WHEN g=0 THEN $3 ELSE $3||'_'||g END,'pl-'||g,'linux','amd64','/w','online','occupied',true
FROM generate_series(0,39) g`, machine, user, env)
	exec(`INSERT INTO paperboat.user_machine_access_sessions(id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,
helper_terminal_session_id,state,revoked_at,revocation_reason,expires_at,capabilities,credential_kind)
SELECT $1||'_access_'||g,CASE WHEN g%40=0 THEN $1 ELSE $1||'_'||(g%40) END,$2,CASE WHEN g%40=0 THEN $3 ELSE $3||'_'||(g%40) END,$4||'_'||(g%25),'https://helper.example.test','t'||g,
 CASE WHEN g%10=0 THEN 'active' ELSE 'revoked' END,CASE WHEN g%10=0 THEN NULL ELSE now() END,CASE WHEN g%10=0 THEN NULL ELSE 'expired' END,
 now()+interval '1 hour',ARRAY['terminal'],'control_plane_signed' FROM generate_series(1,600) g`, machine, user, env, cli)
	exec(`ANALYZE paperboat.user_machine_access_sessions`)
	for i, endpoint := range []struct{ id, role string }{{machine, "machine"}, {cli + "_0", "cli"}} {
		fingerprint := sha256.Sum256([]byte(endpoint.id))
		key := sha256.Sum256([]byte("key" + endpoint.id))
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES($1,$2,$3,$4,1,1,$5,$6,now()-interval '1 minute',now()+interval '1 day','key',$6)`, fingerprint[:], user, endpoint.id, endpoint.role, make([]byte, 200), key[:])
		machineID, machineGeneration := any(nil), 0
		if endpoint.role == "machine" {
			machineID, machineGeneration = endpoint.id, 1
		}
		exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,1,$6,$7,$8::inet,now(),now())`,
			user, endpoint.id, endpoint.role, machineID, machineGeneration, key[:], fingerprint[:], fmt.Sprintf("100.127.%d.%d", time.Now().UnixNano()%250, i+1))
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE paperboat.peer_endpoint_certificates SET revoked_at=now(),revocation_reason='endpoint_removed' WHERE user_id=$1 AND endpoint_id=$2`, []any{user, machine}},
		{`UPDATE paperboat.user_machines SET state='disconnected',seat_state='released',revoked_at=now() WHERE user_id=$1 AND id=$2`, []any{user, machine}},
		{`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE user_id=$1 AND id=$2`, []any{user, cli + "_0"}},
		{`UPDATE paperboat.control_environments SET revoked_at=now() WHERE owner_user_id=$1 AND id=$2`, []any{user, env}},
		{`UPDATE paperboat.peer_network_identities SET revoked_at=now() WHERE user_id=$1 AND endpoint_id=$2`, []any{user, machine}},
	}
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL SERIALIZABLE`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("%v\n%s", err, statement.query)
		}
		var tableWide int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid=l.relation
WHERE l.pid=pg_backend_pid() AND l.mode='SIReadLock' AND l.locktype='relation' AND c.relname='user_machine_access_sessions'`).Scan(&tableWide); err != nil {
			t.Fatal(err)
		}
		if tableWide != 0 {
			t.Fatalf("%s\nholds a table-wide predicate lock on user_machine_access_sessions: every concurrent access-session write would conflict with it", statement.query)
		}
	}
}
