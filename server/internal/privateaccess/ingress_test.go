package privateaccess

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"syscall"
	"testing"
	"time"
)

type nativeRouteSource struct {
	decision connectorprotocol.IngressDecision
	err      error
	calls    int
}

func (s *nativeRouteSource) RestrictedRoute(_ context.Context, node, epoch, audience, route, host string) (connectorprotocol.IngressDecision, error) {
	s.calls++
	d := s.decision
	if s.err != nil {
		return d, s.err
	}
	if node != d.EdgeNodeID || epoch != d.EdgeProcessEpoch || audience != "private" || route != d.Binding.RouteID || host != d.Binding.Hostname {
		return d, connectorprotocol.ErrIngressDenied
	}
	return d, nil
}
func TestNativeIngressRechecksAccessorAndPublisherOnInitialAndRefresh(t *testing.T) {
	testNativeIngress(t, false)
}
func TestNativePreviewIngressRechecksAccessorAndPublisher(t *testing.T) { testNativeIngress(t, true) }
func testNativeIngress(t *testing.T, preview bool) {
	now := testClock()
	q := testRequest(now)
	q.ResourceKind, q.ResourceID, q.ConnectorID, q.Audience = ResourceTunnel, "tunnel_1", "connector_1", AudienceTunnelHTTP
	q.EdgeProcessEpoch = "epoch_12345678901234567890"
	b := testBinding(now)
	b.ResourceKind, b.ResourceID, b.ConnectorID, b.OwnerMachineID, b.EdgeProcessEpoch = ResourceTunnel, q.ResourceID, q.ConnectorID, "publisher_1", q.EdgeProcessEpoch
	d := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_1", AccountID: q.AccountID, TunnelID: q.ResourceID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: q.RouteID, RouteGeneration: q.RouteGeneration, TargetID: q.RouteID, TargetGeneration: q.RouteGeneration, HostID: b.OwnerMachineID, InstallationGeneration: 9, Audience: "private", ConnectionMethod: "edge", Protocol: "http", Hostname: q.Host, PathPrefix: "/api", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: q.RouteID, PublicationGeneration: q.RouteGeneration}, DecisionID: "decision_1", PolicyGeneration: 1, EdgeNodeID: q.EdgeNodeID, EdgeProcessEpoch: q.EdgeProcessEpoch, ConnectorID: q.ConnectorID, SessionID: q.CarrierSessionID, ProcessGeneration: q.ProcessGeneration, ConfigGeneration: q.ConfigGeneration, AssignmentGeneration: q.AssignmentGeneration, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	if preview {
		q.ResourceKind = ResourcePreview
		q.ResourceID = "preview_1"
		q.ConnectorID = ""
		q.Audience = AudiencePreviewHTTP
		b.ResourceKind = ResourcePreview
		b.ResourceID = q.ResourceID
		b.ConnectorID = ""
		d.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
		d.Binding.PublicationID = q.ResourceID
		d.ConnectorID = "publisher_connector"
	}
	source := &nativeRouteSource{decision: d}
	state := &fakeMachineState{}
	provider, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := NewMintGrantIssuer(provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	token, err := grants.MintGrant(t.Context(), q, now)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{binding: b}
	service, err := NewService(resolver, nil, Config{Now: func() time.Time { return now }, GrantVerifier: grants, MachineState: state, IngressSource: source})
	if err != nil {
		t.Fatal(err)
	}
	var admittedID string
	input := AuthorizeInput{Grant: token, Request: q, Edge: EdgeIdentity{NodeID: q.EdgeNodeID, ProcessEpoch: q.EdgeProcessEpoch}}
	for _, stage := range []string{"initial", "refresh"} {
		t.Run(stage, func(t *testing.T) {
			got, err := service.AuthorizeIngress(context.Background(), input)
			if err != nil || got.Ingress == nil {
				t.Fatalf("authorize: %v", err)
			}
			admittedID = got.DecisionID
			current := *got.Ingress
			if current.NativeAuthorization == nil || current.NativeAuthorization.Grant != input.Grant || current.NativeAuthorization.Request != connectorprotocol.PrivateAccessRequest(q) || current.Binding.HostID == q.MachineID || current.Binding.InstallationGeneration == q.InstallationGeneration {
				t.Fatal("publisher and accessor authority conflated")
			}
			state.err = newDenied(ReasonIdentityInvalid)
			if _, err = service.AuthorizeIngress(context.Background(), input); err == nil {
				t.Fatal("revoked accessor admitted")
			}
			state.err = nil
			changed := input
			changed.Request.Path = "/other"
			if _, err = service.AuthorizeIngress(context.Background(), changed); err == nil {
				t.Fatal("altered signed request admitted")
			}
			source.decision.ConfigGeneration++
			if _, err = service.AuthorizeIngress(context.Background(), input); err == nil {
				t.Fatal("changed publisher carrier admitted")
			}
			source.decision = d
			saved := now
			now = q.ExpiresAt
			if _, err = service.AuthorizeIngress(context.Background(), input); err == nil {
				t.Fatal("expired signed request admitted")
			}
			now = saved
		})
	}
	now = q.ExpiresAt.Add(time.Second)
	source.decision.IssuedAt, source.decision.ExpiresAt = now, now.Add(10*time.Second)
	resolver.binding.ExpiresAt = now.Add(time.Minute)
	input.AdmittedDecisionID = admittedID
	if got, err := service.AuthorizeIngress(t.Context(), input); err != nil || got.Ingress == nil {
		t.Fatalf("admitted grant lost after TTL: %v", err)
	}
	state.err = newDenied(ReasonMachineRevoked)
	if _, err := service.AuthorizeIngress(t.Context(), input); err == nil {
		t.Fatal("revoked accessor retained admitted private connection")
	}
	state.err = nil
	source.err = connectorprotocol.ErrIngressDenied
	if _, err := service.AuthorizeIngress(t.Context(), input); err == nil {
		t.Fatal("revoked publisher retained admitted private connection")
	}
	source.err = nil
	input.AdmittedDecisionID = "decision_other"
	if _, err := service.AuthorizeIngress(t.Context(), input); err == nil {
		t.Fatal("foreign admission continuation accepted")
	}

	if state.calls < 4 || resolver.calls < 4 || source.calls < 4 {
		t.Fatal("refresh skipped independent authorization")
	}
}

func (s *nativeRouteSource) RestrictedPreview(ctx context.Context, node, epoch string, evidence connectorprotocol.PrivateAccessOpen) (connectorprotocol.IngressDecision, error) {
	return s.RestrictedRoute(ctx, node, epoch, "private", evidence.Request.RouteID, evidence.Request.Host)
}

type nativeIngressErrorFixture struct {
	service       *Service
	input         AuthorizeInput
	source        *nativeRouteSource
	validDecision connectorprotocol.IngressDecision
	audit         *privateAccessAuditRecorder
}

func newNativeIngressErrorFixture(t *testing.T, sourceErr error) nativeIngressErrorFixture {
	t.Helper()
	now := testClock()
	q := testRequest(now)
	q.ResourceKind, q.ResourceID, q.ConnectorID, q.Audience = ResourceTunnel, "tunnel_1", "connector_1", AudienceTunnelHTTP
	q.EdgeProcessEpoch = "epoch_12345678901234567890"
	b := testBinding(now)
	b.ResourceKind, b.ResourceID, b.ConnectorID, b.OwnerMachineID, b.EdgeProcessEpoch = ResourceTunnel, q.ResourceID, q.ConnectorID, "publisher_1", q.EdgeProcessEpoch
	d := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_1", AccountID: q.AccountID, TunnelID: q.ResourceID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: q.RouteID, RouteGeneration: q.RouteGeneration, TargetID: q.RouteID, TargetGeneration: q.RouteGeneration, HostID: b.OwnerMachineID, InstallationGeneration: 9, Audience: "private", ConnectionMethod: "edge", Protocol: "http", Hostname: q.Host, PathPrefix: "/api", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: q.RouteID, PublicationGeneration: q.RouteGeneration}, DecisionID: "decision_1", PolicyGeneration: 1, EdgeNodeID: q.EdgeNodeID, EdgeProcessEpoch: q.EdgeProcessEpoch, ConnectorID: q.ConnectorID, SessionID: q.CarrierSessionID, ProcessGeneration: q.ProcessGeneration, ConfigGeneration: q.ConfigGeneration, AssignmentGeneration: q.AssignmentGeneration, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	grants := &fakeGrant{request: q, token: "signed_grant"}
	state := &fakeMachineState{}
	resolver := &fakeResolver{binding: b}
	audit := &privateAccessAuditRecorder{}
	source := &nativeRouteSource{decision: d, err: sourceErr}
	service, err := NewService(resolver, audit, Config{Now: func() time.Time { return now }, GrantVerifier: grants, MachineState: state, IngressSource: source})
	if err != nil {
		t.Fatal(err)
	}
	return nativeIngressErrorFixture{service: service, input: AuthorizeInput{Grant: grants.token, Request: q, Edge: EdgeIdentity{NodeID: q.EdgeNodeID, ProcessEpoch: q.EdgeProcessEpoch}}, source: source, validDecision: d, audit: audit}
}

func TestNativeIngressDenialIsAuditedAndOperationalCauseSurvives(t *testing.T) {
	t.Run("source denial is final and recovers", func(t *testing.T) {
		fixture := newNativeIngressErrorFixture(t, connectorprotocol.ErrIngressDenied)
		denied, err := fixture.service.AuthorizeIngress(context.Background(), fixture.input)
		var denial *DeniedError
		if !errors.As(err, &denial) || denial.Reason != ReasonWrongRoute || denied.Allowed {
			t.Fatalf("ingress denial was not projected as an authorization denial: decision=%+v err=%v", denied, err)
		}
		if len(fixture.audit.records) != 2 || !fixture.audit.records[0].Allowed || fixture.audit.records[1].Allowed || fixture.audit.records[1].Reason != ReasonWrongRoute {
			t.Fatalf("ingress denial audit did not follow accessor authorization: %+v", fixture.audit.records)
		}

		fixture.source.err = nil
		allowed, err := fixture.service.AuthorizeIngress(context.Background(), fixture.input)
		if err != nil || !allowed.Allowed || allowed.Ingress == nil {
			t.Fatalf("valid ingress did not recover: decision=%+v err=%v", allowed, err)
		}
	})

	t.Run("invalid composed authority is final and audited", func(t *testing.T) {
		fixture := newNativeIngressErrorFixture(t, nil)
		fixture.source.decision.Binding.OriginAddress = ""
		denied, err := fixture.service.AuthorizeIngress(context.Background(), fixture.input)
		var denial *DeniedError
		if !errors.As(err, &denial) || denial.Reason != ReasonWrongRoute || denied.Allowed {
			t.Fatalf("invalid composed ingress authority was not denied: decision=%+v err=%v", denied, err)
		}
		if len(fixture.audit.records) != 2 || fixture.audit.records[1].Allowed || fixture.audit.records[1].Reason != ReasonWrongRoute {
			t.Fatalf("composed authority denial audit missing: %+v", fixture.audit.records)
		}
		fixture.source.decision = fixture.validDecision
		allowed, err := fixture.service.AuthorizeIngress(context.Background(), fixture.input)
		if err != nil || !allowed.Allowed || allowed.Ingress == nil {
			t.Fatalf("valid ingress did not recover: decision=%+v err=%v", allowed, err)
		}
	})

	t.Run("mixed denial and IO remains operational and recovers", func(t *testing.T) {
		fixture := newNativeIngressErrorFixture(t, errors.Join(connectorprotocol.ErrIngressDenied, syscall.EIO))
		_, err := fixture.service.AuthorizeIngress(context.Background(), fixture.input)
		if !errors.Is(err, connectorprotocol.ErrIngressDenied) || !errors.Is(err, syscall.EIO) || len(fixture.audit.records) != 1 {
			t.Fatalf("mixed ingress failure lost its operational cause or was audited as a denial: err=%v audit=%+v", err, fixture.audit.records)
		}
		fixture.source.err = nil
		allowed, err := fixture.service.AuthorizeIngress(context.Background(), fixture.input)
		if err != nil || !allowed.Allowed || allowed.Ingress == nil {
			t.Fatalf("valid ingress did not recover: decision=%+v err=%v", allowed, err)
		}
	})
}
