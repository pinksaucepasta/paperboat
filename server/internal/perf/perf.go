// Package perf holds the server's performance budgets and the realistic-scale seed they are measured on.
//
// The check itself (internal/app, TestPerformanceBudgetsOnSeededPostgres) drives the real router against a
// database seeded by Prepare and fails when an endpoint's median latency or its query count per request exceeds
// its budget. The gate host is shared with every lane's builds, so its p95 measures their load, not ours: the
// median (with headroom) and the query count are the regression signals, and p95 is reported.
// It runs in `make gate` when PAPERBOAT_PERF_DATABASE_DSN names an isolated *_test database.
package perf

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

//go:embed seed.sql
var seedSQL string

//go:embed budgets.json
var budgetsJSON []byte

// Actors seeded by seed.sql.
const (
	TeamOwner  = "user_perf_0001" // owner of perf-team-001: 200 members, 1000 team machines
	Solo       = "user_perf_0002" // 100 personal machines, many terminals, previews and tunnels
	TeamMember = "user_perf_0003" // member of perf-team-001 with selected-member grants
	// Ordinary member of perf-team-001: may use only the half of its machines that every member may.
	OrdinaryMember = "user_perf_0050"
	Team           = "perf-team-001"
)

// Endpoint is one measured request. Path may contain {team_machine}, {solo_machine}, {solo_tunnel} and
// {solo_preview}, resolved from the seeded data.
type Endpoint struct {
	Actor      string  `json:"actor"`
	Path       string  `json:"path"`
	P50Ms      float64 `json:"p50_ms,omitempty"`
	MaxQueries int     `json:"max_queries"`
	// Reason explains a budget above the default read budget.
	Reason string `json:"reason,omitempty"`
}

type Budgets struct {
	ReadP50Ms  float64    `json:"read_p50_ms"`
	Iterations int        `json:"iterations"`
	Endpoints  []Endpoint `json:"endpoints"`
}

func LoadBudgets() (Budgets, error) {
	var b Budgets
	if err := json.Unmarshal(budgetsJSON, &b); err != nil {
		return Budgets{}, fmt.Errorf("perf budgets: %w", err)
	}
	if b.ReadP50Ms <= 0 || b.Iterations < 5 || len(b.Endpoints) == 0 {
		return Budgets{}, fmt.Errorf("perf budgets: read_p50_ms, iterations >= 5 and endpoints are required")
	}
	for i, e := range b.Endpoints {
		if e.Actor == "" || e.Path == "" || e.MaxQueries < 1 {
			return Budgets{}, fmt.Errorf("perf budgets: endpoint %d needs actor, path and max_queries", i)
		}
		if e.P50Ms > b.ReadP50Ms && e.Reason == "" {
			return Budgets{}, fmt.Errorf("perf budgets: %s %s is above the read budget without a reason", e.Actor, e.Path)
		}
		if e.P50Ms == 0 {
			b.Endpoints[i].P50Ms = b.ReadP50Ms
		}
	}
	return b, nil
}

// Prepare migrates the database and seeds it once per seed version. New migrations apply on top of seeded
// data, as they do in production; a changed seed truncates the schema and seeds again (about two minutes).
func Prepare(ctx context.Context, store *db.DB) error {
	if err := db.Migrate(ctx, store); err != nil {
		return fmt.Errorf("migrate perf database: %w", err)
	}
	sum := sha256.Sum256([]byte(seedSQL))
	version := hex.EncodeToString(sum[:])
	if _, err := store.SQL().ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS perf;
CREATE TABLE IF NOT EXISTS perf.seed(version text NOT NULL, seeded_at timestamptz NOT NULL)`); err != nil {
		return fmt.Errorf("perf seed marker: %w", err)
	}
	var current string
	_ = store.SQL().QueryRowContext(ctx, `SELECT version FROM perf.seed`).Scan(&current)
	if current == version {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	return store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `DO $$ DECLARE tables text; BEGIN
SELECT string_agg(format('%I.%I', schemaname, tablename), ', ') INTO tables FROM pg_tables
WHERE schemaname = 'paperboat' AND tablename <> 'goose_db_version';
EXECUTE 'TRUNCATE TABLE ' || tables || ' CASCADE'; END $$`); err != nil {
			return fmt.Errorf("clear perf database: %w", err)
		}
		if _, err := tx.Exec(ctx, seedSQL); err != nil {
			return fmt.Errorf("seed perf database: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM perf.seed`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO perf.seed(version, seeded_at) VALUES($1, now())`, version)
		return err
	})
}
