package updateobservation

import (
	"encoding/json"
	"testing"
	"time"
)

// The observation rides the machine proof inside the runtime snapshot: the field order and the two omitempty
// members are the wire.
func TestObservationWire(t *testing.T) {
	raw, err := json.Marshal(Observation{Schema: "s", State: "idle", CurrentVersion: "v", Channel: "stable", OperationID: "o", InstallationGeneration: 1, WorkerGeneration: 2, OSBootID: "b", RollbackCount: 3, ObservedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)})
	want := `{"schema":"s","state":"idle","current_version":"v","channel":"stable","operation_id":"o","installation_generation":1,"worker_generation":2,"os_boot_id":"b","rollback_count":3,"observed_at":"2030-01-01T00:00:00Z"}`
	if err != nil || string(raw) != want {
		t.Fatalf("observation = %s, %v", raw, err)
	}
}
