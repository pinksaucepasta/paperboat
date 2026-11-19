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

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A route whose workspace used its fair-use bandwidth keeps its decision (never refused) and the throttle list
// reaches OnThrottled with the workspace and reset time; recovery empties the list.
func TestIngressAuthorityKeepsThrottledRouteAdmittedAndReportsThrottle(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	resets := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	var throttledNow atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		throttled := []connectorprotocol.IngressThrottle{}
		if throttledNow.Load() {
			throttled = append(throttled, connectorprotocol.IngressThrottle{Hostname: decision.Binding.Hostname, Workspace: "personal:account_1", BytesPerSecond: 125000, ResetsAt: resets})
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}, "throttled": throttled})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var reported []connectorprotocol.IngressThrottle
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch, OnThrottled: func(t []connectorprotocol.IngressThrottle) { reported = t }}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, TunnelID: decision.Binding.TunnelID, AccountID: decision.Binding.AccountID, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration, RouteGeneration: decision.Binding.RouteGeneration, AccessMode: decision.Binding.Audience, ViewerPolicyGeneration: decision.PolicyGeneration, Hostname: decision.Binding.Hostname}
	if _, err := authority.Resolve(context.Background(), rule); err != nil || len(reported) != 0 {
		t.Fatalf("unthrottled route: %v, throttle list %v", err, reported)
	}
	throttledNow.Store(true)
	authority.Invalidate()
	if _, err := authority.Resolve(context.Background(), rule); err != nil {
		t.Fatalf("throttled route was refused: %v", err)
	}
	if len(reported) != 1 || reported[0].Workspace != "personal:account_1" || reported[0].BytesPerSecond != 125000 || !reported[0].ResetsAt.Equal(resets) {
		t.Fatalf("throttle list = %+v", reported)
	}
	throttledNow.Store(false)
	authority.Invalidate()
	if _, err := authority.Resolve(context.Background(), rule); err != nil || len(reported) != 0 {
		t.Fatalf("after recovery: %v, throttle list %v", err, reported)
	}
}

// A taken-down hostname is withheld with the typed takedown refusal (legal or not), and still counts as withheld for TLS.
func TestIngressAuthorityRefusesBlockedHostname(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{},
			"blocked": []connectorprotocol.IngressBlock{{Hostname: decision.Binding.Hostname, Legal: true}, {Hostname: "other.customer.test"}}})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, Hostname: decision.Binding.Hostname}
	var blocked connectorprotocol.PublicationBlockedError
	if _, err := authority.Resolve(context.Background(), rule); !errors.As(err, &blocked) || !blocked.Legal {
		t.Fatalf("blocked route: %v", err)
	}
	if !authority.Withheld("other.customer.test") || authority.Withheld("open.customer.test") {
		t.Fatal("withheld set wrong for TLS")
	}
	other := rule
	other.Hostname = "other.customer.test"
	if _, err := authority.Resolve(context.Background(), other); !errors.As(err, &blocked) || blocked.Legal {
		t.Fatalf("non-legal takedown: %v", err)
	}
}

// A preview whose owner machine sleeps is listed by the server without a decision: the edge reports it
// owner-offline (the "machine offline" page, and TLS for it), and a name no longer listed is not.
func TestIngressAuthorityListsOwnerOfflinePreviews(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	var listed atomic.Bool
	listed.Store(true)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offline := []connectorprotocol.IngressOffline{}
		if listed.Load() {
			offline = append(offline, connectorprotocol.IngressOffline{Hostname: "asleep.preview.test"})
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}, "offline": offline})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	if _, err := authority.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !authority.OwnerOffline("asleep.preview.test") || authority.OwnerOffline("other.preview.test") || authority.AdmitsPreview("asleep.preview.test") {
		t.Fatal("owner-offline set wrong: it must list only the named preview and grant no carrier decision")
	}
	listed.Store(false)
	if _, err := authority.snapshot(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if authority.OwnerOffline("asleep.preview.test") {
		t.Fatal("a preview the server no longer lists as owner-offline is still reported so")
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
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Minute})
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

// 2026-11-11 the test host: one decision the edge cannot accept (a file-origin preview on an older edge) made it
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
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Minute})
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
