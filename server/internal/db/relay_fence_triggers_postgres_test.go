package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A relay fence moves only for the endpoint whose own right ended (migration 297). A machine going offline while
// the control plane cannot hear it, and a peer's certificate renewal or key rotation, move configuration
// generations only: a relay-only pair cut off from the server keeps relaying on its last grants until revoked.
func TestRelayFenceMovesOnlyOnRemovalOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user := "fence_user_" + s
	machine, cli := user+"_m", user+"_cli"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	type gens struct{ config, relay int64 }
	read := func(endpoint string) gens {
		t.Helper()
		var g gens
		if err := store.SQL().QueryRowContext(ctx, `SELECT config_generation,relay_revocation_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, user, endpoint).Scan(&g.config, &g.relay); err != nil {
			t.Fatal(err)
		}
		return g
	}
	// step runs one change and checks that both endpoints reconfigure and only the named ones are fenced.
	step := func(name, query string, fenced ...string) {
		t.Helper()
		before := map[string]gens{machine: read(machine), cli: read(cli)}
		exec(query, user)
		for _, endpoint := range []string{machine, cli} {
			after, want := read(endpoint), before[endpoint].relay
			for _, f := range fenced {
				if f == endpoint {
					want = after.config
				}
			}
			if after.config <= before[endpoint].config || after.relay != want {
				t.Fatalf("%s: %s generations %+v -> %+v, want configuration advanced and relay fence %d", name, endpoint, before[endpoint], after, want)
			}
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
VALUES($1||'_env',$1||'_env',$1,'active',1,'ready',1,now(),now())`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
VALUES($2,$1,$1||'_env','m','linux','amd64','/w','online','occupied',true,1)`, user, machine)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at)
VALUES($1,$2,'pb','laptop','cli','linux',ARRAY['machines:connect'],'active',now(),now())`, cli, user)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to('q'||e,'UTF8')),$1,e,r,1,1,decode(repeat('00',200),'hex'),sha256(convert_to('k'||e,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',
 sha256(convert_to('s'||e,'UTF8')) FROM (VALUES($2,'machine'),($3,'cli')) v(e,r)`, user, machine, cli)
	net := sha256.Sum256([]byte(s))
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($1,$2,'machine',$2,1,1,1,sha256(convert_to($2,'UTF8')),sha256(convert_to('q'||$2,'UTF8')),('100.'||(64+$4::int)||'.'||$5::int||'.1')::inet,now(),now()),
 ($1,$3,'cli',NULL,1,0,1,sha256(convert_to($3,'UTF8')),sha256(convert_to('q'||$3,'UTF8')),('100.'||(64+$4::int)||'.'||$5::int||'.2')::inet,now(),now())`,
		user, machine, cli, int(net[0])%64, int(net[1]))
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,
helper_terminal_session_id,expires_at,capabilities,operation_id)
VALUES('external_issuer',$1||'_access',$2,$1,$1||'_env',$3,'https://invalid.test','jti_'||$1,now()+interval '1 hour',ARRAY['terminal'],'op_'||$1)`, user, machine, cli)

	step("machine marked offline", `UPDATE paperboat.user_machines SET state='offline',online=false WHERE user_id=$1`)
	step("machine back online", `UPDATE paperboat.user_machines SET state='online',online=true WHERE user_id=$1`)
	step("CLI certificate superseded", `UPDATE paperboat.peer_endpoint_certificates SET revoked_at=now(),revocation_reason='certificate_superseded' WHERE user_id=$1 AND role='cli'`, cli)
	step("CLI key rotated", `UPDATE paperboat.peer_network_identities SET wireguard_public_key=sha256(convert_to('rotated'||endpoint_id,'UTF8')),key_generation=2 WHERE user_id=$1 AND role='cli'`, cli)
	// Revoking an account E2EE key fences only the endpoint it belongs to (migration 302): it fenced every endpoint
	// of the account, and the fleet's same-account peers were refused at the relay (2026-11-12 20:39Z).
	exec(`INSERT INTO paperboat.account_e2ee_keys(key_id,user_id,public_key,fingerprint,cli_client_session_id) VALUES('aek_'||md5($1),$1,sha256(convert_to('pk'||$1,'UTF8')),sha256(convert_to('fp'||$1,'UTF8')),$2)`, user, cli)
	step("CLI's E2EE key revoked", `UPDATE paperboat.account_e2ee_keys SET revoked_at=now(),revocation_reason='machine_removed' WHERE user_id=$1`, cli)
	step("machine reinstalled", `UPDATE paperboat.user_machines SET installation_generation=2 WHERE user_id=$1`, machine)
	// Revoking the session also revokes its access sessions (revoke_access_sessions_on_client_revocation): an
	// explicit pair revocation, which fences both ends.
	step("CLI session revoked", `UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE user_id=$1`, cli, machine)
	// Revoking the machine revokes its environment too (revoke_machine_environment), which fences both ends.
	step("machine revoked", `UPDATE paperboat.user_machines SET state='revoked',online=false,seat_state='released',revoked_at=now() WHERE user_id=$1`, machine, cli)
}
