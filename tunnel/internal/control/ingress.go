package control

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const maximumIngressDecisions = 4096

// IngressSnapshot is one complete server snapshot for an edge process: the
// public decisions, the routes withheld only for bandwidth quota, and the
// http-01 responses the edge serves for its ACME challenges.
type IngressSnapshot struct {
	Decisions   []connectorprotocol.IngressDecision
	QuotaDenied []connectorprotocol.IngressQuotaDenial
	HTTP01      []connectorprotocol.HTTP01Response
}

// IngressDecisions returns the current snapshot for this edge process.
func (c *HTTPClient) IngressDecisions(ctx context.Context, nodeID, epoch string) (IngressSnapshot, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return IngressSnapshot{}, ErrControlInvalid
	}
	input := struct {
		NodeID string `json:"edge_node_id"`
		Epoch  string `json:"process_epoch"`
	}{nodeID, epoch}
	var output struct {
		Complete        bool                                   `json:"complete"`
		Decisions       []connectorprotocol.IngressDecision    `json:"decisions"`
		QuotaDenied     []connectorprotocol.IngressQuotaDenial `json:"quota_denied"`
		HTTP01Responses []connectorprotocol.HTTP01Response     `json:"http01_responses"`
	}
	if err := c.postNodeWithMaximumAndIdentity(ctx, "/v1/edge/ingress/desired-state", nodeID, epoch, input, &output, 16<<20); err != nil {
		return IngressSnapshot{}, err
	}
	if !output.Complete || len(output.Decisions) > maximumIngressDecisions || len(output.QuotaDenied) > maximumIngressDecisions || len(output.HTTP01Responses) > connectorprotocol.MaxHTTP01Responses {
		return IngressSnapshot{}, ErrControlUnavailable
	}
	now := time.Now().UTC()
	for _, d := range output.Decisions {
		if d.Validate(now) != nil || d.EdgeNodeID != nodeID || d.EdgeProcessEpoch != epoch {
			return IngressSnapshot{}, ErrControlUnavailable
		}
	}
	for _, r := range output.HTTP01Responses {
		if r.Validate() != nil {
			return IngressSnapshot{}, ErrControlUnavailable
		}
	}
	return IngressSnapshot{Decisions: output.Decisions, QuotaDenied: output.QuotaDenied, HTTP01: output.HTTP01Responses}, nil
}
