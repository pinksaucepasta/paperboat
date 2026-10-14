package edgehttp

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

func (r *DataCarrierRouteRegistry) ingressDecision(ctx context.Context, rule route.RouteRule, open connectorprotocol.StreamOpen) (*connectorprotocol.IngressDecision, error) {
	if rule.AccessMode == "private" {
		if d, ok := ctx.Value(privateIngressDecisionKey{}).(connectorprotocol.IngressDecision); ok {
			if !nativeIngressMatches(d, rule) || d.Authorize(d, open, rule.Node, rule.EdgeProcessEpoch, time.Now().UTC()) != nil {
				return nil, connectorprotocol.ErrIngressDenied
			}
			return &d, nil
		}
	}
	if rule.AccessMode == "private" || rule.AccessMode == "team" {
		d, ok := ctx.Value(browserDecisionKey{}).(connectorprotocol.IngressDecision)
		if !ok || !browserDecisionMatches(d, route.RouteMatch{Rule: rule, Host: rule.Hostname}) || d.Authorize(d, open, rule.Node, rule.EdgeProcessEpoch, time.Now().UTC()) != nil {
			return nil, connectorprotocol.ErrIngressDenied
		}
		return &d, nil
	}
	if rule.AccessMode != "public" {
		return nil, connectorprotocol.ErrIngressDenied
	}
	if r.ingressAuthority == nil {
		return nil, nil
	}
	d, err := r.ingressAuthority(ctx, rule)
	if err != nil {
		return nil, err
	}
	if d.Authorize(d, open, rule.Node, rule.EdgeProcessEpoch, time.Now().UTC()) != nil || d.Binding.Audience != rule.AccessMode || d.PolicyGeneration != rule.ViewerPolicyGeneration {
		return nil, connectorprotocol.ErrIngressDenied
	}
	return &d, nil
}

func nativeIngressMatches(d connectorprotocol.IngressDecision, rule route.RouteRule) bool {
	if d.Validate(time.Now().UTC()) != nil || d.NativeAuthorization == nil || rule.AccessMode != "private" || rule.Protocol != "http" || d.PolicyGeneration != rule.ViewerPolicyGeneration {
		return false
	}
	return privateAccessRouteMatches(d.NativeAuthorization.Request, route.RouteMatch{Rule: rule, Host: rule.Hostname}) && d.Binding.HostID == rule.HostID
}
