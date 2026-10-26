package datacarrier

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func TestDurableAdmissionRegistryBindsMachineKeyAndCarrierURN(t *testing.T) {
	public := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for i := range public {
		public[i] = byte(i + 11)
	}
	encoded := base64.RawURLEncoding.EncodeToString(public)
	thumbprint, err := connectorprotocol.IdentityThumbprint(public)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{AccountID: "account_1", HostID: "host_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 3, Generation: 7}
	now := time.Date(2026, time.August, 31, 2, 0, 0, 0, time.UTC)
	registry, err := NewDurableAdmissionRegistry(DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "_edgeepoch1"})
	if err != nil {
		t.Fatal(err)
	}
	base := DurableAdmission{Identity: identity, EdgeNodeID: "edge_1", EdgeProcessEpoch: "_edgeepoch1", AssignmentID: "assignment_1", RouteID: "route_1", AssignmentGeneration: 4, RouteGeneration: 2, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MachineIdentityPublicKey: encoded, MachineIdentityThumbprint: thumbprint, State: "active"}
	second := base
	second.AssignmentID = "assignment_2"
	second.RouteID = "route_2"
	if err := registry.Replace([]DurableAdmission{base, second}, now); err != nil {
		t.Fatal(err)
	}
	uri, err := connectorprotocol.CarrierIdentityURN(connectorprotocol.CarrierIdentityBinding{AccountID: identity.AccountID, HostID: identity.HostID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, EdgeProcessEpoch: "_edgeepoch1"})
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{PublicKey: public, URIs: []*url.URL{uri}}
	got, err := registry.PeerBinding(tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}})
	if err != nil || got != identity {
		t.Fatalf("peer identity=%#v error=%v, want %#v", got, err, identity)
	}
	for _, state := range []struct {
		name string
		cert *x509.Certificate
	}{
		{name: "missing URI", cert: &x509.Certificate{PublicKey: public}},
		{name: "wrong URI", cert: &x509.Certificate{PublicKey: public, URIs: []*url.URL{{Scheme: "urn", Opaque: "wrong"}}}},
		{name: "wrong key", cert: &x509.Certificate{PublicKey: ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)), URIs: []*url.URL{uri}}},
	} {
		t.Run(state.name, func(t *testing.T) {
			if _, err := registry.PeerBinding(tls.ConnectionState{PeerCertificates: []*x509.Certificate{state.cert}}); !errors.Is(err, ErrDurableAdmissionMissing) {
				t.Fatalf("error=%v, want missing admission", err)
			}
		})
	}
	open := StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: "route_2", RequestID: "request_1", Kind: "https"}
	if err := registry.AuthorizeStream(context.Background(), identity, open); err != nil {
		t.Fatalf("authorize route: %v", err)
	}
	base.ExpiresAt, second.ExpiresAt = now.Add(time.Second), now.Add(time.Second)
	if err := registry.Replace([]DurableAdmission{base, second}, now); err != nil {
		t.Fatal(err)
	}
	if !registry.HasIdentity(identity, now.Add(2*time.Second)) {
		t.Fatal("admitted carrier withdrawn solely by credential expiry")
	}
	if err := registry.Replace(nil, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if registry.HasIdentity(identity, now.Add(2*time.Second)) {
		t.Fatal("explicitly withdrawn carrier remains authorized")
	}
	open.RouteID = "route_missing"
	if err := registry.AuthorizeStream(context.Background(), identity, open); !errors.Is(err, ErrDurableAdmissionMissing) {
		t.Fatalf("unknown route error=%v, want missing admission", err)
	}
}

func TestDurableAdmissionRegistryKeepsLKGOnInvalidReplacement(t *testing.T) {
	identity := testCarrierIdentity()
	public := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	encoded := base64.RawURLEncoding.EncodeToString(public)
	thumbprint, err := connectorprotocol.IdentityThumbprint(public)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	registry, err := NewDurableAdmissionRegistry(DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "_epoch123"})
	if err != nil {
		t.Fatal(err)
	}
	admission := DurableAdmission{Identity: identity, EdgeNodeID: "edge_1", EdgeProcessEpoch: "_epoch123", AssignmentID: "assignment_1", RouteID: "route_1", AssignmentGeneration: 1, RouteGeneration: 1, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MachineIdentityPublicKey: encoded, MachineIdentityThumbprint: thumbprint, State: "active"}
	if err := registry.Replace([]DurableAdmission{admission}, now); err != nil {
		t.Fatal(err)
	}
	invalid := admission
	invalid.MachineIdentityThumbprint = "wrong"
	if err := registry.Replace([]DurableAdmission{invalid}, now); !errors.Is(err, ErrDurableAdmissionConflict) {
		t.Fatalf("invalid replacement error=%v, want conflict", err)
	}
	for _, hash := range []string{
		"sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
		"sha256:0123456789abcdeg0123456789abcdef0123456789abcdef0123456789abcdef",
		"sha256:0123",
	} {
		invalidHash := admission
		invalidHash.ConfigContentHash = hash
		if err := registry.Replace([]DurableAdmission{invalidHash}, now); !errors.Is(err, ErrDurableAdmissionInvalid) {
			t.Fatalf("invalid hash %q error=%v, want invalid admission", hash, err)
		}
	}
	if got := registry.Snapshot(); len(got) != 1 || got[0].AssignmentID != admission.AssignmentID {
		t.Fatalf("LKG snapshot=%+v", got)
	}
}

func TestComposedCarrierAuthoritiesPreserveAllFailureCausesAndSuccess(t *testing.T) {
	failure := fmt.Errorf("PRIVATE_AUTHORITY: %w", syscall.EIO)
	binding := AnyPeerBinding(func(tls.ConnectionState) (Identity, error) { return Identity{}, failure }, func(tls.ConnectionState) (Identity, error) { return Identity{}, ErrDurableAdmissionMissing })
	_, err := binding(tls.ConnectionState{})
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrDurableAdmissionMissing) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("composed peer authority lost or exposed original cause")
	}
	authorizer := AnyAuthorizer(AuthorizerFunc(func(context.Context, Identity, StreamOpen) error { return failure }), AuthorizerFunc(func(context.Context, Identity, StreamOpen) error { return ErrDurableAdmissionMissing }))
	err = authorizer.AuthorizeStream(t.Context(), testCarrierIdentity(), testStreamOpen("route-a", "request"))
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrDurableAdmissionMissing) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("composed stream authority lost or exposed original cause")
	}
	authorizer = AnyAuthorizer(authorizer, AuthorizerFunc(func(context.Context, Identity, StreamOpen) error { return nil }))
	if err := authorizer.AuthorizeStream(t.Context(), testCarrierIdentity(), testStreamOpen("route-a", "request")); err != nil {
		t.Fatal("valid alternate authority did not recover")
	}
}

// Prod 2026-10-26: a tunnel moving from its connector onto the agent's runtime carrier gets an
// engine assignment (its own generations, starting at 1) for the same route while the edge still
// holds the connector assignment (generation 7) as last-known-good. Both must install; only an
// older generation of the same assignment is a stale replay.
func TestDurableAdmissionFenceIsPerAssignment(t *testing.T) {
	public := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for i := range public {
		public[i] = byte(i + 31)
	}
	encoded := base64.RawURLEncoding.EncodeToString(public)
	thumbprint, err := connectorprotocol.IdentityThumbprint(public)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	registry, err := NewDurableAdmissionRegistry(DurableAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "_epoch123"})
	if err != nil {
		t.Fatal(err)
	}
	connector := DurableAdmission{Identity: Identity{AccountID: "account_1", HostID: "machine_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_c", ProcessGeneration: 1, Generation: 3},
		EdgeNodeID: "edge_1", EdgeProcessEpoch: "_epoch123", AssignmentID: "assignment_connector", RouteID: "route_1", AssignmentGeneration: 7, RouteGeneration: 2,
		ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MachineIdentityPublicKey: encoded, MachineIdentityThumbprint: thumbprint, State: "active"}
	if err := registry.Replace([]DurableAdmission{connector}, now); err != nil {
		t.Fatal(err)
	}
	engine := connector
	engine.Identity = Identity{AccountID: "account_1", HostID: "machine_1", TunnelID: "machine_1", ConnectorID: "machine_1", SessionID: "session_r", ProcessGeneration: 1, Generation: 1}
	engine.AssignmentID, engine.AssignmentGeneration = "epa_route_1", 1
	if err := registry.Replace([]DurableAdmission{connector, engine}, now); err != nil {
		t.Fatalf("engine assignment next to the connector's last-known-good: %v", err)
	}
	stale := connector
	stale.AssignmentGeneration = 6
	if err := registry.Replace([]DurableAdmission{stale, engine}, now); !errors.Is(err, ErrDurableAdmissionConflict) {
		t.Fatalf("older replay of the same assignment: %v, want conflict", err)
	}
}
