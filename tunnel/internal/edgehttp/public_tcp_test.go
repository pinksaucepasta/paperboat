package edgehttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func publicTCPDecision(now time.Time, port uint16) connectorprotocol.IngressDecision {
	return connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_tcp", AccountID: "account_replica", TunnelID: "tunnel_replica", Lifecycle: connectorprotocol.TunnelDurable, ResourceGeneration: 1, RouteID: "route_tcp", RouteGeneration: 1, TargetID: "target_tcp", TargetGeneration: 1, HostID: "host_pinned", InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "tcp", Hostname: "tcp.customer.test", OriginScheme: "tcp", OriginAddress: "127.0.0.1:9000", TLSVerification: "not_applicable", PublicationID: "publication_tcp", PublicationGeneration: 1, ListenerID: "listener_tcp", PublicPort: port}, DecisionID: "decision_tcp", PolicyGeneration: 1, EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", ConnectorID: "connector_pinned", SessionID: "session_pinned", ProcessGeneration: 3, ConfigGeneration: 3, AssignmentGeneration: 1, Action: "view", IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(9 * time.Second)}
}

func TestForwardPublicTCPValidatesAuthorityAndPreservesHalfClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	carrier, connector := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(carrier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	decision := publicTCPDecision(now, port)
	current := func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return decision, nil
	}
	forwardDone := make(chan error, 1)
	go func() { forwardDone <- registry.ForwardPublicTCP(context.Background(), accepted, decision, current) }()
	stream, open, err := connector.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if open.Kind != "tcp_public" || open.RouteID != decision.Binding.RouteID {
		t.Fatalf("open=%+v", open)
	}
	if got, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC()); err != nil || got.Binding != decision.Binding {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
	uploadBytes := bytes.Repeat([]byte{0, 255, 13, 10, 1, 128, 70, 73, 78}, 8192)
	replyBytes := []byte{255, 0, 10, 13, 128, 254}
	if _, err := client.Write(uploadBytes); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	upload, err := io.ReadAll(stream)
	if err != nil || !bytes.Equal(upload, uploadBytes) {
		t.Fatalf("upload=%q err=%v", upload, err)
	}
	if _, err := stream.Write(replyBytes); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(client)
	if err != nil || !bytes.Equal(reply, replyBytes) {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	select {
	case err := <-forwardDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("forward did not release")
	}
}

func TestForwardPublicTCPRejectsInvalidTargetBeforeOpeningCarrier(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	carrier, connector := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Attach(carrier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	valid := publicTCPDecision(now, uint16(listener.Addr().(*net.TCPAddr).Port))
	current := func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return valid, nil
	}
	for name, mutate := range map[string]func(*connectorprotocol.IngressDecision){
		"wrong port":   func(d *connectorprotocol.IngressDecision) { d.Binding.PublicPort++ },
		"wrong target": func(d *connectorprotocol.IngressDecision) { d.Binding.TargetID = "target_other" },
		"expired": func(d *connectorprotocol.IngressDecision) {
			d.IssuedAt = now.Add(-11 * time.Second)
			d.ExpiresAt = now.Add(-time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := registry.ForwardPublicTCP(context.Background(), accepted, candidate, current); err == nil {
				t.Fatal("invalid decision accepted")
			}
		})
	}
	acceptCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := connector.AcceptStream(acceptCtx); err == nil {
		t.Fatal("invalid decision reached daemon carrier")
	}
}

func TestPublicTCPAdmittedStreamOutlivesTTLAndIsCutByRevocation(t *testing.T) {
	client, accepted := net.Pipe()
	defer client.Close()
	defer accepted.Close()
	stream, origin := net.Pipe()
	defer stream.Close()
	defer origin.Close()
	now := time.Now().UTC()
	decision := publicTCPDecision(now, 4001)
	decision.IssuedAt = now
	decision.ExpiresAt = now.Add(40 * time.Millisecond)
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: decision.Binding.AccountID, TunnelID: decision.Binding.TunnelID, ConnectorID: decision.ConnectorID, SessionID: decision.SessionID, ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration, RouteID: decision.Binding.RouteID, RequestID: "request_live", Kind: "tcp_public"}
	var revoked atomic.Bool
	current := func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		if revoked.Load() {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		fresh := decision
		fresh.IssuedAt = time.Now().UTC()
		fresh.ExpiresAt = fresh.IssuedAt.Add(40 * time.Millisecond)
		return fresh, nil
	}
	done := make(chan error, 1)
	go func() { done <- bridgePublicTCP(t.Context(), accepted, stream, decision, open, current, 0) }()
	select {
	case <-done:
		t.Fatal("admitted TCP expired")
	case <-time.After(100 * time.Millisecond):
	}
	go func() { _, _ = client.Write([]byte{1}) }()
	_ = origin.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := io.ReadFull(origin, b[:]); err != nil || b[0] != 1 {
		t.Fatal("TCP payload lost after admission expiry")
	}
	revoked.Store(true)
	select {
	case err := <-done:
		if !errors.Is(err, connectorprotocol.ErrIngressDenied) {
			t.Fatal("revocation cause lost")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revocation retained admitted TCP")
	}
}

// tcpPair is a connected loopback TCP pair, so half-close and reset are the kernel's.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.Close() })
	return dialed.(*net.TCPConn), accepted.(*net.TCPConn)
}

// Audit T3: a public TCP stream stays open while data moves either way and is closed cleanly
// (EOF on both peers, never a reset) once both sides are silent for the idle timeout.
func TestPublicTCPIdleTimeoutClosesQuietStreamsCleanly(t *testing.T) {
	client, accepted := tcpPair(t)
	stream, origin := tcpPair(t)
	now := time.Now().UTC()
	decision := publicTCPDecision(now, 4002)
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: decision.Binding.AccountID, TunnelID: decision.Binding.TunnelID, ConnectorID: decision.ConnectorID, SessionID: decision.SessionID, ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration, RouteID: decision.Binding.RouteID, RequestID: "request_idle", Kind: "tcp_public"}
	current := func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		fresh := decision
		fresh.IssuedAt = time.Now().UTC().Add(-time.Second)
		fresh.ExpiresAt = fresh.IssuedAt.Add(10 * time.Second)
		return fresh, nil
	}
	const idle = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- bridgePublicTCP(t.Context(), accepted, stream, decision, open, current, idle) }()

	// Traffic alternating direction every 150 ms keeps it open well past the idle timeout.
	var b [1]byte
	for i := range 6 {
		writer, reader := net.Conn(client), net.Conn(origin)
		if i%2 == 1 {
			writer, reader = origin, client
		}
		if _, err := writer.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		_ = reader.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := io.ReadFull(reader, b[:]); err != nil || b[0] != byte(i) {
			t.Fatalf("byte %d lost: %v", i, err)
		}
		time.Sleep(idle / 2)
	}
	select {
	case err := <-done:
		t.Fatalf("active stream closed: %v", err)
	default:
	}

	// Silence: both peers see a clean EOF and the bridge reports no failure.
	for name, peer := range map[string]net.Conn{"client": client, "origin": origin} {
		_ = peer.SetReadDeadline(time.Now().Add(3 * idle))
		if _, err := peer.Read(b[:]); err != io.EOF {
			t.Fatalf("%s after idle timeout: %v, want a clean EOF", name, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("idle close reported %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge kept an idle stream")
	}
}
