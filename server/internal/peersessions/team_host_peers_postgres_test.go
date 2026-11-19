package peersessions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// A member's CLI document names every team host it may use (teamHostPeers). The binding check of a (member, host)
// pair does not depend on the capability, yet it ran once per capability: prod 11-19, a member of a team with 6 hosts
// spent ~150 ms per build in it (about 2.5 ms a call), and the whole account's configurations were rebuilt about
// once a second. It must run once per host.
func TestTeamHostPeersChecksEachHostBindingOnceOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	s := fmt.Sprint(time.Now().UnixNano())
	owner, member, team := "thp_owner_"+s, "thp_member_"+s, "thp-team-"+s
	const hosts = 84
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	for _, account := range []string{owner, member} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=ANY($1)`, []string{owner, member})
	})
	seedMeshMachines(t, ctx, store, owner, 0, hosts-1)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "billing_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) SELECT $1,'machine',id,$2 FROM paperboat.user_machines WHERE user_id=$2`, team, owner)
	for i := range hosts {
		teamstest.SetMachineAccess(t, store, team, fmt.Sprintf("%s_m_%d", owner, i), member, "terminal", "exec")
	}

	conn, err := store.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// pg_stat_xact_user_functions counts this transaction's calls; the functions carry a SET clause, so they are not inlined.
	if _, err = tx.ExecContext(ctx, `SET LOCAL track_functions='all'`); err != nil {
		t.Skipf("track_functions needs a superuser test database: %v", err)
	}
	self := networkBinding{Binding: peernetwork.Binding{AccountID: member, EndpointID: "thp_cli_" + s, Role: peernetwork.RoleCLI}}
	started := time.Now()
	peers, err := teamHostPeers(ctx, tx, self, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	var bound int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(calls),0) FROM pg_stat_xact_user_functions WHERE schemaname='paperboat' AND funcname='team_machine_bound_for'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	t.Logf("RESULT teamHostPeers on a %d-host team: %v, %d binding checks, %d peers", hosts, elapsed, bound, len(peers))
	if len(peers) != hosts {
		t.Fatalf("%d peers, want every host of the team (%d)", len(peers), hosts)
	}
	for _, p := range peers {
		if len(p.capabilities) != 2 || p.capabilities[0] != "exec" || p.capabilities[1] != "terminal" {
			t.Fatalf("host %s: capabilities %v, want exec and terminal", p.binding.EndpointID, p.capabilities)
		}
	}
	if bound > hosts {
		t.Fatalf("%d binding checks for %d hosts, want one per host", bound, hosts)
	}
}
