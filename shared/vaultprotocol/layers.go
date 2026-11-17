// Package vaultprotocol is the JSON wire of the end-to-end encrypted ENV vault between the control plane, the
// CLI and the browser engine: layer, scope, team, grant and password-vault documents. Every encrypted body is an
// opaque base64url envelope here; the cryptography and its validation stay with each side (clients decode
// tolerantly, the server strictly). One definition per type, one Go name per type.
package vaultprotocol

import "time"

type VaultLayerCoordinate struct {
	WorkspaceID string `json:"workspace_id"`
	OwnerKind   string `json:"owner_kind"`
	OwnerID     string `json:"owner_id"`
	MachineID   string `json:"machine_id"`
}
type VaultLayerSource struct {
	VaultLayerCoordinate
	KeyEpoch   uint64 `json:"key_epoch"`
	Revision   uint64 `json:"revision"`
	DocumentID string `json:"document_id"`
}
type VaultLayerRecipient struct {
	RecipientAccount       string `json:"recipient_account"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
	DeliveryGeneration     uint64 `json:"delivery_generation"`
	DocumentID             string `json:"document_id"`
	FenceGeneration        uint64 `json:"fence_generation"`
}
type VaultLayerDelivery struct {
	RecordSequence uint64                 `json:"record_sequence"`
	Observation    *VaultLayerObservation `json:"observation"`
	Applied        bool                   `json:"applied"`
	Recipient      VaultLayerRecipient    `json:"recipient"`
	Source         VaultLayerSource       `json:"source"`
	WriterAccount  string                 `json:"writer_account"`
	WriterPublic   string                 `json:"writer_public"`
	Envelope       string                 `json:"envelope"`
	State          string                 `json:"state"`
}
type VaultLayerPut struct {
	OperationID string `json:"operation_id"`
	Envelope    string `json:"envelope"`
}
type VaultLayerContext struct {
	Recipient      VaultLayerRecipient  `json:"recipient"`
	WorkspaceID    string               `json:"workspace_id"`
	ActorAccountID string               `json:"actor_account_id"`
	MachineID      string               `json:"machine_id"`
	Layers         []VaultLayerDelivery `json:"layers"`
	// NamesListable is the workspace setting "Commands on machines can list ENV names".
	NamesListable bool `json:"names_listable"`
}

type VaultLayerObservation struct {
	WorkspaceID        string    `json:"workspace_id"`
	OwnerKind          string    `json:"owner_kind"`
	OwnerID            string    `json:"owner_id"`
	SourceMachineID    string    `json:"source_machine_id"`
	DeliveryGeneration uint64    `json:"delivery_generation"`
	DocumentID         string    `json:"document_id"`
	FenceGeneration    uint64    `json:"fence_generation"`
	HostRecipientKeyID string    `json:"host_recipient_key_id"`
	ObservationSeq     uint64    `json:"observation_seq"`
	State              string    `json:"state"`
	ErrorCode          *string   `json:"error_code"`
	ObservedAt         time.Time `json:"observed_at"`
}

// Coordinate is the source a delivery observation reports on.
func (o VaultLayerObservation) Coordinate() VaultLayerCoordinate {
	return VaultLayerCoordinate{o.WorkspaceID, o.OwnerKind, o.OwnerID, o.SourceMachineID}
}
