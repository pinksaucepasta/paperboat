package machineservices_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/teams/teamstest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

// A team member's list shows the ports of a machine shared into their team when
// the effective grant includes private_access, and only then.
func TestListShowsTeamSharedMachinesOnlyWithPrivateAccessOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if e = db.Migrate(ctx, store); e != nil {
			t.Fatal(e)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, outsider := "tl-owner-"+suffix, "tl-member-"+suffix, "tl-outsider-"+suffix
	machine, environment := "tl-machine-"+suffix, "tl-env-"+suffix
	teamA, teamB := "tl-team-a-"+suffix, "tl-team-b-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active'),($3,$3,$3||'@invalid.test','active')`, owner, member, outsider)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	s := machineservices.New(store, "local.example.test")
	snapshot := wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: time.Now().UTC().Add(-time.Minute), Generation: 1, Services: []wire.Port{{Port: 5432, AddressFamily: "ipv4"}}}
	if e = s.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	// The announcement must not expire mid-test whatever the database latency.
	exec(`UPDATE paperboat.machine_service_snapshots SET expires_at=now()+interval '1 hour' WHERE machine_id=$1`, machine)
	for _, team := range []string{teamA, teamB} {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
		exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',1,now()-interval '1 hour',now()+interval '1 hour')`, "tl-billing-"+team, team)
	}
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, teamA, machine, owner)
	exec(`UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, machine, teamA)
	listed := func(actor string) int {
		t.Helper()
		items, e := s.List(ctx, actor)
		if e != nil {
			t.Fatal(e)
		}
		n := 0
		for _, item := range items {
			if item.MachineID == machine {
				n++
				if len(item.Services) != 1 || item.Services[0].Port != 5432 {
					t.Fatalf("services %+v", item.Services)
				}
			}
		}
		return n
	}
	want := func(step, actor string, n int) {
		t.Helper()
		if got := listed(actor); got != n {
			t.Fatalf("%s: %s sees the machine %d times, want %d", step, actor, got, n)
		}
	}
	want("shared without a grant", member, 0)
	teamstest.SetMachineAccess(t, store, teamA, machine, member, "terminal")
	want("terminal-only grant", member, 0)
	teamstest.SetMachineAccess(t, store, teamA, machine, member, "terminal", "private_access")
	// The team's own setting gates every team port, whatever the grants say.
	want("team private access off", member, 0)
	teamstest.SetPrivateAccess(t, store, teamA, true)
	want("private_access member grant", member, 1)
	teamstest.SetPrivateAccess(t, store, teamA, false)
	want("team setting turned off", member, 0)
	teamstest.SetPrivateAccess(t, store, teamA, true)
	want("team setting turned back on", member, 1)
	want("outsider", outsider, 0)
	teamstest.SetMachineAccess(t, store, teamA, machine, member)
	want("grant removed", member, 0)
	teamstest.SetMachineAccess(t, store, teamA, machine, "", "terminal", "private_access")
	want("all-members grant", member, 1)
	// A second team whose binding is not active does not leak the machine.
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,false,1)`, teamB, machine, owner)
	// The policy cannot name a machine outside the team; a rule for every machine must still
	// not reach it through the inactive binding.
	exec(`WITH p AS (INSERT INTO paperboat.workspace_policies(team_id,version,source,digest,allow_private_access) VALUES($1,1,'{}','test',true) RETURNING id)
INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports) SELECT id,0,false,'all','','all_machines','','private_access',int4range(1,65536) FROM p`, teamB)
	want("second team grant on an inactive binding", member, 1)
	exec(`UPDATE paperboat.team_members SET active=false,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2`, teamA, member)
	want("member removed from the owning team", member, 0)
	exec(`UPDATE paperboat.team_members SET active=true,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2`, teamA, member)
	want("member restored", member, 1)
	exec(`UPDATE paperboat.team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1`, teamA)
	want("unshared", member, 0)
	exec(`UPDATE paperboat.team_resource_bindings SET active=true,generation=generation+1 WHERE team_id=$1`, teamA)
	want("reshared", member, 1)
	exec(`UPDATE paperboat.billing_workspaces SET state='canceled' WHERE team_id=$1`, teamA)
	want("team entitlement lapsed", member, 0)
	exec(`UPDATE paperboat.billing_workspaces SET state='active' WHERE team_id=$1`, teamA)
	want("entitlement restored", member, 1)

	// The team setting through the real mutation: only an owner or admin can change it; turning it off
	// names the reason to the member and ends the session they were already admitted with.
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$1,$1,'team list','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, member)
	resolver, e := nativeprivateaccess.NewSQLResolver(store)
	if e != nil {
		t.Fatal(e)
	}
	request := nativeprivateaccess.Request{AccountID: member, UserID: member, CLIClientSessionID: member, OperationID: "tl-op", ResourceKind: "machine_service", ResourceID: machine, RouteID: "tcp:5432", Protocol: "tcp"}
	target, e := resolver.ResolveNativePrivate(ctx, request)
	if e != nil {
		t.Fatalf("member with the grant and the setting on: %v", e)
	}
	teamService := teams.NewService(store)
	setting := func(actor string, allow bool, op string) error {
		var generation uint64
		if e := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, teamA).Scan(&generation); e != nil {
			t.Fatal(e)
		}
		_, e := teamService.Mutate(ctx, actor, teamA, teams.MutationRequest{OperationID: op, ExpectedGeneration: generation, Action: "private_access", AllowPrivateAccess: &allow})
		return e
	}
	if e := setting(member, false, "tl-member-off"); !errors.Is(e, teams.ErrForbidden) {
		t.Fatalf("a plain member changed the team setting: %v", e)
	}
	if e := setting(owner, false, "tl-off"); e != nil {
		t.Fatal(e)
	}
	want("setting turned off through the mutation", member, 0)
	if _, e := resolver.ResolveNativePrivate(ctx, request); !errors.Is(e, machineservices.ErrTeamPrivateAccessOff) {
		t.Fatalf("off setting reported %v, want the reason", e)
	}
	if _, e := resolver.MachineServiceCurrent(ctx, request, target.AccessSessionID, target); !errors.Is(e, nativeprivateaccess.ErrDenied) {
		t.Fatalf("session admitted before the setting went off still current: %v", e)
	}
	var events int
	if e := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type='team.private_access'`, teamA).Scan(&events); e != nil || events != 1 {
		t.Fatalf("activity events for the change: %d %v", events, e)
	}
	if e := setting(owner, true, "tl-on"); e != nil {
		t.Fatal(e)
	}
	want("setting turned on through the mutation", member, 1)

	exec(`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, machine)
	want("machine revoked", member, 0)
}
