package control

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const maximumIngressDecisions = 4096

// IngressDecisions returns the current public snapshot and, separately, the public routes the
// server withheld only because their workspace is out of bandwidth allowance.
func (c *HTTPClient) IngressDecisions(ctx context.Context, nodeID, epoch string) ([]connectorprotocol.IngressDecision, []connectorprotocol.IngressQuotaDenial, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return nil, nil, ErrControlInvalid
	}
	input := struct {
		NodeID string `json:"edge_node_id"`
		Epoch  string `json:"process_epoch"`
	}{nodeID, epoch}
	var output struct {
		Complete    bool                                    `json:"complete"`
		Decisions   []connectorprotocol.IngressDecision     `json:"decisions"`
		QuotaDenied []connectorprotocol.IngressQuotaDenial `json:"quota_denied"`
	}
	if err := c.postNodeWithMaximumAndIdentity(ctx, "/v1/edge/ingress/desired-state", nodeID, epoch, input, &output, 16<<20); err != nil {
		return nil, nil, err
	}
	if !output.Complete || len(output.Decisions) > maximumIngressDecisions || len(output.QuotaDenied) > maximumIngressDecisions {
		return nil, nil, ErrControlUnavailable
	}
	now := time.Now().UTC()
	for _, d := range output.Decisions {
		if d.Validate(now) != nil || d.EdgeNodeID != nodeID || d.EdgeProcessEpoch != epoch {
			return nil, nil, ErrControlUnavailable
		}
	}
	return output.Decisions, output.QuotaDenied, nil
}
