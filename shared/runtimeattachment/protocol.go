// Package runtimeattachment defines the machine, server and edge carrier wire contract.
package runtimeattachment

import "time"

const Schema = "paperboat.runtime-carrier/v1"
const RouteKind = "runtime_https_wss"
const MaximumAdmissions = 4096
const MaximumEdgeCandidates = 32
const HostPath = "/v1/runtime/carrier-attachment"
const EdgePullPath = "/v1/edge/runtime/carrier-admissions"
const EdgeObservePath = "/v1/edge/runtime/carrier-observations"

type EdgeCandidate struct {
	NodeID        string `json:"node_id"`
	ProcessEpoch  string `json:"process_epoch"`
	Region        string `json:"region"`
	FailureDomain string `json:"failure_domain"`
	ProbeAddress  string `json:"probe_address"`
}

type EdgePreference struct {
	PrimaryNodeID         string `json:"primary_node_id"`
	PrimaryProcessEpoch   string `json:"primary_process_epoch"`
	SecondaryNodeID       string `json:"secondary_node_id,omitempty"`
	SecondaryProcessEpoch string `json:"secondary_process_epoch,omitempty"`
}

// EdgeLatency is the machine's measured connect time to one server-issued edge candidate. The
// server accepts it only for a candidate it issued; it is display data and never confers access.
type EdgeLatency struct {
	NodeID          string `json:"node_id"`
	ProcessEpoch    string `json:"process_epoch"`
	RTTMilliseconds int64  `json:"rtt_ms"`
}

// MaximumLatencyMilliseconds bounds an accepted measurement; slower paths are reported as failed probes.
const MaximumLatencyMilliseconds = 10000

type Request struct {
	OperationID            string          `json:"operation_id"`
	ProcessID              string          `json:"process_id"`
	WorkerGeneration       uint64          `json:"worker_generation"`
	InstallationGeneration uint64          `json:"installation_generation"`
	RuntimeListenPort      int32           `json:"runtime_listen_port"`
	EdgePreference         *EdgePreference `json:"edge_preference,omitempty"`
	ActiveEdgeNodeID       string          `json:"active_edge_node_id,omitempty"`
	ActiveEdgeProcessEpoch string          `json:"active_edge_process_epoch,omitempty"`
	EdgeLatency            []EdgeLatency   `json:"edge_latency,omitempty"`
}
type Binding struct {
	AccountID                            string `json:"account_id"`
	HostID                               string `json:"host_id"`
	TunnelID                             string `json:"tunnel_id"`
	ConnectorID                          string `json:"connector_id"`
	SessionID                            string `json:"session_id"`
	ProcessGeneration                    uint64 `json:"process_generation"`
	ConfigGeneration                     uint64 `json:"config_generation"`
	RouteID                              string `json:"route_id"`
	RouteGeneration                      uint64 `json:"route_generation"`
	InstallationGeneration               uint64 `json:"installation_generation"`
	EdgeNodeID                           string `json:"edge_node_id"`
	EdgeProcessEpoch                     string `json:"edge_process_epoch"`
	EdgeCarrierServerSPKISHA256          string `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string `json:"edge_carrier_server_certificate_chain_pem"`
	MachineIdentityPublicKey             string `json:"machine_identity_public_key"`
	MachineIdentityThumbprint            string `json:"machine_identity_thumbprint"`
}
type Admission struct {
	Schema               string    `json:"schema"`
	Binding              Binding   `json:"binding"`
	AttachmentGeneration uint64    `json:"attachment_generation"`
	ConfigContentHash    string    `json:"config_content_hash"`
	EdgeEndpoints        []string  `json:"edge_endpoints"`
	Hostname             string    `json:"hostname"`
	RouteKind            string    `json:"route_kind"`
	RouteRevision        uint64    `json:"route_revision"`
	ExpiresAt            time.Time `json:"expires_at"`
}
type HostAdmission struct {
	Admission
	EdgeCandidates         []EdgeCandidate `json:"edge_candidates"`
	Backup                 *Admission      `json:"backup,omitempty"`
	ActiveEdgeNodeID       string          `json:"active_edge_node_id,omitempty"`
	ActiveEdgeProcessEpoch string          `json:"active_edge_process_epoch,omitempty"`
}
type Observation struct {
	RouteID              string `json:"route_id"`
	AttachmentGeneration uint64 `json:"attachment_generation"`
	SessionID            string `json:"session_id"`
	Ready                bool   `json:"ready"`
}
type EdgeRequest struct {
	NodeID       string        `json:"edge_node_id"`
	ProcessEpoch string        `json:"process_epoch"`
	Observations []Observation `json:"observations,omitempty"`
}

// Snapshot requires an explicit admissions array, including when empty.
// A nil pointer identifies an absent/null incomplete snapshot on decode.
type Snapshot struct {
	Schema     string       `json:"schema"`
	Complete   bool         `json:"complete"`
	Admissions *[]Admission `json:"admissions"`
}
