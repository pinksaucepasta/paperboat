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
	// EndpointAdmissions returns the full endpoint set when since is empty, otherwise what changed
	// since that cursor.
	EndpointAdmissions(ctx context.Context, nodeID, epoch, since string) (EndpointUpdate, error)
	ObserveRuntimeCarriers(context.Context, string, string, []runtimewire.Observation) error
}

func (c *HTTPClient) RuntimeCarrierAdmissions(ctx context.Context, nodeID, epoch string) ([]runtimewire.Admission, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return nil, ErrControlInvalid
	}
	var result runtimewire.Snapshot
	if err := c.postNodeWithMaximumAndIdentity(ctx, runtimewire.EdgePullPath, nodeID, epoch, runtimewire.EdgeRequest{NodeID: nodeID, ProcessEpoch: epoch, Capabilities: edgeRuntimeCapabilities}, &result, maxPreviewCarrierSnapshotBytes); err != nil {
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

// EndpointUpdate is one endpoint pull: the full set (Complete) or the changes since the cursor sent.
// Every endpoint admission kept after it expires at ExpiresAt; Cursor is sent as since next time.
type EndpointUpdate struct {
	Complete   bool
	Admissions []runtimewire.Admission
	Removed    []string
	Cursor     string
	ExpiresAt  time.Time
}

// EndpointAdmissions pulls the endpoint routes of the edge's runtime snapshot (PLAN 3a), as deltas
// after the first full set (Lane W scale matrix). The server sends them grouped by carrier in pages;
// all pages of one answer are read here, and the first page's cursor is the answer's cursor.
func (c *HTTPClient) EndpointAdmissions(ctx context.Context, nodeID, epoch, since string) (EndpointUpdate, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil || len(since) > 64 {
		return EndpointUpdate{}, ErrControlInvalid
	}
	var update EndpointUpdate
	now := time.Now().UTC()
	page := ""
	for pages := 0; ; pages++ {
		if pages == maximumEndpointPages {
			return EndpointUpdate{}, ErrControlUnavailable
		}
		var result runtimewire.Snapshot
		request := runtimewire.EdgeRequest{NodeID: nodeID, ProcessEpoch: epoch, Capabilities: edgeRuntimeCapabilities, Since: since, Page: page}
		if err := c.postNodeWithMaximumAndIdentity(ctx, runtimewire.EdgeEndpointPullPath, nodeID, epoch, request, &result, maxPreviewCarrierSnapshotBytes); err != nil {
			return EndpointUpdate{}, err
		}
		if result.Schema != runtimewire.Schema || result.Carriers == nil || result.Cursor == "" || len(result.Cursor) > 64 || len(result.Next) > 1024 {
			return EndpointUpdate{}, ErrControlUnavailable
		}
		if pages == 0 {
			update.Complete, update.Cursor = result.Complete, result.Cursor
		} else if result.Complete != update.Complete {
			return EndpointUpdate{}, ErrControlUnavailable
		}
		if !result.Complete {
			if result.ExpiresAt == nil || !result.ExpiresAt.After(now) {
				return EndpointUpdate{}, ErrControlUnavailable
			}
			update.ExpiresAt = result.ExpiresAt.UTC()
		} else if len(result.RemovedRouteIDs) != 0 {
			return EndpointUpdate{}, ErrControlUnavailable
		}
		for _, id := range result.RemovedRouteIDs {
			if connectorprotocol.ValidateIdentifier(id) != nil {
				return EndpointUpdate{}, documentFailure{sentinel: ErrControlUnavailable, cause: ErrControlInvalid}
			}
		}
		update.Removed = append(update.Removed, result.RemovedRouteIDs...)
		for _, carrier := range *result.Carriers {
			for _, a := range carrier.Admissions() {
				if a.EndpointID == "" {
					return EndpointUpdate{}, documentFailure{sentinel: ErrControlUnavailable, cause: ErrControlInvalid}
				}
				if _, err := ExpectedRuntimeAdmission(a, nodeID, epoch, now); err != nil {
					return EndpointUpdate{}, documentFailure{sentinel: ErrControlUnavailable, cause: err}
				}
				update.Admissions = append(update.Admissions, a)
			}
		}
		if len(update.Admissions)+len(update.Removed) > runtimewire.MaximumAdmissions {
			return EndpointUpdate{}, ErrControlUnavailable
		}
		if page = result.Next; page == "" {
			return update, nil
		}
	}
}

// maximumEndpointPages bounds one paged endpoint answer (MaximumAdmissions over the server's pages).
const maximumEndpointPages = 64

// edgeRuntimeCapabilities is what this edge advertises on its runtime and endpoint pulls.
var edgeRuntimeCapabilities = []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints, runtimewire.CapabilityEndpointDeltas}

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
