// Package managedssh defines the managed SSH wire documents and public-key identity.
package managedssh

type TargetDocument struct {
	Type                  string `json:"type"`
	Version               int    `json:"version"`
	MachineID             string `json:"machine_id"`
	MachineGeneration     uint64 `json:"machine_generation"`
	OSUser                string `json:"os_user"`
	Port                  uint16 `json:"port"`
	ReconciliationVersion uint64 `json:"reconciliation_version"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

type HostKeySetDocument struct {
	Type                  string   `json:"type"`
	Version               int      `json:"version"`
	SetID                 string   `json:"set_id"`
	MachineID             string   `json:"machine_id"`
	MachineGeneration     uint64   `json:"machine_generation"`
	ObservationGeneration uint64   `json:"observation_generation"`
	Fingerprint           string   `json:"fingerprint"`
	Keys                  []string `json:"keys"`
	State                 string   `json:"state"`
	ReconciliationVersion uint64   `json:"reconciliation_version"`
	ObservedAt            string   `json:"observed_at"`
	PromotedAt            string   `json:"promoted_at,omitempty"`
}

// CertificateDocument is a short-lived user certificate for one machine: principal OSUser, usable until ValidBefore.
type CertificateDocument struct {
	Type              string `json:"type"`
	Version           int    `json:"version"`
	MachineID         string `json:"machine_id"`
	MachineGeneration uint64 `json:"machine_generation"`
	OSUser            string `json:"os_user"`
	Certificate       string `json:"certificate"`
	ValidBefore       string `json:"valid_before"`
}

// UserCAsDocument lists the user CA public keys a machine trusts for managed SSH (at most MaxUserCAs) and the
// revocation principal every certificate must carry (the CA lines' principals= option).
type UserCAsDocument struct {
	Type              string   `json:"type"`
	Version           int      `json:"version"`
	MachineID         string   `json:"machine_id"`
	MachineGeneration uint64   `json:"machine_generation"`
	Keys              []string `json:"keys"`
	Principal         string   `json:"principal"`
}
