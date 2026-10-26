package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"
)

// N5: two machines on one LAN connected natively only through a relay, because endpoints travelled only
// over it. With each other's endpoints from the signed configuration, two engines that have no relay at
// all connect directly; without them, the dial cannot even start.
func TestPeersWithSignedEndpointsConnectDirectlyWithoutARelay(t *testing.T) {
	dialerKey, hostKey := key.NewNode(), key.NewNode()
	dialerAddr, hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::20"), netip.MustParseAddr("fd7a:115c:a1e0::21")
	host := &Server{Key: hostKey, LocalAddr: hostAddr, AllowedPeers: map[key.NodePublic]netip.Addr{dialerKey.Public(): dialerAddr}, ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) {
		return func(connection ConnPacketConn) {
			buffer := make([]byte, 64)
			for {
				n, err := connection.Read(buffer)
				if err != nil {
					return
				}
				_, _ = connection.Write(buffer[:n])
			}
		}
	}}
	dialer := &Server{Key: dialerKey, LocalAddr: dialerAddr, AllowedPeers: map[key.NodePublic]netip.Addr{hostKey.Public(): hostAddr}, ServedUDPPorts: []filter.PortRange{}}
	for _, server := range []*Server{host, dialer} {
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
	}
	endpoints := func(s *Server) []netip.AddrPort {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var loopback []netip.AddrPort
			for _, ep := range s.LocalEndpoints() {
				if ep.Addr().IsLoopback() || ep.Addr().IsPrivate() {
					loopback = append(loopback, ep)
				}
			}
			if len(loopback) > 0 {
				return loopback[:min(len(loopback), maxPeerContactEndpoints)]
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the engine found no local endpoint")
		return nil
	}
	if err := host.SetPeerContacts(map[key.NodePublic]PeerContact{dialerKey.Public(): {Disco: dialer.lb.discoPublic(), Endpoints: endpoints(dialer)}}); err != nil {
		t.Fatal(err)
	}
	if err := dialer.SetPeerContacts(map[key.NodePublic]PeerContact{hostKey.Public(): {Disco: host.lb.discoPublic(), Endpoints: endpoints(host)}}); err != nil {
		t.Fatal(err)
	}
	// No relay means no CallMeMaybe: the host gets a path back only when its first handshake response finds
	// none and it pings, so the second WireGuard attempt (5 s later) connects, as in Tailscale without DERP.
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	connection, err := dialer.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(hostAddr, 443))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(14 * time.Second))
	if _, err := connection.Write([]byte("direct")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 6)
	if n, err := connection.Read(buffer); err != nil || string(buffer[:n]) != "direct" {
		t.Fatalf("echo %q err %v: no direct path without a relay", buffer[:n], err)
	}
	if path := dialer.PeerPath(hostKey.Public()); path != "direct" {
		t.Fatalf("path %q, want direct", path)
	}
	t.Logf("first echo with no relay after %v", time.Since(started))
}

// An engine knows its own endpoints shortly after Start, every time: magicsock found them before the status
// callback existed and that status was dropped, so in 2 of 10 starts the backend never learned them and had
// nothing to advertise or report (macOS CI, then reproduced on Linux).
func TestEveryStartedEngineLearnsItsEndpoints(t *testing.T) {
	for start := range 10 {
		server := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::40"), AllowedPeers: map[key.NodePublic]netip.Addr{}}
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for len(server.LocalEndpoints()) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		found := len(server.LocalEndpoints())
		_ = server.Close()
		if found == 0 {
			t.Fatalf("start %d: no endpoint known 3 s after Start", start)
		}
	}
}
