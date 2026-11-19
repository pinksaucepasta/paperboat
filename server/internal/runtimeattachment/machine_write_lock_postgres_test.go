package runtimeattachment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// Prod 2026-11-20: a machine's carrier row (10 in 30 min) and its route assignments (18) were written by two
// transactions at once, and the later one failed its serializable commit with 40001. Writers of a machine now queue on
// its write lock before their snapshot. Here another writer holds the lock and an uncommitted change of the contended
// row while the machine attaches: the attach must wait for it and commit without a single serialization retry.
func TestAttachQueuesBehindAnotherWriterOfTheMachineOnPostgres(t *testing.T) {
	for _, c := range []struct{ name, change string }{
		{"carrier row", `UPDATE runtime_carrier_attachments SET generation=generation WHERE machine_id=$1`},
		{"route assignments", `UPDATE endpoint_edge_assignments SET generation=generation WHERE endpoint_id IN (SELECT id FROM endpoints WHERE agent_machine_id=$1)`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAgentFixture(t, "mlock_"+strings.ReplaceAll(c.name, " ", "_"))
			ctx := f.ctx
			lease := f.prefix + "_preview"
			f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,$2,$3,$3,$4,'owner_session_mlock','foreground','http','127.0.0.1:3000','team',now()+interval '10 minutes')`, lease, "https://"+strings.ReplaceAll(f.prefix, "_", "-")+".preview.example.test", f.account, f.machine)
			if err := f.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: f.account, AgentMachineID: f.machine, AccessMode: "team", Endpoint: "https://" + strings.ReplaceAll(f.prefix, "_", "-") + ".preview.example.test"})
			}); err != nil {
				t.Fatal(err)
			}
			request := f.request
			request.Capabilities = []string{runtimewire.CapabilityEndpoints}
			if _, err := f.service.Attach(ctx, f.proof, request); err != nil { // creates the carrier row and the assignments
				t.Fatal(err)
			}
			var rows int
			if err := f.database.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM runtime_carrier_attachments WHERE machine_id=$1)+(SELECT count(*) FROM endpoint_edge_assignments a JOIN endpoints e ON e.id=a.endpoint_id WHERE e.agent_machine_id=$1)`, f.machine).Scan(&rows); err != nil || rows < 2 {
				t.Fatalf("fixture: %d carrier and assignment rows (%v)", rows, err)
			}

			// The other writer: the machine's write lock, then an uncommitted change of the contended row.
			conn, err := f.database.SQL().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,$2))`, f.machine, endpoints.MachineWriteLockSeed); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL SERIALIZABLE`); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, c.change, f.machine); err != nil {
				t.Fatal(err)
			}

			before := f.database.TransactionStats().Serializable.Retries
			done := make(chan error, 1)
			go func() { _, err := f.service.Attach(ctx, f.proof, request); done <- err }()
			time.Sleep(300 * time.Millisecond) // the attach is now waiting: on the lock (fixed) or on the row (before)
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,$2))`, f.machine, endpoints.MachineWriteLockSeed); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("attach behind another writer of the machine: %v", err)
			}
			if retries := f.database.TransactionStats().Serializable.Retries - before; retries != 0 {
				t.Fatalf("the attach was retried %d times after a serialization failure, want 0 (it must queue)", retries)
			}
		})
	}
}
