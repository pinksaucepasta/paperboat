package resourceusage

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/retention"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestRolledUpBandwidthHistoryReadsTheSameAsTheRawRowsPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	suffix := strings.ToLower(now.Format("150405.000000"))
	suffix = strings.ReplaceAll(suffix, ".", "") + "_rollup"
	user, workspaceID := "rollup_"+suffix, "personal:rollup_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@usage.invalid','active')`, user)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM resource_bandwidth_events WHERE workspace_id=$1`, workspaceID)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, user)
	})
	// Two old hours (older than the exact window) and one recent hour. The traffic_hourly trigger counts direct rows once, as written.
	old := now.Truncate(time.Hour).Add(-20 * 24 * time.Hour)
	recent := now.Truncate(time.Hour).Add(-3 * time.Hour)
	type row struct {
		resource, mode, actor string
		at                    time.Time
		up, down              int64
	}
	rows := []row{
		{"r1", "direct", "", old.Add(5 * time.Minute), 10, 1}, {"r1", "direct", "", old.Add(15 * time.Minute), 20, 2},
		{"r1", "direct", "", old.Add(25 * time.Minute), 30, 3}, {"r1", "direct", "", old.Add(70 * time.Minute), 40, 4},
		{"r2", "edge", "", old.Add(7 * time.Minute), 5, 50}, {"r2", "edge", "", old.Add(8 * time.Minute), 6, 60},
		{"r2", "edge", user, old.Add(9 * time.Minute), 7, 70}, {"r1", "direct", "", recent.Add(5 * time.Minute), 100, 10},
	}
	for i, r := range rows {
		var actor any
		if r.actor != "" {
			actor = r.actor
		}
		exec(`INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,mode,hosting,upload_bytes,download_bytes,interval_start,interval_end,actor_account_id)
VALUES($1,$2,'preview',$3,$3||'-'||$1,$4,'managed',$5,$6,$7,$8,$9)`, suffix+string(rune('a'+i)), workspaceID, r.resource, r.mode, r.up, r.down, r.at.Add(-30*time.Second), r.at, actor)
	}
	service := New(store)
	read := func(start, end time.Time, actor string) string {
		t.Helper()
		got, err := service.Get(ctx, user, workspace.Scope{ID: "personal"}, Query{Start: start, End: end, ActorAccountID: actor})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		return string(raw)
	}
	// The retention run also folds hourly traffic older than its window into daily rows, so count both.
	traffic := func() (up int64) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT
 coalesce((SELECT sum(upload_bytes) FROM traffic_hourly WHERE category='direct' AND hour>=$1 AND hour<$2),0)+
 coalesce((SELECT sum(upload_bytes) FROM traffic_daily WHERE category='direct' AND day>=$1::date AND day<=$2::date),0)`, old, old.Add(2*time.Hour)).Scan(&up); err != nil {
			t.Fatal(err)
		}
		return up
	}
	windows := [][2]time.Time{{old, old.Add(2 * time.Hour)}, {old, old.Add(time.Hour)}, {old.Add(time.Hour), old.Add(2 * time.Hour)}, {old.Add(-time.Hour), now}}
	before := make([]string, len(windows))
	for i, w := range windows {
		before[i] = read(w[0], w[1], "")
	}
	memberBefore, trafficBefore := read(old, old.Add(2*time.Hour), user), traffic()

	cfg := config.Default().Retention
	cfg.Batch = 3 // an hour's rows span several batches
	for pass := 0; pass < 2; pass++ {
		if _, err := retention.Run(ctx, store, cfg); err != nil {
			t.Fatal(err)
		}
	}
	var left, rolled int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE operation_id LIKE 'rollup:%') FROM resource_bandwidth_events WHERE workspace_id=$1`, workspaceID).Scan(&left, &rolled); err != nil {
		t.Fatal(err)
	}
	// r1 hour 0, r1 hour 1, r2 without member, r2 with member, plus the recent raw row.
	if left != 5 || rolled != 4 {
		t.Fatalf("rows = %d (%d rolled), want 5 (4 rolled): old rows merge per hour and member, recent rows stay", left, rolled)
	}
	for i, w := range windows {
		if got := read(w[0], w[1], ""); got != before[i] {
			t.Fatalf("window %d reads differently after the rollup:\nbefore %s\nafter  %s", i, before[i], got)
		}
	}
	if got := read(old, old.Add(2*time.Hour), user); got != memberBefore {
		t.Fatalf("member filter reads differently after the rollup:\nbefore %s\nafter  %s", memberBefore, got)
	}
	if got := traffic(); got != trafficBefore {
		t.Fatalf("operator direct traffic changed from %d to %d: rolled rows were counted again", trafficBefore, got)
	}
}
