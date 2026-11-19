package runtimeattachment

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Prod 2026-11-20: 13 of 70 serialization failures in 30 minutes were attaches whose edge selection locked node rows
// (FOR SHARE) that the edges' heartbeats had updated after the attach's snapshot. Here a heartbeat commits between a
// serializable transaction's snapshot and its edge selection: the old locking form fails with 40001, and
// edgeSelectQuery reads the same edges and commits.
func TestEdgeSelectionDoesNotConflictWithHeartbeatsOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "edgesel")
	ctx := f.ctx
	query := edgeSelectQuery(`$6::text IS NULL`) // every edge; references the team argument as the real eligibility does
	args := []any{f.account, "", f.machine, int64(1), int64(1), nil}
	heartbeat := func() {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, `UPDATE control_tunnel_nodes SET last_heartbeat_at=now(),capacity_observed_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode); err != nil {
			t.Fatal(err)
		}
	}
	selectAfterHeartbeat := func(sql string) (int, error) {
		tx, err := f.database.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil { // takes the snapshot
			t.Fatal(err)
		}
		heartbeat()
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return 0, err
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return n, err
		}
		return n, tx.Commit(ctx)
	}

	_, err := selectAfterHeartbeat(query + ` FOR SHARE OF n`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("the locking form after a heartbeat: %v, want 40001 (the conflict this test guards against did not happen)", err)
	}
	n, err := selectAfterHeartbeat(query)
	if err != nil {
		t.Fatalf("edge selection after a heartbeat failed: %v", err)
	}
	if n == 0 {
		t.Fatal("edge selection found no edge")
	}
}
