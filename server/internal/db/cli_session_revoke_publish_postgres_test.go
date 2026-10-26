package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Revoking a CLI session wakes the peer_network refresh of exactly the hosts that list it as a peer: the account's own
// live machines and the machines its access sessions name; no other machine, and no later update of the revoked row.
func TestCLISessionRevokePublishesPeerNetworkAndMachineAccessToItsHostsOnPostgres(t *testing.T) {
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
	owner, other := "rv_owner_"+s, "rv_other_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	topic := "peer_network"
	generation := func(machine string) int64 {
		t.Helper()
		var n int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(max(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic=$2`, machine, topic).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@example.test','active' FROM unnest(ARRAY[$1,$2]) x`, owner, other)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
SELECT u||'_env_'||g,u||'_env_'||g,u,'active',1,'ready',1,now(),now() FROM unnest(ARRAY[$1,$2]) u, generate_series(0,2) g`, owner, other)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT u||'_m_'||g,u,u||'_env_'||g,'m-'||g,'linux','amd64','/w','online','occupied',true,1 FROM unnest(ARRAY[$1,$2]) u, generate_series(0,2) g`, owner, other)
	own, revoked, teamHost, unrelated := owner+"_m_0", owner+"_m_2", other+"_m_0", other+"_m_1"
	exec(`UPDATE paperboat.user_machines SET state='revoked',revoked_at=now(),online=false WHERE id=$1`, revoked)
	cli, cli2 := owner+"_cli", owner+"_cli2"
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at)
SELECT x,$1,'pb','laptop','cli','linux',ARRAY['machines:connect'],'active',now(),now() FROM unnest(ARRAY[$2,$3]) x`, owner, cli, cli2)
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,
helper_terminal_session_id,expires_at,capabilities,operation_id)
VALUES('external_issuer',$1||'_access',$2,$3,$4,$5,'https://invalid.test','jti_'||$1,now()+interval '1 hour',ARRAY['terminal'],'op_'||$1)`, s, teamHost, owner, other+"_env_0", cli)

	own1, own2, host1, un1 := owner+"_m_1", revoked, teamHost, unrelated
	machines := []string{own, own1, own2, host1, un1}
	snapshot := func(name string) map[string]int64 {
		topic = name
		out := map[string]int64{}
		for _, m := range machines {
			out[m] = generation(m)
		}
		return out
	}
	topics := []string{"peer_network", "machine_access"}
	before := map[string]map[string]int64{}
	for _, name := range topics {
		before[name] = snapshot(name)
	}
	// A different session's revoke publishes to the same account's hosts only.
	exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, cli)
	for _, name := range topics {
		topic = name
		for machine, want := range map[string]bool{own: true, own1: true, host1: true, own2: false, un1: false} {
			if moved := generation(machine) > before[name][machine]; moved != want {
				t.Errorf("%s of %s moved=%v, want %v", name, machine, moved, want)
			}
		}
	}
	// The revoked row changing again publishes nothing, and neither does another session being used.
	after := map[string]map[string]int64{}
	for _, name := range topics {
		after[name] = snapshot(name)
	}
	exec(`UPDATE paperboat.cli_client_sessions SET revoked_at=now() WHERE id=$1`, cli)
	exec(`UPDATE paperboat.cli_client_sessions SET last_used_at=now() WHERE id=$1`, cli2)
	for _, name := range topics {
		topic = name
		for _, m := range machines {
			if generation(m) != after[name][m] {
				t.Errorf("a later update of a revoked session, or the use of another session, published %s on %s", name, m)
			}
		}
	}
}
