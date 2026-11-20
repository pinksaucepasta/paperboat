package retention

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A day of the Fleet page's version counts goes once it is past retention.fleet_daily; a recent one stays.
func TestFleetVersionDailyIsPrunedPastItsWindowOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.fleet_version_daily WHERE version='2097.1.1.1'`)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.fleet_version_daily(day,platform,version,machines)
 VALUES((now() AT TIME ZONE 'UTC')::date-500,'linux','2097.1.1.1',1),((now() AT TIME ZONE 'UTC')::date-2,'linux','2097.1.1.1',1)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Retention
	result, err := Run(ctx, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var left int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.fleet_version_daily WHERE version='2097.1.1.1'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 || result["fleet_version_daily"] < 1 {
		t.Fatalf("%d rows left (want only the recent day), %d deleted", left, result["fleet_version_daily"])
	}
}
