// Package inspector is the traffic-inspector access wire: the control plane resolves a credential to the owner
// machine's current carrier route (to the CLI over the account session, to the edge over its node credential) and
// lists the routes a principal may inspect. The server's output defines the documents; each consumer validates the
// parts it uses (the cli the route fence and expiry, the edge the carrier generations and node binding).
package inspector

import "time"

// Carrier is the owner machine's current carrier route for one resource.
type Carrier struct {
	LeaseGeneration      uint64 `json:"lease_generation"`
	AttachmentGeneration uint64 `json:"attachment_generation"`
	OwnerMachineID       string `json:"owner_machine_id"`
	MachineID            string `json:"machine_id"`
	RouteGeneration      uint64 `json:"route_generation"`
	AccountID            string `json:"account_id"`
	TunnelID             string `json:"tunnel_id"`
	ConnectorID          string `json:"connector_id"`
	SessionID            string `json:"session_id"`
	ProcessGeneration    uint64 `json:"process_generation"`
	Generation           uint64 `json:"generation"`
	RouteID              string `json:"route_id"`
	EdgeNodeID           string `json:"edge_node_id"`
	EdgeProcessEpoch     string `json:"edge_process_epoch"`
	AssignmentID         string `json:"assignment_id"`
	AssignmentGeneration uint64 `json:"assignment_generation"`
	ConfigContentHash    string `json:"config_content_hash"`
}

// Access is what an inspector credential resolves to.
type Access struct {
	CredentialID       string    `json:"credential_id"`
	OwnerAccountID     string    `json:"owner_account_id"`
	MachineID          string    `json:"machine_id"`
	MachineGeneration  uint64    `json:"machine_generation"`
	ResourceKind       string    `json:"resource_kind"`
	ResourceID         string    `json:"resource_id"`
	RouteID            string    `json:"route_id"`
	ResourceGeneration uint64    `json:"resource_generation"`
	RouteGeneration    uint64    `json:"route_generation"`
	TargetGeneration   uint64    `json:"target_generation"`
	ExpiresAt          time.Time `json:"expires_at"`
	EdgeURL            string    `json:"edge_url"`
	Carrier            Carrier   `json:"carrier"`
}

// Target is one route a principal holding the requested inspector action may inspect (routing metadata only).
type Target struct {
	ResourceID string `json:"resource_id"`
	RouteID    string `json:"route_id"`
	RouteName  string `json:"route_name"`
}
