package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func validUpdateObservation() UpdateObservation {
	return UpdateObservation{
		Schema: UpdateObservationSchemaV1, State: "healthy", CurrentVersion: "2026.08.18.1",
		Channel: "stable", OperationID: "update-op-0001", InstallationGeneration: 2,
		WorkerGeneration: 4, OSBootID: "boot-1", RollbackCount: 0,
		ObservedAt: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	}
}

func TestUpdateObservationValidationFencesUnsafeState(t *testing.T) {
	observation := validUpdateObservation()
	if err := ValidateUpdateObservation(observation, observation.ObservedAt); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	for name, mutate := range map[string]func(*UpdateObservation){
		"missing operation":               func(value *UpdateObservation) { value.OperationID = "short" },
		"failed without error":            func(value *UpdateObservation) { value.State = "failed" },
		"unknown error code":              func(value *UpdateObservation) { value.State, value.ErrorCode = "failed", "unsafe-value!" },
		"missing target while activating": func(value *UpdateObservation) { value.State = "activating" },
		"future timestamp":                func(value *UpdateObservation) { value.ObservedAt = time.Date(2026, 8, 18, 12, 6, 0, 0, time.UTC) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := observation
			mutate(&candidate)
			if err := ValidateUpdateObservation(candidate, observation.ObservedAt); !errors.Is(err, ErrUpdateObservationInvalid) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestUpdateObservationHashIsStable(t *testing.T) {
	observation := validUpdateObservation()
	if got, want := string(hashUpdateObservation(observation)), string(hashUpdateObservation(observation)); got != want {
		t.Fatal("equivalent observations produced different hashes")
	}
	changed := observation
	changed.WorkerGeneration++
	if string(hashUpdateObservation(observation)) == string(hashUpdateObservation(changed)) {
		t.Fatal("changed observation reused the same payload hash")
	}
}

// Every heartbeat reports the update state under a new operation. Before, each report rewrote
// user_machine_update_observations (~430 row versions an hour per machine on staging, PLAN 3b); now the same state
// is rewritten only once the stored report is older than half the offline cutoff, and a new version at once.
func TestUnchangedUpdateObservationWritesNoRowPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	environment := "env_" + f.suffix
	var generation int64
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT installation_generation FROM paperboat.user_machines WHERE id=$1`, f.machine).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	xmin := func() string {
		t.Helper()
		var x string
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT xmin::text FROM paperboat.user_machine_update_observations WHERE user_machine_id=$1`, f.machine).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	now := time.Now().UTC().Truncate(time.Second)
	observation := validUpdateObservation()
	observation.InstallationGeneration, observation.ObservedAt = uint64(generation), now.Add(-time.Minute)
	if err := f.service.RecordUpdateObservation(f.ctx, environment, f.machine, observation); err != nil {
		t.Fatal(err)
	}
	stored := xmin()
	for i := range 3 {
		observation.OperationID = fmt.Sprintf("update-op-heartbeat-%d", i)
		observation.ObservedAt = observation.ObservedAt.Add(10 * time.Second)
		if err := f.service.RecordUpdateObservation(f.ctx, environment, f.machine, observation); err != nil {
			t.Fatal(err)
		}
	}
	if after := xmin(); after != stored {
		t.Fatalf("3 unchanged update reports rewrote the row (xmin %s -> %s)", stored, after)
	}
	observation.OperationID, observation.CurrentVersion = "update-op-new-version", "2026.08.18.2"
	if err := f.service.RecordUpdateObservation(f.ctx, environment, f.machine, observation); err != nil {
		t.Fatal(err)
	}
	if after := xmin(); after == stored {
		t.Fatal("a new version was not written")
	}
}

// Re-enrollment invalidates in-flight observations for the old installation.
// Both reports must wait for the machine write, including the first report of
// the new installation, which has no observation row to lock yet.
func TestUpdateReportsWaitForInstallationChangePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	environment := "env_" + f.suffix
	writer, err := f.store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	var generation uint64
	if err := writer.QueryRow(ctx, `UPDATE paperboat.user_machines
SET installation_generation=installation_generation+1 WHERE id=$1
RETURNING installation_generation`, f.machine).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	old := validUpdateObservation()
	old.InstallationGeneration = generation - 1
	newReport := old
	newReport.InstallationGeneration = generation
	newReport.OperationID = "update-op-new-installation"
	before := f.store.TransactionStats()
	report := func(observation UpdateObservation) <-chan error {
		done := make(chan error, 1)
		go func() { done <- f.service.RecordUpdateObservation(ctx, environment, f.machine, observation) }()
		return done
	}
	stale, current := report(old), report(newReport)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := f.store.Pool().QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-stale:
			t.Fatalf("old report finished before the installation write committed: %v", err)
		case err := <-current:
			t.Fatalf("new report finished before the installation write committed: %v", err)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-stale; !errors.Is(err, ErrUpdateObservationStale) {
		t.Fatalf("old installation report: %v", err)
	}
	if err := <-current; err != nil {
		t.Fatalf("new installation report: %v", err)
	}
	after := f.store.TransactionStats()
	if retries := after.Serializable.Retries - before.Serializable.Retries; retries != 0 {
		t.Fatalf("update reports retried %d transaction attempts (40001/40P01)", retries)
	}
	got, err := f.service.GetUpdateObservation(ctx, f.account, f.machine)
	if err != nil || got.InstallationGeneration != generation || got.OperationID != newReport.OperationID {
		t.Fatalf("stored report = %+v, error = %v", got, err)
	}
}
