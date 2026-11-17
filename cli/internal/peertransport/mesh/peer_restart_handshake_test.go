package mesh

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// A carrier that restarts keeps its keys and its UDP port, so nothing tells a peer that the WireGuard session it holds
// is gone: the peer's next flow went out on dead keys and wireguard-go only re-handshakes 15 s later. QUIC gave up at
// its 1 s handshake idle timeout and a retry got through (a test VM q1-a -> q1-b, 9 of 10 execs right after the target's
// carrier restarted took 1001 ms + retry; asking for a handshake on each new flow still lost 1 s in 3 of 28, since
// wireguard-go allows one initiation per 5 s). The restarted peer's meowed carries a new start id and the client drops
// its dead session at once (Tailscale's per-start disco key), so every first flow right after a restart gets through
// well within QUIC's 1 s handshake timeout, also when the last handshake was under 5 s ago.
func TestNewFlowToARestartedPeerWithUnchangedContactsIsReachedQuickly(t *testing.T) {
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	region := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	clientKey, hostKey := key.NewNode(), key.NewNode()
	clientAddr := netip.MustParseAddr("fd7a:115c:a1e0::40")
	hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::41")
	start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr) *Server {
		t.Helper()
		s := &Server{Key: k, LocalAddr: addr, Region: region, AllowedPeers: map[key.NodePublic]netip.Addr{peer: peerAddr},
			ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) {
				return func(c ConnPacketConn) {
					defer c.Close()
					var buffer [64]byte
					for {
						n, err := c.Read(buffer[:])
						if err != nil {
							return
						}
						if _, err := c.Write(buffer[:n]); err != nil {
							return
						}
					}
				}
			}}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		// Back on its relay before a peer dials, as a device is seconds after its daemon restarts: a meow ping the
		// relay got before then would be lost, and with it the new start id.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := s.PrepareRelay(ctx, region.RegionID); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The signed contacts stay byte-identical across the restart, as for a carrier that rebinds its port.
	contact := map[key.NodePublic][]netip.AddrPort{
		clientKey.Public(): {netip.MustParseAddrPort("192.0.2.20:41641")},
		hostKey.Public():   {netip.MustParseAddrPort("192.0.2.21:41642")},
	}
	contacts := func(to, from *Server) {
		t.Helper()
		c := PeerContact{Disco: from.lb.discoPublic(), Endpoints: contact[from.Key.Public()]}
		if err := to.SetPeerContacts(map[key.NodePublic]PeerContact{from.Key.Public(): c}); err != nil {
			t.Fatal(err)
		}
	}
	client := start(clientKey, clientAddr, hostKey.Public(), hostAddr)
	host := start(hostKey, hostAddr, clientKey.Public(), clientAddr)
	contacts(client, host)
	contacts(host, client)
	echoWithin(t, client, host, 10*time.Second)
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		host = start(hostKey, hostAddr, clientKey.Public(), clientAddr)
		contacts(host, client)
		echoWithin(t, client, host, time.Second) // the previous echo handshook under 5 s ago
		if err := host.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// The other way round: the host never dialed the client, so it knows the client's start id only from the
	// client's pings. When the client restarts, the host's first flow to it gets through as quickly (a test VM q2-b's
	// first exec to a restarted q2-a lost 1 s while q2-b had only ever been dialed).
	host = start(hostKey, hostAddr, clientKey.Public(), clientAddr)
	contacts(host, client)
	echoWithin(t, client, host, 10*time.Second)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client = start(clientKey, clientAddr, hostKey.Public(), hostAddr)
	contacts(client, host)
	echoWithin(t, host, client, time.Second)
}

// echoWithin opens a new flow (the relay handshake may take until the restarted peer is on its relay) and resends
// (as QUIC would) until the echo arrives, failing when it takes longer than limit after the flow opened.
func echoWithin(t *testing.T, from, to *Server, limit time.Duration) {
	t.Helper()
	dialCtx, cancelDial := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelDial()
	c, err := from.DialAuthorizedUDP(dialCtx, to.Key.Public(), to.lb.discoPublic(), netip.AddrPortFrom(to.Addr(), 443))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	var body [4]byte
	for ctx.Err() == nil {
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if n, err := c.Read(body[:]); err == nil && string(body[:n]) == "ping" {
			return
		}
	}
	t.Fatalf("restarted peer not reached within %v", limit)
}

// A handshake whose first relay send fails (the region's connect is held or in its backoff) sends again after the
// connect backoff, not a whole second later: on a test VM a fresh dialer's first send failed, the answer with the
// restarted peer's start id came 1 s later, and the exec lost QUIC's 1 s handshake.
func TestHandshakeWhoseFirstSendFailedIsAnsweredWithinOneBackoffStep(t *testing.T) {
	envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	t.Cleanup(func() { envknob.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "") })
	region := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	clientKey, hostKey := key.NewNode(), key.NewNode()
	clientAddr := netip.MustParseAddr("fd7a:115c:a1e0::50")
	hostAddr := netip.MustParseAddr("fd7a:115c:a1e0::51")
	start := func(k key.NodePrivate, addr netip.Addr, peer key.NodePublic, peerAddr netip.Addr) *Server {
		s := &Server{Key: k, LocalAddr: addr, Region: region, AllowedPeers: map[key.NodePublic]netip.Addr{peer: peerAddr}}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := s.PrepareRelay(ctx, region.RegionID); err != nil {
			t.Fatal(err)
		}
		return s
	}
	client := start(clientKey, clientAddr, hostKey.Public(), hostAddr)
	host := start(hostKey, hostAddr, clientKey.Public(), clientAddr)
	for _, p := range [][2]*Server{{client, host}, {host, client}} {
		c := PeerContact{Disco: p[1].lb.discoPublic()}
		if err := p[0].SetPeerContacts(map[key.NodePublic]PeerContact{p[1].Key.Public(): c}); err != nil {
			t.Fatal(err)
		}
	}
	// The region's last connect failed: the handshake's first send is refused while the backoff runs.
	_ = client.relayConnects.ensure(t.Context(), region.RegionID, func(context.Context, tailcfg.DERPRegionID) error {
		return errors.New("relay down")
	})
	began := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := client.DialAuthorizedUDP(ctx, hostKey.Public(), host.lb.discoPublic(), netip.AddrPortFrom(hostAddr, 443))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if took := time.Since(began); took > 800*time.Millisecond {
		t.Fatalf("handshake after a failed first send took %v, want within one backoff step", took)
	}
}
