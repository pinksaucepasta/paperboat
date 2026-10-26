package machineservices_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

// An idle machine polls its service policy and reports the same snapshot every few seconds. Before, each poll
// rewrote machine_service_discovery_policies and each report rewrote machine_service_snapshots to extend a 30 s
// lease (~1,700 + ~900 row versions an hour per machine on staging, PLAN 3b). Now an unchanged policy is read, an
// unchanged report renews the stored lease only once less than half is left, and a stale generation still conflicts.
func TestUnchangedServicePolicyAndSnapshotWriteNoRowsOnPostgres(t *testing.T) {
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
	if e = db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "iw-owner-"+suffix, "iw-machine-"+suffix, "iw-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'idle','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	xmin := func(table string) string {
		t.Helper()
		var x string
		if e := store.SQL().QueryRowContext(ctx, `SELECT xmin::text FROM paperboat.`+table+` WHERE machine_id=$1`, machine).Scan(&x); e != nil {
			t.Fatal(e)
		}
		return x
	}

	s := machineservices.New(store, "local.example.test")
	s.Lease = 2 * time.Minute
	first, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil {
		t.Fatal(e)
	}
	policy := xmin("machine_service_discovery_policies")
	for range 3 {
		again, e := s.RuntimePolicy(ctx, environment, machine)
		if e != nil || again.Generation != first.Generation {
			t.Fatalf("unchanged policy: generation %d -> %d, %v", first.Generation, again.Generation, e)
		}
	}
	if after := xmin("machine_service_discovery_policies"); after != policy {
		t.Fatalf("3 unchanged policy reads rewrote the policy row (xmin %s -> %s)", policy, after)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now.Add(-time.Minute), Generation: 1, Services: []wire.Port{{Port: 5432, AddressFamily: "ipv4"}}}
	report := func(svc *machineservices.Service) error {
		return svc.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().UTC())
	}
	if e = report(s); e != nil {
		t.Fatal(e)
	}
	stored := xmin("machine_service_snapshots")
	for range 3 {
		if e = report(s); e != nil {
			t.Fatalf("unchanged report: %v", e)
		}
	}
	if after := xmin("machine_service_snapshots"); after != stored {
		t.Fatalf("3 unchanged reports rewrote the snapshot (xmin %s -> %s)", stored, after)
	}

	// A lease shorter than twice the time already used renews.
	short := machineservices.New(store, "local.example.test")
	short.Lease = time.Second
	exec(`UPDATE paperboat.machine_service_snapshots SET expires_at=now()+interval '100 milliseconds' WHERE machine_id=$1`, machine)
	renewable := xmin("machine_service_snapshots")
	if e = report(short); e != nil {
		t.Fatal(e)
	}
	if after := xmin("machine_service_snapshots"); after == renewable {
		t.Fatal("a snapshot under half its lease was not renewed")
	}

	// New services are written at once; an older generation is still a conflict.
	snapshot.Generation, snapshot.Services = 2, []wire.Port{{Port: 5432, AddressFamily: "ipv4"}, {Port: 8080, AddressFamily: "ipv4"}}
	changed := xmin("machine_service_snapshots")
	if e = report(s); e != nil {
		t.Fatal(e)
	}
	if after := xmin("machine_service_snapshots"); after == changed {
		t.Fatal("a new service set was not written")
	}
	snapshot.Generation = 1
	if e = report(s); !errors.Is(e, machineservices.ErrConflict) {
		t.Fatalf("an older generation: %v, want ErrConflict", e)
	}
}
