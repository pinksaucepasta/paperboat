package control

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	"testing"
	"time"
)

func TestIngressAuthoritySelectsExactMatchedAlias(t *testing.T) {
	now := time.Now().UTC()
	decision := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_01", AccountID: "account_01", TunnelID: "tunnel_01", Lifecycle: connectorprotocol.TunnelDurable, ResourceGeneration: 1, RouteID: "route_01", RouteGeneration: 1, TargetID: "target_01", TargetGeneration: 1, HostID: "host_01", InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.customer.test", PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:8080", TLSVerification: "not_applicable", PublicationID: "publication_01", PublicationGeneration: 1}, DecisionID: "decision_01", PolicyGeneration: 1, EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", ConnectorID: "connector_01", SessionID: "session_live_01", ProcessGeneration: 1, ConfigGeneration: 7, AssignmentGeneration: 1, Action: "view", IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(9 * time.Second)}
	managed := decision
	managed.Binding.Hostname = "managed.paperboat.test"
	authority := &IngressAuthority{Client: &HTTPClient{}, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch, fetched: now, decisions: []connectorprotocol.IngressDecision{managed, decision}}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, TunnelID: decision.Binding.TunnelID, AccountID: decision.Binding.AccountID, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration, RouteGeneration: decision.Binding.RouteGeneration, AccessMode: decision.Binding.Audience, ViewerPolicyGeneration: decision.PolicyGeneration, Hostname: decision.Binding.Hostname}
	got, err := authority.Resolve(context.Background(), rule)
	if err != nil || got.Binding.Hostname != rule.Hostname {
		t.Fatalf("matched alias not selected: hostname=%s err=%v", got.Binding.Hostname, err)
	}
	rule.Hostname = "unpublished.customer.test"
	if _, err := authority.Resolve(context.Background(), rule); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("unpublished alias accepted: %v", err)
	}
}
