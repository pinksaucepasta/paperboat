package usermachines

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

// A revoke must not lose to the account's other machines refreshing their peer configuration: on a restored prod dump
// (2026-11-20) revoking a machine with 0-1 live sessions failed 4/4 with 40001 after all retries while 4 writers bumped
// config_generation on the same account's other machines. Here 4 writers do that without pause while a machine with no
// access sessions and an active enrollment CLI session is revoked: the revoke must succeed.
func TestRevokeWhileTheAccountsMachinesRefreshTheirConfigOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	user := "rvw_user_" + x
	machine := func(g int) string { return fmt.Sprintf("rvw_m_%d_%s", g, x) }
	cli := "rvw_cli_" + x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.user_machine_access_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.peer_network_identities WHERE user_id=$1`,
			`DELETE FROM paperboat.peer_endpoint_certificates WHERE user_id=$1`,
			`DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.user_machines WHERE user_id=$1`,
			`DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`,
			`DELETE FROM paperboat.users WHERE id=$1`,
		} {
			_, _ = store.SQL().ExecContext(bg, q, user)
		}
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT 'rvw_env_'||g||'_'||$2,'rvw_env_'||g||'_'||$2,$1 FROM generate_series(0,8) g`, user, x)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT 'rvw_m_'||g||'_'||$2,$1,'rvw_env_'||g||'_'||$2,'m'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(0,8) g`, user, x)
	// The target machine's enrollment CLI session (pb pair creates one), with its own peer identity.
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES($1,$2,'client','rvw','desktop','test',ARRAY['machines:connect'],'active',now(),now(),$3)`, cli, user, machine(0))
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to('q'||e.id,'UTF8')),$1,e.id,e.role,1,1,decode(repeat('00',200),'hex'),sha256(convert_to('k'||e.id,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('k'||e.id,'UTF8'))
FROM (SELECT 'rvw_m_'||g||'_'||$2 AS id,'machine' AS role FROM generate_series(0,8) g UNION ALL SELECT $3,'cli') e`, user, x, cli)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,'rvw_m_'||g||'_'||$2,'machine','rvw_m_'||g||'_'||$2,1,1,1,sha256(convert_to('wg'||g||$2,'UTF8')),sha256(convert_to('q'||'rvw_m_'||g||'_'||$2,'UTF8')),('100.72.'||(abs(hashtext($2))%200)||'.'||(1+g))::inet,now(),now() FROM generate_series(0,8) g
UNION ALL SELECT $1,$3,'cli',NULL,1,0,1,sha256(convert_to('wgcli'||$2,'UTF8')),sha256(convert_to('q'||$3,'UTF8')),('100.72.'||(abs(hashtext($2))%200)||'.200')::inet,now(),now()`, user, x, cli)

	// The target's enrollment CLI session ran commands on the account's other machines earlier: its access sessions to
	// them are over (revoked, expired), so the target has no live session, but the CLI session's peer-network trigger
	// still names those machines (advance_peer_network_for_cli_session reads access sessions in any state).
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,revoked_at,helper_revoked_at,helper_terminal_session_id,capabilities,operation_id)
SELECT 'control_plane_signed','rvw_s_'||g||'_'||$2,'rvw_m_'||g||'_'||$2,$1,'rvw_env_'||g||'_'||$2,$3,'',now()-interval '1 hour','revoked',now()-interval '1 hour',now()-interval '1 hour','h_'||g||'_'||$2,ARRAY['exec'],'op' FROM generate_series(1,8) g`, user, x, cli)
	// 4 writers: the other machines' configuration requests (endpointsUpdate's generation bump), without pause.
	loadCtx, stop := context.WithCancel(ctx)
	var load sync.WaitGroup
	for w := range 4 {
		load.Go(func() {
			for i := 0; loadCtx.Err() == nil; i++ {
				_, _ = store.SQL().ExecContext(loadCtx, `UPDATE paperboat.peer_network_identities SET config_generation=config_generation+1,updated_at=now() WHERE user_id=$1 AND endpoint_id=$2`, user, machine(1+(w*2+i)%8))
			}
		})
	}
	defer func() { stop(); load.Wait() }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	err := New(store, audit.NewWriter(store), Policy{}, nil).Disconnect(ctx, user, machine(0))
	took := time.Since(start)
	stats := store.TransactionStats()
	if err != nil {
		t.Fatalf("revoke while 4 writers refresh the account's other machines failed after %v: %v (serializable %+v, read committed %+v)", took, err, stats.Serializable, stats.ReadCommitted)
	}
	var state string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.cli_client_sessions WHERE id=$1`, cli).Scan(&state); err != nil || state == "active" {
		t.Fatalf("the machine's enrollment CLI session is %q after the revoke (%v)", state, err)
	}
	t.Logf("revoke took %v; serializable %+v, read committed %+v", took, stats.Serializable, stats.ReadCommitted)
}
