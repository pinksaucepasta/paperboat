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

// Bulk changes publish once per statement, not once per row (F21-10, migration 193): 1,000 access sessions of one
// machine inserted or revoked in one statement move the machine's machine_access topic once and its endpoint
// identities' generations once; deactivating 1,000 team members moves each member's inventory topic once in one
// upsert; replacing 100 machines' service snapshots moves the owner's machine_services topic once. A statement
// that changes no watched column publishes nothing.
func TestBulkChangesPublishOncePerStatementOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	user, team := "bulk_user_"+s, "bulk-team-"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	value := func(query string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return v
	}
	machineTopic := func(machine, topic string) int64 {
		return value(`SELECT coalesce(max(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic=$2`, machine, topic)
	}
	accountTopic := func(account, topic string) int64 {
		return value(`SELECT coalesce(max(generation),0) FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic=$2`, account, topic)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
SELECT $1||'_env_'||g,$1||'_env_'||g,$1,'active',1,'ready',1,now(),now() FROM generate_series(0,99) g`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT $1||'_m_'||g,$1,$1||'_env_'||g,'m-'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(0,99) g`, user)
	machine, cli := user+"_m_0", user+"_cli"
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
	generations := func() (int64, int64) {
		return value(`SELECT config_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, user, machine),
			value(`SELECT config_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, user, cli)
	}

	// 1,000 access sessions of one machine and one CLI session.
	access := machineTopic(machine, "machine_access")
	machineConfig, cliConfig := generations()
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,
helper_terminal_session_id,expires_at,capabilities,operation_id)
SELECT 'external_issuer',$1||'_access_'||g,$2,$1,$1||'_env_0',$3,'https://invalid.test','jti_'||$1||'_'||g,now()+interval '1 hour',ARRAY['terminal'],'op_'||$1||'_'||g
FROM generate_series(1,1000) g`, user, machine, cli)
	m, c := generations()
	// An insert does not publish machine_access from a trigger: the code paths that create a session publish it
	// after their commit (migration 200), so concurrent sessions of one machine do not conflict on the topic row.
	if got := machineTopic(machine, "machine_access"); got != access || m != machineConfig+1 || c != cliConfig+1 {
		t.Fatalf("inserting 1,000 sessions: machine_access +%d (want 0), identities +%d/+%d; want +1 each", got-access, m-machineConfig, c-cliConfig)
	}
	exec(`UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now() WHERE user_id=$1`, user)
	m2, c2 := generations()
	if got := machineTopic(machine, "machine_access"); got != access+1 || m2 != m+1 || c2 != c+1 {
		t.Fatalf("revoking 1,000 sessions: machine_access +%d, identities +%d/+%d; want +1 each", got-access, m2-m, c2-c)
	}
	if relay := value(`SELECT relay_revocation_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, user, cli); relay != c2 {
		t.Fatalf("a revocation left the relay revocation generation at %d, want %d", relay, c2)
	}
	exec(`UPDATE paperboat.user_machine_access_sessions SET http_base_url=http_base_url WHERE user_id=$1`, user)
	if m3, _ := generations(); machineTopic(machine, "machine_access") != access+1 || m3 != m2 {
		t.Fatal("an update of no watched column published")
	}

	// 1,000 team members deactivated in one statement.
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT $1||'_member_'||g,$1||'_member_'||g,$1||'_member_'||g||'@example.test','active' FROM generate_series(1,1000) g`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) SELECT $1,$2||'_member_'||g,1,'member',true FROM generate_series(1,1000) g`, team, user)
	member := user + "_member_7"
	before := accountTopic(member, "inventory")
	exec(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1`, team)
	if got := accountTopic(member, "inventory"); got != before+1 {
		t.Fatalf("deactivating 1,000 members moved a member's inventory topic %d times, want 1", got-before)
	}
	if moved := value(`SELECT count(*) FROM paperboat.account_topic_generations WHERE account_id LIKE $1||'_member_%' AND topic='inventory' AND generation=$2`, user, before+1); moved != 1000 {
		t.Fatalf("%d of 1,000 members' inventory topics moved", moved)
	}
	exec(`UPDATE paperboat.team_members SET membership_generation=membership_generation WHERE team_id=$1`, team)
	if accountTopic(member, "inventory") != before+1 {
		t.Fatal("an update of no watched member column published")
	}

	// 100 machines' service snapshots replaced in one statement: the owner's audience topic moves once.
	exec(`INSERT INTO paperboat.machine_service_snapshots(machine_id,installation_generation,boot_id,started_at,generation,services,expires_at)
SELECT id,1,repeat('b',16),now(),1,'[]'::jsonb,now()+interval '1 hour' FROM paperboat.user_machines WHERE user_id=$1`, user)
	services := accountTopic(user, "machine_services")
	exec(`UPDATE paperboat.machine_service_snapshots SET services='[{"port":3000}]'::jsonb,generation=2 WHERE machine_id LIKE $1||'_m_%'`, user)
	if got := accountTopic(user, "machine_services"); got != services+1 {
		t.Fatalf("replacing 100 snapshots moved machine_services %d times, want 1", got-services)
	}
	exec(`UPDATE paperboat.machine_service_snapshots SET expires_at=expires_at WHERE machine_id LIKE $1||'_m_%'`, user)
	if accountTopic(user, "machine_services") != services+1 {
		t.Fatal("an update of no watched snapshot column published")
	}
}
