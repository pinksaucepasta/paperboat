package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIngressAuthoritySelectsExactMatchedAlias(t *testing.T) {
	now := time.Now().UTC()
	decision := ingressAuthorityTestDecision(now)
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

func ingressAuthorityTestDecision(now time.Time) connectorprotocol.IngressDecision {
	return connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_01", AccountID: "account_01", TunnelID: "tunnel_01", Lifecycle: connectorprotocol.TunnelDurable, ResourceGeneration: 1, RouteID: "route_01", RouteGeneration: 1, TargetID: "target_01", TargetGeneration: 1, HostID: "host_01", InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.customer.test", PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:8080", TLSVerification: "not_applicable", PublicationID: "publication_01", PublicationGeneration: 1}, DecisionID: "decision_01", PolicyGeneration: 1, EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", ConnectorID: "connector_01", SessionID: "session_live_01", ProcessGeneration: 1, ConfigGeneration: 7, AssignmentGeneration: 1, Action: "view", IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(9 * time.Second)}
}

func TestIngressAuthorityInvalidationRefreshesNewRouteAndFencesInFlightSnapshot(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		decisions := []connectorprotocol.IngressDecision{}
		if n == 1 {
			close(started)
			<-release
		} else {
			decisions = append(decisions, decision)
		}
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": decisions})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	snapshotDone := make(chan error, 1)
	go func() { _, err := authority.Snapshot(context.Background()); snapshotDone <- err }()
	<-started
	invalidated := make(chan struct{})
	go func() { authority.Invalidate(); close(invalidated) }()
	// The stale fetch must finish before invalidation can complete.
	select {
	case <-invalidated:
		t.Fatal("invalidation passed an in-flight snapshot")
	default:
	}
	close(release)
	if err := <-snapshotDone; err != nil {
		t.Fatal(err)
	}
	<-invalidated
	rule := route.RouteRule{RouteID: decision.Binding.RouteID, TunnelID: decision.Binding.TunnelID, AccountID: decision.Binding.AccountID, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration, RouteGeneration: decision.Binding.RouteGeneration, AccessMode: decision.Binding.Audience, ViewerPolicyGeneration: decision.PolicyGeneration, Hostname: decision.Binding.Hostname}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := authority.Resolve(context.Background(), rule); err != nil {
				t.Errorf("first promoted route request: %v", err)
			}
		}()
	}
	wait.Wait()
	if calls.Load() != 2 {
		t.Fatalf("refreshes=%d want 2", calls.Load())
	}
	rule.Hostname = "unpublished.customer.test"
	if _, err := authority.Resolve(context.Background(), rule); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("unknown alias: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("unknown alias triggered extra fetch")
	}
}
