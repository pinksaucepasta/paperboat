package machineservices_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

// Port names are stored on the server, delivered only for listed ports, machine names beat all-machines names,
// a top-level name cannot take a machine's name and is shown shadowed once a machine takes it, and only managers
// (machine names, front desk) or workspace admins (all-machines, top-level names) change them.
func TestPortNamesOnPostgres(t *testing.T) {
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
	owner, member := "pn-owner-"+suffix, "pn-member-"+suffix
	machine, environment, team := "pn-machine-"+suffix, "pn-env-"+suffix, "pn-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, member)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,personal_auto_enabled) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1,true)`, machine, owner, environment)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	s := machineservices.New(store, "local.example.test")
	snapshot := wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: time.Now().UTC().Add(-time.Minute), Generation: 1, Services: []wire.Port{{Port: 3000, AddressFamily: "ipv4"}, {Port: 8080, AddressFamily: "ipv4"}}}
	if e = s.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	exec(`UPDATE paperboat.machine_service_snapshots SET expires_at=now()+interval '1 hour' WHERE machine_id=$1`, machine)
	personal, teamScope := workspace.Scope{ID: "personal"}, workspace.Scope{ID: team, TeamID: team}
	set := func(actor string, scope workspace.Scope, in wire.PortNameSet) (wire.PortNameRecord, error) {
		t.Helper()
		return s.SetPortName(ctx, actor, scope, in)
	}
	app, e := set(owner, personal, wire.PortNameSet{Scope: wire.ScopeMachine, Name: "app", Port: 8080, MachineID: machine})
	if e != nil {
		t.Fatal(e)
	}
	for _, in := range []wire.PortNameSet{{Scope: wire.ScopeAllMachines, Name: "app", Port: 3000}, {Scope: wire.ScopeAllMachines, Name: "db", Port: 3000},
		{Scope: wire.ScopeAllMachines, Name: "hidden", Port: 9999}, {Scope: wire.ScopeTopLevel, Name: "jelly", Port: 8080, MachineID: machine}} {
		if _, e = set(owner, personal, in); e != nil {
			t.Fatal(in, e)
		}
	}
	if _, e = set(owner, personal, wire.PortNameSet{Scope: wire.ScopeTopLevel, Name: "office", Port: 8080, MachineID: machine}); !errors.Is(e, machineservices.ErrNameIsMachine) {
		t.Fatalf("top-level name of a machine: %v", e)
	}
	if _, e = set(owner, personal, wire.PortNameSet{Scope: wire.ScopeMachine, Name: "8080", Port: 8080, MachineID: machine}); !errors.Is(e, wire.ErrInvalid) {
		t.Fatalf("numeric name: %v", e)
	}
	if _, e = set(member, personal, wire.PortNameSet{Scope: wire.ScopeMachine, Name: "x", Port: 8080, MachineID: machine}); !errors.Is(e, machineservices.ErrDenied) {
		t.Fatalf("machine name by a non-manager: %v", e)
	}
	if _, e = set(member, teamScope, wire.PortNameSet{Scope: wire.ScopeAllMachines, Name: "x", Port: 8080}); !errors.Is(e, machineservices.ErrDenied) {
		t.Fatalf("all-machines name by a team member: %v", e)
	}
	if _, e = set(owner, teamScope, wire.PortNameSet{Scope: wire.ScopeAllMachines, Name: "x", Port: 8080}); e != nil {
		t.Fatalf("all-machines name by the team owner: %v", e)
	}
	if e = s.SetFrontDesk(ctx, member, machine, wire.FrontDesk{Port: 443, Scheme: "https"}); !errors.Is(e, machineservices.ErrDenied) {
		t.Fatalf("front desk by a non-manager: %v", e)
	}
	item := func() wire.Item {
		t.Helper()
		items, e := s.List(ctx, owner)
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range items {
			if item.MachineID == machine {
				return item
			}
		}
		t.Fatal("machine not listed")
		return wire.Item{}
	}
	got := item()
	if want := []wire.PortName{{Name: "app", Port: 8080}, {Name: "db", Port: 3000}}; !reflect.DeepEqual(got.PortNames, want) {
		t.Fatalf("port names %+v, want %+v", got.PortNames, want)
	}
	if want := []wire.PortName{{Name: "jelly", Port: 8080}}; !reflect.DeepEqual(got.TopLevelNames, want) || got.FrontDesk != wire.DefaultFrontDesk {
		t.Fatalf("top-level %+v front desk %+v", got.TopLevelNames, got.FrontDesk)
	}
	if e = s.SetFrontDesk(ctx, owner, machine, wire.FrontDesk{Port: 443, Scheme: "https"}); e != nil {
		t.Fatal(e)
	}
	if e = s.DeletePortName(ctx, owner, personal, app.ID); e != nil {
		t.Fatal(e)
	}
	got = item()
	if want := []wire.PortName{{Name: "app", Port: 3000}, {Name: "db", Port: 3000}}; !reflect.DeepEqual(got.PortNames, want) || got.FrontDesk != (wire.FrontDesk{Port: 443, Scheme: "https"}) {
		t.Fatalf("after delete: names %+v front desk %+v", got.PortNames, got.FrontDesk)
	}
	if desk, e := s.GetFrontDesk(ctx, owner, machine); e != nil || desk.Port != 443 {
		t.Fatalf("front desk %+v %v", desk, e)
	}
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment+"-jelly", owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'jelly','linux','amd64','/workspace','online','occupied',true,1)`, machine+"-jelly", owner, environment+"-jelly")
	records, e := s.ListPortNames(ctx, owner, personal)
	if e != nil {
		t.Fatal(e)
	}
	shadowed := map[string]bool{}
	for _, r := range records {
		shadowed[r.Scope+"/"+r.Name] = r.Shadowed
	}
	if len(records) != 4 || !shadowed["top_level/jelly"] || shadowed["all_machines/db"] {
		t.Fatalf("records %+v", records)
	}
	if e = s.DeletePortName(ctx, member, personal, records[0].ID); !errors.Is(e, machineservices.ErrPortNameNotFound) {
		t.Fatalf("delete from another workspace: %v", e)
	}
}
