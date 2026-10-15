package privateaccess

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
)

// AuthorizeIngress rechecks the signed accessor grant and projects the exact
// current publisher route. The host invokes this independently on refresh.
func (s *Service) AuthorizeIngress(ctx context.Context, input AuthorizeInput) (Decision, error) {
	decision, err := s.Authorize(ctx, input)
	if err != nil || !decision.Allowed {
		return decision, err
	}
	if (input.Request.ResourceKind != ResourceTunnel && input.Request.ResourceKind != ResourcePreview) || input.Request.Protocol != ProtocolHTTP {
		return decision, nil
	}
	if s.ingress == nil {
		return Decision{}, ErrIdentityUnavailable
	}
	evidence := connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: input.Grant, Request: connectorprotocol.PrivateAccessRequest(input.Request)}
	var current connectorprotocol.IngressDecision
	if input.Request.ResourceKind == ResourcePreview {
		current, err = s.ingress.RestrictedPreview(ctx, input.Edge.NodeID, input.Edge.ProcessEpoch, evidence)
	} else {
		current, err = s.ingress.RestrictedRoute(ctx, input.Edge.NodeID, input.Edge.ProcessEpoch, "private", input.Request.RouteID, input.Request.Host)
	}
	if err != nil {
		if pureExpectedPrivateAccessError(err, connectorprotocol.ErrIngressDenied) {
			return s.deny(ctx, input, ReasonWrongRoute, s.clock().UTC(), "ingress")
		}
		return Decision{}, err
	}
	current, err = composeNativeIngress(current, evidence, decision, s.clock().UTC(), input.AdmittedDecisionID != "")
	if err != nil {
		if pureExpectedPrivateAccessError(err, connectorprotocol.ErrIngressDenied) {
			return s.deny(ctx, input, ReasonWrongRoute, s.clock().UTC(), "ingress")
		}
		return Decision{}, err
	}
	decision.Ingress = &current
	return decision, nil
}

func composeNativeIngress(current connectorprotocol.IngressDecision, evidence connectorprotocol.PrivateAccessOpen, decision Decision, now time.Time, admitted bool) (connectorprotocol.IngressDecision, error) {
	q := evidence.Request
	current.NativeAuthorization = &evidence
	current.PrincipalID, current.GrantID, current.GrantGeneration, current.MembershipGeneration = q.MachineID, q.Nonce, q.InstallationGeneration, 0
	current.IssuedAt = now
	current.DecisionID = decision.DecisionID
	current.ExpiresAt = minTime(current.ExpiresAt, decision.ExpiresAt, q.ExpiresAt, now.Add(connectorprotocol.IngressAuthorityLifetime))
	if admitted {
		current.ExpiresAt = minTime(decision.ExpiresAt, now.Add(connectorprotocol.IngressAuthorityLifetime))
		open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: current.Binding.AccountID, TunnelID: current.Binding.TunnelID, ConnectorID: current.ConnectorID, SessionID: current.SessionID, ProcessGeneration: current.ProcessGeneration, Generation: current.ConfigGeneration, RouteID: current.Binding.RouteID, RequestID: q.RequestID, Kind: "http_browser"}
		if current.Revalidate(current, open, current.EdgeNodeID, current.EdgeProcessEpoch, now) != nil {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		return current, nil
	}
	if current.Validate(now) != nil {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	return current, nil
}
