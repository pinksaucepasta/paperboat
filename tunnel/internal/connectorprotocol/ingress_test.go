package connectorprotocol

import (
	"bytes"
	"testing"
	"time"
)

func ingressFixture(now time.Time) (IngressDecision, StreamOpen) {
	d := IngressDecision{Binding: IngressBinding{
		EnvironmentID: "env-1", AccountID: "account-1", TunnelID: "tunnel-1", Lifecycle: TunnelDurable, ResourceGeneration: 1,
		RouteID: "route-1", RouteGeneration: 1, TargetID: "route-1", TargetGeneration: 1, HostID: "host-1", InstallationGeneration: 1,
		Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.example.test", PathPrefix: "/",
		OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: "route-1", PublicationGeneration: 1},
		DecisionID: "decision-1", PolicyGeneration: 1, EdgeNodeID: "edge-1", EdgeProcessEpoch: "epoch-1234567890123456789012345678", ConnectorID: "connector-1", SessionID: "session-1", ProcessGeneration: 1, ConfigGeneration: 1, AssignmentGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(IngressAuthorityLifetime)}
	o := StreamOpen{Protocol: ProtocolName, Version: ProtocolVersion, AccountID: d.Binding.AccountID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: 1, Generation: 1, RouteID: d.Binding.RouteID, RequestID: "request-1", Kind: "http"}
	return d, o
}

func TestIngressExactIndependentAuthority(t *testing.T) {
	now := time.Now().UTC()
	d, o := ingressFixture(now)
	if err := d.Authorize(d, o, d.EdgeNodeID, d.EdgeProcessEpoch, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*IngressDecision){
		"target":       func(x *IngressDecision) { x.Binding.OriginAddress = "127.0.0.1:3001" },
		"resource":     func(x *IngressDecision) { x.Binding.ResourceGeneration++ },
		"route":        func(x *IngressDecision) { x.Binding.RouteGeneration++ },
		"publication":  func(x *IngressDecision) { x.Binding.PublicationGeneration++ },
		"policy":       func(x *IngressDecision) { x.PolicyGeneration++ },
		"assignment":   func(x *IngressDecision) { x.AssignmentGeneration++ },
		"session":      func(x *IngressDecision) { x.SessionID = "session-other" },
		"host":         func(x *IngressDecision) { x.Binding.HostID = "host-other" },
		"installation": func(x *IngressDecision) { x.Binding.InstallationGeneration++ },
		"edge":         func(x *IngressDecision) { x.EdgeProcessEpoch = "epoch-other123456789012345678901234" },
		"lifecycle":    func(x *IngressDecision) { x.Binding.Lifecycle = TunnelEphemeral },
	} {
		t.Run(name, func(t *testing.T) {
			current := d
			mutate(&current)
			if d.Authorize(current, o, d.EdgeNodeID, d.EdgeProcessEpoch, now) == nil {
				t.Fatal("stale authority accepted")
			}
		})
	}
	if d.Validate(d.ExpiresAt) == nil {
		t.Fatal("expiry accepted")
	}
	d.Binding.Audience = "private"
	if d.Validate(now) == nil {
		t.Fatal("publication became viewer grant")
	}
}

func TestIngressStrictWireAndTCPBinding(t *testing.T) {
	now := time.Now().UTC()
	d, _ := ingressFixture(now)
	var wire bytes.Buffer
	if err := WriteIngressDecision(&wire, d, now); err != nil {
		t.Fatal(err)
	}
	got, err := ReadIngressDecision(&wire, now)
	if err != nil || got.Binding != d.Binding {
		t.Fatalf("roundtrip: %v", err)
	}
	d.Binding.Protocol = "tcp"
	d.Binding.OriginScheme = "tcp"
	d.Binding.PathPrefix = ""
	if d.Validate(now) == nil {
		t.Fatal("TCP without allocated listener accepted")
	}
	d.Binding.ListenerID = "listener-1"
	d.Binding.PublicPort = 5432
	if d.Validate(now) != nil {
		t.Fatal("valid opaque publication denied")
	}
	d.Binding.OriginAddress = "169.254.169.254:80"
	if d.Validate(now) == nil {
		t.Fatal("nonloopback target accepted")
	}
}

func TestIngressNativeEvidenceExactPublisherAndAccessor(t *testing.T) {
	now := time.Now().UTC()
	d, open := ingressFixture(now)
	d.Binding.Audience = "private"
	d.PrincipalID, d.GrantID, d.GrantGeneration = "accessor-1", "nonce-1", 3
	q := PrivateAccessRequest{AccountID: d.Binding.AccountID, ResourceKind: "tunnel", ResourceID: d.Binding.TunnelID, RouteID: d.Binding.RouteID, Audience: "paperboat-tunnel-http", MachineID: d.PrincipalID, SessionID: "accessor-install-1", InstallationGeneration: 3, ExpiresAt: now.Add(time.Minute), Nonce: d.GrantID, ConnectorID: d.ConnectorID, CarrierSessionID: d.SessionID, RouteGeneration: d.Binding.RouteGeneration, SessionGeneration: 1, ProcessGeneration: d.ProcessGeneration, ConfigGeneration: d.ConfigGeneration, AssignmentGeneration: d.AssignmentGeneration, EdgeNodeID: d.EdgeNodeID, EdgeProcessEpoch: d.EdgeProcessEpoch, Protocol: "http", Method: "CONNECT", Host: d.Binding.Hostname, Path: "/", IdempotencyKey: "operation-1", RequestID: "request-1", CorrelationID: "correlation-1"}
	d.NativeAuthorization = &PrivateAccessOpen{Schema: PrivateAccessSchema, Kind: PrivateAccessKind, Grant: "signed-native-grant", Request: q}
	if d.Binding.HostID == q.MachineID || d.Authorize(d, open, d.EdgeNodeID, d.EdgeProcessEpoch, now) != nil {
		t.Fatal("independent accessor/publisher denied")
	}
	var wire bytes.Buffer
	if err := WriteIngressDecision(&wire, d, now); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadIngressDecision(&wire, now)
	if err != nil || decoded.NativeAuthorization == nil || *decoded.NativeAuthorization != *d.NativeAuthorization {
		t.Fatalf("native evidence roundtrip: %v", err)
	}
	for _, field := range []string{"grant", "hash", "accessor-install", "route", "config", "assignment", "node", "expired", "removed"} {
		t.Run(field, func(t *testing.T) {
			current := d
			evidence := *d.NativeAuthorization
			current.NativeAuthorization = &evidence
			switch field {
			case "grant":
				evidence.Grant = "other-grant"
			case "hash":
				evidence.Request.Path = "/other"
			case "accessor-install":
				evidence.Request.InstallationGeneration++
			case "route":
				current.Binding.RouteGeneration++
			case "config":
				current.ConfigGeneration++
			case "assignment":
				current.AssignmentGeneration++
			case "node":
				current.EdgeProcessEpoch = "epoch_other12345678901234567890"
			case "expired":
				evidence.Request.ExpiresAt = now
			case "removed":
				current.NativeAuthorization = nil
			}
			if d.Authorize(current, open, d.EdgeNodeID, d.EdgeProcessEpoch, now) == nil {
				t.Fatal("changed native authority admitted")
			}
		})
	}
}
