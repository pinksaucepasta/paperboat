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

// Heartbeats repeat the last capability, availability and update reports: an unchanged one opens no transaction and
// locks no row (a writer holding the machine row does not delay it), a changed one is still recorded or refused.
func TestUnchangedObservationsTakeNoLockPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	environment := "env_" + f.suffix
	initial, err := f.service.ResolveMachineCapabilities(f.ctx, f.machine, environment)
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT installation_generation FROM paperboat.user_machines WHERE id=$1`, f.machine).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	status, update := "error", validUpdateObservation()
	update.InstallationGeneration, update.ObservedAt = uint64(generation), at
	health := "healthy"
	reports := map[string]func(context.Context) error{
		"capabilities": func(ctx context.Context) error {
			return f.service.RecordMachineCapabilitiesObservation(ctx, f.machine, environment, MachineCapabilitiesObservation{
				Schema: MachineCapabilitiesSchemaV1, Version: initial.DesiredVersion, Applied: initial.Desired, Status: status, ErrorCode: map[bool]string{true: "apply_failed"}[status == "error"], ObservedAt: at})
		},
		"availability": func(ctx context.Context) error {
			return f.service.RecordAvailabilityObservation(ctx, environment, f.machine, AvailabilityObservation{
				Schema: AvailabilityPolicySchemaV1, Mode: "allow_sleep", Version: 0, Status: "applied", ObservedAt: at, HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateHealth: health})
		},
		"update": func(ctx context.Context) error {
			update.OperationID += "x"
			update.ObservedAt = update.ObservedAt.Add(time.Second)
			return f.service.RecordUpdateObservation(ctx, environment, f.machine, update)
		},
	}
	for name, report := range reports {
		if err := report(f.ctx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	writer, err := f.store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(f.ctx)
	if _, err := writer.Exec(f.ctx, `UPDATE paperboat.user_machines SET updated_at=now() WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	before := f.store.TransactionStats().ReadCommitted.Calls
	for name, report := range reports {
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		if err := report(ctx); err != nil {
			t.Fatalf("unchanged %s: %v", name, err)
		}
		cancel()
	}
	if n := f.store.TransactionStats().ReadCommitted.Calls - before; n != 0 {
		t.Fatalf("3 unchanged reports opened %d transactions", n)
	}
	if err := writer.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	status, health, update.CurrentVersion = "applied", "recovery_required", "2026.08.18.2"
	for name, report := range reports {
		if err := report(f.ctx); err != nil {
			t.Fatalf("changed %s: %v", name, err)
		}
	}
	var ok bool
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT m.capabilities_status='applied' AND m.update_health='recovery_required' AND o.current_version='2026.08.18.2'
 FROM paperboat.user_machines m JOIN paperboat.user_machine_update_observations o ON o.user_machine_id=m.id WHERE m.id=$1`, f.machine).Scan(&ok); err != nil || !ok {
		t.Fatalf("changed reports not recorded: %v %v", ok, err)
	}
	// The desired version moved on: the repeated report is stale, not unchanged.
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET capabilities_desired_version=capabilities_desired_version+1,availability_desired_version=availability_desired_version+1 WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"capabilities", "availability"} {
		if err := reports[name](f.ctx); err == nil {
			t.Fatalf("%s report of an old version was accepted", name)
		}
	}
}
