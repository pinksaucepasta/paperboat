package control

import (
	"context"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"net/url"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
)

func ExpectedRuntimeAdmission(a runtimewire.Admission, nodeID, epoch string, now time.Time) (datacarrier.ExpectedAdmission, error) {
	b := a.Binding
	endpoint := a.RouteKind == runtimewire.EndpointPublicRouteKind || a.RouteKind == runtimewire.EndpointRestrictedRouteKind
	if endpoint != (a.EndpointID != "") || endpoint != (a.AccessMode != "") {
		return datacarrier.ExpectedAdmission{}, ErrControlInvalid
	}
	if a.Schema != runtimewire.Schema || (a.RouteKind != runtimewire.RouteKind && !endpoint) || b.EdgeProcessEpoch != epoch || a.RouteRevision != b.RouteGeneration || len(a.EdgeEndpoints) != 2 {
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
	if endpoint {
		value.PreviewID, value.OperationID, value.AccessMode = a.EndpointID, b.RouteID, a.AccessMode
	}
	if err := value.Validate(now, nodeID); err != nil {
		return datacarrier.ExpectedAdmission{}, err
	}
	return value, nil
}

type RuntimeCarrierSource interface {
	RuntimeCarrierAdmissions(context.Context, string, string) ([]runtimewire.Admission, error)
	EndpointAdmissions(context.Context, string, string) ([]runtimewire.Admission, error)
	ObserveRuntimeCarriers(context.Context, string, string, []runtimewire.Observation) error
}

func (c *HTTPClient) RuntimeCarrierAdmissions(ctx context.Context, nodeID, epoch string) ([]runtimewire.Admission, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return nil, ErrControlInvalid
	}
	var result runtimewire.Snapshot
	if err := c.postNodeWithMaximumAndIdentity(ctx, runtimewire.EdgePullPath, nodeID, epoch, runtimewire.EdgeRequest{NodeID: nodeID, ProcessEpoch: epoch, Capabilities: []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}}, &result, maxPreviewCarrierSnapshotBytes); err != nil {
		return nil, err
	}
	if result.Schema != runtimewire.Schema || !result.Complete || result.Admissions == nil || len(*result.Admissions) > runtimewire.MaximumAdmissions {
		return nil, ErrControlUnavailable
	}
	now := time.Now().UTC()
	for _, a := range *result.Admissions {
		if _, err := ExpectedRuntimeAdmission(a, nodeID, epoch, now); err != nil {
			return nil, documentFailure{sentinel: ErrControlUnavailable, cause: err}
		}
	}
	return *result.Admissions, nil
}

// EndpointAdmissions pulls only the endpoint routes of the edge's runtime snapshot (PLAN 3a).
func (c *HTTPClient) EndpointAdmissions(ctx context.Context, nodeID, epoch string) ([]runtimewire.Admission, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return nil, ErrControlInvalid
	}
	var result runtimewire.Snapshot
	if err := c.postNodeWithMaximumAndIdentity(ctx, runtimewire.EdgeEndpointPullPath, nodeID, epoch, runtimewire.EdgeRequest{NodeID: nodeID, ProcessEpoch: epoch, Capabilities: []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}}, &result, maxPreviewCarrierSnapshotBytes); err != nil {
		return nil, err
	}
	if result.Schema != runtimewire.Schema || !result.Complete || result.Admissions == nil || len(*result.Admissions) > runtimewire.MaximumAdmissions {
		return nil, ErrControlUnavailable
	}
	now := time.Now().UTC()
	for _, a := range *result.Admissions {
		if a.EndpointID == "" {
			return nil, documentFailure{sentinel: ErrControlUnavailable, cause: ErrControlInvalid}
		}
		if _, err := ExpectedRuntimeAdmission(a, nodeID, epoch, now); err != nil {
			return nil, documentFailure{sentinel: ErrControlUnavailable, cause: err}
		}
	}
	return *result.Admissions, nil
}

func (c *HTTPClient) ObserveRuntimeCarriers(ctx context.Context, nodeID, epoch string, observations []runtimewire.Observation) error {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil || len(observations) > runtimewire.MaximumAdmissions {
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
	return c.postNodeWithMaximumAndIdentity(ctx, runtimewire.EdgeObservePath, nodeID, epoch, runtimewire.EdgeRequest{NodeID: nodeID, ProcessEpoch: epoch, Observations: observations}, nil, maxPreviewCarrierSnapshotBytes)
}
