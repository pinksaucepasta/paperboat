package retention

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Every per-operation table is bounded (F21-23): for each table that had no retention, one pass deletes a row past
// its window and keeps a live one (the connector enrollment policy, whose rows need a whole tunnel, runs on the
// schema in TestRetentionDrainsBacklogsAndKeepsLiveRowsOnPostgres). Fixtures are ordinary rows, so the test runs as
// a non-superuser app role too. A long-expired peer certificate goes with its revocation, unless an identity
// still names it; a mesh log entry goes once a newer entry of its endpoint replaced it, or when it is the removal of
// an endpoint long gone, and the latest entry of a present endpoint stays.
func TestRetentionBoundsEveryPerOperationTableOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user := "ret_ops_" + s
	fixture, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Rollback(context.Background())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := fixture.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	// old: past its window; live: inside it.
	exec(`INSERT INTO paperboat.browser_machine_credentials(credential_id,token_hash,account_id,hostname,resource_kind,resource_id,route_id,action,
resource_generation,owner_account_id,access_mode,issued_at,expires_at)
SELECT $1||k,sha256(convert_to($1||k,'UTF8')),$1,'h.example.test','preview','p','r','use',1,$1,'private',t-interval '1 minute',t
FROM (VALUES('_old',now()-interval '2 hours'),('_live',now()+interval '1 minute')) v(k,t)`, user)
	exec(`INSERT INTO paperboat.control_operations(id,operation_key,operation_type,request_hash,state,completed_at)
SELECT $1||k,$1||k,'t',sha256(convert_to(k,'UTF8')),st,c FROM (VALUES('_old','succeeded',now()-interval '8 days'),('_live','succeeded',now()),('_running','running',NULL::timestamptz)) v(k,st,c)`, user)
	exec(`INSERT INTO paperboat.transactional_mail_outbox(mail_id,payload,state,created_at)
SELECT $1||k,'{}'::jsonb,st,c FROM (VALUES('_old','sent',now()-interval '8 days'),('_live','sent',now()),('_queued','queued',now()-interval '8 days')) v(k,st,c)`, user)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at)
VALUES($1||'_cli',$1,'pb','laptop','cli','linux',ARRAY['machines:connect'],'active',now(),now())`, user)
	exec(`INSERT INTO paperboat.managed_ssh_client_keys(fingerprint,user_id,cli_client_session_id,algorithm,public_key,state,created_at,revoked_at,revocation_reason)
SELECT sha256(convert_to($1||k,'UTF8')),$1,$1||'_cli','ssh-ed25519','ssh-ed25519 '||md5($1||k)||repeat('k',64),st,now()-interval '60 days',r,CASE WHEN r IS NULL THEN NULL ELSE 'client_logout' END
FROM (VALUES('_old','revoked',now()-interval '31 days'),('_live','revoked',now()),('_active','active',NULL::timestamptz)) v(k,st,r)`, user)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to($1||k,'UTF8')),$1,$1||k,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('q'||k,'UTF8')),e-interval '1 day',e,'key',sha256(convert_to('s'||k,'UTF8'))
FROM (VALUES('_old',now()-interval '31 days'),('_named',now()-interval '31 days'),('_live',now()+interval '1 day')) v(k,e)`, user)
	exec(`INSERT INTO paperboat.peer_endpoint_certificate_revocations(operation_id,user_id,certificate_fingerprint,serial,reason,created_at)
VALUES($1||'_revocation_old',$1,sha256(convert_to($1||'_old','UTF8')),1,'endpoint_removed',now()-interval '31 days')`, user)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($1,$1||'_named','cli',NULL,1,0,1,sha256(convert_to('w'||$1,'UTF8')),sha256(convert_to($1||'_named','UTF8')),'100.127.255.254',now(),now())`, user)
	exec(`INSERT INTO paperboat.peer_mesh_accounts(account_id) VALUES($1)`, user)
	exec(`INSERT INTO paperboat.peer_mesh_log(account_id,mesh_seq,endpoint_id,op,entry,recorded_at) VALUES
 ($1,1,'a','add','{}',now()-interval '3 hours'),($1,2,'a','update','{}',now()-interval '2 hours'),
 ($1,3,'b','add','{}',now()-interval '40 days'),($1,4,'b','remove',NULL,now()-interval '31 days'),
 ($1,5,'c','add','{}',now()-interval '40 days'),($1,6,'c','update','{}',now())`, user)
	if err := fixture.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, store, config.Default().Retention); err != nil {
		t.Fatal(err)
	}
	left := func(query string) string {
		t.Helper()
		var out string
		if err := store.SQL().QueryRowContext(ctx, query, user).Scan(&out); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return out
	}
	for name, test := range map[string]struct{ query, want string }{
		"browser_machine_credentials": {`SELECT string_agg(replace(credential_id,$1,''),',' ORDER BY credential_id) FROM paperboat.browser_machine_credentials WHERE account_id=$1`, "_live"},
		"control_operations":          {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.control_operations WHERE id LIKE $1||'%'`, "_live,_running"},
		"transactional_mail_outbox":   {`SELECT string_agg(replace(mail_id,$1,''),',' ORDER BY mail_id) FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE $1||'%'`, "_live,_queued"},
		"managed_ssh_client_keys":     {`SELECT count(*)::text FROM paperboat.managed_ssh_client_keys WHERE user_id=$1`, "2"},
		"peer_endpoint_certificates":  {`SELECT string_agg(replace(endpoint_id,$1,''),',' ORDER BY endpoint_id) FROM paperboat.peer_endpoint_certificates WHERE user_id=$1`, "_live,_named"},
		"revocations":                 {`SELECT count(*)::text FROM paperboat.peer_endpoint_certificate_revocations WHERE user_id=$1`, "0"},
		"peer_mesh_log":               {`SELECT string_agg(mesh_seq::text,',' ORDER BY mesh_seq) FROM paperboat.peer_mesh_log WHERE account_id=$1`, "2,5,6"},
	} {
		if got := left(test.query); got != test.want {
			t.Errorf("%s: left %q, want %q", name, got, test.want)
		}
	}
}
