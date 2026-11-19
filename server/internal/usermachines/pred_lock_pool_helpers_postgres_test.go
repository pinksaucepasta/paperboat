package usermachines

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// tinyPoolFlags make PostgreSQL's predicate-lock pool smaller than the schema (restart-only settings).
const tinyPoolFlags = "-c max_pred_locks_per_transaction=10 -c max_connections=10 -c max_wal_senders=0 -c wal_level=minimal -c max_worker_processes=2 -c autovacuum_max_workers=2"

// requireSmallPredicateLockPool skips unless the server's predicate-lock target table is smaller than the number of
// tables the holder reads, so the holder is certain to hit 53200. PostgreSQL 17 sizes that table as
// max_pred_locks_per_transaction * (MaxBackends + max_prepared_transactions) (predicate.c NPREDICATELOCKTARGETENTS) with
// MaxBackends = max_connections + autovacuum_max_workers + 1 + max_worker_processes + max_wal_senders + 2
// (postinit.c InitializeMaxBackends, NUM_SPECIAL_WORKER_PROCS). Measured on postgres:17: 159 reads before 53200 with
// tinyPoolFlags (nominal 170), 329 with only the first two flags (nominal 340). It returns max_pred_locks_per_transaction.
func requireSmallPredicateLockPool(t *testing.T, store *db.DB) int {
	t.Helper()
	var perTx, conns, prepared, senders, workers, autovac, tables int
	if err := store.SQL().QueryRowContext(t.Context(), `SELECT
 current_setting('max_pred_locks_per_transaction')::int, current_setting('max_connections')::int,
 current_setting('max_prepared_transactions')::int, current_setting('max_wal_senders')::int,
 current_setting('max_worker_processes')::int, current_setting('autovacuum_max_workers')::int,
 (SELECT count(*) FROM pg_tables WHERE schemaname='paperboat')`).Scan(&perTx, &conns, &prepared, &senders, &workers, &autovac, &tables); err != nil {
		t.Fatal(err)
	}
	const special = 2 // NUM_SPECIAL_WORKER_PROCS in PostgreSQL 17
	capacity := perTx * (conns + autovac + 1 + workers + senders + special + prepared)
	t.Logf("predicate-lock pool: max_pred_locks_per_transaction=%d max_connections=%d max_prepared_transactions=%d max_wal_senders=%d max_worker_processes=%d autovacuum_max_workers=%d: capacity %d targets, %d tables in the schema",
		perTx, conns, prepared, senders, workers, autovac, capacity, tables)
	if capacity >= tables {
		t.Skipf("the predicate-lock pool holds %d targets (max_pred_locks_per_transaction=%d max_connections=%d max_prepared_transactions=%d max_wal_senders=%d max_worker_processes=%d autovacuum_max_workers=%d) and the schema has %d tables, so the holder could not fill it: run PostgreSQL with `docker run ... postgres:17 %s` (restart-only settings) to run this test",
			capacity, perTx, conns, prepared, senders, workers, autovac, tables, tinyPoolFlags)
	}
	return perTx
}
