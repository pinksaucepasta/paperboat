package edgehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

type publicTCPAuthorityFixture struct {
	mu        sync.Mutex
	decisions []connectorprotocol.IngressDecision
}

func TestPublicTCPListenersBindBeforeReadyObservation(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	authority := &publicTCPAuthorityFixture{}
	routes, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	manager, err := NewPublicTCPListeners(PublicTCPListenerConfig{ListenHost: "127.0.0.1", Authority: authority, Routes: routes, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	rule := route.RouteRule{Kind: route.TunnelTCP, PublicTCPListenerID: "listener_1", PublicTCPPort: uint16(port)}
	if err = manager.PrepareTCPRoutes(t.Context(), []route.RouteRule{rule}); err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("listener was not bound before readiness: %v", err)
	}
	_ = connection.Close()
	authority.set(publicTCPListenerDecision(port, "listener_1"))
	if err = manager.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	preparedUntil := manager.listeners["listener_1"].preparedUntil
	manager.mu.Unlock()
	if !preparedUntil.IsZero() {
		t.Fatal("authoritative decision did not adopt prepared listener")
	}
}

func (f *publicTCPAuthorityFixture) Snapshot(context.Context) ([]connectorprotocol.IngressDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]connectorprotocol.IngressDecision(nil), f.decisions...), nil
}
func (f *publicTCPAuthorityFixture) ResolveDecision(_ context.Context, d connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	return d, nil
}
func (f *publicTCPAuthorityFixture) set(d ...connectorprotocol.IngressDecision) {
	f.mu.Lock()
	f.decisions = d
	f.mu.Unlock()
}

func publicTCPListenerDecision(port int, listener string) connectorprotocol.IngressDecision {
	now := time.Now().UTC()
	return connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_1", AccountID: "account_1", TunnelID: "tunnel_1", Lifecycle: "durable", ResourceGeneration: 1, RouteID: "route_1", RouteGeneration: 1, TargetID: "route_1", TargetGeneration: 1, HostID: "host_1", InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "tcp", Hostname: "11111111-1111-4111-8111-111111111111.tunnels.example.test", OriginScheme: "tcp", OriginAddress: "127.0.0.1:5432", TLSVerification: "not_applicable", PublicationID: "route_1", PublicationGeneration: 1, ListenerID: listener, PublicPort: uint16(port)}, DecisionID: "decision_1", PolicyGeneration: 1, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_12345678", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 1, ConfigGeneration: 1, AssignmentGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(9 * time.Second)}
}

func TestPublicTCPListenersBindPersistAndWithdrawExactReservation(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	authority := &publicTCPAuthorityFixture{}
	authority.set(publicTCPListenerDecision(port, "listener_1"))
	routes, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewPublicTCPListeners(PublicTCPListenerConfig{ListenHost: "127.0.0.1", Authority: authority, Routes: routes, Interval: time.Hour, MaximumListeners: 4, MaximumConnections: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("reserved listener not bound: %v", err)
	}
	_ = connection.Close()
	authority.set()
	if err := manager.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if connection, err = net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("withdrawn listener still accepted a connection")
	}
	authority.set(publicTCPListenerDecision(port, "listener_2"))
	if err := manager.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	_, old := manager.listeners["listener_1"]
	replacement := manager.listeners["listener_2"]
	manager.mu.Unlock()
	if old || replacement == nil {
		t.Fatalf("listener identity was not fenced: old=%v replacement=%v", old, replacement != nil)
	}
}

func TestPublicTCPFatalAcceptWithdrawsOnlyItsIdentityAndRebinds(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	authority := &publicTCPAuthorityFixture{}
	authority.set(publicTCPListenerDecision(port, "listener_1"))
	routes, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer routes.Close()
	type ctxKey struct{}
	reported := make(chan error, 4)
	manager, err := NewPublicTCPListeners(PublicTCPListenerConfig{ListenHost: "127.0.0.1", Authority: authority, Routes: routes, Interval: time.Hour, OnFailure: func(ctx context.Context, phase string, cause error) {
		if phase == "public_tcp_listener" {
			if ctx.Value(ctxKey{}) != "borrowed" {
				t.Error("invocation context lost")
			}
			reported <- cause
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Start(context.WithValue(t.Context(), ctxKey{}, "borrowed")); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	manager.mu.Lock()
	first := manager.listeners["listener_1"]
	manager.mu.Unlock()
	if first == nil {
		t.Fatal("authorized listener missing")
	}
	_ = first.listener.Close()
	select {
	case cause := <-reported:
		if !errors.Is(cause, net.ErrClosed) {
			t.Fatalf("original accept cause lost: %v", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal accept unobserved")
	}
	<-first.done
	manager.mu.Lock()
	remaining := manager.listeners["listener_1"]
	manager.mu.Unlock()
	if remaining != nil {
		t.Fatal("dead listener retained")
	}
	if err = manager.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	replacement := manager.listeners["listener_1"]
	manager.mu.Unlock()
	if replacement == nil || replacement == first {
		t.Fatal("authorized replacement missing")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatal("replacement unusable", err)
	}
	_ = conn.Close()
	// A delayed fatal exit must never erase a replacement already adopted under the same ID.
	staleDone := make(chan struct{})
	stale := &publicTCPListener{listener: first.listener, cancel: func() {}, done: staleDone}
	manager.streams.Add(1)
	go manager.accept(context.WithoutCancel(manager.ctx), stale)
	<-staleDone
	manager.mu.Lock()
	current := manager.listeners["listener_1"]
	manager.mu.Unlock()
	if current != replacement {
		t.Fatal("stale accept erased replacement")
	}
}

func TestPublicTCPObservationKeepsMixedOperationalCause(t *testing.T) {
	observed := 0
	manager := &PublicTCPListeners{cfg: PublicTCPListenerConfig{OnFailure: func(context.Context, string, error) { observed++ }}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager.observe(ctx, "public_tcp_stream", errors.Join(context.Canceled, syscall.EIO))
	manager.observe(ctx, "public_tcp_stream", errors.Join(io.EOF, syscall.EIO))
	manager.observe(ctx, "public_tcp_stream", errors.Join(context.Canceled, io.EOF))
	if observed != 2 {
		t.Fatalf("observed=%d", observed)
	}
}
