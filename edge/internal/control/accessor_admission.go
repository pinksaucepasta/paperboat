// Package control is the edge's client of the control plane's private API. It pulls route assignments
// and snapshots, verifies credentials and fetches current connector authority, reports usage and node
// observations, pulls certificate distribution actions and requests on-demand certificates, and
// obtains browser-access, browser-terminal and private-access decisions. Requests and responses are
// size-bounded and strictly decoded, and a failure is reported as a typed error that says whether
// control is unavailable or the observation is stale.
package control

import (
	"context"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const PrivateAccessCarrierAdmissionsPath = "/v1/edge/private-access/carrier-admissions"

type PrivateAccessCarrierAdmission = connectorprotocol.PrivateAccessAdmission

// PrivateAccessCarrierDurable is the durable carrier admission one private-access admission projects to.
func PrivateAccessCarrierDurable(a PrivateAccessCarrierAdmission) datacarrier.DurableAdmission {
	return datacarrier.DurableAdmission{Identity: datacarrier.Identity{AccountID: a.AccountID, HostID: a.MachineID, TunnelID: a.TunnelID, ConnectorID: a.CarrierConnectorID, SessionID: a.CarrierSessionID, ProcessGeneration: a.ProcessGeneration, Generation: a.ConfigGeneration}, EdgeNodeID: a.EdgeNodeID, EdgeProcessEpoch: a.EdgeProcessEpoch, AssignmentID: a.AssignmentID, RouteID: a.RouteID, AssignmentGeneration: a.AssignmentGeneration, RouteGeneration: a.RouteGeneration, ConfigContentHash: a.ConfigContentHash, MachineIdentityPublicKey: a.AccessorPublicKey, MachineIdentityThumbprint: a.AccessorThumbprint, State: "active", ExpiresAt: a.ExpiresAt}
}

// ValidatePrivateAccessCarrierAdmission is the shared admission check plus this edge's own: the
// admission is for this node and epoch, and its carrier certificate chain verifies for the carrier host.
func ValidatePrivateAccessCarrierAdmission(a PrivateAccessCarrierAdmission, nodeID, processEpoch string, now time.Time) error {
	if a.Validate(time.Time{}) != nil {
		return ErrControlUnavailable
	}
	if err := PrivateAccessCarrierDurable(a).Validate(nodeID, processEpoch, now); err != nil {
		return documentFailure{sentinel: ErrControlUnavailable, cause: err}
	}
	carrierEndpoint, _ := url.Parse(a.EdgeEndpoints[0])
	if ValidateCarrierServerCertificateChain(a.EdgeCarrierServerCertificateChainPEM, a.EdgeCarrierServerSPKISHA256, carrierEndpoint.Hostname(), now.UTC()) != nil {
		return ErrControlUnavailable
	}
	return nil
}

func (c *HTTPClient) PrivateAccessCarrierAdmissions(ctx context.Context, nodeID, processEpoch string) ([]PrivateAccessCarrierAdmission, error) {
	request := struct {
		EdgeNodeID       string `json:"edge_node_id"`
		EdgeProcessEpoch string `json:"edge_process_epoch"`
	}{nodeID, processEpoch}
	var out connectorprotocol.PrivateAccessSnapshot
	if err := c.postNodeWithMaximumAndIdentity(ctx, PrivateAccessCarrierAdmissionsPath, nodeID, processEpoch, request, &out, connectorprotocol.MaximumSnapshotBytes); err != nil {
		return nil, err
	}
	if out.ValidateEnvelope() != nil {
		return nil, ErrControlUnavailable
	}
	seen := make(map[string]struct{}, len(out.Admissions))
	for _, admission := range out.Admissions {
		if err := ValidatePrivateAccessCarrierAdmission(admission, nodeID, processEpoch, time.Now().UTC()); err != nil {
			return nil, documentFailure{sentinel: ErrControlUnavailable, cause: err}
		}
		key := admission.MachineID + "\x00" + admission.ResourceID + "\x00" + admission.RouteID + "\x00" + admission.AssignmentID
		if _, ok := seen[key]; ok {
			return nil, ErrControlUnavailable
		}
		seen[key] = struct{}{}
	}
	return out.Admissions, nil
}
