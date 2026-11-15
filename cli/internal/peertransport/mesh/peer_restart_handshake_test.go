package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// A carrier that restarts keeps its keys and its UDP port, so nothing tells a peer that the WireGuard session it holds
// is gone: the peer's next flow went out on dead keys and wireguard-go only re-handshakes 15 s later. QUIC gave up at
// its 1 s handshake idle timeout and a retry got through (pbvm2 q1-a -> q1-b, 9 of 10 execs right after the target's
// carrier restarted took 1001 ms + retry). A new flow now asks for a handshake alongside its first packets
// (Tailscale's MarkDevicePeerForHandshake on a peer restart), so the restarted peer is reached within a round trip.
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
	host = start(hostKey, hostAddr, clientKey.Public(), clientAddr)
	contacts(host, client)
	// wireguard-go suppresses a new initiation within RekeyTimeout (5 s) of the last one; the first echo sent one.
	time.Sleep(6 * time.Second)
	echoWithin(t, client, host, 3*time.Second)
}

// echoWithin opens a new flow and resends (as QUIC would) until the echo arrives, failing past limit.
func echoWithin(t *testing.T, from, to *Server, limit time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	c, err := from.DialAuthorizedUDP(ctx, to.Key.Public(), to.lb.discoPublic(), netip.AddrPortFrom(to.Addr(), 443))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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
