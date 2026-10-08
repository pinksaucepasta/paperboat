package derpquic

import (
	"context"
	"net"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func TestRegisteredConnectionCapacity(t *testing.T) {
	f := newFixture(t)
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if server.SetConnectionLimit(0) == nil || server.SetConnectionLimit(MaxConnections+1) == nil {
		t.Fatal("invalid capacity accepted")
	}
	if err = server.SetConnectionLimit(1); err != nil {
		t.Fatal(err)
	}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, socket, f.tls) }()
	defer func() { cancel(); server.Close(); <-done; server.Wait() }()
	eventually(t, server.Ready)
	if server.SetConnectionLimit(2) == nil {
		t.Fatal("live capacity mutation accepted")
	}
	cert := certificate(t)
	grant := f.grant(key.NewNode().Public(), cert, "first")
	first := f.client(socket.LocalAddr().String(), cert, grant)
	if err = first.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	secondCert := certificate(t)
	second := f.client(socket.LocalAddr().String(), secondCert, f.grant(key.NewNode().Public(), secondCert, "second"))
	if err = second.Connect(ctx); err == nil {
		t.Fatal("registered capacity exceeded")
	}
	if err = first.Ping(ctx); err != nil {
		t.Fatal("overload disrupted admitted peer")
	}
}

func TestRegisteredServiceReadiness(t *testing.T) {
	f := newFixture(t)
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	identity := ServiceDescriptor{WireGuardPublicKey: KeyString(key.NewNode().Public()), DiscoPublicKey: DiscoKeyString(key.NewDisco().Public()), VirtualAddress: "fd7a:115c:a1e0::123"}
	if !server.MatchesControlService(nil) || server.MatchesControlService(&identity) {
		t.Fatal("missing service advertised ready")
	}
	if err = server.SetControlService(identity, func(context.Context, ControlRequest) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.DiscoPublicKey = DiscoKeyString(key.NewDisco().Public())
	if server.MatchesControlService(nil) || server.MatchesControlService(&wrong) || !server.MatchesControlService(&identity) {
		t.Fatal("service readiness ignored registered identity")
	}
}
