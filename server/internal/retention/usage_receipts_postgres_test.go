package retention

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Edge usage receipts are deleted by age, in bounded batches. They used to wait for acknowledged_at, which nothing ever
// set: 311,028 receipts (237 MB) piled up in production in five weeks. A receipt inside the window stays, because it
// answers a replay of the same operation.
func TestRetentionDeletesUsageReceiptsPastTheirWindowOnPostgres(t *testing.T) {
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
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("ret_rcpt_%d_", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_usage_receipts WHERE operation_id LIKE $1`, prefix+"%")
	})
	cfg := config.Default().Retention
	window := cfg.UsageReceipts
	insert := func(label string, count int, age time.Duration) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_usage_receipts
 (operation_id,edge_node_id,counter_epoch,environment_id,route_id,route_revision,direction,observed_bytes,delta_bytes,interval_start,interval_end,created_at)
SELECT $1||$2||'_'||g,'node','epoch','env','route',1,'ingress',0,0,now()-make_interval(secs=>$3::float8),now()-make_interval(secs=>$3::float8),now()-make_interval(secs=>$3::float8)
FROM generate_series(1,$4::int) g`, prefix, label, age.Seconds(), count); err != nil {
			t.Fatal(err)
		}
	}
	// More old receipts than one batch (never acknowledged), a few an hour inside the window, and fresh ones.
	insert("old", cfg.Batch*2+7, window+time.Hour)
	insert("inside", 3, window-time.Hour)
	insert("fresh", 2, time.Minute)
	if _, err := Run(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	left := func(label string) (n int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_usage_receipts WHERE operation_id LIKE $1`, prefix+label+"_%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := left("old"); got != 0 {
		t.Fatalf("%d receipts past the window (never acknowledged) survived retention", got)
	}
	if left("inside") != 3 || left("fresh") != 2 {
		t.Fatalf("receipts inside the window were deleted: inside %d of 3, fresh %d of 2", left("inside"), left("fresh"))
	}
}
