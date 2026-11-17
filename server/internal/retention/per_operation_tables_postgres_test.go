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

// Every per-operation table is bounded (F21-23); ended previews and deleted tunnels and routes go 30 days later; replay records after a day, finished reconciliation attempts after 7 days, withdrawn DNS publications after 30: for each table that had no retention, one pass deletes a row past
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
	exec(`INSERT INTO paperboat.managed_ssh_certificate_holders(machine_id,cli_client_session_id,user_id,valid_before)
SELECT $1||'_machine',$1||k,$1,v FROM (VALUES('_old',now()-interval '2 hours'),('_live',now()+interval '1 hour')) x(k,v)`, user)
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
	// Previews: one ended 31 days ago, one ended now, one still active.
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state)
VALUES($1||'_host',$1,'env_'||$1,'host','linux','amd64','/workspace','offline','released')`, user)
	exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,
target_address,lease_deadline,created_at,terminal_state,stopped_at)
SELECT 'foreground',$1||k,'https://'||replace($1||k,'_','-')||'.preview.example.test',$1,$1,$1||'_host','s','http','127.0.0.1:3000',c+interval '1 hour',c,st,e
FROM (VALUES('_old',now()-interval '40 days','stopped',now()-interval '31 days'),('_recent',now()-interval '1 hour','expired',now()),
 ('_active',now(),'active',NULL::timestamptz)) v(k,c,st,e)`, user)
	// Tunnels: one deleted 31 days ago, one deleted now, one live with a route deleted 31 days ago and a live route.
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,
created_by_actor_id,created_at,updated_at,deleted_at)
SELECT $1||k,$1,k,st,'private',1,'endpoint_'||u,'https://'||u||'.tunnels.example.test',$1||'_host',$1,now()-interval '40 days',now(),d
FROM (SELECT gen_random_uuid()::text u,* FROM (VALUES('_t_old','deleted',now()-interval '31 days'),('_t_recent','deleted',now()),('_t_live','active',NULL::timestamptz)) v(k,st,d)) w`, user)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,
created_by_actor_id,updated_by_actor_id,created_at,updated_at,deleted_at)
SELECT $1||k,$1||t,k,'http','catch_all','http','127.0.0.1:3000',1,st,$1,$1,now()-interval '40 days',now(),d
FROM (VALUES('_r_old','_t_live','deleted',now()-interval '31 days'),('_r_live','_t_live','active',NULL::timestamptz),
 ('_r_of_old','_t_old','active',NULL::timestamptz)) v(k,t,st,d)`, user)
	// Replay records: one a day old, one fresh, and a route record still waiting for its result.
	exec(`INSERT INTO paperboat.control_route_operations(operation_key,operation_type,request_hash,route_id,result_revision,created_at)
SELECT $1||k,'create',sha256(convert_to(k,'UTF8')),'r',rev,c FROM (VALUES('_old',1,now()-interval '25 hours'),('_recent',1,now()),('_pending',NULL::bigint,now()-interval '25 hours')) v(k,rev,c)`, user)
	exec(`INSERT INTO paperboat.environment_vault_operations(account_id,operation_id,request_digest,result,created_at)
SELECT $1,$1||k,sha256(convert_to(k,'UTF8')),'\x01',c FROM (VALUES('_old',now()-interval '25 hours'),('_recent',now())) v(k,c)`, user)
	exec(`INSERT INTO paperboat.user_machine_capability_operations(user_machine_id,user_id,idempotency_key,request_hash,expected_version,resulting_version,configured_capabilities,result,created_at)
SELECT $1||'_host',$1,k,sha256(convert_to(k,'UTF8')),1,2,ARRAY[]::text[],'{}'::jsonb,c FROM (VALUES('old',now()-interval '25 hours'),('recent',now())) v(k,c)`, user)
	// Reconciliation attempts: finished 8 days ago, finished now, and one still started.
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1||'_env',$1||'_env',$1)`, user)
	exec(`INSERT INTO paperboat.control_reconciliation_attempts(id,environment_id,desired_version,state,started_at,finished_at)
SELECT $1||k,$1||'_env',1,st,now()-interval '9 days',f FROM (VALUES('_old','succeeded',now()-interval '8 days'),('_recent','failed',now()),
 ('_started','started',NULL::timestamptz)) v(k,st,f)`, user)
	// DNS publications: withdrawn 31 days ago, withdrawn now, and one 31 days old that still holds a provider record.
	exec(`INSERT INTO paperboat.edge_dns_publications(hostname,owner_id,resource_generation,readiness_version,observed_at,publication_generation,
desired_addresses,provider_records,state,created_at,updated_at)
SELECT $1||k||'.example.test',$1,1,'v',now(),1,'[]'::jsonb,rec,st,now()-interval '40 days',u
FROM (VALUES('_old','[]'::jsonb,'verified',now()-interval '31 days'),('_recent','[]'::jsonb,'verified',now()),
 ('_held','[{"id":"x"}]'::jsonb,'verified',now()-interval '31 days')) v(k,rec,st,u)`, user)
	// Terminal sessions: deleted 8 days ago, deleted 8 days ago with its host delete still pending, deleted now, live.
	exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,name,launch_cwd,desired_state,runtime_state,owner_account,deleted_at)
SELECT $1||k,$1||'_host','n'||replace(k,'_',''),'',CASE WHEN d IS NULL THEN 'open' ELSE 'deleted' END,'closed',$1,d
FROM (VALUES('_s_old',now()-interval '8 days'),('_s_pending',now()-interval '8 days'),('_s_recent',now()),('_s_live',NULL::timestamptz)) v(k,d)`, user)
	exec(`INSERT INTO paperboat.user_machine_terminal_session_operations(id,user_machine_id,terminal_session_id,operation,state)
VALUES($1||'_o_old',$1||'_host',$1||'_s_old','delete_history','applied'),($1||'_o_pending',$1||'_host',$1||'_s_pending','delete_history','pending')`, user)
	// Control routes: a detached route replaced by a newer one, a detached route that is its environment's newest,
	// a recently detached replaced route and the replacing live route.
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1||'_e1',$1,$1),($1||'_e2',$1,$1),($1||'_e3',$1,$1)`, user)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_state,created_at,updated_at)
SELECT $1||k,$1||e,'runtime_https_wss',replace($1||k,'_','-')||'.runtime.example.test','127.0.0.1',38080,st,c,u
FROM (VALUES('_cr_old','_e1','detached',now()-interval '40 days',now()-interval '8 days'),('_cr_new','_e1','attached',now()-interval '1 day',now()),
 ('_cr_only','_e2','detached',now()-interval '40 days',now()-interval '8 days'),
 ('_cr_recent','_e3','detached',now()-interval '40 days',now()),('_cr_recent_new','_e3','attached',now()-interval '1 day',now())) v(k,e,st,c,u)`, user)
	// Configuration generations of a live tunnel at generation 5: the oldest two are past their retention, 3 is not, 4 is
	// the previous generation the next one needs, 5 is active.
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,
created_by_actor_id,created_at,updated_at)
VALUES($1||'_t_gens',$1,'gens','active','private',5,'endpoint_'||gen_random_uuid(),'https://'||gen_random_uuid()||'.tunnels.example.test',$1||'_host',$1,now()-interval '200 days',now())`, user)
	exec(`INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,
created_at,activated_at,retained_until)
SELECT $1||'_t_gens',g,CASE WHEN g>1 THEN g-1 END,sha256(convert_to('g'||g,'UTF8')),'{}'::bytea,CASE WHEN g=5 THEN 'active' ELSE 'superseded' END,$1,
now()-interval '200 days',CASE WHEN g=5 THEN now() END,CASE WHEN g=3 THEN now()+interval '1 day' ELSE now()-interval '1 day' END
FROM generate_series(1,5) g`, user)
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
		"certificate_holders":         {`SELECT string_agg(replace(cli_client_session_id,$1,''),',') FROM paperboat.managed_ssh_certificate_holders WHERE user_id=$1`, "_live"},
		"managed_ssh_client_keys":     {`SELECT count(*)::text FROM paperboat.managed_ssh_client_keys WHERE user_id=$1`, "2"},
		"peer_endpoint_certificates":  {`SELECT string_agg(replace(endpoint_id,$1,''),',' ORDER BY endpoint_id) FROM paperboat.peer_endpoint_certificates WHERE user_id=$1`, "_live,_named"},
		"revocations":                 {`SELECT count(*)::text FROM paperboat.peer_endpoint_certificate_revocations WHERE user_id=$1`, "0"},
		"preview_leases":              {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.preview_leases WHERE account_id=$1`, "_active,_recent"},
		"tunnels":                     {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.tunnels WHERE account_id=$1`, "_t_gens,_t_live,_t_recent"},
		"tunnel_routes":               {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.tunnel_routes WHERE id LIKE $1||'%'`, "_r_live"},
		"control_route_operations":    {`SELECT string_agg(replace(operation_key,$1,''),',' ORDER BY operation_key) FROM paperboat.control_route_operations WHERE operation_key LIKE $1||'%'`, "_pending,_recent"},
		"vault_operations":            {`SELECT string_agg(replace(operation_id,$1,''),',' ORDER BY operation_id) FROM paperboat.environment_vault_operations WHERE account_id=$1`, "_recent"},
		"capability_operations":       {`SELECT string_agg(idempotency_key,',' ORDER BY idempotency_key) FROM paperboat.user_machine_capability_operations WHERE user_id=$1`, "recent"},
		"reconciliation_attempts":     {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.control_reconciliation_attempts WHERE environment_id=$1||'_env'`, "_recent,_started"},
		"edge_dns_publications":       {`SELECT string_agg(replace(replace(hostname,'.example.test',''),$1,''),',' ORDER BY hostname) FROM paperboat.edge_dns_publications WHERE owner_id=$1`, "_held,_recent"},
		"terminal_sessions":           {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.user_machine_terminal_sessions WHERE owner_account=$1`, "_s_live,_s_pending,_s_recent"},
		"terminal_operations":         {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.user_machine_terminal_session_operations WHERE id LIKE $1||'%'`, "_o_pending"},
		"control_routes":              {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.control_routes WHERE id LIKE $1||'%'`, "_cr_new,_cr_only,_cr_recent,_cr_recent_new"},
		"tunnel_config_generations":   {`SELECT string_agg(generation::text,',' ORDER BY generation) FROM paperboat.tunnel_config_generations WHERE tunnel_id=$1||'_t_gens'`, "3,4,5"},
		"peer_mesh_log":               {`SELECT string_agg(mesh_seq::text,',' ORDER BY mesh_seq) FROM paperboat.peer_mesh_log WHERE account_id=$1`, "2,5,6"},
	} {
		if got := left(test.query); got != test.want {
			t.Errorf("%s: left %q, want %q", name, got, test.want)
		}
	}
}
