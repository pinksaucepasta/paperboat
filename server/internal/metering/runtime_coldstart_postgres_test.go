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
	s := fmt.Sprint(time.Now().UnixNano())
	// Predicate locks are counted by this store's own connections (its application_name): the database also holds
	// whatever serializable work other tests and workers left behind, which a database-wide count blamed on the storm.
	application := "coldstart_" + s
	store, err := db.Open(config.Database{Driver: "postgres", DSN: db.TestDSNWithApplicationName(dsn, application)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	const machines = 300
	user, env := "storm_user_"+s, "storm_env_"+s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT 'storm_m_'||$3||'_'||g, $1, $2||'_'||g, 'storm-'||g||'-'||right($3,6), 'linux','amd64','/w','offline','occupied',false FROM generate_series(1,$4::int) g`, user, env, s, machines); err != nil {
		t.Fatal(err)
	}
	// siRead is the predicate locks of this store's connections by owning transaction. newSIRead counts those of
	// transactions that owned none in known. A total would not do: a btree page split copies a page lock to the new
	// page for the same owner, so the storm's own index inserts grow the locks of a transaction counted before it
	// ("300 runtime observations kept 1 predicate locks", though none was serializable).
	siRead := func() map[string]int {
		t.Helper()
		rows, err := store.SQL().QueryContext(ctx, `SELECT l.virtualtransaction, count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE l.mode = 'SIReadLock' AND a.application_name = $1 GROUP BY 1`, application)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		owners := map[string]int{}
		for rows.Next() {
			var owner string
			var locks int
			if err := rows.Scan(&owner, &locks); err != nil {
				t.Fatal(err)
			}
			owners[owner] = locks
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return owners
	}
	newSIRead := func(known map[string]int) (n int) {
		t.Helper()
		for owner, locks := range siRead() {
			if _, ok := known[owner]; !ok {
				n += locks
			}
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
	// Positive control: a serializable transaction of this store that commits now is counted as new, so a zero
	// below means the storm left none.
	beforeControl := siRead()
	control, err := store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback()
	if _, err := control.ExecContext(ctx, `SET LOCAL enable_seqscan TO off`); err != nil {
		t.Fatal(err)
	}
	if err := control.QueryRowContext(ctx, `SELECT online FROM paperboat.user_machines WHERE id = $1`, "storm_m_"+s+"_3").Scan(new(bool)); err != nil {
		t.Fatal(err)
	}
	if err := control.Commit(); err != nil {
		t.Fatal(err)
	}
	if newSIRead(beforeControl) == 0 {
		t.Fatal("a serializable read took no predicate lock the count can see: the check would pass vacuously")
	}
	before := siRead()
	// Serializable work of another client, committed while the storm runs, is not this store's and must not count.
	other, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherTx, err := other.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherTx.ExecContext(ctx, `SET LOCAL enable_seqscan TO off`); err != nil {
		t.Fatal(err)
	}
	if err := otherTx.QueryRowContext(ctx, `SELECT online FROM paperboat.user_machines WHERE id = $1`, "storm_m_"+s+"_2").Scan(new(bool)); err != nil {
		t.Fatal(err)
	}
	if err := otherTx.Commit(); err != nil {
		t.Fatal(err)
	}

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
	if kept := newSIRead(before); kept != 0 {
		t.Fatalf("%d runtime observations kept %d predicate locks (want 0)", machines, kept)
	}
	var online int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machines WHERE user_id = $1 AND online`, user).Scan(&online); err != nil || online != machines {
		t.Fatalf("online machines = %d, want %d (%v)", online, machines, err)
	}
	storm(2, true)
}
