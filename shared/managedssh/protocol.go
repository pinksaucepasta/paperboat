// Package managedssh defines the managed SSH wire documents and public-key identity.
package managedssh

type ClientKeyDocument struct {
	Type                  string `json:"type"`
	Version               int    `json:"version"`
	Fingerprint           string `json:"fingerprint"`
	PublicKey             string `json:"public_key"`
	State                 string `json:"state"`
	ReconciliationVersion uint64 `json:"reconciliation_version"`
	CreatedAt             string `json:"created_at"`
	RevokedAt             string `json:"revoked_at,omitempty"`
}

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

type AuthorizedKeysDocument struct {
	Type              string   `json:"type"`
	Version           int      `json:"version"`
	MachineID         string   `json:"machine_id"`
	MachineGeneration uint64   `json:"machine_generation"`
	Keys              []string `json:"keys"`
}
