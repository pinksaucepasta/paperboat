package control

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
)

const RuntimeCarrierAdmissionsPath = "/v1/edge/runtime/carrier-admissions"
const RuntimeCarrierObservationsPath = "/v1/edge/runtime/carrier-observations"

type RuntimeCarrierBinding struct {
	AccountID                            string `json:"account_id"`
	HostID                               string `json:"host_id"`
	TunnelID                             string `json:"tunnel_id"`
	ConnectorID                          string `json:"connector_id"`
	SessionID                            string `json:"session_id"`
	ProcessGeneration                    uint64 `json:"process_generation"`
	ConfigGeneration                     uint64 `json:"config_generation"`
	InstallationGeneration               uint64 `json:"installation_generation"`
	RouteID                              string `json:"route_id"`
	RouteGeneration                      uint64 `json:"route_generation"`
	EdgeNodeID                           string `json:"edge_node_id"`
	EdgeProcessEpoch                     string `json:"edge_process_epoch"`
	EdgeCarrierServerSPKISHA256          string `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string `json:"edge_carrier_server_certificate_chain_pem"`
	MachineIdentityPublicKey             string `json:"machine_identity_public_key"`
	MachineIdentityThumbprint            string `json:"machine_identity_thumbprint"`
}

type RuntimeCarrierAdmission struct {
	Schema               string                `json:"schema"`
	Binding              RuntimeCarrierBinding `json:"binding"`
	AttachmentGeneration uint64                `json:"attachment_generation"`
	ConfigContentHash    string                `json:"config_content_hash"`
	EdgeEndpoints        []string              `json:"edge_endpoints"`
	Hostname             string                `json:"hostname"`
	RouteKind            string                `json:"route_kind"`
	RouteRevision        uint64                `json:"route_revision"`
	ExpiresAt            time.Time             `json:"expires_at"`
}

func (a RuntimeCarrierAdmission) Expected(nodeID, epoch string, now time.Time) (datacarrier.ExpectedAdmission, error) {
	b := a.Binding
	if a.Schema != datacarrier.RuntimeCarrierSchema || a.RouteKind != datacarrier.RuntimeCarrierRoute || b.EdgeProcessEpoch != epoch || a.RouteRevision != b.RouteGeneration || len(a.EdgeEndpoints) != 2 {
		return datacarrier.ExpectedAdmission{}, ErrControlInvalid
	}
	seen := map[string]bool{}
	for _, endpoint := range a.EdgeEndpoints {
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "h2" && u.Scheme != "h3") || seen[u.Scheme] {
			return datacarrier.ExpectedAdmission{}, ErrControlInvalid
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return datacarrier.ExpectedAdmission{}, ErrControlInvalid
		}
		if ValidateCarrierServerCertificateChain(b.EdgeCarrierServerCertificateChainPEM, b.EdgeCarrierServerSPKISHA256, u.Hostname(), now) != nil {
			return datacarrier.ExpectedAdmission{}, ErrControlInvalid
		}
		seen[u.Scheme] = true
	}
	value := datacarrier.ExpectedAdmission{
		Schema: a.Schema, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch,
		Identity:               datacarrier.Identity{AccountID: b.AccountID, HostID: b.HostID, TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID, ProcessGeneration: b.ProcessGeneration, Generation: b.ConfigGeneration},
		InstallationGeneration: b.InstallationGeneration, ConfigGeneration: b.ConfigGeneration, ConfigContentHash: a.ConfigContentHash,
		RouteID: b.RouteID, RouteKind: a.RouteKind, Hostname: a.Hostname, RouteRevision: a.RouteRevision, AttachmentGeneration: a.AttachmentGeneration,
		Endpoint: "https://" + a.Hostname, ExpiresAt: a.ExpiresAt,
		EdgeCarrierServerSPKISHA256: b.EdgeCarrierServerSPKISHA256, EdgeCarrierServerCertificateChainPEM: b.EdgeCarrierServerCertificateChainPEM,
		MachineIdentityPublicKey: b.MachineIdentityPublicKey, MachineIdentityThumbprint: b.MachineIdentityThumbprint, Admitted: true,
	}
	if err := value.Validate(now, nodeID); err != nil {
		return datacarrier.ExpectedAdmission{}, err
	}
	return value, nil
}

type RuntimeCarrierObservation struct {
	RouteID              string `json:"route_id"`
	AttachmentGeneration uint64 `json:"attachment_generation"`
	SessionID            string `json:"session_id"`
	Ready                bool   `json:"ready"`
}

type RuntimeCarrierSource interface {
	RuntimeCarrierAdmissions(context.Context, string, string) ([]RuntimeCarrierAdmission, error)
	ObserveRuntimeCarriers(context.Context, string, string, []RuntimeCarrierObservation) error
}

func (c *HTTPClient) RuntimeCarrierAdmissions(ctx context.Context, nodeID, epoch string) ([]RuntimeCarrierAdmission, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return nil, ErrControlInvalid
	}
	var result struct {
		Schema     string                     `json:"schema"`
		Complete   bool                       `json:"complete"`
		Admissions *[]RuntimeCarrierAdmission `json:"admissions"`
	}
	if err := c.postNodeWithMaximumAndIdentity(ctx, RuntimeCarrierAdmissionsPath, nodeID, epoch, struct {
		NodeID       string `json:"edge_node_id"`
		ProcessEpoch string `json:"process_epoch"`
	}{nodeID, epoch}, &result, maxPreviewCarrierSnapshotBytes); err != nil {
		return nil, err
	}
	if result.Schema != datacarrier.RuntimeCarrierSchema || !result.Complete || result.Admissions == nil || len(*result.Admissions) > maxPreviewCarrierAdmissions {
		return nil, ErrControlUnavailable
	}
	now := time.Now().UTC()
	for _, a := range *result.Admissions {
		if _, err := a.Expected(nodeID, epoch, now); err != nil {
			return nil, documentFailure{sentinel: ErrControlUnavailable, cause: err}
		}
	}
	return *result.Admissions, nil
}

func (c *HTTPClient) ObserveRuntimeCarriers(ctx context.Context, nodeID, epoch string, observations []RuntimeCarrierObservation) error {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil || len(observations) > maxPreviewCarrierAdmissions {
		return ErrControlInvalid
	}
	for _, o := range observations {
		if connectorprotocol.ValidateIdentifier(o.RouteID) != nil || connectorprotocol.ValidateIdentifier(o.SessionID) != nil || o.AttachmentGeneration == 0 {
			return ErrControlInvalid
		}
	}
	if len(observations) == 0 {
		return nil
	}
	return c.postNodeWithMaximumAndIdentity(ctx, RuntimeCarrierObservationsPath, nodeID, epoch, struct {
		NodeID       string                      `json:"edge_node_id"`
		ProcessEpoch string                      `json:"process_epoch"`
		Observations []RuntimeCarrierObservation `json:"observations"`
	}{nodeID, epoch, observations}, nil, maxPreviewCarrierSnapshotBytes)
}
