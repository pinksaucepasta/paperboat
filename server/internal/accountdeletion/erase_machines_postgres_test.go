package accountdeletion

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

// deleteWithMachines gives a fresh account n personal machines, deletes the account and returns the number of
// statements the client issued for it (db.SetQueryObserver sees client statements, not trigger SQL) and how long it took.
func deleteWithMachines(t *testing.T, f *fixture, name string, n int) (u auth.User, statements int64, took time.Duration) {
	t.Helper()
	u = f.user(name)
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
		SELECT $1||g, $2, $1||g, 'm-'||g, 'linux','amd64','/workspace','online','occupied',true,1 FROM generate_series(1,$3::int) g`, "many_"+u.ID+"_", u.ID, n)
	if got := f.count(`SELECT count(*) FROM user_machines WHERE user_id=$1 AND deleted_at IS NULL`, u.ID); got != n {
		t.Fatalf("seeded %d machines, want %d", got, n)
	}
	var count atomic.Int64
	f.store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		count.Add(1)
		return ctx, func(string) {}
	})
	defer f.store.SetQueryObserver(nil)
	started := time.Now()
	if err := f.svc.Delete(t.Context(), u, time.Now(), u.PrimaryEmail); err != nil {
		t.Fatal(err)
	}
	return u, count.Load(), time.Since(started)
}

// Deleting an account removes its machines a chunk at a time, not one by one. 200 machines leave nothing behind,
// each leaves its audit event, and the statement count grows by at most one (the audit INSERT) per extra machine:
// the ~30 per-table statements and the account's other erase steps do not repeat per machine.
func TestAccountDeletionErasesManyMachinesSetBasedPostgres(t *testing.T) {
	f := newFixture(t)
	const small, large = 20, 200
	_, fewStatements, _ := deleteWithMachines(t, f, "erase_few", small)
	u, manyStatements, took := deleteWithMachines(t, f, "erase_many", large)
	if fewStatements == 0 || manyStatements == 0 {
		t.Fatalf("the query observer counted no statements (%d, %d)", fewStatements, manyStatements)
	}
	if extra, allowed := manyStatements-fewStatements, int64(large-small)+10; extra > allowed {
		t.Fatalf("%d machines took %d statements against %d for %d: %d more, want at most %d (one audit INSERT per extra machine)", large, manyStatements, fewStatements, small, extra, allowed)
	}
	// The time is the per-row triggers of the machine UPDATE (peer policy, inventory, team access: ~85% of it), not the statement count.
	if took > 30*time.Second {
		t.Fatalf("deleting %d machines took %v", large, took)
	}
	for name, query := range map[string]string{
		"machines still live":      `SELECT count(*) FROM user_machines WHERE user_id=$1 AND deleted_at IS NULL`,
		"machine rows not deleted": `SELECT count(*) FROM user_machines WHERE user_id=$1 AND state<>'deleted'`,
		"seats still occupied":     `SELECT count(*) FROM user_machines WHERE user_id=$1 AND seat_state<>'released'`,
		"machines still online":    `SELECT count(*) FROM user_machines WHERE user_id=$1 AND online`,
		"active access sessions":   `SELECT count(*) FROM user_machine_access_sessions WHERE user_id=$1 AND state='active'`,
	} {
		if got := f.count(query, u.ID); got != 0 {
			t.Errorf("%s: %d, want 0", name, got)
		}
	}
	if got := f.count(`SELECT count(*) FROM audit_events WHERE event_type='user_machine.deleted' AND resource_id LIKE $1`, "many_"+u.ID+"_%"); got != large {
		t.Errorf("user_machine.deleted audit events = %d, want %d", got, large)
	}
}
