package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A change to what a team's members may dial reaches the members' own devices, not only the hosts (migrations 235-237):
// unsharing a machine, removing a member and losing the paid entitlement each move the affected accounts' inventory
// (the daemon renews its signed peer network on it) and machine_access (fast machine-list refresh) topics, and a
// billing update that keeps the entitlement publishes nothing.
func TestTeamAccessChangesPublishToMembersOnPostgres(t *testing.T) {
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
	owner, member, other, team, machine := "tap_owner_"+s, "tap_member_"+s, "tap_other_"+s, "tap-team-"+s, "tap_m_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	generation := func(query string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return v
	}
	account := func(id, topic string) int64 {
		return generation(`SELECT coalesce(max(generation),0) FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic=$2`, id, topic)
	}
	host := func(topic string) int64 {
		return generation(`SELECT coalesce(max(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic=$2`, machine, topic)
	}
	for _, id := range []string{owner, member, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, id)
	}
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
VALUES($1,$1,$2,'active',1,'ready',1,now(),now())`, machine+"_env", owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
VALUES($1,$2,$3,'host','linux','amd64','/w','online','occupied',true,1)`, machine, owner, machine+"_env")
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'machine',$2,$3,true)`, team, machine, owner)

	// A tunnel binding is no machine binding: no member topic moves.
	inv, acc := account(member, "inventory"), account(member, "machine_access")
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'tunnel',$2,$3,true)`, team, "tap_t_"+s, owner)
	if account(member, "inventory") != inv || account(member, "machine_access") != acc {
		t.Fatal("a tunnel binding moved the members' topics")
	}

	// Unshare: members (not outsiders) get inventory and machine_access once; the host keeps its own topics.
	inv, acc = account(member, "inventory"), account(member, "machine_access")
	otherInv, hostAccess := account(other, "inventory"), host("machine_access")
	exec(`UPDATE paperboat.team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_id=$2`, team, machine)
	if got := account(member, "inventory") - inv; got != 1 {
		t.Fatalf("unshare moved the member's inventory topic %d times, want 1", got)
	}
	if got := account(member, "machine_access") - acc; got != 1 {
		t.Fatalf("unshare moved the member's machine_access topic %d times, want 1", got)
	}
	if account(other, "inventory") != otherInv {
		t.Fatal("unshare moved a non-member's topic")
	}
	if host("machine_access") <= hostAccess {
		t.Fatal("unshare no longer moves the host's machine_access topic")
	}
	inv = account(member, "inventory")
	exec(`UPDATE paperboat.team_resource_bindings SET owner_account=owner_account WHERE team_id=$1 AND resource_id=$2`, team, machine)
	if account(member, "inventory") != inv {
		t.Fatal("a binding update that changed no watched column published")
	}
	exec(`UPDATE paperboat.team_resource_bindings SET active=true,generation=generation+1 WHERE team_id=$1 AND resource_id=$2`, team, machine)

	// Membership removal: the removed account's machine_access moves with its inventory (deactivate and delete).
	for _, remove := range []string{`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`,
		`DELETE FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2`} {
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true) ON CONFLICT (team_id,account_id) DO UPDATE SET active=true`, team, member)
		inv, acc = account(member, "inventory"), account(member, "machine_access")
		exec(remove, team, member)
		if account(member, "inventory") != inv+1 || account(member, "machine_access") != acc+1 {
			t.Fatalf("%s: removed member's topics moved inventory +%d, machine_access +%d; want +1 each", remove, account(member, "inventory")-inv, account(member, "machine_access")-acc)
		}
	}
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true) ON CONFLICT (team_id,account_id) DO UPDATE SET active=true`, team, member)

	// Entitlement: a paid row, then a past_due row inside no grace, then paid again. Only flips publish.
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES('team:'||$1,$1,'active',2,now()-interval '1 day',now()+interval '30 days')`, team)
	inv, acc = account(member, "inventory"), account(member, "machine_access")
	hostPeer, hostAccess := host("peer_network"), host("machine_access")
	exec(`UPDATE paperboat.billing_workspaces SET quantity=3 WHERE team_id=$1`, team)
	if account(member, "inventory") != inv || host("machine_access") != hostAccess {
		t.Fatal("a billing update that kept the entitlement published")
	}
	exec(`UPDATE paperboat.billing_workspaces SET state='canceled' WHERE team_id=$1`, team)
	for _, c := range []struct {
		name      string
		got, last int64
	}{
		{"member inventory", account(member, "inventory"), inv}, {"member machine_access", account(member, "machine_access"), acc},
		{"host peer_network", host("peer_network"), hostPeer}, {"host machine_access", host("machine_access"), hostAccess},
	} {
		if c.got != c.last+1 {
			t.Fatalf("losing the entitlement moved %s +%d, want +1", c.name, c.got-c.last)
		}
	}
}
