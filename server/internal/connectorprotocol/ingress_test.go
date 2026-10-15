package connectorprotocol

import (
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func ingressFixture(now time.Time) (wire.IngressDecision, wire.StreamOpen) {
	d := wire.IngressDecision{Binding: wire.IngressBinding{
		EnvironmentID: "env-1", AccountID: "account-1", TunnelID: "tunnel-1", Lifecycle: wire.TunnelDurable, ResourceGeneration: 1,
		RouteID: "route-1", RouteGeneration: 1, TargetID: "route-1", TargetGeneration: 1, HostID: "host-1", InstallationGeneration: 1,
		Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.example.test", PathPrefix: "/",
		OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: "route-1", PublicationGeneration: 1},
		DecisionID: "decision-1", PolicyGeneration: 1, EdgeNodeID: "edge-1", EdgeProcessEpoch: "epoch-1234567890123456789012345678", ConnectorID: "connector-1", SessionID: "session-1", ProcessGeneration: 1, ConfigGeneration: 1, AssignmentGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(wire.IngressAuthorityLifetime)}
	o := wire.StreamOpen{Protocol: wire.ProtocolName, Version: wire.ProtocolVersion, AccountID: d.Binding.AccountID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: 1, Generation: 1, RouteID: d.Binding.RouteID, RequestID: "request-1", Kind: "http"}
	return d, o
}
