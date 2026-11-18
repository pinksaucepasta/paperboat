package control

import (
	"context"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
	"strings"
	"time"

	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const maximumIngressDecisions = 4096

// IngressSnapshot is one complete server snapshot for an edge process: the
// public decisions, the routes whose workspace is throttled, and the
// http-01 responses the edge serves for its ACME challenges.
type IngressSnapshot struct {
	Decisions []connectorprotocol.IngressDecision
	Throttled []connectorprotocol.IngressThrottle
	Blocked   []connectorprotocol.IngressBlock
	IPRules   []connectorprotocol.IngressIPRule
	Offline   []connectorprotocol.IngressOffline
	HTTP01    []connectorprotocol.HTTP01Response
	// Version identifies this answer. Unchanged marks an answer to a request that carried the
	// current version: it holds only IssuedAt and Expiries (one per kept decision, in order).
	Version   string
	Unchanged bool
	IssuedAt  time.Time
	Expiries  []time.Time
}

const ingressDecisionsPath = "/v1/edge/ingress/desired-state"

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
		Version         string                              `json:"version"`
		Unchanged       bool                                `json:"unchanged"`
		IssuedAt        time.Time                           `json:"issued_at"`
		Expiries        []time.Time                         `json:"expires_at"`
		Complete        bool                                `json:"complete"`
		Decisions       []connectorprotocol.IngressDecision `json:"decisions"`
		Throttled       []connectorprotocol.IngressThrottle `json:"throttled"`
		Blocked         []connectorprotocol.IngressBlock    `json:"blocked"`
		IPRules         []connectorprotocol.IngressIPRule   `json:"ip_rules"`
		Offline         []connectorprotocol.IngressOffline  `json:"offline"`
		HTTP01Responses []connectorprotocol.HTTP01Response  `json:"http01_responses"`
	}
	if err := c.postNodeWithMaximumAndIdentity(ctx, ingressDecisionsPath, nodeID, epoch, input, &output, 16<<20); err != nil {
		return IngressSnapshot{}, err
	}
	if output.Unchanged {
		if !output.Complete || version == "" || output.Version != version || len(output.Expiries) > maximumIngressDecisions {
			return IngressSnapshot{}, ErrControlUnavailable
		}
		return IngressSnapshot{Version: version, Unchanged: true, IssuedAt: output.IssuedAt, Expiries: output.Expiries}, nil
	}
	if !output.Complete || len(output.Decisions) > maximumIngressDecisions || len(output.Throttled) > maximumIngressDecisions || len(output.Blocked) > maximumIngressDecisions || len(output.IPRules) > maximumIngressDecisions || len(output.Offline) > maximumIngressDecisions || len(output.HTTP01Responses) > connectorprotocol.MaxHTTP01Responses {
		return IngressSnapshot{}, ErrControlUnavailable
	}
	// A decision this edge cannot accept refuses only its own name. Rejecting the whole snapshot would
	// freeze every other name on the last good one (2026-11-11: one file-origin preview the edge did not
	// know made the test host 404 every name created after it). Prior art: tailscale's netmap drops one invalid
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
		edgetelemetry.Log(ctx, sharedtelemetry.SeverityWarn, "ingress_decisions_rejected", sharedtelemetry.OutcomeRejected, "count", len(rejected), "route_ids", strings.Join(rejected[:min(len(rejected), 8)], ","))
	}
	// A throttle entry without a hostname or workspace would merge unrelated routes into one bucket: dropped.
	throttled := output.Throttled[:0]
	for _, t := range output.Throttled {
		if t.Hostname != "" && t.Workspace != "" && len(t.Hostname) <= 253 && len(t.Workspace) <= 256 {
			throttled = append(throttled, t)
		}
	}
	output.Throttled = throttled
	for _, r := range output.HTTP01Responses {
		if r.Validate() != nil {
			return IngressSnapshot{}, ErrControlUnavailable
		}
	}
	return IngressSnapshot{Decisions: valid, Throttled: output.Throttled, Blocked: output.Blocked, IPRules: output.IPRules, Offline: output.Offline, HTTP01: output.HTTP01Responses, Version: output.Version}, nil
}
