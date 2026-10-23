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
	var exhausted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		denied := []connectorprotocol.IngressQuotaDenial{}
		decisions := []connectorprotocol.IngressDecision{}
		if exhausted.Load() {
			denied = append(denied, connectorprotocol.IngressQuotaDenial{RouteID: decision.Binding.RouteID, Hostname: decision.Binding.Hostname})
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
