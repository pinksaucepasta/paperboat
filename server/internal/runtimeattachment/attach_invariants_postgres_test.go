package runtimeattachment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
)

// The runtime attach runs at READ COMMITTED: each read it decides on is protected by a row lock or a lock above the
// transaction. Each case holds the protected row in another, uncommitted transaction while the machine attaches: the
// attach must wait for it and then decide on its committed state. Without the lock it would read the old state and
// commit against it.
func TestAttachDecidesOnLockedRowsOnPostgres(t *testing.T) {
	for _, c := range []struct {
		name  string
		hold  string // the other transaction's change, $1 = machine, $2 = account, $3 = route
		check func(t *testing.T, err error)
	}{
		{"account suspended (users FOR SHARE)", `UPDATE users SET status='suspended' WHERE id=$2 AND $1<>'' AND $3<>''`, wantUnavailable},
		{"machine revoked (user_machines FOR UPDATE)", `UPDATE user_machines SET revoked_at=now() WHERE id=$1 AND $2<>'' AND $3<>''`, wantUnavailable},
		{"another process attached (route FOR UPDATE)", `WITH r AS (SELECT id FROM control_routes WHERE id=$3 FOR UPDATE)
UPDATE runtime_carrier_attachments SET process_id='process_other' WHERE route_id IN (SELECT id FROM r) AND $1<>'' AND $2<>''`, func(t *testing.T, err error) {
			if !errors.Is(err, ErrStale) {
				t.Fatalf("attach after another process attached the route: %v, want ErrStale", err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAgentFixture(t, "inv")
			ctx := f.ctx
			if _, err := f.service.Attach(ctx, f.proof, f.request); err != nil { // the carrier rows exist
				t.Fatal(err)
			}
			conn, err := f.database.SQL().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, c.hold, f.machine, f.account, f.route); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := f.service.Attach(ctx, f.proof, f.request); done <- err }()
			select {
			case err := <-done:
				_, _ = conn.ExecContext(ctx, `ROLLBACK`)
				t.Fatalf("the attach decided while the other transaction held the row (err %v)", err)
			case <-time.After(300 * time.Millisecond):
			}
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				c.check(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("the attach did not finish after the other transaction committed")
			}
		})
	}
}

func wantUnavailable(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("attach after the change committed: %v, want ErrUnavailable", err)
	}
}

// The workspace edge set is created at most once when two attaches create it at READ COMMITTED at the same time:
// LockWorkspace orders them and the second finds the first's set.
func TestWorkspaceEdgeSetIsCreatedOnceAtReadCommittedOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "edgeonce")
	ctx := f.ctx
	f.exec(t, `UPDATE control_tunnel_nodes SET public_ingress_verified_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode)
	scope := edgeset.Scope{Kind: "account", ID: f.account, Account: f.account}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			errs <- f.database.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				return edgeset.EnsureTx(ctx, tx, scope, time.Now().UTC())
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensure the edge set: %v", err)
		}
	}
	var sets int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2`, scope.Kind, scope.ID).Scan(&sets); err != nil || sets != 1 {
		t.Fatalf("%d edge sets for the workspace (%v), want 1", sets, err)
	}
}
