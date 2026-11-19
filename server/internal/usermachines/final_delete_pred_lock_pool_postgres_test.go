package usermachines

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

// Prod 2026-11-19 12:09Z: the worker's final `DELETE FROM user_machines` of canary-b-old (18k access sessions) failed
// with "out of shared memory" (max_pred_locks_per_transaction), because the FK cascade removed every session in one
// SERIALIZABLE statement. finalDeleteMachine now removes the child rows in bounded transactions first. This test needs a
// PostgreSQL with a tiny predicate-lock pool (see the skip message). A machine with 18,000 sessions shaped like
// canary-b-old (mostly expired but still active, some revoked, a few live) is revoked, then final-deleted by the worker
// tick while a serializable reader keeps the pool full (400 ms held, 300 ms free, repeating) and enrollments and the
// edge-set reconcile run. The tick must delete the machine and every child row, in bounded time, with no error and a
// bounded number of predicate locks.
func TestFinalDeleteOfAMachineWithManySessionsCompletesWhenThePredicateLockPoolIsSmallOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	perTx := requireSmallPredicateLockPool(t, store)
	owner, stamp := seedPredPoolAccount(t, store)
	machine := "machine_7_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	// 18,000 sessions: 14,000 active and expired, 3,800 revoked (helper told), 200 active and live.
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,revoked_at,helper_revoked_at,helper_terminal_session_id,capabilities,operation_id)
SELECT 'control_plane_signed',$1||'_s_'||g,$1,$2,u.environment_id,'fixture-client','',
 CASE WHEN g<=17800 THEN now()-interval '1 hour' ELSE now()+interval '1 hour' END,
 CASE WHEN g>14000 AND g<=17800 THEN 'revoked' ELSE 'active' END,
 CASE WHEN g>14000 AND g<=17800 THEN now()-interval '30 minutes' END,
 CASE WHEN g>14000 AND g<=17800 THEN now()-interval '30 minutes' END,
 'h_'||$1||'_'||g,ARRAY['exec'],'op' FROM generate_series(1,18000) g, paperboat.user_machines u WHERE u.id=$1`, machine, owner)
	exec(`ANALYZE paperboat.user_machine_access_sessions`) // fresh statistics: the retirement query's plan depends on them
	var seeded int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, machine).Scan(&seeded); err != nil || seeded != 18000 {
		t.Fatalf("seeded %d sessions: %v", seeded, err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.native_usage_authorities WHERE actor_account_id=$1`, owner)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machine_access_sessions WHERE user_id=$1`, owner)
	})
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if err := service.Disconnect(ctx, owner, machine); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// The retention worker retires the expired sessions the revoke left active; the final delete waits for that.
	for range 20 {
		if err := service.retireExpiredAccessSessions(ctx); err != nil {
			t.Fatalf("retire: %v", err)
		}
	}
	var active int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1 AND state='active'`, machine).Scan(&active); err != nil || active != 0 {
		t.Fatalf("%d sessions still active after the revoke and retirement: %v", active, err)
	}

	stopLoad := startPredPoolLoad(t, store, owner, stamp) // the revoke above is not under it: see TestRevokeMachineWithManyAccessSessionsOnPostgres
	// Holders: serializable readers filling the pool, one after another, until the final delete is over.
	holdCtx, stopHolders := context.WithCancel(ctx)
	var holders sync.WaitGroup
	var holderErr atomic.Value
	var held atomic.Int64
	holders.Go(func() {
		for holdCtx.Err() == nil {
			full := make(chan error, 1)
			released := make(chan error, 1)
			go func() { released <- holdPredicateLockPoolFull(holdCtx, store.SQL(), full, 400*time.Millisecond) }()
			if err := <-full; err != nil {
				if holdCtx.Err() == nil {
					holderErr.Store(err)
				}
				return
			}
			held.Add(1)
			if err := <-released; err != nil && holdCtx.Err() == nil {
				holderErr.Store(err)
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	})
	// Peak SIREAD lock count while the delete runs.
	var peak atomic.Int64
	holders.Go(func() {
		for holdCtx.Err() == nil {
			var n int64
			if err := store.SQL().QueryRowContext(holdCtx, `SELECT count(*) FROM pg_locks WHERE mode='SIReadLock'`).Scan(&n); err == nil && n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	for held.Load() == 0 && holderErr.Load() == nil {
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	err := service.RetryPendingRevocations(ctx)
	took := time.Since(start)
	stopHolders()
	holders.Wait()
	stopLoad()
	if e, _ := holderErr.Load().(error); e != nil {
		t.Fatalf("holder: %v", e)
	}
	if err != nil {
		t.Fatalf("final delete failed after %v: %v", took, err)
	}
	// One-off on this pool without holders: 1.43 s. The holders add at most the 400 ms hold per bounded batch.
	if took > 10*time.Second {
		t.Fatalf("final delete took %v, want under 10 s", took)
	}
	// A leak guard, not the pool size: 317-320 SIREAD locks were counted with the pool full (revoke test), so 40 x the per-transaction limit is generous.
	if bound := int64(40 * perTx); peak.Load() > bound {
		t.Fatalf("%d SIREAD locks at peak, want at most %d", peak.Load(), bound)
	}
	var machines, sessions, retained int
	if err := store.SQL().QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM paperboat.user_machines WHERE id=$1),
 (SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1),
 (SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type='user_machine.final_deleted')`, machine).Scan(&machines, &sessions, &retained); err != nil {
		t.Fatal(err)
	}
	if machines != 0 || sessions != 0 || retained != 1 {
		t.Fatalf("after the final delete: %d machine rows, %d sessions, %d final_deleted events (want 0, 0, 1)", machines, sessions, retained)
	}
	t.Logf("final delete of 18,000 sessions took %v under %d full-pool holders (peak %d SIREAD locks, pool %d per transaction); %+v", took, held.Load(), peak.Load(), perTx, store.TransactionStats().Serializable)
}
