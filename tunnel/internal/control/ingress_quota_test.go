package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

// F13-03: a public route the server withheld only for bandwidth quota is refused with the
// typed reason at admission (never forwarded), and ordinary denial stays generic.
func TestIngressAuthorityRefusesQuotaDeniedRouteWithTypedReasonAndRecovers(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	resets := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	var exhausted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		denied := []connectorprotocol.IngressQuotaDenial{}
		decisions := []connectorprotocol.IngressDecision{}
		if exhausted.Load() {
			denied = append(denied, connectorprotocol.IngressQuotaDenial{RouteID: decision.Binding.RouteID, Hostname: decision.Binding.Hostname, ResetsAt: resets})
		} else {
			decisions = append(decisions, decision)
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions, "quota_denied": denied})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, TunnelID: decision.Binding.TunnelID, AccountID: decision.Binding.AccountID, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration, RouteGeneration: decision.Binding.RouteGeneration, AccessMode: decision.Binding.Audience, ViewerPolicyGeneration: decision.PolicyGeneration, Hostname: decision.Binding.Hostname}
	if _, err := authority.Resolve(context.Background(), rule); err != nil {
		t.Fatalf("route with allowance was refused: %v", err)
	}

	exhausted.Store(true)
	authority.Invalidate()
	if _, err := authority.Resolve(context.Background(), rule); !errors.Is(err, connectorprotocol.ErrIngressQuotaExhausted) || errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("quota-denied route: %v, want ErrIngressQuotaExhausted only", err)
	}
	var quota connectorprotocol.IngressQuotaError
	if _, err := authority.Resolve(context.Background(), rule); !errors.As(err, &quota) || !quota.ResetsAt.Equal(resets) {
		t.Fatalf("quota refusal lost the reset time: %v", err)
	}
	other := rule
	other.Hostname = "unpublished.customer.test"
	if _, err := authority.Resolve(context.Background(), other); !errors.Is(err, connectorprotocol.ErrIngressDenied) || errors.Is(err, connectorprotocol.ErrIngressQuotaExhausted) {
		t.Fatalf("unrelated host: %v, want generic denial", err)
	}

	exhausted.Store(false)
	authority.Invalidate()
	if _, err := authority.Resolve(context.Background(), rule); err != nil {
		t.Fatalf("route stayed refused after allowance recovery: %v", err)
	}
}

// Previews are published through a different projection but get the same typed refusal.
func TestIngressAuthorityRefusesQuotaDeniedPreviewWithTypedReason(t *testing.T) {
	var exhausted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		denied := []connectorprotocol.IngressQuotaDenial{}
		if exhausted.Load() {
			denied = append(denied, connectorprotocol.IngressQuotaDenial{RouteID: "route_preview_01", Hostname: "open-amber-road.preview.test"})
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{}, "quota_denied": denied})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: "edge_01", ProcessEpoch: "epoch_0001"}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: "account_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", SessionID: "session_live_01", ProcessGeneration: 1, Generation: 1, RouteID: "route_preview_01", RequestID: "preview-authority", Kind: "http"}
	resolve := func(host string) error {
		_, err := authority.ResolvePreview(context.Background(), "preview_01", host, 1, open)
		return err
	}
	exhausted.Store(true)
	authority.Invalidate()
	if err := resolve("open-amber-road.preview.test"); !errors.Is(err, connectorprotocol.ErrIngressQuotaExhausted) || errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("quota-denied preview: %v, want ErrIngressQuotaExhausted only", err)
	}
	if err := resolve("other.preview.test"); !errors.Is(err, connectorprotocol.ErrIngressDenied) || errors.Is(err, connectorprotocol.ErrIngressQuotaExhausted) {
		t.Fatalf("unrelated preview: %v, want generic denial", err)
	}
	exhausted.Store(false)
	authority.Invalidate()
	if err := resolve("open-amber-road.preview.test"); errors.Is(err, connectorprotocol.ErrIngressQuotaExhausted) {
		t.Fatalf("preview stayed quota-denied after recovery: %v", err)
	}
}

// A taken-down hostname is withheld with the typed takedown refusal (legal or not), wins over a quota
// denial for the same name, and still counts as withheld for TLS.
func TestIngressAuthorityRefusesBlockedHostname(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{},
			"quota_denied": []connectorprotocol.IngressQuotaDenial{{RouteID: decision.Binding.RouteID, Hostname: decision.Binding.Hostname}},
			"blocked":      []connectorprotocol.IngressBlock{{Hostname: decision.Binding.Hostname, Legal: true}, {Hostname: "other.customer.test"}}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, Hostname: decision.Binding.Hostname}
	var blocked connectorprotocol.PublicationBlockedError
	if _, err := authority.Resolve(context.Background(), rule); !errors.As(err, &blocked) || !blocked.Legal || errors.Is(err, connectorprotocol.ErrIngressQuotaExhausted) {
		t.Fatalf("blocked route: %v", err)
	}
	if !authority.QuotaDenied("other.customer.test") || authority.QuotaDenied("open.customer.test") {
		t.Fatal("withheld set wrong for TLS")
	}
	other := rule
	other.Hostname = "other.customer.test"
	if _, err := authority.Resolve(context.Background(), other); !errors.As(err, &blocked) || blocked.Legal {
		t.Fatalf("non-legal takedown: %v", err)
	}
}

// The owner's IP lists from the snapshot: deny wins, a non-empty allow list admits only its entries,
// IPv4-mapped addresses match IPv4 entries, and a hostname without lists admits everyone.
func TestIngressAuthorityAppliesOwnerIPRules(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision},
			"ip_rules": []connectorprotocol.IngressIPRule{
				{Hostname: "office.customer.test", Allow: []string{"198.51.100.0/24", "2001:db8::/32"}, Deny: []string{"198.51.100.66/32"}},
				{Hostname: "open.customer.test", Deny: []string{"203.0.113.0/24"}},
			}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	if _, err := authority.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host, address string
		want          bool
	}{
		{"office.customer.test", "198.51.100.7", true},
		{"office.customer.test", "::ffff:198.51.100.7", true},
		{"office.customer.test", "2001:db8::5", true},
		{"office.customer.test", "198.51.100.66", false},
		{"office.customer.test", "192.0.2.1", false},
		{"office.customer.test", "not-an-address", false},
		{"open.customer.test", "203.0.113.4", false},
		{"open.customer.test", "192.0.2.1", true},
		{"unlisted.customer.test", "203.0.113.4", true},
	} {
		if got := authority.AddressAllowed(c.host, c.address); got != c.want {
			t.Errorf("%s from %s = %v, want %v", c.host, c.address, got, c.want)
		}
	}
}

// 2026-11-11 anvit: one decision the edge cannot accept (a file-origin preview on an older edge) made it
// reject the whole snapshot, so every name created afterwards 404ed. Now only that decision is dropped,
// the others serve, and the edge does not hold the server's version while it drops one, so the next
// refresh is a full fetch instead of an "unchanged" answer whose expiries would pair with the wrong set.
func TestIngressSnapshotDropsOnlyTheDecisionItCannotAccept(t *testing.T) {
	good := ingressAuthorityTestDecision(time.Now().UTC())
	bad := good
	bad.DecisionID = "decision_bad"
	bad.Binding.RouteID, bad.Binding.Hostname = "route_bad", "bad.customer.test"
	bad.Binding.OriginScheme = "gopher"
	var versions []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		versions = append(versions, input.Version)
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "version": "v1", "decisions": []connectorprotocol.IngressDecision{bad, good}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.IngressDecisions(context.Background(), good.EdgeNodeID, good.EdgeProcessEpoch, "")
	if err != nil || len(snapshot.Decisions) != 1 || snapshot.Decisions[0].Binding.Hostname != good.Binding.Hostname || snapshot.Version != "" {
		t.Fatalf("snapshot = %+v, %v; want only the good decision and no held version", snapshot, err)
	}
	authority := &IngressAuthority{Client: client, NodeID: good.EdgeNodeID, ProcessEpoch: good.EdgeProcessEpoch}
	rule := route.RouteRule{RouteID: good.Binding.RouteID, TunnelID: good.Binding.TunnelID, AccountID: good.Binding.AccountID, ConnectorID: good.ConnectorID, ConnectorSessionID: good.SessionID, ConnectorProcessGeneration: good.ProcessGeneration, ConfigGeneration: good.ConfigGeneration, AssignmentGeneration: good.AssignmentGeneration, RouteGeneration: good.Binding.RouteGeneration, AccessMode: good.Binding.Audience, ViewerPolicyGeneration: good.PolicyGeneration, Hostname: good.Binding.Hostname}
	if _, err := authority.Resolve(context.Background(), rule); err != nil {
		t.Fatalf("the good name is not served next to a bad decision: %v", err)
	}
	authority.Invalidate()
	if _, err := authority.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := versions[len(versions)-1]; last != "" {
		t.Fatalf("refresh sent held version %q while a decision was dropped", last)
	}
}
