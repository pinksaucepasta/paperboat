package usermachines

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// Prod 2026-11-19: a machine with ~16k active, expired access sessions (an agent opening sessions all day; expired ones
// stay active for the retention period) could not be revoked: the session cascade ran 15 s inside the revoke's first
// statement while the account's peer writes queued behind it. On a 100-machine account whose revoked machine holds
// 20,000 active sessions (all but 100 expired), with peer config writes and SSH CA authority locks running on other
// machines, the revoke commits in under 2 s, revokes every unexpired session of that machine and no other (the
// worker's next tick retires the expired ones), keeps the usage
// authority of expired sessions as it was, and bumps the client endpoint's peer generation.
func TestRevokeMachineWithManyAccessSessionsOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	user, cli := "many_user_"+x, "many_cli_"+x
	machine := func(g int) string { return fmt.Sprintf("many_m_%d_%s", g, x) }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.native_usage_authorities WHERE actor_account_id=$1`,
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
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT 'many_env_'||g||'_'||$2,'many_env_'||g||'_'||$2,$1 FROM generate_series(0,87) g`, user, x)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT 'many_m_'||g||'_'||$2,$1,'many_env_'||g||'_'||$2,'m'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(0,87) g`, user, x)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'client','many','desktop','test',ARRAY['machines:connect'],'active',now(),now())`, cli, user)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to('q'||e.id,'UTF8')),$1,e.id,e.role,1,1,decode(repeat('00',200),'hex'),sha256(convert_to('k'||e.id,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('k'||e.id,'UTF8'))
FROM (SELECT 'many_m_'||g||'_'||$2 AS id,'machine' AS role FROM generate_series(0,87) g UNION ALL SELECT $3,'cli') e`, user, x, cli)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,'many_m_'||g||'_'||$2,'machine','many_m_'||g||'_'||$2,1,1,1,sha256(convert_to('wg'||g||$2,'UTF8')),sha256(convert_to('q'||'many_m_'||g||'_'||$2,'UTF8')),('100.71.'||(g/250)||'.'||(1+g%250))::inet,now(),now() FROM generate_series(0,87) g
UNION ALL SELECT $1,$3,'cli',NULL,1,0,1,sha256(convert_to('wgcli'||$2,'UTF8')),sha256(convert_to('q'||$3,'UTF8')),'100.71.9.1'::inet,now(),now()`, user, x, cli)
	// Inserted through the normal triggers (each gets its usage authority). Machine 0: 19,900 expired + 100 live.
	sessions := func(m string, from, to int, expires string) {
		exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,helper_terminal_session_id,capabilities,operation_id)
SELECT 'control_plane_signed',$1||'_s_'||g,$1,$2,u.environment_id,$3,'',now()+$6::interval,'active','h_'||$1||'_'||g,ARRAY['exec'],'op' FROM generate_series($4::int,$5::int) g, paperboat.user_machines u WHERE u.id=$1`,
			m, user, cli, from, to, expires)
	}
	sessions(machine(0), 1, 17700, "-1 hour")
	sessions(machine(0), 17701, 20000, "1 hour")
	sessions(machine(1), 1, 10, "1 hour")
	var cliGeneration int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT config_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, user, cli).Scan(&cliGeneration); err != nil {
		t.Fatal(err)
	}

	// Concurrent peer writes of the account, as captured on prod: config generation bumps and SSH CA authority locks.
	loadCtx, stopLoad := context.WithCancel(ctx)
	var load sync.WaitGroup
	var writes atomic.Int64
	var slowest atomic.Int64
	timed := func(f func() error) {
		start := time.Now()
		if f() == nil {
			writes.Add(1)
		}
		if d := int64(time.Since(start)); d > slowest.Load() {
			slowest.Store(d)
		}
	}
	for w := 0; w < 4; w++ {
		load.Go(func() {
			for i := 0; loadCtx.Err() == nil; i++ {
				timed(func() error {
					_, err := store.SQL().ExecContext(loadCtx, `UPDATE paperboat.peer_network_identities SET config_generation=config_generation+1,updated_at=now() WHERE user_id=$1 AND endpoint_id=$2`, user, machine(2+(w*31+i)%86))
					return err
				})
			}
		})
	}
	load.Go(func() {
		for i := 0; loadCtx.Err() == nil; i++ {
			timed(func() error {
				return store.InTx(loadCtx, func(ctx context.Context, tx *db.Tx) error {
					_, err := tx.Queries().ResolveMachineSSHHostKeyAuthorityForUpdate(ctx, dbsqlc.ResolveMachineSSHHostKeyAuthorityForUpdateParams{UserMachineID: machine(2 + i%86), UserID: user, MachineGeneration: 1})
					return err
				})
			})
		}
	})
	defer func() { stopLoad(); load.Wait() }()
	for writes.Load() < 20 {
		if ctx.Err() != nil {
			t.Fatal("concurrent writers never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}

	revokeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	err := service.Disconnect(revokeCtx, user, machine(0))
	took := time.Since(start)
	before := writes.Load()
	time.Sleep(200 * time.Millisecond)
	stopLoad()
	load.Wait()
	if err != nil {
		t.Fatalf("revoke failed after %v: %v", took, err)
	}
	if took > 2*time.Second {
		t.Fatalf("revoke of a machine with 20,000 access sessions took %v, want under 2 s", took)
	}
	if writes.Load() == before || time.Duration(slowest.Load()) > took+2*time.Second {
		t.Fatalf("concurrent writers stalled: %d writes after the revoke, slowest %v (revoke %v)", writes.Load()-before, time.Duration(slowest.Load()), took)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// At commit every unexpired session is revoked; the expired ones carry no authority once the machine ended and
	// the worker's next tick retires them, in batches.
	if n := count(`SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1 AND state<>'revoked' AND expires_at>now()`, machine(0)); n != 0 {
		t.Fatalf("%d unexpired sessions of the revoked machine are not revoked", n)
	}
	if err := service.retireExpiredAccessSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1 AND state<>'revoked'`, machine(0)); n != 0 {
		t.Fatalf("%d sessions of the revoked machine are not revoked after a worker tick", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1 AND state='active' AND revoked_at IS NULL`, machine(1)); n != 10 {
		t.Fatalf("another machine has %d of 10 sessions active after the revoke", n)
	}
	// Usage authority: expired sessions keep their expiry; live ones now end at the revocation.
	if n := count(`SELECT count(*) FROM paperboat.native_usage_authorities a JOIN paperboat.user_machine_access_sessions s ON s.id=a.access_session_id
WHERE s.user_machine_id=$1 AND a.expires_at IS DISTINCT FROM least(s.expires_at,s.revoked_at)`, machine(0)); n != 0 {
		t.Fatalf("%d usage authorities of the revoked machine's sessions do not end at least(expiry, revocation)", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND config_generation>$3 AND relay_revocation_generation>$3`, user, cli, cliGeneration); n != 1 {
		t.Fatalf("the client endpoint's peer generation did not advance past %d", cliGeneration)
	}
	if n := count(`SELECT count(*) FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND revoked_at IS NOT NULL`, user, machine(0)); n != 1 {
		t.Fatal("the revoked machine's peer identity is not revoked")
	}
	var state sql.NullString
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machines WHERE id=$1`, machine(0)).Scan(&state); err != nil || state.String != "disconnected" {
		t.Fatalf("machine state %q (%v)", state.String, err)
	}
	// The final hard delete removes the machine's 20,000 sessions in bounded transactions, then the machine row
	// (one statement over all of them ran prod out of predicate locks), within one worker tick.
	start = time.Now()
	if err := service.RetryPendingRevocations(ctx); err != nil {
		t.Fatalf("final delete: %v", err)
	}
	finalTook := time.Since(start)
	if n := count(`SELECT count(*) FROM paperboat.user_machines WHERE id=$1`, machine(0)); n != 0 {
		t.Fatal("the revoked machine was not hard-deleted in one worker tick")
	}
	if n := count(`SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, machine(1)); n != 10 {
		t.Fatalf("final delete of one machine touched another's sessions: %d of 10 left", n)
	}
	t.Logf("revoke of a machine with 20,000 sessions took %v; %d concurrent writes, slowest %v; final delete %v", took, writes.Load(), time.Duration(slowest.Load()), finalTook)
}
