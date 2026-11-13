package edgehttp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type expiredPrivateFixture struct {
	registry *PrivateAccessConnectionRegistry
	address  string
	match    route.RouteMatch
	clock    *atomic.Int64 // unix nanoseconds the registry sees
}

// newExpiredPrivateFixture registers an admitted durable private connection and moves the registry's
// clock past its 10 s decision.
func newExpiredPrivateFixture(t *testing.T) expiredPrivateFixture {
	t.Helper()
	now := time.Now().UTC()
	identity := testEdgePreviewIdentity(2, 3)
	id := func(noun string) string { return noun + "_11111111-1111-4111-8111-111111111111" }
	identity.AccountID, identity.TunnelID, identity.ConnectorID, identity.HostID, identity.SessionID = id("user"), id("machine"), id("machine"), id("machine"), id("session")
	request := edgePrivateAccessRequest(now)
	request.AccountID, request.MachineID, request.CarrierSessionID = identity.AccountID, "machine_22222222-2222-4222-8222-222222222222", identity.SessionID
	request.ResourceID, request.RouteID, request.OperationID = id("preview"), id("route"), id("operation")
	request.SessionID, request.IdempotencyKey, request.RequestID, request.CorrelationID = id("session"), id("operation"), id("request"), id("correlation")
	request.ResourceKind, request.ResourceID, request.ConnectorID, request.Audience = "tunnel", identity.TunnelID, identity.ConnectorID, "paperboat-tunnel-http"
	request.ProcessGeneration, request.ConfigGeneration = identity.ProcessGeneration, identity.Generation
	request.SessionGeneration, request.AssignmentGeneration = 1, 1
	request.EdgeProcessEpoch = "epoch_private_preview_12345678"
	rule := replicaRule()
	rule.Generation, rule.Revision = 1, request.RouteGeneration
	rule.Target, rule.ObservedState = request.RouteID, "ready"
	rule.ID, rule.RouteID, rule.AccountID, rule.TunnelID, rule.HostID = request.RouteID, request.RouteID, identity.AccountID, identity.TunnelID, identity.HostID
	rule.ConnectorID, rule.ConnectorSessionID = identity.ConnectorID, identity.SessionID
	rule.ConnectorProcessGeneration, rule.ConfigGeneration, rule.SessionGeneration = identity.ProcessGeneration, identity.Generation, request.SessionGeneration
	rule.RouteGeneration, rule.AssignmentGeneration = request.RouteGeneration, request.AssignmentGeneration
	rule.AccessMode, rule.ResourceKind, rule.Hostname, rule.PathPrefix, rule.MatchType = "private", "tunnel", request.Host, "/", route.MatchExact
	rule.Node, rule.EdgeProcessEpoch = request.EdgeNodeID, request.EdgeProcessEpoch
	evidence := connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed-grant", Request: request}
	decision := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_test", AccountID: request.AccountID, TunnelID: request.ResourceID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: request.RouteID, RouteGeneration: request.RouteGeneration, TargetID: request.RouteID, TargetGeneration: request.RouteGeneration, HostID: identity.HostID, InstallationGeneration: 1, Audience: "private", ConnectionMethod: "edge", Protocol: "http", Hostname: request.Host, PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: request.RouteID, PublicationGeneration: request.RouteGeneration}, DecisionID: "decision_test", PolicyGeneration: 1, EdgeNodeID: request.EdgeNodeID, EdgeProcessEpoch: request.EdgeProcessEpoch, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, AssignmentGeneration: request.AssignmentGeneration, PrincipalID: request.MachineID, GrantID: request.Nonce, GrantGeneration: request.InstallationGeneration, Action: "view", IssuedAt: now, ExpiresAt: now.Add(connectorprotocol.IngressAuthorityLifetime), NativeAuthorization: &evidence}
	registry, _ := NewPrivateAccessConnectionRegistry(8)
	clock := &atomic.Int64{}
	clock.Store(now.UnixNano())
	registry.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	address := "127.0.0.1:41001"
	if _, err := registry.RegisterIngress(address, request, request.ExpiresAt, decision); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(connectorprotocol.IngressAuthorityLifetime + time.Second))
	return expiredPrivateFixture{registry: registry, address: address, match: route.RouteMatch{Rule: rule, Host: request.Host}, clock: clock}
}

func (f expiredPrivateFixture) ask() bool {
	_, ok := f.registry.ingress(context.Background(), f.address, f.match)
	return ok
}

func (f expiredPrivateFixture) refresh(current connectorprotocol.IngressDecision) connectorprotocol.IngressDecision {
	current.IssuedAt = f.registry.now()
	current.ExpiresAt = current.IssuedAt.Add(connectorprotocol.IngressAuthorityLifetime)
	return current
}

func TestPrivateIngressRevalidatesOncePerConcurrentBurst(t *testing.T) {
	f := newExpiredPrivateFixture(t)
	var calls atomic.Int32
	release := make(chan struct{})
	f.registry.Revalidate = func(_ context.Context, current connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		calls.Add(1)
		<-release
		return f.refresh(current), nil
	}
	var wg sync.WaitGroup
	var allowed atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.ask() {
				allowed.Add(1)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 || allowed.Load() != 20 {
		t.Fatalf("calls=%d allowed=%d, want 1 and 20", calls.Load(), allowed.Load())
	}
	if !f.ask() || calls.Load() != 1 {
		t.Fatalf("refreshed decision not kept: calls=%d", calls.Load())
	}
}

func TestPrivateIngressTransportFailureBacksOffThenRecovers(t *testing.T) {
	f := newExpiredPrivateFixture(t)
	var calls atomic.Int32
	var down atomic.Bool
	down.Store(true)
	f.registry.Revalidate = func(_ context.Context, current connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		calls.Add(1)
		if down.Load() {
			return connectorprotocol.IngressDecision{}, errors.New("server unreachable")
		}
		return f.refresh(current), nil
	}
	for range 5 {
		if f.ask() {
			t.Fatal("expired decision allowed while the server is down")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d during the backoff, want 1", calls.Load())
	}
	down.Store(false)
	if f.ask() || calls.Load() != 1 {
		t.Fatalf("asked the server again inside the backoff: calls=%d", calls.Load())
	}
	f.clock.Add(int64(privateRevalidateBackoff + time.Millisecond))
	if !f.ask() || calls.Load() != 2 {
		t.Fatalf("no recovery after the backoff: calls=%d", calls.Load())
	}
}

func TestPrivateIngressExplicitDenyHoldsForTheConnection(t *testing.T) {
	f := newExpiredPrivateFixture(t)
	var calls atomic.Int32
	f.registry.Revalidate = func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		calls.Add(1)
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	f.ask()
	f.clock.Add(int64(privateRevalidateBackoff + time.Millisecond)) // past any transport backoff, inside the connection's lifetime
	if f.ask() || calls.Load() != 1 {
		t.Fatalf("revoked connection asked the server again: calls=%d", calls.Load())
	}
}

func TestPrivateIngressExpiredDecisionWithoutRevalidateIsDenied(t *testing.T) {
	f := newExpiredPrivateFixture(t)
	if f.ask() {
		t.Fatal("expired decision allowed with no way to revalidate it")
	}
}
