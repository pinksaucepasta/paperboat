package retention

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestBandwidthHistoryExpiresInBatchesWithoutNewReportsPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	const workspace = "personal:bandwidth_retention"
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM paperboat.resource_bandwidth_events WHERE workspace_id=$1`, workspace)
	})
	cfg := config.Default().Retention
	cfg.Batch = 2
	const historyWindow = 90 * 24 * time.Hour
	if _, err := store.Pool().Exec(ctx, `INSERT INTO paperboat.resource_bandwidth_events
 (operation_id,workspace_id,resource_kind,resource_id,resource_name,mode,hosting,upload_bytes,download_bytes,interval_start,interval_end)
 SELECT 'bandwidth_retention_'||g,$1,'preview','preview','preview','edge','managed',1,0,stamp,stamp
 FROM generate_series(1,6) g CROSS JOIN LATERAL
 (SELECT now()-make_interval(secs=>$2::float8)+CASE WHEN g<6 THEN '-1 hour' ELSE '1 hour' END::interval stamp) v`,
		workspace, historyWindow.Seconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	var left string
	if err := store.Pool().QueryRow(ctx, `SELECT string_agg(operation_id,',' ORDER BY operation_id)
 FROM paperboat.resource_bandwidth_events WHERE workspace_id=$1`, workspace).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "bandwidth_retention_6" {
		t.Fatalf("remaining rows = %q, want only the in-window report", left)
	}
}
