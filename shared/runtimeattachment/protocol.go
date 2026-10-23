// Package runtimeattachment defines the machine, server and edge carrier wire contract.
package runtimeattachment

import (
	"encoding/json"
	"time"
)

const Schema = "paperboat.runtime-carrier/v1"
const RouteKind = "runtime_https_wss"

// Endpoint route kinds and capabilities (PLAN 3a). An agent advertises CapabilityEndpoints in
// Request.Capabilities and an edge in EdgeRequest.Capabilities; the server sends endpoint routes
// to an edge only when both did.
const (
	EndpointPublicRouteKind     = "preview_public_https_wss"
	EndpointRestrictedRouteKind = "preview_private_https_wss"
	CapabilityEndpoints         = "endpoints"
	// CapabilityTunnelEndpoints (with CapabilityEndpoints): an agent serves its tunnels' routes on
	// its runtime carriers, and an edge serves tunnel routes whose replica is an agent's runtime
	// carrier. A tunnel moves onto the engine only when its agent and every edge it holds carriers
	// to advertise it; until then it keeps its own connector.
	CapabilityTunnelEndpoints = "tunnel_endpoints"
)
const MaximumAdmissions = 4096
const MaximumEdgeCandidates = 32
const HostPath = "/v1/runtime/carrier-attachment"
const EdgePullPath = "/v1/edge/runtime/carrier-admissions"
const EdgeObservePath = "/v1/edge/runtime/carrier-observations"

// EdgeEndpointPullPath returns only the endpoint routes of an edge's runtime snapshot (PLAN 3a). It
// is small, so an edge pulls it at its preview interval and a new preview or tunnel route is
// admitted within a second instead of waiting for the next full runtime pull.
const EdgeEndpointPullPath = "/v1/edge/runtime/endpoint-admissions"

// HostTunnelsPath returns, to an agent that serves tunnel endpoints, its tunnels on the engine with
// their active config snapshots (the routes and their origins), so it can forward their streams.
const HostTunnelsPath = "/v1/runtime/endpoint-tunnels"

// TunnelsRequest asks for the calling machine's engine tunnels; the machine proof binds it.
type TunnelsRequest struct {
	OperationID string `json:"operation_id"`
}

// EngineTunnel is one tunnel the agent serves on its runtime carriers.
type EngineTunnel struct {
	TunnelID          string `json:"tunnel_id"`
	ConfigGeneration  uint64 `json:"config_generation"`
	ConfigContentHash string `json:"config_content_hash"`
	// Snapshot is the tunnel's active config snapshot (paperboat.preview-tunnel/v1), as a
	// connector would receive it.
	Snapshot json.RawMessage `json:"snapshot"`
}

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
	Capabilities           []string        `json:"capabilities,omitempty"`
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
	// Endpoint routes (PLAN 3a) ride on the agent's runtime carrier: same Binding identity, their
	// own route id, hostname and kind (EndpointPublicRouteKind or EndpointRestrictedRouteKind).
	EndpointID string `json:"endpoint_id,omitempty"`
	AccessMode string `json:"access_mode,omitempty"`
}
type HostAdmission struct {
	Admission
	EdgeCandidates         []EdgeCandidate `json:"edge_candidates"`
	Backup                 *Admission      `json:"backup,omitempty"`
	ActiveEdgeNodeID       string          `json:"active_edge_node_id,omitempty"`
	ActiveEdgeProcessEpoch string          `json:"active_edge_process_epoch,omitempty"`
	// EndpointsServed: every edge of this attachment admits endpoint routes, so the agent serves
	// its previews as endpoint routes on these carriers (PLAN 3a).
	EndpointsServed bool `json:"endpoints_served,omitempty"`
	// TunnelEndpointsServed: every edge of this attachment also serves tunnel routes on these
	// carriers, so the agent serves its tunnels here instead of running their connectors.
	TunnelEndpointsServed bool `json:"tunnel_endpoints_served,omitempty"`
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
	Capabilities []string      `json:"capabilities,omitempty"`
}

// Snapshot requires an explicit admissions array, including when empty.
// A nil pointer identifies an absent/null incomplete snapshot on decode.
type Snapshot struct {
	Schema     string       `json:"schema"`
	Complete   bool         `json:"complete"`
	Admissions *[]Admission `json:"admissions"`
}

// ValidCapabilities accepts no capabilities, CapabilityEndpoints, or CapabilityEndpoints with
// CapabilityTunnelEndpoints, each at most once.
func ValidCapabilities(values []string) bool {
	switch len(values) {
	case 0:
		return true
	case 1:
		return values[0] == CapabilityEndpoints
	case 2:
		return values[0] == CapabilityEndpoints && values[1] == CapabilityTunnelEndpoints ||
			values[1] == CapabilityEndpoints && values[0] == CapabilityTunnelEndpoints
	}
	return false
}

// HasCapability reports whether values contains capability.
func HasCapability(values []string, capability string) bool {
	for _, value := range values {
		if value == capability {
			return true
		}
	}
	return false
}
