package usermachines

import (
	"fmt"
	"errors"
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
	if err := observation.Validate(observation.ObservedAt); err != nil {
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
			if err := candidate.Validate(observation.ObservedAt); !errors.Is(err, ErrUpdateObservationInvalid) {
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
