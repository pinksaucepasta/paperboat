package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/types/key"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
)

// N5: two machines on one LAN connected natively only through a relay, because endpoints travelled only
// over it. With each other's endpoints from the signed configuration, two engines that have no relay at
// all connect directly; without them, the dial cannot even start.
func TestPeersWithSignedEndpointsConnectDirectlyWithoutARelay(t *testing.T) {
	t.Run("both contacts first", func(t *testing.T) { connectDirectlyWithoutARelay(t, false, false) })
	t.Run("first ping unanswered", func(t *testing.T) { connectDirectlyWithoutARelay(t, true, false) })
	t.Run("inbound peer without advertised endpoints", func(t *testing.T) { connectDirectlyWithoutARelay(t, false, true) })
}

func connectDirectlyWithoutARelay(t *testing.T, lateHost, noInboundEndpoints bool) {
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
	hostContact := func() {
		eps := endpoints(dialer)
		if noInboundEndpoints {
			eps = nil
		}
		if err := host.SetPeerContacts(map[key.NodePublic]PeerContact{dialerKey.Public(): {Disco: dialer.lb.discoPublic(), Endpoints: eps}}); err != nil {
			t.Error(err)
		}
	}
	if lateHost {
		// The host learns the dialer's contact only after the dialer's first ping went unanswered: one ping and no
		// retry left the dial waiting until its deadline (CI run 40464406585).
		time.AfterFunc(time.Second, hostContact)
	} else {
		hostContact()
	}
	if err := dialer.SetPeerContacts(map[key.NodePublic]PeerContact{hostKey.Public(): {Disco: host.lb.discoPublic(), Endpoints: endpoints(host)}}); err != nil {
		t.Fatal(err)
	}
	// No relay means no CallMeMaybe and no DERP fallback: the host's first handshake response finds no trusted
	// path. It goes to the dialer's signed endpoints instead of being dropped until wireguard-go's 5 s retry.
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	connection, err := dialer.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(hostAddr, 443))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(14 * time.Second))
	// Resend like a QUIC Initial until the echo comes back.
	resend := time.NewTicker(200 * time.Millisecond)
	defer resend.Stop()
	go func() {
		for {
			if _, err := connection.Write([]byte("direct")); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-resend.C:
			}
		}
	}()
	buffer := make([]byte, 6)
	if n, err := connection.Read(buffer); err != nil || string(buffer[:n]) != "direct" {
		t.Fatalf("echo %q err %v: no direct path without a relay", buffer[:n], err)
	}
	// The first initiation waits newHandshakeHold by design; a lost response costs wireguard-go's 5 s retry on top.
	// Over 1 s the dialer's QUIC handshake times out and the peer is redialed (peerquic HandshakeTimeout).
	if took := time.Since(started); !lateHost && !noInboundEndpoints && took >= time.Second {
		t.Fatalf("first echo after %v, want under 1 s (hold %v): the host's first handshake response was lost", took, newHandshakeHold)
	}
	if path := dialer.PeerPath(hostKey.Public()); path != "direct" {
		t.Fatalf("path %q, want direct", path)
	}
	t.Logf("first echo with no relay after %v", time.Since(started))
	if lateHost || noInboundEndpoints {
		return
	}
	// An endpoint update after the path is up (ReSTUN for a new peer, a link change) must not cut it. With an
	// empty relay map netcheck reported "IPv4 cannot send", and each update rebound the socket and reset every
	// peer path; under load the reset landed after the host's path and its replies stopped for 14 s (CI).
	for _, server := range []*Server{host, dialer} {
		server.lb.sys.MagicSock.Get().ReSTUN("test")
	}
	for i := range 10 {
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := connection.Read(buffer); err != nil || string(buffer[:n]) != "direct" {
			t.Fatalf("echo %d after an endpoint update: %q err %v", i, buffer[:n], err)
		}
		time.Sleep(100 * time.Millisecond)
	}
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

// Measured under CPU load: a status read before magicsock stored its endpoints
// was stamped after the status that had them, applied last, and erased them
// until magicsock's 20-26 s re-STUN (carrier staging timed out at 25 s).
func TestEngineRelearnsEndpointsAfterALateStaleStatus(t *testing.T) {
	server := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::41"), AllowedPeers: map[key.NodePublic]netip.Addr{}}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	deadline := time.Now().Add(3 * time.Second)
	for len(server.LocalEndpoints()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(server.LocalEndpoints()) == 0 {
		t.Fatal("no endpoint known 3 s after Start")
	}
	server.lb.onEngineStatus(&wgengine.Status{AsOf: time.Now()}, nil)
	deadline = time.Now().Add(2 * time.Second)
	for len(server.LocalEndpoints()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(server.LocalEndpoints()) == 0 {
		t.Fatal("a late stale engine status left the engine without endpoints")
	}
}
