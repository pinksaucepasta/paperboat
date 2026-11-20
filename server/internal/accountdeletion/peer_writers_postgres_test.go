package accountdeletion

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Deleting an account must not lose to its machines refreshing their peer configuration: the erase revokes the CLI
// sessions (whose trigger bumps every machine they reached) and deletes the account's peer identity rows, the rows
// those configuration requests update all the time; in one serializable transaction that lost every retry (the machine
// revoke failed 4/4 so on a restored prod dump, 2026-11-20). Here 4 writers bump them without pause during the deletion.
func TestAccountDeletionWhileItsMachinesRefreshTheirConfig(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	u := f.user("peerw")
	x := f.suffix
	machine := func(g int) string { return fmt.Sprintf("pw_m_%d_%s", g, x) }
	cli := "pw_cli_" + x
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT 'pw_m_'||g||'_'||$2,$1,'pw_m_'||g||'_'||$2,'m'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(0,8) g`, u.ID, x)
	f.exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES($1,$2,'client','pw','desktop','test',ARRAY['machines:connect'],'active',now(),now(),$3)`, cli, u.ID, machine(0))
	f.exec(`INSERT INTO peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to('q'||e.id,'UTF8')),$1,e.id,e.role,1,1,decode(repeat('00',200),'hex'),sha256(convert_to('k'||e.id,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('k'||e.id,'UTF8'))
FROM (SELECT 'pw_m_'||g||'_'||$2 AS id,'machine' AS role FROM generate_series(0,8) g UNION ALL SELECT $3,'cli') e`, u.ID, x, cli)
	f.exec(`INSERT INTO peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,'pw_m_'||g||'_'||$2,'machine','pw_m_'||g||'_'||$2,1,1,1,sha256(convert_to('wg'||g||$2,'UTF8')),sha256(convert_to('q'||'pw_m_'||g||'_'||$2,'UTF8')),('100.74.'||(abs(hashtext($2))%200)||'.'||(1+g))::inet,now(),now() FROM generate_series(0,8) g
UNION ALL SELECT $1,$3,'cli',NULL,1,0,1,sha256(convert_to('wgcli'||$2,'UTF8')),sha256(convert_to('q'||$3,'UTF8')),('100.74.'||(abs(hashtext($2))%200)||'.200')::inet,now(),now()`, u.ID, x, cli)
	// The CLI session ran commands on the other machines earlier (sessions over), so its peer-network trigger names them.
	f.exec(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,revoked_at,helper_revoked_at,helper_terminal_session_id,capabilities,operation_id)
SELECT 'control_plane_signed','pw_s_'||g||'_'||$2,'pw_m_'||g||'_'||$2,$1,'pw_m_'||g||'_'||$2,$3,'',now()-interval '1 hour','revoked',now()-interval '1 hour',now()-interval '1 hour','h_'||g||'_'||$2,ARRAY['exec'],'op' FROM generate_series(1,8) g`, u.ID, x, cli)

	loadCtx, stop := context.WithCancel(ctx)
	var load sync.WaitGroup
	for w := range 4 {
		load.Go(func() {
			for i := 0; loadCtx.Err() == nil; i++ {
				_, _ = f.store.SQL().ExecContext(loadCtx, `UPDATE peer_network_identities SET config_generation=config_generation+1,updated_at=now() WHERE user_id=$1 AND endpoint_id=$2`, u.ID, machine(1+(w*2+i)%8))
			}
		})
	}
	defer func() { stop(); load.Wait() }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	err := f.svc.Delete(ctx, u, time.Now(), u.PrimaryEmail)
	took := time.Since(start)
	stats := f.store.TransactionStats()
	if err != nil {
		t.Fatalf("account deletion while 4 writers refresh its machines failed after %v: %v (serializable %+v, read committed %+v)", took, err, stats.Serializable, stats.ReadCommitted)
	}
	if n := f.count(`SELECT count(*) FROM users WHERE id=$1 AND status='deleted'`, u.ID); n != 1 {
		t.Fatalf("the account is not deleted")
	}
	if n := f.count(`SELECT count(*) FROM cli_client_sessions WHERE user_id=$1 AND state='active'`, u.ID); n != 0 {
		t.Fatalf("%d CLI sessions still active", n)
	}
	t.Logf("deletion took %v; serializable %+v, read committed %+v", took, stats.Serializable, stats.ReadCommitted)
}
