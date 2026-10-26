package lazyaccess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Every heartbeat repeats the lazy runtime observation. Before, each one rewrote lazy_runtime_owners to extend a
// 30 s lease (~900 row versions an hour per machine on staging, PLAN 3b). Now the same runtime renews the lease only
// once less than half is left; a new boot is written at once and an older boot or a revoked machine still conflicts.
func TestUnchangedLazyRuntimeObservationWritesNoRowOnPostgres(t *testing.T) {
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
	owner, machine, environment := "lo-owner-"+suffix, "lo-machine-"+suffix, "lo-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'lazy','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	xmin := func() string {
		t.Helper()
		var x string
		if e := store.SQL().QueryRowContext(ctx, `SELECT xmin::text FROM paperboat.lazy_runtime_owners WHERE machine_id=$1`, machine).Scan(&x); e != nil {
			t.Fatal(e)
		}
		return x
	}
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	observation := RuntimeObservation{Schema: "paperboat.lazy-runtime/v1", BootID: "boot-0123456789abcdef", InstallationGeneration: 1, StartedAt: started}
	activator := &Activator{DB: store, OwnerLease: 2 * time.Minute}
	if e = activator.RecordLazyRuntimeObservation(ctx, environment, machine, observation); e != nil {
		t.Fatal(e)
	}
	stored := xmin()
	for range 3 {
		if e = activator.RecordLazyRuntimeObservation(ctx, environment, machine, observation); e != nil {
			t.Fatalf("unchanged observation: %v", e)
		}
	}
	if after := xmin(); after != stored {
		t.Fatalf("3 unchanged observations rewrote the runtime owner (xmin %s -> %s)", stored, after)
	}

	exec(`UPDATE paperboat.lazy_runtime_owners SET expires_at=now()+interval '30 seconds' WHERE machine_id=$1`, machine)
	renewable := xmin()
	if e = activator.RecordLazyRuntimeObservation(ctx, environment, machine, observation); e != nil {
		t.Fatal(e)
	}
	if after := xmin(); after == renewable {
		t.Fatal("an owner under half its lease was not renewed")
	}

	restarted := observation
	restarted.BootID, restarted.StartedAt = "boot-fedcba9876543210", started.Add(30*time.Second)
	if e = activator.RecordLazyRuntimeObservation(ctx, environment, machine, restarted); e != nil {
		t.Fatalf("a new boot: %v", e)
	}
	if e = activator.RecordLazyRuntimeObservation(ctx, environment, machine, observation); !errors.Is(e, ErrConflict) {
		t.Fatalf("the older boot after a restart: %v, want ErrConflict", e)
	}
	exec(`UPDATE paperboat.user_machines SET state='revoked',revoked_at=now() WHERE id=$1`, machine)
	if e = activator.RecordLazyRuntimeObservation(ctx, environment, machine, restarted); !errors.Is(e, ErrConflict) {
		t.Fatalf("a revoked machine's unchanged observation: %v, want ErrConflict", e)
	}
}
