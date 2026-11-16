package metering

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Every deploy made ~300 daemons send runtime observations in the same seconds. Each was a SERIALIZABLE transaction,
// and the predicate locks it and its triggers took stayed held while any overlapping serializable transaction ran,
// until the shared table ran out (SQLSTATE 53200, 186 errors in 13 s on 2026-11-16) and every serializable caller
// (preview and tunnel operations too) failed with 5xx. Runtime observations now take no predicate locks.
func TestReconnectStormRuntimeObservationsTakeNoPredicateLocksOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	const machines = 300
	s := fmt.Sprint(time.Now().UnixNano())
	user, env := "storm_user_"+s, "storm_env_"+s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT 'storm_m_'||$3||'_'||g, $1, $2||'_'||g, 'storm-'||g||'-'||right($3,6), 'linux','amd64','/w','offline','occupied',false FROM generate_series(1,$4::int) g`, user, env, s, machines); err != nil {
		t.Fatal(err)
	}
	countSIRead := func() int {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE mode = 'SIReadLock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// An open serializable transaction keeps the predicate locks of every serializable transaction that commits
	// while it runs, as the server's other serializable work did during the storm.
	held, err := store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	if _, err := held.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	before := countSIRead()

	repo := NewRuntimeRepository(store, time.Hour)
	// storm runs one observation per machine at once, plus one serializable read per machine when withReaders (the
	// preview, tunnel and carrier callers that failed beside them); any failure fails the test.
	storm := func(generation uint64, withReaders bool) {
		t.Helper()
		errs := make(chan error, 2*machines)
		var wg sync.WaitGroup
		for i := 1; i <= machines; i++ {
			machine, env := fmt.Sprintf("storm_m_%s_%d", s, i), fmt.Sprintf("%s_%d", env, i)
			wg.Add(1)
			go func() {
				defer wg.Done()
				now := time.Now().UTC()
				errs <- repo.RecordRuntimeObservation(ctx, RuntimeObservation{EnvironmentID: env, MachineID: machine, ObservedAt: now, WorkerGeneration: generation, OSBootID: "boot-" + machine, WorkerServiceScope: "user", ConnectorState: "ready", DiagnosticsObservedAt: now, Capabilities: []string{"terminal_host"}})
			}()
			if withReaders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
						var online bool
						return tx.QueryRow(ctx, `SELECT online FROM user_machines WHERE id = $1`, machine).Scan(&online)
					})
				}()
			}
		}
		wg.Wait()
		close(errs)
		failed := 0
		for err := range errs {
			if err != nil {
				if failed++; failed <= 3 {
					t.Errorf("storm call failed: %v", err)
				}
			}
		}
		if failed > 0 {
			t.Fatalf("%d storm calls failed (generation %d)", failed, generation)
		}
	}

	storm(1, false) // offline to online: every write and trigger runs
	if grown := countSIRead() - before; grown != 0 {
		t.Fatalf("%d runtime observations kept %d predicate locks (want 0)", machines, grown)
	}
	var online int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machines WHERE user_id = $1 AND online`, user).Scan(&online); err != nil || online != machines {
		t.Fatalf("online machines = %d, want %d (%v)", online, machines, err)
	}
	storm(2, true)
}
