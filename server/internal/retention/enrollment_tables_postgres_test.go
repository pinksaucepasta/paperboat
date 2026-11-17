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

// Helper enrollments, machine pairings, pairing reset receipts and account deletion jobs are bounded; each case
// deletes a finished row past its window and keeps a recent one and a live one. A deleted preview domain and a
// deleted tunnel domain go once the 30-day window and their 7-day hostname hold are over, never while the hold runs.
func TestRetentionBoundsEnrollmentsAndHeldDomainsOnPostgres(t *testing.T) {
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
	user := "ret_enr_" + fmt.Sprint(time.Now().UnixNano())
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
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1||'_env',$1||'_env',$1)`, user)
	exec(`INSERT INTO paperboat.control_helpers(id,environment_id) VALUES($1||'_helper',$1||'_env')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state)
VALUES($1||'_host',$1,$1||'_env','host','linux','amd64','/workspace','offline','released')`, user)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at)
VALUES($1||'_cli',$1,'pb','laptop','cli','linux',ARRAY['machines:connect'],'active',now(),now())`, user)

	// Helper enrollments (credential window, 1 h after expiry): expired long ago, expired just now, still pending, and an old one a
	// pairing still names.
	exec(`INSERT INTO paperboat.control_helper_enrollments(id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at,created_at)
SELECT $1||k,$1||'_env',$1||'_helper',sha256(convert_to($1||k,'UTF8')),$1||k,sha256(convert_to('r'||k,'UTF8')),'\x00',st,e,e-interval '10 minutes'
FROM (VALUES('_h_old','consumed',now()-interval '2 hours'),('_h_recent','consumed',now()-interval '10 minutes'),
 ('_h_live','pending',now()+interval '5 minutes'),('_h_named','expired',now()-interval '2 hours')) v(k,st,e)`, user)
	// Pairings (window 7 days): cancelled 10 days ago, cancelled 2 days ago, waiting for bootstrap, failed but retryable.
	exec(`INSERT INTO paperboat.user_machine_pairings(id,state,installation_state,generation,expires_at,created_at,updated_at)
SELECT $1||k,st,ist,1,e,e-interval '1 hour',e FROM (VALUES
 ('_p_old','expired','cancelled',now()-interval '10 days'),('_p_recent','expired','cancelled',now()-interval '2 days'),
 ('_p_live','awaiting_bootstrap','awaiting_bootstrap',now()+interval '1 hour'),('_p_retry','expired','failed_retryable',now()-interval '10 days')) v(k,st,ist,e)`, user)
	exec(`UPDATE paperboat.user_machine_pairings SET approved_by_user_id=$1,user_machine_id=$1||'_host',authenticated_setup_cli_session_id=$1||'_cli',
authenticated_setup_operation_id='operation_'||id,authenticated_setup_generation=1,authenticated_setup_helper_enrollment_id=$1||'_h_named'
WHERE id=$1||'_p_retry'`, user)
	// Failed pairings (window 30 days): untouched for 40 days (deleted), 10 days (kept). Installed pairings (window 7
	// days): of a live machine an old one replaced by a newer one (deleted), a replaced one only 2 days old (kept) and the
	// newest (kept); of a removed machine an old one (deleted) and a recent one (kept).
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1||'_env2',$1||'_env2',$1,'revoked')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,revoked_at,deleted_at)
VALUES($1||'_gone',$1,$1||'_env2','gone','linux','amd64','/workspace','disconnected','released',now()-interval '20 days',now()-interval '20 days')`, user)
	exec(`INSERT INTO paperboat.user_machine_pairings(id,state,installation_state,generation,expires_at,created_at,updated_at,user_machine_id,
verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,public_identity_key)
SELECT $1||k,'expired',ist,1,e,c,e,$1||m,sha256(convert_to($1||k,'UTF8')),upper(substr(md5($1||k),1,8)),'a','linux','amd64','/workspace',repeat(md5($1||k),2)
FROM (VALUES
 ('_p_fail_old','failed_retryable','_host',now()-interval '40 days',now()-interval '41 days'),
 ('_p_fail_recent','failed_retryable','_host',now()-interval '10 days',now()-interval '11 days'),
 ('_p_ready_old','ready','_host',now()-interval '10 days',now()-interval '20 days'),
 ('_p_ready_replaced_recent','ready','_host',now()-interval '2 days',now()-interval '15 days'),
 ('_p_ready_new','ready','_host',now()-interval '1 day',now()-interval '1 day'),
 ('_p_gone_old','ready','_gone',now()-interval '10 days',now()-interval '20 days'),
 ('_p_gone_recent','ready','_gone',now()-interval '2 days',now()-interval '3 days')) v(k,ist,m,e,c)`, user)
	// Reset receipts (window 7 days): an old one of a pairing that lives on, a recent one, one of a pairing still
	// waiting for bootstrap (a retried reset replays it), and one of a pairing the policy deletes (cascade).
	exec(`INSERT INTO paperboat.user_machine_pairing_resets(enrollment_id,enrollment_generation,account_id,verifier_hash,request_hash,retired_machine_ids,completed_at)
SELECT $1||p,g,$1,sha256(convert_to('v','UTF8')),sha256(convert_to('q','UTF8')),'["m"]'::jsonb,c FROM (VALUES
 ('_p_retry',1,now()-interval '10 days'),('_p_retry',2,now()-interval '1 day'),('_p_live',1,now()-interval '10 days'),
 ('_p_old',1,now()-interval '10 days')) v(p,g,c)`, user)
	// Account deletion jobs (window 90 days): done and failed long ago, done recently, queued long ago.
	exec(`INSERT INTO paperboat.account_deletion_jobs(id,user_id,kind,target,state,created_at,completed_at)
SELECT $1||k,$1,'workos_user',k,st,c,d FROM (VALUES
 ('_j_done_old','done',now()-interval '100 days',now()-interval '100 days'),('_j_failed_old','failed',now()-interval '100 days',NULL::timestamptz),
 ('_j_done_recent','done',now()-interval '10 days',now()-interval '10 days'),('_j_queued_old','queued',now()-interval '100 days',NULL::timestamptz))
 v(k,st,c,d)`, user)

	// Preview and tunnel domains deleted 40 days ago: hold over (deleted), hold still running (kept); one deleted 10
	// days ago (kept, inside the window) and one live.
	exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,
target_address,lease_deadline,created_at,terminal_state)
VALUES('foreground',$1||'_preview','https://'||replace($1,'_','-')||'.preview.example.test',$1,$1,$1||'_host','s','http','127.0.0.1:3000',now()+interval '1 hour',now(),'active')`, user)
	exec(`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,dns_target,
conflict_state,quarantine_until,deleted_at)
SELECT $1||'_pd'||k,$1,$1||'_preview',1,k||'.'||$1||'.preview-domains.example.test','exact','c','t',
 CASE WHEN q IS NULL THEN 'clear' ELSE 'quarantined' END,q,d
FROM (VALUES('old',now()-interval '33 days',now()-interval '40 days'),('hold',now()+interval '1 day',now()-interval '40 days'),
 ('recent',now()-interval '3 days',now()-interval '10 days'),('live',NULL::timestamptz,NULL::timestamptz)) v(k,q,d)`, user)
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,
created_by_actor_id) SELECT $1||'_tunnel',$1,'t','active','private',1,'endpoint_'||u,'https://'||u||'.tunnels.example.test',$1||'_host',$1
FROM (SELECT gen_random_uuid()::text u) w`, user)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id)
VALUES($1||'_route',$1||'_tunnel','r','http','catch_all','http','127.0.0.1:3000',1,'active',$1,$1)`, user)
	exec(`INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,
conflict_state,quarantine_until,deleted_at)
SELECT $1||'_td'||k,$1,$1||'_tunnel',$1||'_route',k||'.'||$1||'.tunnel-domains.example.test','exact','c','t',
 CASE WHEN q IS NULL THEN 'clear' ELSE 'quarantined' END,q,d
FROM (VALUES('old',now()-interval '33 days',now()-interval '40 days'),('hold',now()+interval '1 day',now()-interval '40 days'),
 ('recent',now()-interval '3 days',now()-interval '10 days'),('live',NULL::timestamptz,NULL::timestamptz)) v(k,q,d)`, user)
	if err := fixture.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, store, config.Default().Retention); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct{ query, want string }{
		"control_helper_enrollments": {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.control_helper_enrollments WHERE id LIKE $1||'%'`, "_h_live,_h_named,_h_recent"},
		"user_machine_pairings":      {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.user_machine_pairings WHERE id LIKE $1||'%'`, "_p_fail_recent,_p_gone_recent,_p_live,_p_ready_new,_p_ready_replaced_recent,_p_recent,_p_retry"},
		"user_machine_pairing_resets": {`SELECT string_agg(replace(enrollment_id,$1,'')||':'||enrollment_generation,',' ORDER BY enrollment_id,enrollment_generation)
FROM paperboat.user_machine_pairing_resets WHERE account_id=$1`, "_p_live:1,_p_retry:2"},
		"account_deletion_jobs": {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.account_deletion_jobs WHERE user_id=$1`, "_j_done_recent,_j_queued_old"},
		"preview_domains":       {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.preview_domains WHERE account_id=$1`, "_pdhold,_pdlive,_pdrecent"},
		"tunnel_domains":        {`SELECT string_agg(replace(id,$1,''),',' ORDER BY id) FROM paperboat.tunnel_domains WHERE account_id=$1`, "_tdhold,_tdlive,_tdrecent"},
	} {
		var got string
		if err := store.SQL().QueryRowContext(ctx, test.query, user).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != test.want {
			t.Errorf("%s: left %q, want %q", name, got, test.want)
		}
	}
}
