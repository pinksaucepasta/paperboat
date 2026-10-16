package teaminbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Extend the actual request/approval owner with a concurrent creation/removal
// boundary, without a query hook or a second set of authorization fixtures.
func checkConcurrentInboxInventorySnapshot(t *testing.T, ctx context.Context, store *db.DB, service *Service, account, original string) {
	t.Helper()
	id := "inventory_" + original
	defer store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_inbox_requests WHERE request_id=$1`, id)
	run, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	var changes atomic.Int32
	go func() {
		for run.Err() == nil {
			_, err := store.SQL().ExecContext(run, `INSERT INTO paperboat.team_inbox_requests SELECT (jsonb_populate_record(NULL::paperboat.team_inbox_requests,to_jsonb(r)||jsonb_build_object('request_id',$2::text,'operation_id',$2::text,'batch_id',$2::text))).* FROM paperboat.team_inbox_requests r WHERE r.request_id=$1`, original, id)
			if err != nil {
				done <- err
				return
			}
			changes.Add(1)
			if _, err = store.SQL().ExecContext(run, `DELETE FROM paperboat.team_inbox_requests WHERE request_id=$1`, id); err != nil {
				done <- err
				return
			}
		}
		done <- run.Err()
	}()
	defer func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error("inventory writer failed", err)
		}
	}()
	for i := 0; i < 512; i++ {
		page, total, err := service.Inventory(ctx, account, id, "", "", 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != len(page.Requests) {
			t.Fatalf("single filtered page has %d requests but total %d during concurrent creation", len(page.Requests), total)
		}
	}
	if changes.Load() < 2 {
		t.Fatal("fixture did not overlap inventory with request mutations")
	}
}
