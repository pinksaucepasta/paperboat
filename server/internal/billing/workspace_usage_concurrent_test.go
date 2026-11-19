package billing_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Edges report the first usage of a workspace's period in the same instant: the period row is created
// once, nobody fails on its primary key, and every byte is counted.
func TestWorkspaceUsageFirstReportsOfAPeriodAtOnce(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "usr_usage_cc_" + suffix
	personal := "personal:" + user
	insertUser(t, store, user, "usage-cc-"+suffix+"@example.test")
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, personal)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, personal)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,user_id,team_id,state) VALUES($1,$2,NULL,'active')`, personal, user); err != nil {
		t.Fatal(err)
	}
	service := billing.NewWorkspaceService(store, nil, "")
	const edges, bytes = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, edges)
	for range edges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				_, _, err := service.DebitWorkspaceBandwidthTx(ctx, tx, personal, bytes, time.Now().UTC())
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	var periods, observed int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*), coalesce(sum(observed_bytes),0) FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, personal).Scan(&periods, &observed); err != nil {
		t.Fatal(err)
	}
	if periods != 1 || observed != edges*bytes {
		t.Fatalf("periods=%d observed=%d, want 1 and %d", periods, observed, edges*bytes)
	}
	t.Run("admission leaves the period row unchanged for a concurrent overview", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		before := store.TransactionStats()
		ready, release := make(chan int32, 1), make(chan struct{})
		admission := make(chan error, 1)
		go func() {
			admission <- store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				if _, err := service.CheckWorkspaceBandwidthTx(ctx, tx, personal, time.Now().UTC()); err != nil {
					return err
				}
				var pid int32
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				ready <- pid
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		var pid int32
		select {
		case pid = <-ready:
		case err := <-admission:
			t.Fatalf("admission did not hold the workspace lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		overview := make(chan error, 1)
		go func() {
			w, err := service.Overview(ctx, user, "")
			if err == nil && w.UsageBytes != observed {
				err = fmt.Errorf("overview usage = %d, want %d", w.UsageBytes, observed)
			}
			overview <- err
		}()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting bool
			if err := store.Pool().QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case err := <-overview:
				t.Fatalf("overview finished while admission held the workspace lock: %v", err)
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		close(release)
		if err := <-admission; err != nil {
			t.Fatal(err)
		}
		if err := <-overview; err != nil {
			t.Fatal(err)
		}
		if retries := store.TransactionStats().Serializable.Retries - before.Serializable.Retries; retries != 0 {
			t.Fatalf("workspace overview retried %d transaction attempts (40001/40P01)", retries)
		}
	})

}
