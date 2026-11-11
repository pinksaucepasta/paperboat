package control

import (
	"context"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	edgetelemetry "github.com/pinksaucepasta/paperboat/tunnel/internal/telemetry"
)

const maximumIngressDecisions = 4096

// IngressSnapshot is one complete server snapshot for an edge process: the
// public decisions, the routes withheld only for bandwidth quota, and the
// http-01 responses the edge serves for its ACME challenges.
type IngressSnapshot struct {
	Decisions   []connectorprotocol.IngressDecision
	QuotaDenied []connectorprotocol.IngressQuotaDenial
	Blocked     []connectorprotocol.IngressBlock
	IPRules     []connectorprotocol.IngressIPRule
	HTTP01      []connectorprotocol.HTTP01Response
	// Version identifies this answer. Unchanged marks an answer to a request that carried the
	// current version: it holds only IssuedAt and Expiries (one per kept decision, in order).
	Version   string
	Unchanged bool
	IssuedAt  time.Time
	Expiries  []time.Time
}

// IngressDecisions returns the current snapshot for this edge process; a non-empty version is the
// one the edge holds (F21-38).
func (c *HTTPClient) IngressDecisions(ctx context.Context, nodeID, epoch, version string) (IngressSnapshot, error) {
	if connectorprotocol.ValidateIdentifier(nodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(epoch) != nil {
		return IngressSnapshot{}, ErrControlInvalid
	}
	input := struct {
		NodeID  string `json:"edge_node_id"`
		Epoch   string `json:"process_epoch"`
		Version string `json:"version,omitempty"`
	}{nodeID, epoch, version}
	var output struct {
		Version         string                                 `json:"version"`
		Unchanged       bool                                   `json:"unchanged"`
		IssuedAt        time.Time                              `json:"issued_at"`
		Expiries        []time.Time                            `json:"expires_at"`
		Complete        bool                                   `json:"complete"`
		Decisions       []connectorprotocol.IngressDecision    `json:"decisions"`
		QuotaDenied     []connectorprotocol.IngressQuotaDenial `json:"quota_denied"`
		Blocked         []connectorprotocol.IngressBlock       `json:"blocked"`
		IPRules         []connectorprotocol.IngressIPRule      `json:"ip_rules"`
		HTTP01Responses []connectorprotocol.HTTP01Response     `json:"http01_responses"`
	}
	if err := c.postNodeWithMaximumAndIdentity(ctx, "/v1/edge/ingress/desired-state", nodeID, epoch, input, &output, 16<<20); err != nil {
		return IngressSnapshot{}, err
	}
	if output.Unchanged {
		if !output.Complete || version == "" || output.Version != version || len(output.Expiries) > maximumIngressDecisions {
			return IngressSnapshot{}, ErrControlUnavailable
		}
		return IngressSnapshot{Version: version, Unchanged: true, IssuedAt: output.IssuedAt, Expiries: output.Expiries}, nil
	}
	if !output.Complete || len(output.Decisions) > maximumIngressDecisions || len(output.QuotaDenied) > maximumIngressDecisions || len(output.Blocked) > maximumIngressDecisions || len(output.IPRules) > maximumIngressDecisions || len(output.HTTP01Responses) > connectorprotocol.MaxHTTP01Responses {
		return IngressSnapshot{}, ErrControlUnavailable
	}
	// A decision this edge cannot accept refuses only its own name. Rejecting the whole snapshot would
	// freeze every other name on the last good one (2026-11-11: one file-origin preview the edge did not
	// know made anvit 404 every name created after it). Prior art: tailscale's netmap drops one invalid
	// peer, not the map (ipn/ipnlocal).
	now := time.Now().UTC()
	valid := output.Decisions[:0]
	var rejected []string
	for _, d := range output.Decisions {
		if d.Validate(now) != nil || d.EdgeNodeID != nodeID || d.EdgeProcessEpoch != epoch {
			rejected = append(rejected, d.Binding.RouteID)
			continue
		}
		valid = append(valid, d)
	}
	if len(rejected) > 0 {
		// The server's version covers the rejected decisions too; holding it would let an "unchanged"
		// answer's expiries pair with the wrong decisions, so the next refresh fetches the whole set.
		output.Version = ""
		edgetelemetry.Log(ctx, edgetelemetry.SeverityWarn, "ingress_decisions_rejected", edgetelemetry.OutcomeRejected, "count", len(rejected), "route_ids", strings.Join(rejected[:min(len(rejected), 8)], ","))
	}
	for _, r := range output.HTTP01Responses {
		if r.Validate() != nil {
			return IngressSnapshot{}, ErrControlUnavailable
		}
	}
	return IngressSnapshot{Decisions: valid, QuotaDenied: output.QuotaDenied, Blocked: output.Blocked, IPRules: output.IPRules, HTTP01: output.HTTP01Responses, Version: output.Version}, nil
}
