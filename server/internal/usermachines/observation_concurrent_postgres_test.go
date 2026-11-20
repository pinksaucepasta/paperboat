package usermachines

import (
	"context"
	"testing"
	"time"
)

// A machine's capability or availability report waits for a concurrent write to its machine row instead of failing
// with 40001 and replaying: it decides on the row it locks and writes under that lock.
func TestMachineObservationsWaitForAMachineWritePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	environment := "env_" + f.suffix
	initial, err := f.service.ResolveMachineCapabilities(f.ctx, f.machine, environment)
	if err != nil {
		t.Fatal(err)
	}
	reports := map[string]func(context.Context) error{
		"capabilities": func(ctx context.Context) error {
			return f.service.RecordMachineCapabilitiesObservation(ctx, f.machine, environment, MachineCapabilitiesObservation{
				Schema: MachineCapabilitiesSchemaV1, Version: initial.DesiredVersion, Applied: initial.Desired, Status: "error", ErrorCode: "apply_failed", ObservedAt: time.Now().UTC()})
		},
		"availability": func(ctx context.Context) error {
			return f.service.RecordAvailabilityObservation(ctx, environment, f.machine, AvailabilityObservation{
				Schema: AvailabilityPolicySchemaV1, Mode: "allow_sleep", Version: 0, Status: "applied", ObservedAt: time.Now().UTC(), HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateHealth: "healthy"})
		},
	}
	for name, report := range reports {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			writer, err := f.store.Pool().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(ctx)
			if _, err := writer.Exec(ctx, `UPDATE paperboat.user_machines SET updated_at=now() WHERE id=$1`, f.machine); err != nil {
				t.Fatal(err)
			}
			var pid int32
			if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			before := f.store.TransactionStats()
			done := make(chan error, 1)
			go func() { done <- report(ctx) }()
			for waiting := false; !waiting; {
				select {
				case err := <-done:
					t.Fatalf("the report finished before the machine write committed: %v", err)
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err := f.store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			after := f.store.TransactionStats()
			if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
				t.Fatalf("the %s report retried %d transaction attempts (40001/40P01)", name, n)
			}
		})
	}
}
