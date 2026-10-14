package machineservices_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
)

func TestMachineServicePostgresAuthorityLifecycle(t *testing.T) {
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
	owner, other, machine, environment, team := "ds-owner-"+suffix, "ds-other-"+suffix, "ds-machine-"+suffix, "ds-env-"+suffix, "ds-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, other)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$1,$1,'machine service','desktop','linux',ARRAY['machines:connect'],'active',now(),now()),($2,$2,$2,'machine service','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, owner, other)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	exec(`UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1`, machine)
	s := machineservices.New(store, "local.example.test")
	resolver, e := nativeprivateaccess.NewSQLResolver(store)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := machineservices.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now.Add(-time.Minute), Generation: 1, Services: []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}}}
	announce := func() {
		t.Helper()
		if e := s.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().UTC()); e != nil {
			t.Fatal(e)
		}
	}
	announce()
	request := func(actor string, port int) nativeprivateaccess.Request {
		return nativeprivateaccess.Request{AccountID: actor, UserID: actor, CLIClientSessionID: actor, OperationID: "machine-service-test", ResourceKind: "machine_service", ResourceID: machine, RouteID: fmt.Sprintf("tcp:%d", port), Protocol: "tcp"}
	}
	denied := func(actor string, port int) {
		t.Helper()
		if _, e := resolver.ResolveNativePrivate(ctx, request(actor, port)); !errors.Is(e, nativeprivateaccess.ErrDenied) {
			t.Fatalf("expected denial: %v", e)
		}
	}
	denied(other, 5432)
	denied(owner, 22)
	var count int
	if e = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, machine).Scan(&count); e != nil || count != 0 {
		t.Fatalf("unauthorized request mutated sessions: %d %v", count, e)
	}
	target, e := resolver.ResolveNativePrivate(ctx, request(owner, 5432))
	if e != nil {
		t.Fatal(e)
	}
	if target.AccountID != owner || target.TargetAddress != "127.0.0.1:5432" {
		t.Fatalf("target %+v", target)
	}
	var helper *string
	if e = store.SQL().QueryRowContext(ctx, `SELECT helper_terminal_session_id FROM paperboat.user_machine_access_sessions WHERE id=$1`, target.AccessSessionID).Scan(&helper); e != nil || helper != nil {
		t.Fatalf("private access fabricated terminal: %v %v", helper, e)
	}
	if expiry, e := resolver.MachineServiceCurrent(ctx, request(owner, 5432), target.AccessSessionID, target); e != nil || !expiry.After(time.Now()) {
		t.Fatalf("current %v %v", expiry, e)
	}
	if items, e := s.List(ctx, other); e != nil || len(items) != 0 {
		t.Fatalf("outsider list %v %v", items, e)
	}
	policy, e := s.GetPolicy(ctx, owner, machine)
	if e != nil || !policy.PersonalAutoEnabled || policy.TeamAutoEnabled {
		t.Fatalf("defaults %+v %v", policy, e)
	}
	if _, e = s.UpdatePolicy(ctx, other, machine, machineservices.PolicyUpdate{ExpectedGeneration: 1}); !errors.Is(e, machineservices.ErrDenied) {
		t.Fatalf("outsider settings %v", e)
	}
	_, e = s.UpdatePolicy(ctx, owner, machine, machineservices.PolicyUpdate{ExpectedGeneration: policy.Generation})
	if e != nil {
		t.Fatal(e)
	}
	denied(owner, 5432)
	if _, e = resolver.MachineServiceCurrent(ctx, request(owner, 5432), target.AccessSessionID, target); !errors.Is(e, nativeprivateaccess.ErrDenied) {
		t.Fatalf("revoked automatic lease %v", e)
	}
	// Existing explicit lazy policy is an independent allowance, including a low port.
	beforeExplicit, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil || beforeExplicit.AutomaticDiscoveryEnabled || len(beforeExplicit.ExplicitPorts) != 0 {
		t.Fatalf("disabled discovery before explicit: %+v %v", beforeExplicit, e)
	}
	exec(`INSERT INTO paperboat.lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at) VALUES($1,$2,$3,$4,$5,1,1,'http','127.0.0.1:22','private','persistent_port',now()+interval '1 hour')`, "ds-policy-"+suffix, "ds-"+suffix+".example.test", owner, environment, machine)
	afterExplicit, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil || afterExplicit.Generation != beforeExplicit.Generation+1 || len(afterExplicit.ExplicitPorts) != 1 || afterExplicit.ExplicitPorts[0].Port != 22 {
		t.Fatalf("explicit discovery did not advance independently: %+v %v", afterExplicit, e)
	}
	stable, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil || stable.Generation != afterExplicit.Generation {
		t.Fatalf("unchanged discovery generation advanced: %+v %v", stable, e)
	}
	machinePolicy, e := s.GetPolicy(ctx, owner, machine)
	if e != nil || machinePolicy.Generation != 2 {
		t.Fatalf("discovery changed native policy generation: %+v %v", machinePolicy, e)
	}
	snapshot.Generation++
	snapshot.Services = []machineservices.Port{{Port: 22, AddressFamily: "ipv4"}}
	announce()
	explicit, e := resolver.ResolveNativePrivate(ctx, request(owner, 22))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.UpdatePolicy(ctx, owner, machine, machineservices.PolicyUpdate{ExpectedGeneration: 2, PersonalAutoEnabled: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = resolver.MachineServiceCurrent(ctx, request(owner, 22), explicit.AccessSessionID, explicit); e != nil {
		t.Fatalf("automatic toggle revoked explicit: %v", e)
	}
	// A changed generation cannot be refreshed with different content; stale boot
	// and signed observation replay cannot extend the current announcement.
	bad := snapshot
	bad.Services = []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}}
	if e = s.RecordMachineServices(ctx, environment, machine, bad, time.Now()); !errors.Is(e, machineservices.ErrConflict) {
		t.Fatalf("same generation changed set %v", e)
	}
	if e = s.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().Add(-31*time.Second)); !errors.Is(e, machineservices.ErrInvalid) {
		t.Fatalf("signed replay %v", e)
	}
	snapshot.BootID = "boot-112345678901"
	snapshot.StartedAt = now
	announce()
	if _, e = resolver.MachineServiceCurrent(ctx, request(owner, 22), explicit.AccessSessionID, explicit); !errors.Is(e, nativeprivateaccess.ErrDenied) {
		t.Fatalf("old boot %v", e)
	}
	// Team ownership never inherits personal-auto permission or admin use.
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, other)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',1,now()-interval '1 hour',now()+interval '1 hour')`, "ds-billing-"+suffix, team)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, team, machine, owner)
	exec(`UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, machine, team)
	denied(owner, 5432)
	denied(other, 5432)
	_, e = s.UpdatePolicy(ctx, owner, machine, machineservices.PolicyUpdate{ExpectedGeneration: 3, PersonalAutoEnabled: true, TeamAutoEnabled: true})
	if e != nil {
		t.Fatal(e)
	}
	denied(owner, 5432)
	beforeAudience, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil || beforeAudience.AutomaticDiscoveryEnabled {
		t.Fatalf("ungranted team discovery enabled: %+v %v", beforeAudience, e)
	}
	exec(`INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,account_id,capabilities) VALUES($1,$2,'selected_member',$3,ARRAY['private_access'])`, team, machine, other)
	afterAudience, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil || !afterAudience.AutomaticDiscoveryEnabled || afterAudience.Generation != beforeAudience.Generation+1 {
		t.Fatalf("team audience discovery did not advance: %+v %v", afterAudience, e)
	}
	machinePolicy, e = s.GetPolicy(ctx, owner, machine)
	if e != nil || machinePolicy.Generation != 4 {
		t.Fatalf("audience discovery changed native policy generation: %+v %v", machinePolicy, e)
	}
	snapshot.Generation++
	snapshot.Services = []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}}
	announce()
	shared, e := resolver.ResolveNativePrivate(ctx, request(other, 5432))
	if e != nil || shared.AccountID != other || shared.UserID != other {
		t.Fatalf("team caller identity %+v %v", shared, e)
	}
	denied(owner, 5432)
	exec(`UPDATE paperboat.team_machine_grants SET active=false,generation=generation+1 WHERE team_id=$1`, team)
	if _, e = resolver.MachineServiceCurrent(ctx, request(other, 5432), shared.AccessSessionID, shared); !errors.Is(e, nativeprivateaccess.ErrDenied) {
		t.Fatalf("team revocation %v", e)
	}
	exec(`UPDATE paperboat.team_machine_grants SET active=true,generation=generation+1 WHERE team_id=$1`, team)
	shared, e = resolver.ResolveNativePrivate(ctx, request(other, 5432))
	if e != nil {
		t.Fatal(e)
	}
	exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, other)
	if _, e = resolver.MachineServiceCurrent(ctx, request(other, 5432), shared.AccessSessionID, shared); !errors.Is(e, nativeprivateaccess.ErrDenied) {
		t.Fatalf("caller revocation %v", e)
	}
}
