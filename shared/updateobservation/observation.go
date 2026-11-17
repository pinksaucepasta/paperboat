// Package updateobservation is the machine's update-supervisor status, reported inside the signed runtime
// observation and shown in the account's update dashboard. The cli builds it, the server validates it strictly
// and stores it; the ordering fields fence a late packet from replacing newer state.
package updateobservation

import "time"

// Observation is the machine-authenticated status of the local update supervisor.
type Observation struct {
	Schema                 string    `json:"schema"`
	State                  string    `json:"state"`
	CurrentVersion         string    `json:"current_version"`
	TargetVersion          string    `json:"target_version,omitempty"`
	Channel                string    `json:"channel"`
	OperationID            string    `json:"operation_id"`
	InstallationGeneration uint64    `json:"installation_generation"`
	WorkerGeneration       uint64    `json:"worker_generation"`
	OSBootID               string    `json:"os_boot_id"`
	RollbackCount          uint64    `json:"rollback_count"`
	ErrorCode              string    `json:"error_code,omitempty"`
	ObservedAt             time.Time `json:"observed_at"`
}
