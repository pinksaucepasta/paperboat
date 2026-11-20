package teams

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Removing a team machine must not lose to the enroller's other machines refreshing their peer configuration: it
// revokes the machine's enrollment CLI session and certificates, whose triggers bump those machines' peer identities,
// and in one serializable transaction that lost every retry to their configuration requests (the personal revoke failed
// 4/4 so on a restored prod dump, 2026-11-20). Here 4 writers bump them without pause while the team removes the machine.
// Deleting the team removes its machines the same way and must not lose either.
func TestPostgresTeamMachineRemoveWhileTheEnrollersMachinesRefreshTheirConfig(t *testing.T) {
	for _, action := range []string{"remove", "delete"} {
		t.Run(action, func(t *testing.T) { teamRemovalUnderPeerWriters(t, action) })
	}
}

func teamRemovalUnderPeerWriters(t *testing.T, action string) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	x := fmt.Sprint(time.Now().UnixNano())
	user, team, cli := "trw_user_"+x, "trw-team-"+x, "trw_cli_"+x
	machine := func(g int) string { return fmt.Sprintf("trw_m_%d_%s", g, x) }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	defer func() {
		bg, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		for _, q := range []string{
			`DELETE FROM paperboat.user_machine_access_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.peer_network_identities WHERE user_id=$1`,
			`DELETE FROM paperboat.peer_endpoint_certificates WHERE user_id=$1`,
			`DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.user_machines WHERE user_id=$1`,
			`DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`,
		} {
			_, _ = store.SQL().ExecContext(bg, q, user)
		}
		tx, e := store.SQL().BeginTx(bg, nil)
		if e != nil {
			return
		}
		defer tx.Rollback()
		for _, q := range []string{
			`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`,
			`DELETE FROM paperboat.audit_events WHERE actor_user_id='` + user + `'`,
			`ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`,
			`DELETE FROM paperboat.teams WHERE team_id='` + team + `'`,
			`DELETE FROM paperboat.users WHERE id='` + user + `'`,
		} {
			if _, e = tx.ExecContext(bg, q); e != nil {
				t.Error(e)
				return
			}
		}
		_ = tx.Commit()
	}()
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, user)
	s := NewService(store)
	if _, err = s.Create(ctx, user, CreateRequest{OperationID: "create", TeamID: team}); err != nil {
		t.Fatal(err)
	}
	// Machine 0 belongs to the team; 1..8 are the enroller's own.
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT 'trw_env_'||g||'_'||$2,'trw_env_'||g||'_'||$2,$1 FROM generate_series(0,8) g`, user, x)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,owner_team_id)
SELECT 'trw_m_'||g||'_'||$2,$1,'trw_env_'||g||'_'||$2,'m'||g,'linux','amd64','/w','online','occupied',true,1,CASE WHEN g=0 THEN $3 END FROM generate_series(0,8) g`, user, x, team)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES($1,$2,'client','trw','desktop','test',ARRAY['machines:connect'],'active',now(),now(),$3)`, cli, user, machine(0))
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to('q'||e.id,'UTF8')),$1,e.id,e.role,1,1,decode(repeat('00',200),'hex'),sha256(convert_to('k'||e.id,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('k'||e.id,'UTF8'))
FROM (SELECT 'trw_m_'||g||'_'||$2 AS id,'machine' AS role FROM generate_series(0,8) g UNION ALL SELECT $3,'cli') e`, user, x, cli)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,'trw_m_'||g||'_'||$2,'machine','trw_m_'||g||'_'||$2,1,1,1,sha256(convert_to('wg'||g||$2,'UTF8')),sha256(convert_to('q'||'trw_m_'||g||'_'||$2,'UTF8')),('100.73.'||(abs(hashtext($2))%200)||'.'||(1+g))::inet,now(),now() FROM generate_series(0,8) g
UNION ALL SELECT $1,$3,'cli',NULL,1,0,1,sha256(convert_to('wgcli'||$2,'UTF8')),sha256(convert_to('q'||$3,'UTF8')),('100.73.'||(abs(hashtext($2))%200)||'.200')::inet,now(),now()`, user, x, cli)
	// The enrollment CLI session ran commands on the other machines earlier (sessions over), so its peer-network
	// trigger names them.
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,revoked_at,helper_revoked_at,helper_terminal_session_id,capabilities,operation_id)
SELECT 'control_plane_signed','trw_s_'||g||'_'||$2,'trw_m_'||g||'_'||$2,$1,'trw_env_'||g||'_'||$2,$3,'',now()-interval '1 hour','revoked',now()-interval '1 hour',now()-interval '1 hour','h_'||g||'_'||$2,ARRAY['exec'],'op' FROM generate_series(1,8) g`, user, x, cli)
	current, err := s.Get(ctx, user, team)
	if err != nil {
		t.Fatal(err)
	}

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
	if action == "remove" {
		_, err = s.Machine(ctx, user, team, MachineRequest{OperationID: "remove", ExpectedGeneration: current.Generation, MachineID: machine(0), Action: "remove", Confirmation: machine(0)})
	} else {
		_, err = s.Mutate(ctx, user, team, MutationRequest{OperationID: "delete", ExpectedGeneration: current.Generation, Action: "delete", Confirmation: team})
	}
	took := time.Since(start)
	stats := store.TransactionStats()
	if err != nil {
		t.Fatalf("team %s while 4 writers refresh the enroller's other machines failed after %v: %v (serializable %+v, read committed %+v)", action, took, err, stats.Serializable, stats.ReadCommitted)
	}
	var state, machineState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT state FROM paperboat.cli_client_sessions WHERE id=$1),(SELECT state FROM paperboat.user_machines WHERE id=$2)`, cli, machine(0)).Scan(&state, &machineState); err != nil || state == "active" || machineState != "revoked" {
		t.Fatalf("after the removal the CLI session is %q and the machine %q (%v)", state, machineState, err)
	}
	t.Logf("%s took %v; serializable %+v, read committed %+v", action, took, stats.Serializable, stats.ReadCommitted)
}
